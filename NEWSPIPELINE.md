I built a single-pass news scanner that cron can run every 2 minutes. I ran it live about five times: it pulls the feeds, scores new articles, remembers what it has already seen between runs, and writes a watchlist. `sec8.py` and `newspipeline.py` are unchanged.

**How to run it**
```
python3 news_scan.py                  # fetch, analyze, write the watchlist
python3 news_scan.py --json           # also print the stock objects to stdout
python3 news_scan.py --watchlist-only # rebuild the watchlist without fetching
```

**What one run does**
1. **Reads the feeds**: your four RSS feeds plus the SEC 8-K feed. Tickers are matched from tags like `(NASDAQ: ABC)`, `$ABC`, and Yahoo's `Company (ABC)` style, then checked against the SEC's ticker list. Canadian-only exchanges are dropped.
2. **Skips anything already processed**, including the same press release appearing on two sites. If a run is still going when the next one starts, the new one just exits.
3. **Fetches the article text.** For 8-Ks it grabs the press-release exhibit (EX-99.1) when there is one.
4. **Scores each stock** from −1 (bearish) to +1 (bullish), with a confidence and a list of catalysts.
5. **Looks for future dated events** (PDUFA dates, earnings dates, shareholder votes, split dates, deal closings, conferences, dividends) and saves them.
6. **Writes the watchlist** to `data/news/watchlist_latest.json` and a dated copy. It has one object per ticker (score, direction, confidence, catalysts, the articles, upcoming events) plus a list of all saved future events. It covers news since the last weekday 4pm ET close. Other code can call `NewsStore().watchlist(...)` to get the same objects directly.

**Two ways to score**
- **Default (free, no setup):** a general sentiment tool (VADER) with finance words added, plus about 30 weighted catalyst phrases (FDA approval, trial failure, guidance raise or cut, offerings, reverse splits, being acquired, and so on) and a starting bias for each 8-K item type. In my tests these came out right: offerings bearish, FDA approval and buyouts bullish, conference announcements neutral.
- **Optional, `--analyzer claude`:** asks Claude to judge each ticker separately (an acquirer and its target get different calls) and to pull out event dates. It uses `claude-opus-5` at low effort, and a server-side fallback model is turned on in case of a refusal. To use it:
  - Run `pip install anthropic`.
  - It costs money per article. Expect a few dollars a day at premarket volumes.
  - I confirmed the request format against the current SDK but haven't run it against the live API, because there's no API key here.
  - If a call fails, it falls back to the default scoring.

**Things to know**
- **Add a line to `.env`:** `SEC_USER_AGENT="Your Name your@email.com"`. The SEC blocks requests without a real contact, so the 8-K feed is skipped until this is set.
- **Your BusinessWire feed URL is broken.** It returns "channel unavailable" with no entries, so you'll want a new feed ID.
- **PRNewswire is flaky.** It returns 502/404 at random. Each run retries it, and anything missed gets picked up by the next run.
- **The SEC feed is slow**, about 10 seconds per request. A full run took around 40 seconds.
- **Scores from the default method are rough.** Most 8-Ks come out about +0.1 from general tone alone. The strong signals come from actual catalysts, so judge by confidence and catalysts, not score alone.
- **Tone is ignored for law-firm lawsuit ads.** A "Securities Lawsuit" solicitation scored bullish from its upbeat wording, so the default method now sets tone to zero for those.
- **Test data was left in place.** My test runs populated `data/news/`, which is already gitignored. It's safe to delete if you want a clean start.

**Cron line.** Your Mac is on Pacific time, so 4:00–9:58am ET is 1:00–6:58 PT:
```
*/2 1-6 * * 1-5 cd /Users/austin/Stocko2 && /Users/austin/miniforge3/bin/python news_scan.py >> logs/news_cron.log 2>&1
```

**Where things are:** `news_scan.py` is the entry point. The rest is in `news/`: `sources.py` for feeds, `article.py` for fetching text, `analyzers.py` for scoring, `events.py` for future dates, `store.py` for storage and the watchlist, and `config.py` for the feed list and settings. Results are stored in `data/news/news.duckdb`.

A possible next step: your `.env` already has Alpaca keys, and Alpaca's news API tags tickers explicitly. It would be the most reliable source to add.
