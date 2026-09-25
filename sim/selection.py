import json
from collections import defaultdict
from datetime import datetime, timedelta

from news.store import NewsStore
from sim import config as cfg
from sim.market import Session


def _quantile(values: list, q: float) -> float:
    values = sorted(values)
    if not values:
        return 0.0
    idx = min(len(values) - 1, max(0, int(round(q * (len(values) - 1)))))
    return values[idx]


def _aggregate(rows: list) -> dict:
    weights = [max(r["confidence"], 0.05) for r in rows]
    score = sum(r["score"] * w for r, w in zip(rows, weights)) / sum(weights)
    confidence = max(r["confidence"] for r in rows)
    return {
        "score": round(score, 3),
        "confidence": round(confidence, 3),
        "strength": round(score * confidence, 4),
        "catalysts": sorted({c for r in rows for c in json.loads(r["catalysts"] or "[]")}),
        "exchange": next((r["exchange"] for r in rows if r["exchange"]), None),
        "articles": [{"title": r["title"], "link": r["link"], "source": r["source"], "published": r["published"],
                      "score": r["score"], "timing": r["timing"]} for r in rows],
    }


def _rows(conn, sql: str, params: list) -> list:
    cur = conn.execute(sql, params)
    cols = [d[0] for d in cur.description]
    return [dict(zip(cols, r)) for r in cur.fetchall()]


def select_candidates(session: Session, prev_session: Session, now_et: datetime, held: set):
    """Returns (ordered candidates, strength threshold). Event-day plays come first, then fresh news."""
    with NewsStore(read_only=True) as ns:
        population = [r[0] for r in ns.conn.execute(
            "SELECT score * confidence FROM articles WHERE published >= ? AND score IS NOT NULL",
            [now_et - timedelta(days=cfg.POPULATION_LOOKBACK_DAYS)],
        ).fetchall()]
        fresh = _rows(ns.conn, """
            SELECT ticker, exchange, source, title, link, published, score, confidence, catalysts, timing
            FROM articles WHERE published >= ? AND published <= ?
            ORDER BY published DESC""", [prev_session.close, now_et])
        # Events landing today (or over the weekend/holiday since the last session), announced in any earlier article
        events = _rows(ns.conn, """
            SELECT e.ticker, e.event_date, e.event_type, e.description, e.link,
                   a.score, a.confidence, a.published, a.title, a.exchange
            FROM events e LEFT JOIN articles a ON a.article_id = e.article_id AND a.ticker = e.ticker
            WHERE e.event_date > ? AND e.event_date <= ?
            ORDER BY a.published""", [prev_session.day, session.day])

    floor = cfg.MIN_SCORE * cfg.MIN_CONFIDENCE
    threshold = floor
    if len(population) >= cfg.MIN_POPULATION:
        threshold = max(floor, _quantile(population, cfg.POPULATION_PERCENTILE))

    by_ticker_all, by_ticker_immediate = defaultdict(list), defaultdict(list)
    for r in fresh:
        by_ticker_all[r["ticker"]].append(r)
        if r["timing"] == "immediate":
            by_ticker_immediate[r["ticker"]].append(r)
    fresh_all = {t: _aggregate(rows) for t, rows in by_ticker_all.items()}

    event_cands = {}
    for e in events:
        t = e["ticker"]
        if t in held or e["event_type"] not in cfg.EVENT_TYPES:
            continue
        score, conf = e["score"] or 0.0, e["confidence"] or 0.0
        if score < cfg.EVENT_MIN_SCORE:
            continue
        if t in fresh_all and fresh_all[t]["score"] <= -0.15:
            continue  # today's news turned against the event
        if t in event_cands and event_cands[t]["strength"] >= score * conf:
            continue
        event_cands[t] = {
            "ticker": t, "source": "event", "score": score, "confidence": conf, "strength": round(score * conf, 4),
            "threshold": threshold, "exchange": e["exchange"], "event_type": e["event_type"],
            "event_date": e["event_date"], "event_description": e["description"], "event_link": e["link"],
            "event_announced": e["published"], "catalysts": [e["event_type"]],
            "fresh_news": fresh_all.get(t),
        }

    news_cands = {}
    for t, rows in by_ticker_immediate.items():
        if t in held or t in event_cands:
            continue
        agg = _aggregate(rows)
        if agg["score"] < cfg.MIN_SCORE or agg["confidence"] < cfg.MIN_CONFIDENCE or agg["strength"] < threshold:
            continue
        news_cands[t] = {"ticker": t, "source": "news", "threshold": threshold, "event_type": None, **agg}

    ordered = sorted(event_cands.values(), key=lambda c: c["strength"], reverse=True)
    ordered += sorted(news_cands.values(), key=lambda c: c["strength"], reverse=True)
    return ordered, threshold
