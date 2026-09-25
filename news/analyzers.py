import json
import math
import re
from datetime import date

from vaderSentiment.vaderSentiment import SentimentIntensityAnalyzer

from news.config import CLAUDE_MODEL, CLAUDE_EFFORT
from news.events import extract_events
from news.models import NewsItem, ItemAnalysis, TickerAssessment, Event, direction_for

# (label, pattern, weight) - concrete catalysts dominate the score; generic tone is secondary
CATALYSTS = [
    ("fda_approval", r"FDA (?:approv|clear)|receives? (?:FDA )?(?:approval|clearance)|granted (?:accelerated )?approval", 0.7),
    ("fda_designation", r"breakthrough (?:therapy )?designation|fast track|orphan drug designation|priority review", 0.4),
    ("fda_rejection", r"complete response letter|\bCRL\b|refuse to file", -0.8),
    ("trial_success", r"met (?:its |the )?primary endpoint|positive (?:topline|top-line|phase|interim|results)|statistically significant", 0.7),
    ("trial_failure", r"(?:did not|fail(?:s|ed)? to) (?:meet|achieve)|discontinu\w+ (?:the )?(?:trial|study|program|development)|clinical hold", -0.8),
    ("guidance_raise", r"rais(?:es|ed|ing) (?:its )?(?:full[- ]year |fiscal |annual )?(?:\w+ )?(?:guidance|outlook|forecast)", 0.6),
    ("guidance_cut", r"(?:lower|cut|reduc)(?:s|ed|ing)? (?:its )?(?:full[- ]year |fiscal |annual )?(?:\w+ )?(?:guidance|outlook|forecast)|withdr[ae]w\w* (?:its )?guidance", -0.6),
    ("earnings_beat", r"\bbeats?\b|exceed(?:s|ed)? (?:consensus|expectations|estimates)|above (?:consensus|expectations|estimates)|record (?:revenue|quarter|sales)", 0.5),
    ("earnings_miss", r"miss(?:es|ed)? (?:consensus|expectations|estimates)|below (?:consensus|expectations|estimates)", -0.5),
    ("acquisition_target", r"to be acquired|definitive agreement to be acquired|per share in cash|take[- ]private|go[- ]private", 0.8),
    ("merger", r"merger agreement|definitive agreement to acquire|to acquire|business combination", 0.2),
    ("buyback", r"share repurchase|stock repurchase|buyback", 0.3),
    ("dividend_up", r"(?:increas|rais)\w* (?:its |the )?(?:quarterly )?dividend|special dividend", 0.3),
    ("dividend_cut", r"suspend\w* (?:its |the )?dividend|(?:cut|reduc)\w* (?:its |the )?dividend", -0.5),
    ("contract_win", r"award(?:ed|s)? (?:a |an )?(?:\$[\d.]+ ?(?:million|billion) )?contract|purchase order|selected by|multi-year (?:agreement|contract)", 0.4),
    ("partnership", r"strategic (?:partnership|collaboration|alliance)|partners? with|collaboration agreement|licensing agreement", 0.3),
    ("analyst_upgrade", r"upgrade[sd]?\b|initiat\w+ (?:coverage )?with (?:a )?(?:buy|outperform|overweight)|raises? price target", 0.4),
    ("analyst_downgrade", r"downgrade[sd]?\b|cuts? price target|lowers? price target", -0.4),
    ("dilution", r"public offering|registered direct|private placement|at[- ]the[- ]market|pricing of (?:\$[\d.]+ ?(?:million|billion) )?(?:offering|shares)|convertible (?:notes|debentures)|warrants? to purchase", -0.6),
    ("reverse_split", r"reverse (?:stock )?split", -0.5),
    ("delisting_risk", r"delist\w*|notice of non-?compliance|minimum bid price|deficiency (?:letter|notice)", -0.5),
    ("bankruptcy", r"chapter 11|bankruptcy|insolven\w+|wind[- ]down", -0.9),
    ("going_concern", r"going concern", -0.6),
    ("restatement", r"restate\w*|non-reliance|material weakness", -0.6),
    ("legal", r"subpoena|\bSEC investigation|\bDOJ\b|indict\w+", -0.4),
    ("shareholder_suit_spam", r"class action|investors (?:who (?:lost|purchased)|have opportunity)|securities (?:fraud )?(?:lawsuit|litigation)|lead plaintiff|law firm|shareholder rights", -0.15),
    ("exec_departure", r"(?:CEO|chief executive|CFO|chief financial)\w* (?:\w+ )?(?:resign|step(?:s|ping)? down|depart|terminat)", -0.3),
    ("recall", r"\brecall\w*", -0.4),
    ("cyber_incident", r"cyber(?:security)? incident|data breach|ransomware", -0.4),
]
CATALYST_RES = [(label, re.compile(p, re.I), w) for label, p, w in CATALYSTS]

