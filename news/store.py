import hashlib
import json
import re
import time
from collections import defaultdict
from datetime import datetime, date, timezone

import duckdb

from news.config import DB_PATH
from news.models import NewsItem, ItemAnalysis, StockSignal, direction_for

SCHEMA = """
CREATE TABLE IF NOT EXISTS seen (
    link VARCHAR PRIMARY KEY,
    first_seen TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS articles (
    article_id VARCHAR,
    ticker VARCHAR,
    exchange VARCHAR,
    title_key VARCHAR,
    source VARCHAR,
    title VARCHAR,
    link VARCHAR,
    published TIMESTAMPTZ,
    fetched_at TIMESTAMPTZ,
    score DOUBLE,
    direction VARCHAR,
    confidence DOUBLE,
    catalysts VARCHAR,
    rationale VARCHAR,
    analyzer VARCHAR,
    timing VARCHAR,
    PRIMARY KEY (article_id, ticker)
);
CREATE TABLE IF NOT EXISTS events (
    article_id VARCHAR,
    ticker VARCHAR,
    event_date DATE,
    event_type VARCHAR,
    description VARCHAR,
    link VARCHAR,
    PRIMARY KEY (article_id, ticker, event_date, event_type)
);
"""
MIGRATIONS = ["ALTER TABLE articles ADD COLUMN IF NOT EXISTS timing VARCHAR"]
NON_ALNUM_RE = re.compile(r"[^a-z0-9]+")


def connect(path, read_only: bool = False, timeout: float = 90.0):
    """DuckDB allows one writer process at a time, so wait out the other script's lock."""
    deadline = time.monotonic() + timeout
    while True:
        try:
            return duckdb.connect(str(path), read_only=read_only)
        except duckdb.IOException as e:
            if "lock" not in str(e).lower() or time.monotonic() > deadline:
                raise
            time.sleep(0.5)


def _hash(value: str) -> str:
    return hashlib.sha1(value.encode()).hexdigest()[:16]


def title_key(title: str) -> str:
    # Same release syndicated to PRNewswire, Yahoo, etc. shares a headline but not a link
    return _hash(NON_ALNUM_RE.sub(" ", title.lower()).strip())


class NewsStore:
    def __init__(self, path=DB_PATH, read_only: bool = False):
        self.conn = connect(path, read_only)
        if not read_only:
            self.conn.execute(SCHEMA)
            for m in MIGRATIONS:
                self.conn.execute(m)

    def close(self):
        self.conn.close()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()

    def unseen(self, items: list) -> list:
        links = list({i.link for i in items if i.link})
        if not links:
            return []
        seen = {r[0] for r in self.conn.execute("SELECT link FROM seen WHERE link IN (SELECT UNNEST(?))", [links]).fetchall()}
        out, batch = [], set()
        for item in items:
            if item.link and item.link not in seen and item.link not in batch:
                batch.add(item.link)
                out.append(item)
        return out

    def mark_seen(self, items: list):
        now = datetime.now(timezone.utc)
        rows = [(i.link, now) for i in items if i.link]
        if rows:
            self.conn.executemany("INSERT OR IGNORE INTO seen VALUES (?, ?)", rows)

    def is_duplicate_headline(self, item: NewsItem) -> bool:
        key = title_key(item.title)
        tickers = list(item.tickers)
        row = self.conn.execute(
            "SELECT 1 FROM articles WHERE title_key = ? AND ticker IN (SELECT UNNEST(?)) LIMIT 1", [key, tickers]
        ).fetchone()
        return row is not None

    def save(self, item: NewsItem, analysis: ItemAnalysis):
        article_id = _hash(item.link)
        now = datetime.now(timezone.utc)
        self.conn.executemany(
            """INSERT OR REPLACE INTO articles (article_id, ticker, exchange, title_key, source, title, link, published,
                   fetched_at, score, direction, confidence, catalysts, rationale, analyzer, timing)
               VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
            [
                (article_id, a.ticker, item.tickers.get(a.ticker), title_key(item.title), item.source, item.title,
                 item.link, item.published, now, a.score, a.direction, a.confidence, json.dumps(a.catalysts),
                 a.rationale, analysis.analyzer, a.timing)
                for a in analysis.assessments
            ],
        )
        if analysis.events:
            self.conn.executemany(
                "INSERT OR IGNORE INTO events VALUES (?, ?, ?, ?, ?, ?)",
                [(article_id, e.ticker, e.event_date, e.event_type, e.description, item.link) for e in analysis.events],
            )

    def upcoming_events(self, today: date, tickers=None) -> list:
        query = "SELECT ticker, event_date, event_type, description, link FROM events WHERE event_date >= ?"
        params = [today]
        if tickers is not None:
            query += " AND ticker IN (SELECT UNNEST(?))"
            params.append(list(tickers))
        query += " ORDER BY event_date, ticker"
        return [
            {"ticker": t, "date": d.isoformat(), "type": et, "description": desc, "link": link}
            for t, d, et, desc, link in self.conn.execute(query, params).fetchall()
        ]

    def watchlist(self, since: datetime, today: date, min_confidence: float = 0.0) -> list:
        """Aggregates every analyzed article since `since` into one StockSignal per ticker."""
        rows = self.conn.execute(
            """
            SELECT ticker, exchange, source, title, link, published, score, direction, confidence, catalysts, rationale, timing
            FROM articles WHERE published >= ? ORDER BY published DESC
            """,
            [since],
        ).fetchall()

        by_ticker = defaultdict(list)
        for r in rows:
            by_ticker[r[0]].append(r)

        events = defaultdict(list)
        for e in self.upcoming_events(today, by_ticker.keys()):
            events[e["ticker"]].append({k: v for k, v in e.items() if k != "ticker"})

        signals = []
        for ticker, arts in by_ticker.items():
            weights = [max(a[8], 0.05) for a in arts]
            score = sum(a[6] * w for a, w in zip(arts, weights)) / sum(weights)
            confidence = max(a[8] for a in arts)
            if confidence < min_confidence:
                continue
            catalysts = sorted({c for a in arts for c in json.loads(a[9])})
            signals.append(StockSignal(
                ticker=ticker,
                exchange=next((a[1] for a in arts if a[1]), None),
                score=round(score, 3),
                direction=direction_for(score),
                confidence=round(confidence, 3),
                article_count=len(arts),
                catalysts=catalysts,
                latest_published=arts[0][5].isoformat(),
                articles=[
                    {"title": a[3], "link": a[4], "source": a[2], "published": a[5].isoformat(), "score": a[6],
                     "direction": a[7], "catalysts": json.loads(a[9]), "rationale": a[10], "timing": a[11]}
                    for a in arts
                ],
                upcoming_events=events.get(ticker, []),
            ))
        signals.sort(key=lambda s: (abs(s.score) * s.confidence, s.article_count), reverse=True)
        return signals
