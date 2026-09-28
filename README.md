# Stocko2: News-Driven Trading Simulator

Stocko2 collects premarket news on US stocks, scores each article bullish or bearish, and paper-trades the strongest candidates at the market open. It never places real orders: buys and sells are simulated against live Alpaca quotes, 1 share at a time.

Two scripts do the work, and cron runs both:

| Script | Schedule | What it does |
|---|---|---|
| `news_scan.py` | every 2 min, all day | Reads news feeds, scores new articles, saves future events. |
| `sim_trader.py` | every minute, market hours | Checks the market calendar, buys at the open, watches positions, and sells when an exit rule fires. |

Each run does one pass and exits. The two scripts share their state through DuckDB files in `data/`.

---

## Setup (Ubuntu)

```bash
sudo apt install python3-venv
cd ~/Stocko2
python3 -m venv .venv
.venv/bin/pip install alpaca-py feedparser requests beautifulsoup4 lxml vaderSentiment duckdb python-dotenv
# optional, only for --analyzer claude:
.venv/bin/pip install anthropic
mkdir -p logs
```

Create `.env` in the repo root. It's gitignored, so it won't come with a clone.

```
APCA_API_KEY_ID=...
APCA_API_SECRET_KEY=...
SEC_USER_AGENT="Your Name your@email.com"   # required by the SEC, or the 8-K feed is skipped
# optional:
# APCA_DATA_FEED=sip        # sip (default) or iex
# NEWS_ANALYZER=claude      # lexicon (default) or claude
# NEWS_CLAUDE_MODEL=claude-opus-5
# NEWS_CLAUDE_EFFORT=low
```

The machine should be on Eastern time. Check with `timedatectl`; if it's wrong, run `sudo timedatectl set-timezone America/New_York && sudo systemctl restart cron`.

### Cron

Install these with `crontab -e`:

```cron
# News scanner: every 2 minutes, every day
*/2 * * * * cd $HOME/Stocko2 && $HOME/Stocko2/.venv/bin/python news_scan.py >> logs/news_cron.log 2>&1

# Simulated trader: every minute, 9:00-16:59 ET on weekdays
* 9-16 * * 1-5 cd $HOME/Stocko2 && $HOME/Stocko2/.venv/bin/python sim_trader.py >> logs/sim_cron.log 2>&1

# Nightly backup of the two databases
0 18 * * 1-5 cd $HOME/Stocko2 && mkdir -p backups && tar czf backups/data-$(date +\%F).tgz data/news/news.duckdb data/sim/sim.duckdb
```

- **The news scanner runs around the clock.** PRNewswire's feed only keeps its latest 20 releases, so overnight and weekend news would scroll off before a 4am start.
- **The trader's hours can be generous.** It checks the Alpaca calendar itself, so holidays, late opens and early closes need no cron changes.
- **If the machine isn't on Eastern time,** shift the trader's hour range. On Pacific time it's `6-13`. You can also use `* * * * 1-5`; runs outside the session do nothing.
- **Overlapping runs are safe.** Each script exits if its previous run is still going, and the two wait on each other when the news database is locked.

---

## Commands

```bash
.venv/bin/python news_scan.py                  # one scan: fetch, score, save, write the watchlist
.venv/bin/python news_scan.py --json           # also print the watchlist objects
.venv/bin/python news_scan.py --watchlist-only # rebuild the watchlist without fetching
.venv/bin/python news_scan.py --analyzer claude

.venv/bin/python sim_trader.py                 # one trader pass (what cron runs)
.venv/bin/python sim_trader.py --preview       # what would be bought at the next open (read-only)
.venv/bin/python sim_trader.py --report        # win rate and P&L by exit reason and source
```

---

## How it works

### 1. News collection (`news_scan.py`)

