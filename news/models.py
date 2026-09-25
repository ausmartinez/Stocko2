from dataclasses import dataclass, field, asdict
from datetime import datetime, date
from typing import Optional


@dataclass
class NewsItem:
    source: str
    title: str
    link: str
    published: datetime  # tz-aware UTC
    summary: str = ""
    tickers: dict = field(default_factory=dict)  # {ticker: exchange}
    sec_items: list = field(default_factory=list)  # 8-K item codes, e.g. ["2.02", "9.01"]


@dataclass
class Event:
    ticker: str
    event_date: date
    event_type: str
    description: str


@dataclass
class TickerAssessment:
    ticker: str
    score: float  # -1 (bearish) .. +1 (bullish)
    direction: str  # bullish | bearish | neutral
    confidence: float  # 0..1
    catalysts: list = field(default_factory=list)
    rationale: str = ""
    timing: str = "none"  # immediate (moves at next open) | future_event | none


@dataclass
class ItemAnalysis:
    analyzer: str
    assessments: list  # [TickerAssessment]
    events: list  # [Event]


@dataclass
class StockSignal:
    ticker: str
    exchange: Optional[str]
    score: float
    direction: str
    confidence: float
    article_count: int
    catalysts: list
    latest_published: Optional[str]
    articles: list  # [{title, link, source, published, score, direction, catalysts, rationale}]
    upcoming_events: list  # [{date, type, description, link}]

    def to_dict(self) -> dict:
        return asdict(self)


def direction_for(score: float, threshold: float = 0.15) -> str:
    if score >= threshold:
        return "bullish"
    if score <= -threshold:
        return "bearish"
    return "neutral"
