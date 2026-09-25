import json
import os
from dataclasses import dataclass
from datetime import date, datetime, time as dtime, timedelta
from functools import lru_cache
from pathlib import Path
from zoneinfo import ZoneInfo

from dotenv import load_dotenv
from alpaca.trading.client import TradingClient
from alpaca.trading.requests import GetCalendarRequest
from alpaca.data.historical import StockHistoricalDataClient
from alpaca.data.requests import StockSnapshotRequest, StockTradesRequest, StockBarsRequest
from alpaca.data.enums import DataFeed
from alpaca.data.timeframe import TimeFrame

ROOT = Path(__file__).resolve().parent.parent
load_dotenv(ROOT / ".env")

ET = ZoneInfo("America/New_York")
REGULAR_OPEN = dtime(9, 30)
CALENDAR_CACHE = ROOT / "data" / "sim" / "calendar.json"
TRADABLE_EXCHANGES = {"NASDAQ", "NYSE", "AMEX", "ARCA", "BATS"}
FEED = DataFeed(os.getenv("APCA_DATA_FEED", "sip"))


@dataclass
class Session:
    day: date
    open: datetime  # tz-aware ET
    close: datetime

    @property
    def normal_open(self) -> bool:
        return self.open.time() == REGULAR_OPEN


@lru_cache(maxsize=1)
def trading_client() -> TradingClient:
    return TradingClient(os.getenv("APCA_API_KEY_ID"), os.getenv("APCA_API_SECRET_KEY"), paper=True)


@lru_cache(maxsize=1)
def data_client() -> StockHistoricalDataClient:
    return StockHistoricalDataClient(os.getenv("APCA_API_KEY_ID"), os.getenv("APCA_API_SECRET_KEY"))


def _load_calendar() -> list:
    """Trading sessions from ~60 days back to ~30 ahead, cached on disk per day."""
    today = datetime.now(ET).date()
    if CALENDAR_CACHE.exists():
        cached = json.loads(CALENDAR_CACHE.read_text())
        if cached.get("fetched") == today.isoformat():
            return cached["sessions"]
    cal = trading_client().get_calendar(GetCalendarRequest(start=today - timedelta(days=60), end=today + timedelta(days=30)))
    sessions = [[c.date.isoformat(), c.open.strftime("%H:%M"), c.close.strftime("%H:%M")] for c in cal]
    CALENDAR_CACHE.parent.mkdir(parents=True, exist_ok=True)
    CALENDAR_CACHE.write_text(json.dumps({"fetched": today.isoformat(), "sessions": sessions}))
    return sessions


def sessions() -> list:
    out = []
    for d, o, c in _load_calendar():
        day = date.fromisoformat(d)
        out.append(Session(
            day=day,
            open=datetime.combine(day, dtime.fromisoformat(o), tzinfo=ET),
            close=datetime.combine(day, dtime.fromisoformat(c), tzinfo=ET),
        ))
    return out


def session_for(day: date):
    return next((s for s in sessions() if s.day == day), None)


def previous_session(day: date):
    return next((s for s in reversed(sessions()) if s.day < day), None)


def trading_days_between(start: date, end: date) -> int:
    """Sessions in [start, end], so a same-day hold counts as day 1."""
    return sum(1 for s in sessions() if start <= s.day <= end)


def news_window_start(now_et: datetime) -> datetime:
    """Close of the last session that ended before `now`, so the window spans after-hours + premarket."""
    closed = [s for s in sessions() if s.close <= now_et]
    return closed[-1].close


def asset_info(symbol: str):
    try:
        a = trading_client().get_asset(symbol)
    except Exception:
        return None
    return {
        "exchange": a.exchange.value if a.exchange else None,
        "tradable": a.tradable,
        "shortable": a.shortable,
        "easy_to_borrow": a.easy_to_borrow,
        "fractionable": a.fractionable,
    }


def snapshots(symbols: list) -> dict:
    if not symbols:
        return {}
    return data_client().get_stock_snapshot(StockSnapshotRequest(symbol_or_symbols=symbols, feed=FEED))


def minute_bars(symbols: list, start: datetime, end: datetime) -> dict:
    if not symbols:
        return {}
    bars = data_client().get_stock_bars(StockBarsRequest(
        symbol_or_symbols=symbols, timeframe=TimeFrame.Minute, start=start, end=end, feed=FEED,
    ))
    return bars.data


def trades(symbol: str, start: datetime, end: datetime, limit: int = 50000) -> list:
    result = data_client().get_stock_trades(StockTradesRequest(
        symbol_or_symbols=symbol, start=start, end=end, feed=FEED, limit=limit,
    ))
    return result.data.get(symbol, [])