# 8-K item codes carry a prior independent of the wording
SEC_ITEM_PRIORS = {
    "1.01": ("material_agreement", 0.2),
    "1.02": ("agreement_terminated", -0.3),
    "1.03": ("bankruptcy", -0.9),
    "1.05": ("cyber_incident", -0.4),
    "2.01": ("acquisition_completed", 0.1),
    "2.05": ("restructuring", -0.2),
    "2.06": ("impairment", -0.4),
    "3.01": ("delisting_risk", -0.5),
    "3.02": ("dilution", -0.4),
    "4.01": ("auditor_change", -0.2),
    "4.02": ("restatement", -0.7),
    "5.02": ("exec_change", -0.1),
}

# Headlines announcing something scheduled rather than something that happened
FUTURE_HEADLINE_RE = re.compile(
    r"\b(?:to|will) (?:report|present|host|announce|participate|hold|release|webcast|ring)\b"
    r"|\bschedul\w+|\bupcoming\b|\bconference call\b|\bto be held\b",
    re.I,
)
NON_IMMEDIATE_LABELS = {"shareholder_suit_spam"}
# Routine 8-K items (credit agreements, officer changes...) only count toward score, not as a catalyst on their own
WEAK_SEC_LABELS = {label for label, w in SEC_ITEM_PRIORS.values() if abs(w) < 0.3}

# Finance-specific tone words, plus neutralizing medical terms that make biotech releases read as negative
VADER_UPDATES = {
    "beats": 2.0, "surpasses": 2.0, "outperform": 1.5, "underperform": -1.5, "upgrade": 1.8,
    "upgraded": 1.8, "downgrade": -1.8, "downgraded": -1.8, "dilution": -2.0, "dilutive": -2.0,
    "bankruptcy": -3.0, "delisting": -2.5, "bullish": 2.0, "bearish": -2.0, "surge": 1.8,
    "surges": 1.8, "soars": 2.0, "plunge": -2.0, "plunges": -2.0, "tumbles": -2.0, "misses": -1.5,
    "approval": 1.8, "approved": 1.5, "halted": -1.5, "lawsuit": -1.5, "recall": -1.8, "layoffs": -1.2,
    "cancer": 0.0, "tumor": 0.0, "tumors": 0.0, "disease": 0.0, "diseases": 0.0, "death": 0.0,
    "deaths": 0.0, "pain": 0.0, "infection": 0.0, "disorder": 0.0, "fatal": 0.0, "risk": 0.0,
    "risks": 0.0, "uncertainties": 0.0,
}


class LexiconAnalyzer:
    name = "lexicon"

    def __init__(self):
        self.vader = SentimentIntensityAnalyzer()
        self.vader.lexicon.update(VADER_UPDATES)

    def _tone(self, item: NewsItem, text: str) -> float:
        title = self.vader.polarity_scores(item.title)["compound"]
        # Forward-looking-statement boilerplate sits at the bottom, so only score the lead
        lead = self.vader.polarity_scores(text[:2000])["compound"]
        return 0.65 * title + 0.35 * lead

    def _catalysts(self, item: NewsItem, text: str):
        labels, headline_labels, total = [], [], 0.0
        headline = f"{item.title} {item.summary}"
        body = text[:4000]
        for label, pattern, weight in CATALYST_RES:
            if pattern.search(headline):
                labels.append(label)
                headline_labels.append(label)
                total += weight
            elif pattern.search(body):
                labels.append(label)
                total += weight * 0.5  # body-only mentions are weaker evidence
        for code in item.sec_items:
            if code in SEC_ITEM_PRIORS:
                label, weight = SEC_ITEM_PRIORS[code]
                if label not in labels:
                    labels.append(label)
                    headline_labels.append(label)
                    total += weight
        return labels, headline_labels, math.tanh(total)

    @staticmethod
    def _timing(item: NewsItem, labels, headline_labels) -> str:
        concrete = [l for l in labels if l not in NON_IMMEDIATE_LABELS and l not in WEAK_SEC_LABELS]
        concrete_headline = [l for l in headline_labels if l not in NON_IMMEDIATE_LABELS]
        if FUTURE_HEADLINE_RE.search(item.title) and not concrete_headline:
            return "future_event"
        # Press-release bodies recap old milestones; 8-K titles are boilerplate so their body has to count
        if item.sec_items or item.source == "SEC 8-K":
            return "immediate" if concrete else "none"
        return "immediate" if concrete_headline else "none"

    def analyze(self, item: NewsItem, text: str, today: date) -> ItemAnalysis:
        labels, headline_labels, catalyst = self._catalysts(item, text)
        timing = self._timing(item, labels, headline_labels)
        # Law-firm solicitations are upbeat boilerplate about stocks that already moved
        tone = 0.0 if "shareholder_suit_spam" in labels else self._tone(item, text)
        # Without a concrete catalyst, tone alone is a weak signal
        score = 0.35 * tone + 0.65 * catalyst if labels else 0.4 * tone
        confidence = min(1.0, abs(score) + (0.1 if labels else 0.0))
        assessments = [
            TickerAssessment(
                ticker=t, score=round(score, 3), direction=direction_for(score),
                confidence=round(confidence, 3), catalysts=labels,
                rationale=f"tone={tone:+.2f} catalyst={catalyst:+.2f}", timing=timing,
            )
            for t in item.tickers
        ]
        return ItemAnalysis(self.name, assessments, extract_events(text, item.tickers, today))