1. **Sources:** four RSS feeds (PRNewswire, BusinessWire, GlobeNewswire Technology, Yahoo Finance) and the SEC 8-K Atom feed. The feed list is in `news/config.py`.
2. **Tickers:** matched from tags like `(NASDAQ: ABC)`, `(NYSE American: XYZ)` and `$ABC`, plus `Company (ABC)` in Yahoo headlines, then checked against the SEC's ticker list. Canadian-only listings are dropped. SEC filings map a company's CIK to its ticker. Filings from funds and ETFs are skipped.
3. **Dedupe:** links already seen are skipped, and so is the same headline syndicated to several sites. Anything over 60 new articles per run waits for the next run.
4. **Article text:** each article's page is fetched. For 8-Ks, the press-release exhibit (EX-99.1) is used when there is one.
5. **Scoring:** each ticker in an article gets:
   - **score:** −1 to +1. Bullish at +0.15 or above, bearish at −0.15 or below, neutral in between.
   - **confidence:** 0 to 1.
   - **catalysts:** labels for the kind of news (see below).
   - **timing:** when the effect should hit.
     - `immediate`: the market should react at the next open.
     - `future_event`: the article announces something dated later.
     - `none`: no clear price effect.
6. **Events:** future dates in the article are saved with a type (see "Event types" below).
7. **Watchlist:** `data/news/watchlist_latest.json` and a dated copy list one object per ticker with news since the last market close.

### 2. Two ways to score

- **`lexicon` (default, free).** VADER sentiment with finance words added, combined with the weighted catalyst patterns below. Concrete catalysts outweigh tone, and tone alone is heavily discounted. It's keyword-based and therefore rough: "beat" in an unrelated sentence can trigger `earnings_beat`.
- **`claude` (optional).** It asks Claude for per-ticker direction, score, confidence, catalysts, timing and dated events. An acquirer and its target get separate judgments. It costs money per article, a few dollars a day at premarket volumes, and falls back to the lexicon scorer if a call fails.

### 3. Trading (`sim_trader.py`)

Each run checks the Alpaca calendar first.
- **Closed day:** logged, nothing done.
- **Late open:** no new buys, but existing positions are still watched.
- **Entry window:** from 1 to 15 minutes after the open. The trader buys once, on its first run in that window.

**Picking stocks** (`sim/selection.py`):
1. **Event plays first.** These are events dated today, or on a weekend or holiday since the last session, announced in any earlier article.
   - Only types in `EVENT_TYPES` count: PDUFA, AdCom, data readout, earnings, product launch, investor conference, deal close.
   - The announcing article's strength (score × confidence) must be at least `EVENT_MIN_STRENGTH`.
   - They're skipped if today's news on the stock is bearish.
2. **Then fresh news.** These are `immediate` articles published since the last close, combined per ticker. They must meet `MIN_SCORE` and `MIN_CONFIDENCE`. Their strength must also beat the 80th percentile of all articles scored in the last 30 days; that percentile only applies once there are 100 articles.

**Entry filters:** each candidate must also pass these.
- It trades on NASDAQ, NYSE, AMEX, ARCA or BATS (no OTC).
- Price is at least $1.
- Spread is at most 3%.
- The quote is no older than 2 minutes.
- The gap from the previous close is no more than `MAX_GAP_PCT` (35%).
- The day's cap of 10 new buys hasn't been reached.

Every candidate is logged, rejected ones included, with the reason.

**Simulated buy:** 1 share at the ask. It records:
- the bid, last trade and spread
- previous close and gap %
- premarket volume and high
- the full Alpaca snapshot
- the news behind the pick

**Monitoring:** every run, each position is valued at the bid. Each check records P&L, the peak so far, and order-flow metrics.

**Exits,** in the order they're checked (`sim/exits.py`):

| Rule | Default | Fires when |
|---|---|---|
| `stop_loss` | 4% | P&L ≤ −4% |
| `take_profit` | 6% | P&L ≥ +6% |
| `trailing_stop` | 3% / 2% | the position was up at least 3% and has fallen 2% from its peak |
| `buy_climax` | ×4, 65%, 1.5% | last minute's volume ≥ 4× the session's typical minute, 65%+ of recent volume on upticks, and P&L ≥ +1.5% |
| `max_hold` | 3 days | held for 3 trading days (entry day counts), in the last 10 minutes of the session |

