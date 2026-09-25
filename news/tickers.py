import json
import re
import time
from dataclasses import dataclass

from news import net
from news.config import TICKER_CACHE

SEC_TICKERS_URL = "https://www.sec.gov/files/company_tickers_exchange.json"
CACHE_MAX_AGE = 24 * 3600

# Matches "(NASDAQ: ABCD)", "(NYSE American: XYZ)", "NasdaqGS:ABC", "(OTCQB: ABCD)", "(NYSE: BRK.B)"
EXCHANGE_TICKER_RE = re.compile(
    r"\b(?i:(nasdaq|nyse(?:\s+american|\s+arca|\s+mkt)?|amex|cboe|otc(?:qb|qx|\s+markets|\s+pink)?|tsx(?:-?v)?|cse))"
    r"(?i:\s*(?:gs|gm|cm|global\s+select(?:\s+market)?|global\s+market|capital\s+market))?"
    r"\s*:\s*([A-Z]{1,5}(?:\.[A-Z]{1,2})?)\b"
)
CASHTAG_RE = re.compile(r"(?<![\w$])\$([A-Z]{1,5})\b")
# Yahoo/Zacks headlines tag tickers as "Ennis (EBF)"
PAREN_TICKER_RE = re.compile(r"\(([A-Z]{2,5})\)")
ACRONYM_STOPLIST = {"CEO", "CFO", "COO", "CTO", "AI", "IPO", "ETF", "ETFS", "EPS", "GDP", "USA", "US", "EU", "UK",
                    "IT", "FED", "EV", "EVS", "ESG", "AM", "PM", "TV", "PR", "FDA", "SEC", "DOJ", "FTC", "IRS", "NFL"}
NON_US_EXCHANGES = ("tsx", "cse")


@dataclass
class TickerMap:
    by_cik: dict  # {'0000320193': 'AAPL'}
    exchange: dict  # {'AAPL': 'Nasdaq'}

    def normalize(self, symbol: str) -> str:
        return symbol.upper().replace(".", "-")

    def is_valid(self, symbol: str) -> bool:
        return self.normalize(symbol) in self.exchange


def load_ticker_map() -> TickerMap:
    """Loads the SEC CIK/ticker/exchange table, re-downloading at most once a day."""
    if not TICKER_CACHE.exists() or time.time() - TICKER_CACHE.stat().st_mtime > CACHE_MAX_AGE:
        try:
            resp = net.get(SEC_TICKERS_URL)
            resp.raise_for_status()
            TICKER_CACHE.write_text(resp.text)
        except Exception:
            if not TICKER_CACHE.exists():
                raise  # no stale cache to fall back on

    raw = json.loads(TICKER_CACHE.read_text())
    fields = raw["fields"]
    by_cik, exchange = {}, {}
    for row in raw["data"]:
        rec = dict(zip(fields, row))
        cik = str(rec["cik"]).zfill(10)
        ticker = rec["ticker"].upper()
        by_cik.setdefault(cik, ticker)  # first listing per CIK is the primary share class
        exchange.setdefault(ticker, rec.get("exchange"))
    return TickerMap(by_cik=by_cik, exchange=exchange)


def extract_tickers(text: str, ticker_map: TickerMap, parenthetical: bool = False) -> dict:
    """Returns {ticker: exchange} for US-listed symbols explicitly tagged in the text."""
    found = {}
    for exch, symbol in EXCHANGE_TICKER_RE.findall(text):
        if exch.lower().startswith(NON_US_EXCHANGES):
            continue
        if ticker_map.is_valid(symbol):
            sym = ticker_map.normalize(symbol)
            found[sym] = ticker_map.exchange.get(sym)
    loose = CASHTAG_RE.findall(text)
    if parenthetical:
        loose += [s for s in PAREN_TICKER_RE.findall(text) if s not in ACRONYM_STOPLIST]
    for symbol in loose:
        if ticker_map.is_valid(symbol):
            sym = ticker_map.normalize(symbol)
            found.setdefault(sym, ticker_map.exchange.get(sym))
    return found
