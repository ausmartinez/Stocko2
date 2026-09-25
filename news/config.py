import os
from pathlib import Path
from dotenv import load_dotenv

ROOT = Path(__file__).resolve().parent.parent
load_dotenv(ROOT / ".env")

DATA_DIR = ROOT / "data" / "news"
DATA_DIR.mkdir(parents=True, exist_ok=True)
DB_PATH = DATA_DIR / "news.duckdb"
TICKER_CACHE = DATA_DIR / "sec_tickers.json"
LOCK_PATH = DATA_DIR / ".scan.lock"

# SEC blocks requests without a real contact User-Agent, e.g. "Jane Doe jane@example.com"
SEC_USER_AGENT = os.getenv("SEC_USER_AGENT", "")
# GlobeNewswire and PRNewswire stall on browser UAs but serve feedparser's
DEFAULT_USER_AGENT = "feedparser/6.0 +https://github.com/kurtmckee/feedparser/"

RSS_FEEDS = [
    "https://www.prnewswire.com/rss/news-releases-list.rss",
    "https://feed.businesswire.com/rss/home/?rss=G1QFDERJXkJeGVtRXw==",
    "https://www.globenewswire.com/RssFeed/industry/8500-Technology/feedTitle/GlobeNewswire%20-%20Industry%20News%20on%20Technology",
    "https://finance.yahoo.com/news/rssindex",
]
SEC_8K_FEED = "https://www.sec.gov/cgi-bin/browse-edgar?action=getcurrent&type=8-k&count=100&output=atom"

ANALYZER = os.getenv("NEWS_ANALYZER", "lexicon")  # lexicon | claude
CLAUDE_MODEL = os.getenv("NEWS_CLAUDE_MODEL", "claude-opus-5")
CLAUDE_EFFORT = os.getenv("NEWS_CLAUDE_EFFORT", "low")

MAX_ARTICLE_CHARS = 20000  # caps body text sent to analyzers; press releases rarely exceed this
MAX_ITEM_AGE_HOURS = 24  # older feed entries are marked seen but not analyzed
FETCH_WORKERS = 8
HTTP_TIMEOUT = 20