`buy_climax` is a proxy for "a rush of buy orders marks the top". Actual orders aren't visible, so upticks stand in for buyers.

All thresholds are starting guesses and live in `sim/config.py`.

---

## Catalysts

A catalyst is a label the lexicon scorer attaches when an article matches a known pattern of market-moving news. The weights add up to the article's catalyst score, which is capped between −1 and +1. A match in the headline or summary counts at full weight; a match only in the body counts at half.

For a press release, only headline or summary matches can make it `immediate`, because bodies often recap old milestones. For an 8-K the title is boilerplate, so body matches count too.

Patterns and weights are in `news/analyzers.py`.

### Matched from article text

**FDA and biotech**
| Catalyst | Weight | Meaning |
|---|---|---|
| `fda_approval` | +0.7 | The FDA approved or cleared a drug or device. Often the biggest single mover for small biotechs. |
| `fda_designation` | +0.4 | Breakthrough Therapy, Fast Track, Orphan Drug or Priority Review. It speeds up the path to approval but isn't an approval. |
| `fda_rejection` | −0.8 | A Complete Response Letter (CRL) or "refuse to file", meaning the FDA won't approve as submitted. |
| `trial_success` | +0.7 | A trial met its primary endpoint, or the company reported positive topline results or statistically significant data. |
| `trial_failure` | −0.8 | A trial failed to meet its endpoint, a program was discontinued, or the FDA put it on clinical hold. |

**Earnings and guidance**
| Catalyst | Weight | Meaning |
|---|---|---|
| `guidance_raise` | +0.6 | The company raised its revenue or earnings forecast. |
| `guidance_cut` | −0.6 | The company lowered or withdrew its forecast. |
| `earnings_beat` | +0.5 | Results beat or exceeded expectations, or the company reported a record quarter or record revenue. |
| `earnings_miss` | −0.5 | Results came in below consensus or estimates. |

**Deals and capital returns**
| Catalyst | Weight | Meaning |
|---|---|---|
| `acquisition_target` | +0.8 | The company is being bought ("to be acquired", "$X per share in cash", take-private). The stock usually jumps toward the offer price. |
| `merger` | +0.2 | A merger or acquisition agreement that doesn't clearly say which side is being bought. The acquirer often doesn't rise. |
| `buyback` | +0.3 | A share repurchase program. |
| `dividend_up` | +0.3 | A dividend increase or special dividend. |
| `dividend_cut` | −0.5 | A dividend was cut or suspended, often a sign of financial stress. |
| `contract_win` | +0.4 | A contract award, purchase order, or multi-year deal. |
| `partnership` | +0.3 | A strategic partnership, collaboration, or licensing deal. Often more hype than substance. |

**Analysts**
| Catalyst | Weight | Meaning |
|---|---|---|
| `analyst_upgrade` | +0.4 | An upgrade, coverage started with a Buy rating, or a price target raised. |
| `analyst_downgrade` | −0.4 | A downgrade or price target cut. |

**Warning signs** (mostly small-cap)
| Catalyst | Weight | Meaning |
|---|---|---|
| `dilution` | −0.6 | New shares or warrants sold: public offering, registered direct, private placement, at-the-market (ATM) program, or convertible notes. Small caps usually drop even when the release sounds upbeat. |
| `reverse_split` | −0.5 | Shares are combined (for example 1-for-10), usually to stay above an exchange's $1 minimum. Typically a distress signal. |
| `delisting_risk` | −0.5 | The exchange sent a non-compliance or minimum-bid-price notice. |
| `bankruptcy` | −0.9 | Chapter 11, insolvency, or a wind-down. |
| `going_concern` | −0.6 | The auditor doubts the company can survive the next year. |
| `restatement` | −0.6 | Past financials were wrong: a restatement, a "non-reliance" notice, or a material weakness in controls. |
| `legal` | −0.4 | A subpoena, SEC investigation, DOJ involvement, or indictment. |
| `exec_departure` | −0.3 | The CEO or CFO resigned, stepped down, or was terminated. |
| `recall` | −0.4 | A product recall. |
| `cyber_incident` | −0.4 | A data breach, ransomware, or other cybersecurity incident. |
| `shareholder_suit_spam` | −0.15 | Law-firm ads ("investors who lost money…", "class action", "lead plaintiff"). They're about stocks that already dropped, so the tone is ignored and they never count as a trade signal. |

