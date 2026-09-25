"""Single-pass premarket news scan, meant to be run by cron every ~2 minutes."""
import argparse
import fcntl
import json
import sys
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta, timezone, time as dtime
from zoneinfo import ZoneInfo

from logging_config import setup_logger
from news.analyzers import make_analyzer
from news.article import fetch_text
from news.config import ANALYZER, DATA_DIR, LOCK_PATH, MAX_ITEM_AGE_HOURS, FETCH_WORKERS
from news.sources import fetch_rss, fetch_sec_8k
from news.store import NewsStore, title_key
from news.tickers import load_ticker_map

ET = ZoneInfo("America/New_York")
logger = setup_logger(name="news_scan", log_file="news.log")


def session_start(now_et: datetime) -> datetime:
    """Close of the previous trading session (Alpaca calendar), falling back to the last weekday 4pm ET."""
    try:
        from sim.market import news_window_start
        return news_window_start(now_et)
    except Exception as e:
        logger.warning(f"Calendar lookup failed, using weekday fallback: {e}")
    d = now_et.date()
    if now_et.time() < dtime(16, 0):
        d -= timedelta(days=1)
    while d.weekday() >= 5:
        d -= timedelta(days=1)
    return datetime.combine(d, dtime(16, 0), tzinfo=ET)


def scan(analyzer, use_rss=True, use_sec=True, max_articles=60) -> int:
    ticker_map = load_ticker_map()
    items = (fetch_rss(ticker_map, logger) if use_rss else []) + (fetch_sec_8k(ticker_map, logger) if use_sec else [])

    # The DB is only held open briefly so the trader process can read it between our writes
    cutoff = datetime.now(timezone.utc) - timedelta(hours=MAX_ITEM_AGE_HOURS)
    candidates, batch_keys = [], set()
    with NewsStore() as store:
        new_items = store.unseen(items)
        for item in sorted(new_items, key=lambda i: i.published, reverse=True):
            key = (title_key(item.title), tuple(sorted(item.tickers)))
            if item.tickers and item.published >= cutoff and key not in batch_keys and not store.is_duplicate_headline(item):
                batch_keys.add(key)
                candidates.append(item)
    deferred = candidates[max_articles:]  # left unseen so the next run picks them up
    candidates = candidates[:max_articles]
    deferred_links = {i.link for i in deferred}
    logger.info(f"{len(items)} feed entries, {len(new_items)} new, {len(candidates)} to analyze, {len(deferred)} deferred")

    today = datetime.now(ET).date()

    def work(item):
        return item, analyzer.analyze(item, fetch_text(item), today)

    with ThreadPoolExecutor(max_workers=FETCH_WORKERS) as pool:
        results = list(pool.map(work, candidates))

    with NewsStore() as store:
        for item, analysis in results:
            store.save(item, analysis)
            for a in analysis.assessments:
                logger.info(f"{a.direction:>7} {a.score:+.2f} {a.timing:<12} ${a.ticker:<6} {item.source}: {item.title[:80]}")
            for e in analysis.events:
                logger.info(f"  event {e.event_date} {e.event_type} ${e.ticker}")
        store.mark_seen([i for i in new_items if i.link not in deferred_links])
    return len(results)


def write_watchlist(store: NewsStore, since: datetime, min_confidence: float) -> list:
    today = datetime.now(ET).date()
    signals = store.watchlist(since, today, min_confidence)
    payload = {
        "generated_at": datetime.now(ET).isoformat(),
        "session_start": since.isoformat(),
        "signals": [s.to_dict() for s in signals],
        "upcoming_events": store.upcoming_events(today),
    }
    (DATA_DIR / f"watchlist_{today.isoformat()}.json").write_text(json.dumps(payload, indent=2, default=str))
    (DATA_DIR / "watchlist_latest.json").write_text(json.dumps(payload, indent=2, default=str))
    return signals


def parse_args():
    parser = argparse.ArgumentParser(description="Premarket news/sentiment scan (one pass).")
    parser.add_argument("--analyzer", choices=["lexicon", "claude"], default=ANALYZER)
    parser.add_argument("--no-rss", action="store_true", help="Skip press-release RSS feeds")
    parser.add_argument("--no-sec", action="store_true", help="Skip the SEC 8-K feed")
    parser.add_argument("--max-articles", type=int, default=60, help="Cap on articles analyzed per run")
    parser.add_argument("--min-confidence", type=float, default=0.0)
    parser.add_argument("--watchlist-only", action="store_true", help="Rebuild the watchlist without fetching")
    parser.add_argument("--json", action="store_true", help="Print the watchlist JSON to stdout")
    return parser.parse_args()


def main():
    args = parse_args()
    lock = open(LOCK_PATH, "w")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        logger.warning("Previous scan still running; skipping this run")
        return 0

    if not args.watchlist_only:
        scan(make_analyzer(args.analyzer, logger), not args.no_rss, not args.no_sec, args.max_articles)
    since = session_start(datetime.now(ET))
    with NewsStore() as store:
        signals = write_watchlist(store, since, args.min_confidence)

    if args.json:
        print(json.dumps([s.to_dict() for s in signals], indent=2, default=str))
    else:
        logger.info(f"Watchlist since {since:%a %H:%M ET}: {len(signals)} tickers")
        for s in signals[:25]:
            logger.info(f"  ${s.ticker:<6} {s.direction:>7} {s.score:+.2f} conf={s.confidence:.2f} "
                        f"n={s.article_count} {','.join(s.catalysts)[:60]}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
