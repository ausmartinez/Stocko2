import json
from datetime import datetime, date, timezone

from news.store import connect
from sim.config import DB_PATH

SCHEMA = """
CREATE SEQUENCE IF NOT EXISTS position_ids START 1;
CREATE TABLE IF NOT EXISTS session_log (
    day DATE PRIMARY KEY,
    status VARCHAR,          -- trading | closed | late_open
    entries_done BOOLEAN,
    note VARCHAR,
    updated_at TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS candidates (
    day DATE,
    ticker VARCHAR,
    source VARCHAR,          -- news | event
    strength DOUBLE,
    score DOUBLE,
    confidence DOUBLE,
    threshold DOUBLE,
    status VARCHAR,          -- entered | rejected
    reject_reason VARCHAR,
    details VARCHAR,
    created_at TIMESTAMPTZ,
    PRIMARY KEY (day, ticker)
);
CREATE TABLE IF NOT EXISTS positions (
    id INTEGER PRIMARY KEY DEFAULT nextval('position_ids'),
    ticker VARCHAR,
    source VARCHAR,
    status VARCHAR,          -- open | closed
    qty DOUBLE,
    entry_day DATE,
    entry_time TIMESTAMPTZ,
    entry_price DOUBLE,      -- ask at entry (simulated market buy)
    entry_bid DOUBLE,
    entry_last DOUBLE,
    spread_pct DOUBLE,
    prev_close DOUBLE,
    gap_pct DOUBLE,
    premarket_volume DOUBLE,
    premarket_high DOUBLE,
    news_score DOUBLE,
    news_confidence DOUBLE,
    catalysts VARCHAR,
    event_type VARCHAR,
    entry_context VARCHAR,
    peak_bid DOUBLE,
    peak_pnl_pct DOUBLE,
    exit_time TIMESTAMPTZ,
    exit_price DOUBLE,       -- bid at exit (simulated market sell)
    exit_reason VARCHAR,
    exit_context VARCHAR,
    pnl_usd DOUBLE,
    pnl_pct DOUBLE,
    days_held INTEGER
);
CREATE TABLE IF NOT EXISTS marks (
    position_id INTEGER,
    ts TIMESTAMPTZ,
    bid DOUBLE,
    ask DOUBLE,
    last DOUBLE,
    pnl_pct DOUBLE,
    peak_pnl_pct DOUBLE,
    drawdown_pct DOUBLE,
    minute_volume DOUBLE,
    median_minute_volume DOUBLE,
    volume_ratio DOUBLE,
    trades_per_min DOUBLE,
    volume_per_min DOUBLE,
    buy_ratio DOUBLE,
    session_volume DOUBLE
);
"""


def _now():
    return datetime.now(timezone.utc)


class SimStore:
    def __init__(self, path=DB_PATH):
        self.conn = connect(path)
        self.conn.execute(SCHEMA)

    def close(self):
        self.conn.close()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()

    # --- session bookkeeping ---
    def session_entry(self, day: date):
        row = self.conn.execute("SELECT status, entries_done, note FROM session_log WHERE day = ?", [day]).fetchone()
        return None if row is None else {"status": row[0], "entries_done": row[1], "note": row[2]}

    def set_session(self, day: date, status: str, entries_done: bool, note: str = ""):
        self.conn.execute("INSERT OR REPLACE INTO session_log VALUES (?, ?, ?, ?, ?)", [day, status, entries_done, note, _now()])

    # --- candidates ---
    def log_candidate(self, day: date, c: dict, status: str, reject_reason: str = ""):
        self.conn.execute(
            "INSERT OR REPLACE INTO candidates VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            [day, c["ticker"], c["source"], c["strength"], c["score"], c["confidence"], c.get("threshold"),
             status, reject_reason, json.dumps(c, default=str), _now()],
        )

    # --- positions ---
    def open_positions(self) -> list:
        cur = self.conn.execute("SELECT * FROM positions WHERE status = 'open' ORDER BY id")
        cols = [d[0] for d in cur.description]
        return [dict(zip(cols, r)) for r in cur.fetchall()]

    def open_tickers(self) -> set:
        return {r[0] for r in self.conn.execute("SELECT ticker FROM positions WHERE status = 'open'").fetchall()}

    def open_position(self, p: dict) -> int:
        cols = list(p)
        row = self.conn.execute(
            f"INSERT INTO positions ({', '.join(cols)}) VALUES ({', '.join('?' for _ in cols)}) RETURNING id",
            [p[c] for c in cols],
        ).fetchone()
        return row[0]

    def update_peak(self, position_id: int, peak_bid: float, peak_pnl_pct: float):
        self.conn.execute("UPDATE positions SET peak_bid = ?, peak_pnl_pct = ? WHERE id = ?", [peak_bid, peak_pnl_pct, position_id])

    def close_position(self, position_id: int, exit_price: float, reason: str, context: dict,
                       pnl_usd: float, pnl_pct: float, days_held: int):
        self.conn.execute(
            """UPDATE positions SET status = 'closed', exit_time = ?, exit_price = ?, exit_reason = ?,
               exit_context = ?, pnl_usd = ?, pnl_pct = ?, days_held = ? WHERE id = ?""",
            [_now(), exit_price, reason, json.dumps(context, default=str), pnl_usd, pnl_pct, days_held, position_id],
        )

    def add_mark(self, m: dict):
        cols = list(m)
        self.conn.execute(f"INSERT INTO marks ({', '.join(cols)}) VALUES ({', '.join('?' for _ in cols)})", [m[c] for c in cols])

    # --- reporting ---
    def query(self, sql: str, params=None) -> list:
        cur = self.conn.execute(sql, params or [])
        cols = [d[0] for d in cur.description]
        return [dict(zip(cols, r)) for r in cur.fetchall()]