### From SEC 8-K item codes

Every 8-K lists item numbers saying what kind of event it reports. These add their weight even when the filing's wording matches nothing above.

| Item | Catalyst | Weight | Meaning |
|---|---|---|---|
| 1.01 | `material_agreement` | +0.2 | Signed a significant contract. Often just a credit agreement. |
| 1.02 | `agreement_terminated` | −0.3 | A significant contract ended. |
| 1.03 | `bankruptcy` | −0.9 | Bankruptcy or receivership. |
| 1.05 | `cyber_incident` | −0.4 | A material cybersecurity incident. |
| 2.01 | `acquisition_completed` | +0.1 | Finished buying or selling assets. |
| 2.05 | `restructuring` | −0.2 | Layoffs or exit costs. |
| 2.06 | `impairment` | −0.4 | Wrote down the value of assets. |
| 3.01 | `delisting_risk` | −0.5 | Exchange delisting or non-compliance notice. |
| 3.02 | `dilution` | −0.4 | Sold unregistered shares. |
| 4.01 | `auditor_change` | −0.2 | Changed auditors. |
| 4.02 | `restatement` | −0.7 | Past financial statements can't be relied on. |
| 5.02 | `exec_change` | −0.1 | A director or officer joined or left. |

Items weighted under 0.3 either way are **weak**: 1.01, 2.01, 2.05, 4.01 and 5.02. They count toward the score, but can't on their own make an 8-K `immediate`, and so can't make it a trade candidate.

### Event types

Future-dated events are saved with one of these types. Only the ones in `EVENT_TYPES` (in `sim/config.py`) can become event plays.

| Type | Examples | Event play? |
|---|---|---|
| `fda_pdufa` | PDUFA / target action date | yes |
| `fda_adcom` | FDA advisory committee meeting | yes |
| `data_readout` | topline data, interim analysis, data presentation | yes |
| `earnings` | financial results release, earnings call | yes |
| `product_launch` | launch, release date | yes |
| `investor_conference` | conference, fireside chat, investor day, webcast | yes |
| `deal_close` | transaction or offering expected to close | yes |
| `shareholder_vote` | annual or special meeting | no |
| `stock_split` | reverse or forward split effective date | no |
| `dividend` | ex-dividend, record or payable date | no |
| `lockup_expiry` | lock-up expiration | no |

The weights and event choices are judgment calls, not fitted to data. After a week of trades, compare results by catalyst and by source (`--report`, or the `positions` table).

---

## Reading the logs

| File | Contents |
|---|---|
| `logs/news.log` | Everything the news scanner logs. |
| `logs/sim.log` | Everything the trader logs. |
| `logs/news_cron.log`, `logs/sim_cron.log` | What cron captured: the same lines again, plus Python tracebacks if a script crashes. |

The main logs rotate at 5 MB and keep 3 old copies. The cron logs grow until you clear them. Check the cron logs when something seems to have stopped, because a crash only shows up there.

Every line has the same shape:
```
2026-09-28 09:31:02 | INFO     | sim_trader:123 |   BUY  $AAPL ...
   when (machine local time)     level   file:line      message
```
The level is `INFO` for normal activity and `WARNING` when something was skipped or failed but the run carried on.

### news.log

**Per-run summary**
```
177 feed entries, 108 new, 60 to analyze, 33 deferred
```
- **feed entries:** everything currently in the feeds.
- **new:** entries not seen in earlier runs.
- **to analyze:** new entries that name a ticker, aren't duplicates, and are under 24 hours old.
- **deferred:** anything over the 60-per-run cap, which the next run picks up.

`0 new` is normal between releases.