CLAUDE_SYSTEM = """You analyze breaking premarket news for US-listed stocks and judge the likely direction of each stock's price move after the next market open, relative to the prior close.

Weigh what traders actually react to: concrete, surprising, material information (FDA decisions, trial results, guidance changes, M&A terms, large contracts, offerings/dilution, delisting or bankruptcy risk) matters far more than promotional tone. Routine announcements, conference attendance, and law-firm "investigation" solicitations usually move the stock very little. For small caps, financings and warrant deals are usually negative even when framed positively. When a release involves an acquirer and a target, assess them separately.

Score each ticker from -1 (strongly bearish) to +1 (strongly bullish); confidence from 0 to 1 reflects how sure you are the market will move in that direction. Use "neutral" when the news is immaterial or ambiguous.

Set timing to "immediate" when the market should react at the next open (something already happened or was just disclosed), "future_event" when the main price effect is tied to a later date (e.g. an announcement that results will be reported next week), and "none" when there is no meaningful price effect.

Also list future-dated events the article mentions that could move the stock on that date (earnings releases, PDUFA dates, advisory committees, data readouts, shareholder votes, split effective dates, deal closings, lock-up expirations, investor presentations). Only include events with a specific calendar date after today; use YYYY-MM-DD."""

CLAUDE_SCHEMA = {
    "type": "object",
    "properties": {
        "assessments": {
            "type": "array",
            "items": {
                "type": "object",
                "properties": {
                    "ticker": {"type": "string"},
                    "direction": {"type": "string", "enum": ["bullish", "bearish", "neutral"]},
                    "score": {"type": "number"},
                    "confidence": {"type": "number"},
                    "catalysts": {"type": "array", "items": {"type": "string"}},
                    "rationale": {"type": "string"},
                    "timing": {"type": "string", "enum": ["immediate", "future_event", "none"]},
                },
                "required": ["ticker", "direction", "score", "confidence", "catalysts", "rationale", "timing"],
                "additionalProperties": False,
            },
        },
        "events": {
            "type": "array",
            "items": {
                "type": "object",
                "properties": {
                    "ticker": {"type": "string"},
                    "date": {"type": "string"},
                    "event_type": {"type": "string"},
                    "description": {"type": "string"},
                },
                "required": ["ticker", "date", "event_type", "description"],
                "additionalProperties": False,
            },
        },
    },
    "required": ["assessments", "events"],
    "additionalProperties": False,
}


class ClaudeAnalyzer:
    name = "claude"

    def __init__(self, logger):
        import anthropic  # optional dependency, only needed for this analyzer

        self.client = anthropic.Anthropic()
        self.logger = logger
        self.fallback = LexiconAnalyzer()

    def analyze(self, item: NewsItem, text: str, today: date) -> ItemAnalysis:
        prompt = (
            f"Today is {today.isoformat()}. Source: {item.source}. Published: {item.published.isoformat()}.\n"
            f"Tickers to assess: {', '.join(item.tickers)}\n"
            + (f"SEC 8-K items: {', '.join(item.sec_items)}\n" if item.sec_items else "")
            + f"\n<article>\n{text}\n</article>"
        )
        try:
            response = self.client.beta.messages.create(
                model=CLAUDE_MODEL,
                max_tokens=16000,
                betas=["server-side-fallback-2026-07-01"],
                fallbacks="default",
                output_config={"effort": CLAUDE_EFFORT, "format": {"type": "json_schema", "schema": CLAUDE_SCHEMA}},
                system=CLAUDE_SYSTEM,
                messages=[{"role": "user", "content": prompt}],
            )
            if response.stop_reason in ("refusal", "max_tokens"):
                raise RuntimeError(f"stop_reason={response.stop_reason}")
            data = json.loads(next(b.text for b in response.content if b.type == "text"))
        except Exception as e:
            self.logger.warning(f"Claude analysis failed for {item.link}, using lexicon: {e}")
            return self.fallback.analyze(item, text, today)

        wanted = set(item.tickers)
        assessments = []
        for a in data["assessments"]:
            ticker = a["ticker"].upper().replace(".", "-")
            if ticker not in wanted:
                continue
            score = max(-1.0, min(1.0, float(a["score"])))
            assessments.append(TickerAssessment(
                ticker=ticker, score=round(score, 3), direction=a["direction"],
                confidence=round(max(0.0, min(1.0, float(a["confidence"]))), 3),
                catalysts=a["catalysts"], rationale=a["rationale"], timing=a["timing"],
            ))
        events = []
        for e in data["events"]:
            ticker = e["ticker"].upper().replace(".", "-")
            try:
                event_date = date.fromisoformat(e["date"])
            except ValueError:
                continue
            if ticker in wanted and event_date > today:
                events.append(Event(ticker, event_date, e["event_type"], e["description"][:300]))
        return ItemAnalysis(self.name, assessments, events)


def make_analyzer(kind: str, logger):
    return ClaudeAnalyzer(logger) if kind == "claude" else LexiconAnalyzer()
