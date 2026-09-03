# Stocko2 — Deployment (Ubuntu)

Paper only. `main.go` hardcodes `paper-api.alpaca.markets` and submits no orders.

Assumes `/home/austin/stocko2` — adjust paths throughout if you use another.

## 1. Setup

Ubuntu's `golang-go` package is usually older than the required 1.26.3, so install from the tarball:

```bash
curl -LO https://go.dev/dl/go1.27.0.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.27.0.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.profile && . ~/.profile
go version    # must be >= 1.26.3
```

```bash
cd /home/austin/stocko2
go build -o stocko2 .     # not `go run` from cron: recompiles, needs Go on PATH
cat .env                  # APCA_API_KEY_ID, APCA_API_SECRET_KEY
mkdir -p logs
./stocko2                 # first run writes config.json — read it before scheduling
```

Then smoke-test each mode: `./stocko2 -track` (market hours), `-outcomes` (after 16:00 ET), `-export`, `-sweep`, `-swing`, `-swing-sweep`.

Config keys that bite:

| Key | Why |
|---|---|
| `scanner.enabled` | `false` makes every mode a silent no-op |
| `scanner.feed` | `iex` is free but has almost no pre-market; `sip` is $99/mo |
| `scanner.entry_minutes_from_open` | Must match your first `-track` tick (§4) |
| `scanner.data_dir` | Relative — resolves against the working directory |

## 2. Timezone

Set the box to Eastern so crontab times are market times and DST is handled for you:

```bash
sudo timedatectl set-timezone America/New_York
sudo systemctl restart cron
timedatectl        # confirm the zone and that NTP sync is active
```

If you can't change the system zone, put `CRON_TZ=America/New_York` at the top of the crontab instead — Ubuntu's cron honours it.

**Don't run a plain UTC crontab.** ET is UTC-4 in summer and UTC-5 in winter, so every entry would need re-editing twice a year, and a missed edit silently shifts the 15:55 flatten tick out of its window (§4).

## 3. Working directory is mandatory

`config.json`, `app.log`, `watchlist.json`, `data/` and `.env` are all **relative**. Cron's cwd is `$HOME`. Get it wrong and the bot silently builds a second config and data tree there with no credentials.

Use `&&`, not `;` — a failed `cd` must abort, not run in the wrong place.

## 4. Crontab

Times below are **Eastern**, matching §2. Market = 09:30–16:00 ET.

```cron
MAILTO=""

# ─── Stocko2 (Eastern times; market 09:30-16:00) ───

# Pre-market scan
0 9 * * 1-5 cd /home/austin/stocko2 && ./stocko2 >> logs/scan.log 2>&1

# Open scan. Writes open.json, which the tracker reads.
31 9 * * 1-5 cd /home/austin/stocko2 && ./stocko2 >> logs/scan.log 2>&1

# Tracking: first tick opens the book at 09:35
35,40,45,50,55 9 * * 1-5 cd /home/austin/stocko2 && ./stocko2 -track >> logs/track.log 2>&1
# then every 5 min to 15:55 — that last tick flattens the book
*/5 10-15 * * 1-5 cd /home/austin/stocko2 && ./stocko2 -track >> logs/track.log 2>&1

# Score the session
30 16 * * 1-5 cd /home/austin/stocko2 && ./stocko2 -outcomes >> logs/outcomes.log 2>&1

# CSV
45 16 * * 1-5 cd /home/austin/stocko2 && ./stocko2 -export >> logs/export.log 2>&1

# Intraday target sweep (local files only)
0 17 * * 1-5 cd /home/austin/stocko2 && ./stocko2 -sweep >> logs/sweep.log 2>&1

# Swing scoring. Idempotent; horizons fill in as days elapse.
15 17 * * 1-5 cd /home/austin/stocko2 && ./stocko2 -swing >> logs/swing.log 2>&1

# Swing grid (local files only)
30 17 * * 1-5 cd /home/austin/stocko2 && ./stocko2 -swing-sweep >> logs/swing-sweep.log 2>&1
```

Install with `crontab -e`. `MAILTO=""` stops cron mailing every run's output to a box with no MTA.

For dated logs, cron needs `%` escaped: `` `date +\%Y\%m\%d` ``.

## 5. Why those times