**One line per scored stock**
```
bullish +0.40 immediate    $UNL    SEC 8-K: 8-K - United States 12 Month Natural Gas Fund...
```
Direction, score, timing, ticker, source and headline. Only `immediate` articles can become news trades.

**Future events found in the article**
```
  event 2026-10-01 stock_split $MYPS
```

**Watchlist at the end of each run**
```
Watchlist since Fri 16:00 ET: 60 tickers
  $FGPR   bullish +0.38 conf=0.48 n=1 analyst_upgrade
```
Every stock with news since the last close. `n` is the number of articles and the last column lists the catalysts. It includes all timings and filters nothing, so the trader's actual picks come from `--preview` or `sim.log`.

**Warnings**
| Message | Meaning |
|---|---|
| `Feed failed <url>: 502...` | A feed was down. PRNewswire does this often. The next run tries again. |
| `SEC_USER_AGENT not set in .env` | The 8-K feed is being skipped. Fix `.env`. |
| `SEC 8-K feed failed: Read timed out` | The SEC was slow this time. Only a problem if it keeps happening. |
| `Previous scan still running; skipping this run` | Runs overlapped. Occasional is fine; constant means runs take over 2 minutes. |
| `Calendar lookup failed, using weekday fallback` | Couldn't reach Alpaca, so it guessed the last close. Check keys and network. |

### sim.log

**Start of the day** (one per day)
```
Session 2026-09-28: trading, 09:30-16:00 ET     ← normal day
Session 2026-11-27: trading, 09:30-13:00 ET     ← half day; monitors until 1pm
Late open today; skipping new entries           ← no buys, still watches holdings
Market closed on 2026-11-26; not trading        ← holiday
Missed the entry window; no new entries today   ← first run came after 9:45
```
If a second `Session ...` line appears later the same day, the trader found no record of today in `data/sim/sim.duckdb`, which means that file was deleted or replaced.

**Entry** (once, shortly after 9:31)
```
4 candidates (strength threshold 0.075, news since Fri 16:00)
  BUY  $AAPL   event #1 1 @ 341.3500 (bid 341.2500, spread 0.03%, gap 1.616%, score +0.40 conf 0.50)
  skip $FGPR   news  not_tradable (OTC)
```
The threshold rises as news history builds, because it's the 80th percentile of the last 30 days.

A `BUY` line shows:
- the source: `event` or `news`
- the position ID
- 1 share at the ask
- the bid at the time
- the spread
- the gap from the previous close
- the news score and confidence

A `skip` line gives the reason:

| Reason | Meaning |
|---|---|
| `max_new_positions` | Already bought 10 today. |
| `not_tradable (OTC)` | Not on a major US exchange, or not tradable on Alpaca. |
| `no_quote` / `stale_quote (Ns)` | No live bid/ask, or the quote is over 2 minutes old. |
| `price 0.85 < 1.0` | Under $1. |
| `spread 4.20%` | The gap between bid and ask is too wide to trade fairly. |
| `gap 93.1% > 35.0%` | Already ran too far past the previous close. |
| `gap X% < Y%` | Only appears if `MIN_GAP_PCT` is set. |

**Monitoring** (every minute, per open position)
```
  mark $AAPL   #1 bid 341.2500 pnl -0.03% peak -0.03% vol x0.0 buy 0.678 tpm 14.67 day 1
```
| Field | Meaning |
|---|---|
| `bid` | What you'd get selling now. Positions are always valued at the bid. |
| `pnl` | Profit or loss vs. entry. It starts slightly negative because you bought at the ask; that's the spread cost. |
| `peak` | Best P&L so far. The trailing stop uses it. |
| `vol x` | Last minute's volume as a multiple of the session's typical minute. `x4`+ is a spike. It shows `None` for the first few minutes, until there are 3 bars. |
| `buy` | Share of the last 3 minutes' volume on upticks, a rough stand-in for buyer aggression. 0.5 is balanced and 0.65+ is heavy buying. `None` means no trades. |
| `tpm` | Trades per minute over the last 3 minutes. |
| `day` | Trading days held, counting the entry day as day 1. |

