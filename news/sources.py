import calendar
import re
import time
from datetime import datetime, timezone

import feedparser

from news import net
from news.config import RSS_FEEDS, SEC_8K_FEED, SEC_USER_AGENT
from news.models import NewsItem
from news.tickers import TickerMap, extract_tickers

CIK_RE = re.compile(r"\((\d{10})\)")
# Commodity pools and ETFs file boilerplate 8-Ks that aren't company news
FUND_FILER_RE = re.compile(r"\b(?:funds?|etf|etn|etp)\b", re.I)
SEC_ITEM_RE = re.compile(r"Item\s+(\d\.\d{2})")
TAG_RE = re.compile(r"<[^>]+>")


def _entry_time(entry) -> datetime:
    parsed = entry.get("published_parsed") or entry.get("updated_parsed")
    if parsed:
        return datetime.fromtimestamp(calendar.timegm(parsed), tz=timezone.utc)
    return datetime.now(timezone.utc)


def _get_feed(url: str, attempts: int = 3):
    for attempt in range(attempts):
        try:
            resp = net.get(url)
            resp.raise_for_status()
            return resp
        except Exception:
            if attempt == attempts - 1:
                raise
            time.sleep(1 + attempt)  # PRNewswire intermittently 404/502s


def fetch_rss(ticker_map: TickerMap, logger, feeds=RSS_FEEDS) -> list:
    items = []
    for url in feeds:
        try:
            resp = _get_feed(url)
        except Exception as e:
            logger.warning(f"Feed failed {url}: {e}")
            continue
        feed = feedparser.parse(resp.content)
        source = feed.feed.get("title", url)
        for entry in feed.entries:
            title = entry.get("title", "")
            summary = TAG_RE.sub(" ", entry.get("summary", ""))
            items.append(NewsItem(
                source=source,
                title=title,
                link=entry.get("link", ""),
                published=_entry_time(entry),
                summary=summary,
                tickers=extract_tickers(f"{title} {summary}", ticker_map, parenthetical="yahoo" in url),
            ))
    return items


def fetch_sec_8k(ticker_map: TickerMap, logger) -> list:
    if not SEC_USER_AGENT:
        logger.error("SEC_USER_AGENT not set in .env; skipping SEC 8-K feed")
        return []
    try:
        resp = net.get(SEC_8K_FEED, timeout=40)  # this endpoint is often slow
        resp.raise_for_status()
    except Exception as e:
        logger.warning(f"SEC 8-K feed failed: {e}")
        return []

    items = []
    for entry in feedparser.parse(resp.content).entries:
        title = entry.get("title", "")
        if FUND_FILER_RE.search(title):
            continue
        cik_match = CIK_RE.search(title)
        ticker = ticker_map.by_cik.get(cik_match.group(1)) if cik_match else None
        if not ticker:
            continue  # private funds, debt-only filers, shells
        summary = entry.get("summary", "")
        items.append(NewsItem(
            source="SEC 8-K",
            title=title,
            link=entry.get("link", ""),
            published=_entry_time(entry),
            summary=TAG_RE.sub(" ", summary),
            tickers={ticker: ticker_map.exchange.get(ticker)},
            sec_items=SEC_ITEM_RE.findall(summary),
        ))
    return items