- **The 15:55 tick is mandatory.** `tracker.go` flattens when `nowMinutes >= sessionMinutes - CloseAllMinutesBeforeClose`. No `-track` between **15:55 and 15:59** means positions never close. After 16:00 the session rolls forward and `-track` exits with "has not opened yet".
- **Missed mid-session ticks self-heal.** Each tick pulls minute bars since `LastSampledAt`, so a target touched during a gap is still caught. Only the flatten tick is timing-critical.
- **Open scan before first tick.** If the 09:31 scan hasn't written `open.json`, the tracker silently falls back to the pre-market list. Check `app.log` for `outcomes: scoring the <phase> scan`; if it says `premarket`, push the track start later.
- **Keep `entry_minutes_from_open` honest.** The tracker enters on its first tick; that config field only affects `-outcomes`. First tick 09:35 → leave it at `5`.

## 6. Holidays and half days

The code reads Alpaca's calendar; cron doesn't.

- **Holidays** — scans write a `premarket` file for the next open session. Harmless.
- **Half days** (13:00 close) — the 12:55 tick flattens correctly. Ticks from 13:00–15:55 then log "has not opened yet". Filter the log; don't narrow the cron window or you lose the flatten.

## 7. Output files

| Stream | Where | Notes |
|---|---|---|
| Diagnostics | `app.log` | Written by the app itself; grows forever |
| Tables | stdout | `Print*` functions — needs the `>>` redirects, or it's discarded |
| Dataset | `data/`, `watchlist.json` | Gitignored; never pruned |

```
watchlist.json                     latest scan
data/<date>/premarket.json open.json      scans
            ledger.json                   paper book, rewritten each tick
            outcome.json                  intraday scoring (read by -sweep)
            swing.json swing-tracks.json  multi-day scoring (read by -swing-sweep)
            portfolio.json tracks.json samples.json
data/observations.jsonl outcomes.jsonl portfolio.jsonl samples.jsonl
data/sweep.json swing-sweep.json
data/export/candidates.csv samples.csv
```

Rotate with logrotate — `/etc/logrotate.d/stocko2`:

```
/home/austin/stocko2/app.log /home/austin/stocko2/logs/*.log {
    monthly
    rotate 12
    compress
    missingok
    notifempty
    copytruncate
}
```

`copytruncate` because a run may hold the file open when rotation fires. Test with `sudo logrotate -d /etc/logrotate.d/stocko2`.

## 8. Service health

```bash
systemctl status cron        # must be active and enabled
timedatectl                  # zone = America/New_York, NTP synchronized
grep CRON /var/log/syslog | tail -20
```

A server doesn't sleep, so there's no missed-wakeup problem. If you'd rather not use cron, systemd timers with `Persistent=true` will replay a run missed across a reboot.

## 9. Verify

```bash
cd /home/austin/stocko2
tail -f app.log
ls -la data/$(date +%F)/

# Book flattened? Should be 0 after 15:55.
grep -c '"status": "open"' data/$(date +%F)/ledger.json

grep "scoring the" app.log | tail -5          # which scan phase the tracker used
grep "available as buying power" app.log | tail -1
```

Expected daily sequence in `app.log`:

```
scanner: wrote N rows to data/<date>/premarket.json
scanner: N carried from pre-open, N new at open, N faded
tracker: opened N paper positions
tracker: tick 77, 0 open, N closed, N samples
outcomes: scored N candidates
export / sweep / swing / swing-sweep lines
```

## 10. Rough edges

- **`-export`, `-sweep`, `-swing-sweep` need credentials they don't use.** All are local-file only, but `main.go:281` calls `GetAccount` before dispatch.
- **`-outcomes` re-runs duplicate rows** in `data/outcomes.jsonl`. `-sweep` reads per-session `outcome.json` instead, so it's unaffected — but dedupe on `(session_date, symbol)` in your own analysis.
- **`-swing` re-fetches every session each run** (that's what makes it idempotent). One request per session: ~250 sessions ≈ 1.5 min at 180/min.
- **No stop loss on the intraday path.** Downside is uncapped within the session. The swing path does have one.
- **Sample size.** `-sweep` warns below 10 sessions / 100 trades; `-swing-sweep` below 20 / 200. Overlapping multi-day holds correlate, so the effective sample is smaller than the count. Read median alongside mean — a right-tailed distribution posts a positive mean on mostly-losing trades.