**Exit**
```
  SELL $AAPL   #1 @ 346.1000 trailing_stop pnl +1.39% ($+4.7500) after 1d
```

**Warnings**
| Message | Meaning |
|---|---|
| `no bid for $X, can't mark position #N` | No quote this minute, possibly a trading halt. It retries next run, but no exit can fire while this lasts. |
| `trades failed for $X` / `Minute bars failed` | Alpaca data hiccup. That minute's order-flow numbers are empty. |
| `Batch snapshot failed; retrying per symbol` | One bad ticker broke the batch request, so it fetched them one at a time. |
| `Previous trader run still going; skipping` | A run took over a minute. Occasional is fine. |

### A healthy day

```
news.log   all day      small "N new" counts every 2 min, occasional Feed failed
sim.log    09:25        Session 2026-09-28: trading, 09:30-16:00 ET
           09:31        N candidates / BUY and skip lines
           09:31–16:00  mark lines every minute, SELL lines as exits fire
```

### Handy commands

```bash
tail -f logs/sim.log                                   # watch the trader live
grep -E "BUY|SELL" logs/sim.log                        # every trade
grep "SELL" logs/sim.log | grep -o "[a-z_]* pnl.*"     # exits with reason and P&L
grep "mark \$AAPL" logs/sim.log                        # one stock's minute-by-minute path
grep -E "WARNING|ERROR" logs/*.log                     # anything that went wrong
grep -B2 -A20 Traceback logs/*_cron.log                # crashes
```

---

## Data files

| Path | Contents | Safe to delete? |
|---|---|---|
| `data/news/news.duckdb` | All scored articles, seen links, future events. The population threshold and event plays depend on this history. | **No.** Back it up. |
| `data/sim/sim.duckdb` | Positions, per-minute marks, candidates (including rejects), per-day session records. | **No.** Back it up. |
| `data/news/sec_tickers.json` | SEC ticker list, refreshed daily. | Yes |
| `data/sim/calendar.json` | Alpaca market calendar, refreshed daily. | Yes |
| `data/news/watchlist_*.json` | Watchlist snapshots. | Yes |
| `data/sim/report_latest.json` | Latest `--report` output. | Yes |

Everything under `data/` is gitignored. When copying code updates between machines, don't overwrite `data/`, for example `rsync -av --exclude data/ --exclude logs/ --exclude .venv/ ...`.

For deeper analysis, open the databases directly:
```bash
.venv/bin/python -c "import duckdb; c=duckdb.connect('data/sim/sim.duckdb', read_only=True); print(c.sql('select ticker, source, exit_reason, pnl_pct from positions'))"
```
This can fail with a lock error if the trader is writing at that moment; just retry.

---

## Code layout

```
news_scan.py        cron entry point: news
sim_trader.py       cron entry point: trader (--preview, --report)
logging_config.py   shared logger
news/
  config.py         feeds, analyzer choice, limits
  net.py            HTTP with SEC rate limiting and user agents
  sources.py        RSS and SEC 8-K feeds
  tickers.py        SEC ticker list and ticker extraction
  article.py        article and 8-K exhibit text
  analyzers.py      lexicon and Claude scorers, catalyst tables
  events.py         future-date extraction
  store.py          news database and watchlist
  models.py         data classes
sim/
  config.py         all trading thresholds
  market.py         Alpaca calendar, snapshots, bars, trades
  selection.py      candidate selection
  exits.py          exit rules and order-flow metrics
  store.py          simulation database
```

---

## Known limitations

- **Feeds:** the BusinessWire feed ID in `news/config.py` returns an error channel with no entries and needs replacing. PRNewswire fails intermittently.
- **Scoring:** the lexicon scorer is keyword-based and noisy. The Claude analyzer judges context much better, at a cost.
- **Order flow:** only upticks and volume are visible, not the order book, so `buy_climax` is an approximation.
- **Early data:** the percentile threshold only turns on after 100 scored articles, so the first days use the fixed minimums.
- **Possible addition:** Alpaca's news API, which uses the same keys and tags tickers explicitly.
