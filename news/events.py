import re
from datetime import date, timedelta

from news.models import Event

MONTHS = {
    "jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
    "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}
MONTH_DATE_RE = re.compile(
    r"\b(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|june?|july?|aug(?:ust)?|sept?(?:ember)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\.?"
    r"\s+(\d{1,2})(?:st|nd|rd|th)?(?:,?\s+(\d{4}))?\b",
    re.IGNORECASE,
)
NUMERIC_DATE_RE = re.compile(r"\b(\d{1,2})/(\d{1,2})/(\d{2,4})\b")
ISO_DATE_RE = re.compile(r"\b(\d{4})-(\d{2})-(\d{2})\b")
# "quarter ended September 30" / "as of December 31" describe periods, not events
PERIOD_PREFIX_RE = re.compile(r"(ended|ending|through|as of|since|expir\w*|matur\w*)\s*$", re.IGNORECASE)
SENTENCE_RE = re.compile(r"(?<=[.!?])\s+(?=[A-Z])")

# First match wins, so more specific types come first
EVENT_TYPES = [
    ("fda_pdufa", re.compile(r"\bPDUFA\b|target action date", re.I)),
    ("fda_adcom", re.compile(r"advisory committee", re.I)),
    ("data_readout", re.compile(r"topline|top-line|data readout|interim (?:data|analysis)|late-breaking|present(?:s|ed)? (?:new |additional )?data", re.I)),
    ("earnings", re.compile(r"financial results|earnings (?:call|release|report)|report(?:s)? (?:its )?(?:first|second|third|fourth|q[1-4]|full[- ]year|fiscal)", re.I)),
    ("shareholder_vote", re.compile(r"(?:annual|special) meeting|shareholders? meeting|stockholders? meeting", re.I)),
    ("stock_split", re.compile(r"reverse (?:stock )?split|stock split", re.I)),
    ("dividend", re.compile(r"ex-dividend|record date|payable on", re.I)),
    ("deal_close", re.compile(r"expected to close|closing of the (?:transaction|merger|acquisition|offering)|expected to be completed", re.I)),
    ("lockup_expiry", re.compile(r"lock-?up", re.I)),
    ("investor_conference", re.compile(r"\bconference\b|fireside chat|investor day|\bsummit\b|webcast", re.I)),
    ("product_launch", re.compile(r"\blaunch(?:es|ing)?\b|\brelease date\b|available (?:on|beginning)", re.I)),
]


def _resolve(month: int, day: int, year, today: date):
    try:
        if year:
            year = int(year)
            if year < 100:
                year += 2000
            return date(year, month, day)
        d = date(today.year, month, day)
        # Year-less dates well in the past most likely refer to next year (e.g. "January 15" said in December)
        return d.replace(year=today.year + 1) if d < today - timedelta(days=7) else d
    except ValueError:
        return None


def _dates_in(sentence: str, today: date):
    for m in MONTH_DATE_RE.finditer(sentence):
        if PERIOD_PREFIX_RE.search(sentence[:m.start()]):
            continue
        yield _resolve(MONTHS[m.group(1).lower()[:3]], int(m.group(2)), m.group(3), today)
    for m in NUMERIC_DATE_RE.finditer(sentence):
        if not PERIOD_PREFIX_RE.search(sentence[:m.start()]):
            yield _resolve(int(m.group(1)), int(m.group(2)), m.group(3), today)
    for m in ISO_DATE_RE.finditer(sentence):
        if not PERIOD_PREFIX_RE.search(sentence[:m.start()]):
            yield _resolve(int(m.group(2)), int(m.group(3)), m.group(1), today)


def extract_events(text: str, tickers, today: date, horizon_days: int = 365) -> list:
    """Finds future-dated catalysts (earnings, PDUFA, votes, splits...) mentioned in the text."""
    events, seen = [], set()
    for sentence in SENTENCE_RE.split(text):
        if len(sentence) > 600:
            sentence = sentence[:600]
        event_type = next((name for name, pattern in EVENT_TYPES if pattern.search(sentence)), None)
        if not event_type:
            continue
        for d in _dates_in(sentence, today):
            if d is None or not (today < d <= today + timedelta(days=horizon_days)):
                continue
            for ticker in tickers:
                key = (ticker, d, event_type)
                if key not in seen:
                    seen.add(key)
                    events.append(Event(ticker=ticker, event_date=d, event_type=event_type, description=sentence.strip()[:300]))
    return events
