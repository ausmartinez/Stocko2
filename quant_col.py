import math
import os
import random
import re
import time
import datetime as dt
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import timedelta, date
from pathlib import Path
from dotenv import load_dotenv
import duckdb
import numpy as np
import pandas as pd
import yfinance as yf

from alpaca.data.historical import StockHistoricalDataClient
from alpaca.data.historical.news import NewsClient
from alpaca.data.requests import NewsRequest, StockBarsRequest
from alpaca.data.timeframe import TimeFrame
from alpaca.trading.client import TradingClient
from alpaca.trading.enums import AssetClass, AssetExchange, AssetStatus
from alpaca.trading.requests import GetAssetsRequest

PM_OPEN = dt.time(4, 0)
MARKET_OPEN = dt.time(9, 30)
MARKET_CLOSE = dt.time(16, 0)

# Column order of quantitative_dataset, used to align the frame before insert.
SCHEMA_COLUMNS = [
    "symbol", "trade_date", "prev_close", "open_price", "pm_gap_pct",
    "open_gap_pct", "pm_volume", "atr_14d", "rvol_5d", "has_news", "headline",
    "pm_high_retention", "pm_vwap_distance_pct", "rvol_pm", "gap_atr_ratio",
    "float_turnover", "is_52w_high_breakout", "has_active_offering",
    "spy_gap_pct", "dist_to_50sma_pct", "consecutive_green_days",
    "shares_float", "short_interest_pct", "market_cap",
    "capital_allocated_usd", "shares_bought", "y_exit_reason", "y_exit_price",
    "y_exit_date", "y_days_held", "y_realized_pnl_usd", "y_realized_pnl_pct",
    "y_max_drawup_usd", "y_max_drawup_pct", "y_max_drawdown_usd",
    "y_max_drawdown_pct", "y_best_day_idx", "y_worst_day_idx", "y_is_winner",
]

OFFERING_KEYWORDS = (
    r"\b(offering|direct offering|underwritten|s-1|s-3|prospectus|dilution"
    r"|registered direct)\b"
)


# =====================================================================
# 1. DATABASE SCHEMA INITIALIZATION
# =====================================================================

def _init_db(db_path: str):
    """Initializes DuckDB storage schema for X features and Y targets."""
    conn = duckdb.connect(db_path)
    conn.execute("""
        CREATE TABLE IF NOT EXISTS quantitative_dataset (
            symbol VARCHAR,
            trade_date DATE,
            prev_close DOUBLE,
            open_price DOUBLE,
            pm_gap_pct DOUBLE,
            open_gap_pct DOUBLE,
            pm_volume BIGINT,
            atr_14d DOUBLE,
            rvol_5d DOUBLE,
            has_news BOOLEAN,
            headline VARCHAR,
            pm_high_retention DOUBLE,
            pm_vwap_distance_pct DOUBLE,
            rvol_pm DOUBLE,
            gap_atr_ratio DOUBLE,
            float_turnover DOUBLE,
            is_52w_high_breakout BOOLEAN,
            has_active_offering BOOLEAN,
            spy_gap_pct DOUBLE,
            dist_to_50sma_pct DOUBLE,
            consecutive_green_days INT,
            shares_float DOUBLE,
            short_interest_pct DOUBLE,
            market_cap DOUBLE,
            capital_allocated_usd DOUBLE,
            shares_bought DOUBLE,
            y_exit_reason VARCHAR,
            y_exit_price DOUBLE,
            y_exit_date DATE,
            y_days_held INT,
            y_realized_pnl_usd DOUBLE,
            y_realized_pnl_pct DOUBLE,
            y_max_drawup_usd DOUBLE,
            y_max_drawup_pct DOUBLE,
            y_max_drawdown_usd DOUBLE,
            y_max_drawdown_pct DOUBLE,
            y_best_day_idx INT,
            y_worst_day_idx INT,
            y_is_winner BOOLEAN,
            PRIMARY KEY (symbol, trade_date)
        )
    """)
    # Persistent fundamentals cache so repeat runs never re-hit Yahoo.
    conn.execute("""
        CREATE TABLE IF NOT EXISTS fundamentals_cache (
            symbol VARCHAR PRIMARY KEY,
            shares_float DOUBLE,
            short_interest_pct DOUBLE,
            market_cap DOUBLE,
            fetched_at TIMESTAMP
        )
    """)
    conn.close()


# =====================================================================
# 2. FUNDAMENTALS HELPER
# =====================================================================

def _yf_fundamentals_one(symbol: str) -> dict:
    """Pulls float / short interest / market cap for a single symbol."""
    try:
        info = yf.Ticker(symbol).info
        short_pct = info.get("shortPercentOfFloat")
        return {
            "shares_float": info.get("floatShares"),
            "short_interest_pct": (
                short_pct * 100.0 if short_pct is not None else None
            ),
            "market_cap": info.get("marketCap"),
        }
    except Exception:
        return {
            "shares_float": None,
            "short_interest_pct": None,
            "market_cap": None,
        }


def fetch_yfinance_fundamentals(
    symbols: list,
    db_path: str | None = None,
    max_age_days: int = 7,
    max_workers: int = 8,
) -> dict:
    """Fetches fundamentals via yfinance, served from the DuckDB cache when fresh."""
    symbols = list(dict.fromkeys(symbols))
    if not symbols:
        return {}

    cached: dict[str, dict] = {}
    if db_path:
        conn = duckdb.connect(db_path)
        rows = conn.execute(
            """
            SELECT symbol, shares_float, short_interest_pct, market_cap
            FROM fundamentals_cache
            WHERE symbol IN ? AND fetched_at >= ?
            """,
            [symbols, dt.datetime.now() - timedelta(days=max_age_days)],
        ).fetchall()
        conn.close()
        cached = {
            r[0]: {
                "shares_float": r[1],
                "short_interest_pct": r[2],
                "market_cap": r[3],
            }
            for r in rows
        }

    missing = [s for s in symbols if s not in cached]
    if missing:
        print(
            f"  Fetching fundamentals for {len(missing)} symbols "
            f"({len(cached)} served from cache)..."
        )
        with ThreadPoolExecutor(max_workers=max_workers) as pool:
            fetched = dict(zip(missing, pool.map(_yf_fundamentals_one, missing)))
        cached.update(fetched)

        if db_path and fetched:
            df_cache = pd.DataFrame(
                [{"symbol": s, **v} for s, v in fetched.items()]
            )
            df_cache["fetched_at"] = dt.datetime.now()
            conn = duckdb.connect(db_path)
            conn.register("df_cache", df_cache)
            conn.execute(
                "INSERT OR REPLACE INTO fundamentals_cache BY NAME "
                "SELECT * FROM df_cache"
            )
            conn.close()
    elif cached:
        print(f"  All {len(cached)} fundamentals served from cache.")

    return cached


# =====================================================================
# 3. FEATURE EXTRACTION LOGIC
# =====================================================================

def compute_daily_features(df_daily: pd.DataFrame) -> dict[str, dict]:
    """Vectorizes the daily-bar features for every symbol in one pass."""
    if df_daily.empty:
        return {}

    df = df_daily.sort_values(["symbol", "timestamp"])
    grp = df.groupby("symbol", sort=False)

    # True range needs the prior close within each symbol.
    prev_close = grp["close"].shift(1)
    df["tr"] = np.maximum(
        df["high"] - df["low"],
        np.maximum(
            (df["high"] - prev_close).abs(),
            (df["low"] - prev_close).abs(),
        ),
    )
    grp = df.groupby("symbol", sort=False)
    atr_14d = (
        grp["tr"].rolling(14).mean().groupby(level=0).last()
    )

    # Trailing green-day run length: rows after the last non-green bar.
    df["is_green"] = df["close"] > df["open"]
    df["pos"] = grp.cumcount()
    bar_counts = grp.size()
    last_red_pos = df.loc[~df["is_green"]].groupby("symbol")["pos"].max()
    green_run = (bar_counts - 1 - last_red_pos).fillna(bar_counts)

    feats = pd.DataFrame({
        "prev_close": grp["close"].last(),
        "high_52w": grp["high"].max(),
        "adv_20": grp["volume"].apply(lambda s: s.tail(20).mean()),
        "adv_5": grp["volume"].apply(lambda s: s.tail(5).mean()),
        "sma_50": grp["close"].apply(lambda s: s.tail(50).mean()),
        "atr_14d": atr_14d,
        "consecutive_green_days": green_run,
    })
    feats["atr_14d"] = feats["atr_14d"].fillna(1.0)
    feats["consecutive_green_days"] = (
        feats["consecutive_green_days"].astype(int)
    )
    return feats.to_dict("index")


def extract_pm_features(
    symbol: str,
    target_date: dt.date,
    daily_feats: dict,
    intraday_bars: pd.DataFrame,
    spy_gap_pct: float | None = None,
) -> dict | None:
    """Calculates the pre-market and open features for one symbol-day."""
    prev_close = float(daily_feats["prev_close"])
    if prev_close <= 0:
        return None

    times = intraday_bars["time"].values
    pm_bars = intraday_bars[(times >= PM_OPEN) & (times < MARKET_OPEN)]
    if pm_bars.empty:
        return None

    pm_last = float(pm_bars["close"].iloc[-1])
    pm_high = float(pm_bars["high"].max())
    pm_volume = int(pm_bars["volume"].sum())

    pm_gap_pct = ((pm_last - prev_close) / prev_close) * 100.0
    pm_high_retention = pm_last / pm_high if pm_high > 0 else 0.0

    # Volume-weight the per-bar VWAP, falling back to close-weighted VWAP.
    pm_vwap = None
    if "vwap" in pm_bars.columns:
        valid = pm_bars.dropna(subset=["vwap"])
        vol = valid["volume"].sum()
        if vol > 0:
            pm_vwap = float((valid["vwap"] * valid["volume"]).sum() / vol)

    if pm_vwap is None or math.isnan(pm_vwap) or pm_vwap == 0:
        if pm_volume > 0:
            pm_vwap = float(
                (pm_bars["close"] * pm_bars["volume"]).sum() / pm_volume
            )
        else:
            pm_vwap = pm_last

    pm_vwap_distance_pct = (
        ((pm_last - pm_vwap) / pm_vwap) * 100.0 if pm_vwap > 0 else 0.0
    )

    open_bars = intraday_bars[times >= MARKET_OPEN]
    if open_bars.empty:
        return None

    open_price = float(open_bars["open"].iloc[0])
    open_gap_pct = ((open_price - prev_close) / prev_close) * 100.0

    adv_20 = float(daily_feats["adv_20"] or 0.0)
    adv_5 = float(daily_feats["adv_5"] or 0.0)
    atr_14d = float(daily_feats["atr_14d"])
    sma_50 = float(daily_feats["sma_50"] or 0.0)

    return {
        "symbol": symbol,
        "trade_date": target_date,
        "prev_close": round(prev_close, 2),
        "open_price": round(open_price, 2),
        "pm_gap_pct": round(pm_gap_pct, 2),
        "open_gap_pct": round(open_gap_pct, 2),
        "pm_volume": pm_volume,
        "atr_14d": round(atr_14d, 4),
        "rvol_5d": round(pm_volume / adv_5, 4) if adv_5 > 0 else 0.0,
        "rvol_pm": round(pm_volume / adv_20, 4) if adv_20 > 0 else 0.0,
        "pm_high_retention": round(pm_high_retention, 4),
        "pm_vwap_distance_pct": round(pm_vwap_distance_pct, 2),
        "gap_atr_ratio": (
            round((open_price - prev_close) / atr_14d, 2) if atr_14d > 0 else 0.0
        ),
        "is_52w_high_breakout": bool(open_price > float(daily_feats["high_52w"])),
        "spy_gap_pct": round(spy_gap_pct, 2) if spy_gap_pct is not None else None,
        "dist_to_50sma_pct": (
            round(((prev_close - sma_50) / sma_50) * 100.0, 2)
            if sma_50 > 0
            else 0.0
        ),
        "consecutive_green_days": int(daily_feats["consecutive_green_days"]),
    }


# =====================================================================
# 4. PIPELINE ORCHESTRATION CLASS
# =====================================================================

class QuantPipeline:
    def __init__(
        self,
        api_key: str,
        secret_key: str,
        db_path: str = "quant_dataset.duckdb",
        max_workers: int = 8,
    ):
        self.api_key = api_key
        self.secret_key = secret_key
        self.db_path = db_path
        self.max_workers = max_workers

        self.data_client = StockHistoricalDataClient(api_key, secret_key)
        try:
            self.news_client = NewsClient(api_key, secret_key)
        except Exception:
            self.news_client = None

        _init_db(self.db_path)

    # -- bar fetching -------------------------------------------------

    def _fetch_bars(
        self,
        symbols: list,
        timeframe: TimeFrame,
        start: dt.date,
        end: dt.date,
        retries: int = 3,
    ) -> pd.DataFrame:
        """Requests bars for one chunk, retrying on transient API errors."""
        for attempt in range(retries):
            try:
                res = self.data_client.get_stock_bars(
                    StockBarsRequest(
                        symbol_or_symbols=symbols,
                        timeframe=timeframe,
                        start=start,
                        end=end,
                    )
                )
                return res.df.reset_index() if res.data else pd.DataFrame()
            except Exception as e:
                if attempt == retries - 1:
                    print(f"  Warning: chunk failed after {retries} tries ({e})")
                    return pd.DataFrame()
                time.sleep(2 ** attempt)
        return pd.DataFrame()

    def _fetch_bars_parallel(
        self,
        symbols: list,
        timeframe: TimeFrame,
        start: dt.date,
        end: dt.date,
        chunk_size: int,
        label: str = "bars",
    ) -> pd.DataFrame:
        """Fans chunked bar requests across a thread pool and concatenates them."""
        chunks = [
            symbols[i : i + chunk_size]
            for i in range(0, len(symbols), chunk_size)
        ]
        frames = []
        done = 0
        t0 = time.time()

        with ThreadPoolExecutor(max_workers=self.max_workers) as pool:
            futures = [
                pool.submit(self._fetch_bars, c, timeframe, start, end)
                for c in chunks
            ]
            for fut in as_completed(futures):
                df = fut.result()
                if not df.empty:
                    frames.append(df)
                done += 1
                print(
                    f"\r  {label}: {done}/{len(chunks)} chunks "
                    f"({time.time() - t0:.0f}s)",
                    end="",
                    flush=True,
                )
        print()

        if not frames:
            return pd.DataFrame()
        return pd.concat(frames, ignore_index=True)

    def _get_spy_gap(self, target_dt: dt.date) -> float | None:
        """Computes SPY's open gap on the target date as a broad-market control."""
        try:
            # end is exclusive, so reach past the target to include its bar.
            df = self._fetch_bars(
                ["SPY"],
                TimeFrame.Day,
                target_dt - timedelta(days=10),
                target_dt + timedelta(days=1),
            )
            if df.empty:
                return None

            df["bar_date"] = pd.to_datetime(df["timestamp"]).dt.date
            df = df.sort_values("bar_date").reset_index(drop=True)

            # Anchor on the target date itself; a holiday has no gap to report.
            match = df.index[df["bar_date"] == target_dt]
            if len(match) == 0 or match[0] == 0:
                print(f"Warning: no SPY bar for {target_dt}; skipping SPY gap")
                return None

            i = match[0]
            prev_close = float(df.loc[i - 1, "close"])
            curr_open = float(df.loc[i, "open"])
            return (
                ((curr_open - prev_close) / prev_close) * 100.0
                if prev_close > 0
                else 0.0
            )
        except Exception as e:
            print(f"Warning: Could not compute SPY gap percentage ({e})")
            return None

    # -- news ---------------------------------------------------------

    def _fetch_news(
        self, symbols: list, target_dt: dt.date, group_size: int = 10
    ) -> dict[str, list]:
        """Fetches news for many symbols per request and regroups it by symbol."""
        if not self.news_client or not symbols:
            return {}

        start, end = target_dt - timedelta(days=1), target_dt + timedelta(days=1)
        groups = [
            symbols[i : i + group_size]
            for i in range(0, len(symbols), group_size)
        ]

        def fetch(group: list) -> dict[str, list]:
            try:
                res = self.news_client.get_news(
                    NewsRequest(
                        symbols=",".join(group),
                        start=start,
                        end=end,
                        limit=50 * len(group),
                    )
                )
                articles = getattr(res, "news", []) or []
            except Exception:
                return {}

            out: dict[str, list] = {}
            for art in articles:
                for sym in getattr(art, "symbols", None) or []:
                    if sym in group:
                        out.setdefault(sym, []).append(art)
            return out

        news_map: dict[str, list] = {}
        with ThreadPoolExecutor(max_workers=self.max_workers) as pool:
            for partial in pool.map(fetch, groups):
                news_map.update(partial)
        return news_map

    # -- targets ------------------------------------------------------

    def calculate_y_targets(
        self,
        open_price: float,
        forward_bars: pd.DataFrame,
        capital_per_trade: float,
        stop_loss_pct: float,
        take_profit_pct: float,
        max_holding_days: int,
    ) -> dict:
        """Simulates trade execution."""
        shares_bought = capital_per_trade / open_price if open_price > 0 else 0.0
        stop_price = open_price * (1.0 - stop_loss_pct / 100.0)
        tp_price = open_price * (1.0 + take_profit_pct / 100.0)

        exit_reason = "MAX_HOLD_EXPIRATION"
        exit_price = open_price
        exit_date = forward_bars["trade_date"].iloc[-1]
        days_held = 0

        max_high = open_price
        min_low = open_price
        best_day_idx = 0
        worst_day_idx = 0

        trading_days = np.sort(forward_bars["trade_date"].unique())

        for day_idx, t_date in enumerate(trading_days[:max_holding_days]):
            day_bars = forward_bars[forward_bars["trade_date"] == t_date]

            for row in day_bars.itertuples(index=False):
                high = float(row.high)
                low = float(row.low)
                bar_open = float(row.open)

                if high > max_high:
                    max_high = high
                    best_day_idx = day_idx
                if low < min_low:
                    min_low = low
                    worst_day_idx = day_idx

                if low <= stop_price:
                    exit_reason = "STOP_LOSS"
                    exit_price = min(stop_price, bar_open)
                    exit_date = t_date
                    days_held = day_idx + 1
                    break

                if high >= tp_price:
                    exit_reason = "TAKE_PROFIT"
                    exit_price = max(tp_price, bar_open)
                    exit_date = t_date
                    days_held = day_idx + 1
                    break

            if exit_reason != "MAX_HOLD_EXPIRATION":
                break

        if exit_reason == "MAX_HOLD_EXPIRATION":
            held_days = trading_days[:max_holding_days]
            last_day = held_days[-1]
            exit_price = float(
                forward_bars.loc[
                    forward_bars["trade_date"] == last_day, "close"
                ].iloc[-1]
            )
            exit_date = last_day
            days_held = len(held_days)

        return {
            "capital_allocated_usd": round(capital_per_trade, 2),
            "shares_bought": round(shares_bought, 4),
            "y_exit_reason": exit_reason,
            "y_exit_price": round(exit_price, 2),
            "y_exit_date": exit_date,
            "y_days_held": days_held,
            "y_realized_pnl_usd": round(
                (exit_price - open_price) * shares_bought, 2
            ),
            "y_realized_pnl_pct": round(
                ((exit_price - open_price) / open_price) * 100.0
                if open_price > 0
                else 0.0,
                2,
            ),
            "y_max_drawup_usd": round((max_high - open_price) * shares_bought, 2),
            "y_max_drawup_pct": round(
                ((max_high - open_price) / open_price) * 100.0
                if open_price > 0
                else 0.0,
                2,
            ),
            "y_max_drawdown_usd": round((min_low - open_price) * shares_bought, 2),
            "y_max_drawdown_pct": round(
                ((min_low - open_price) / open_price) * 100.0
                if open_price > 0
                else 0.0,
                2,
            ),
            "y_best_day_idx": best_day_idx,
            "y_worst_day_idx": worst_day_idx,
            "y_is_winner": bool(exit_price > open_price),
        }

    # -- driver -------------------------------------------------------

    def process_and_save_batch(
        self,
        symbols: list,
        start_date: str,
        max_holding_days: int = 10,
        min_pm_gap_pct: float = 3.0,
        min_pm_volume: int = 10000,
        chunk_size: int = 25,
        capital_per_trade: float = 1000.0,
        stop_loss_pct: float = 5.0,
        take_profit_pct: float = 10.0,
        fundamentals_lookup: dict | None = None,
        daily_chunk_size: int = 200,
        include_extended_hours: bool = False,
    ):
        """Screens the universe for gappers, then simulates forward returns.

        Runs as a funnel: cheap daily bars over the whole universe, then
        target-day minute bars, then forward minute bars, news and
        fundamentals only for the symbols that clear the gap filter.
        """
        target_dt = dt.datetime.strptime(start_date, "%Y-%m-%d").date()
        # Calendar buffer wide enough to contain max_holding_days sessions.
        forward_end = target_dt + timedelta(days=max_holding_days * 2 + 10)
        t_start = time.time()

        print(f"Starting pipeline run for target date: {target_dt}")
        print(f"Universe: {len(symbols)} symbols")

        spy_gap_pct = self._get_spy_gap(target_dt)

        # --- Stage 1: daily history for the whole universe ---
        print("\n[1/4] Daily history (features + prev close)...")
        # end is exclusive: target_dt yields history through the prior session,
        # so prev_close is the real previous close with no lookahead.
        df_daily = self._fetch_bars_parallel(
            symbols,
            TimeFrame.Day,
            target_dt - timedelta(days=365),
            target_dt,
            chunk_size=daily_chunk_size,
            label="daily",
        )
        if df_daily.empty:
            print("Pipeline aborted: no daily bars returned.")
            return

        daily_feats = compute_daily_features(df_daily)
        print(f"  Daily features computed for {len(daily_feats)} symbols.")

        # --- Stage 2: target-day minute bars, then screen ---
        print("\n[2/4] Target-day minute bars (pre-market screen)...")
        screen_symbols = [s for s in symbols if s in daily_feats]
        df_min = self._fetch_bars_parallel(
            screen_symbols,
            TimeFrame.Minute,
            target_dt,
            target_dt + timedelta(days=1),
            chunk_size=chunk_size,
            label="minute",
        )
        if df_min.empty:
            print("Pipeline aborted: no minute bars returned.")
            return

        df_min["timestamp"] = pd.to_datetime(df_min["timestamp"]).dt.tz_convert(
            "America/New_York"
        )
        df_min["trade_date"] = df_min["timestamp"].dt.date
        df_min["time"] = df_min["timestamp"].dt.time
        df_min = df_min[df_min["trade_date"] == target_dt].sort_values(
            ["symbol", "timestamp"]
        )

        candidates = {}
        for symbol, sym_min in df_min.groupby("symbol", sort=False):
            feats = daily_feats.get(symbol)
            if not feats:
                continue
            x = extract_pm_features(
                symbol=symbol,
                target_date=target_dt,
                daily_feats=feats,
                intraday_bars=sym_min,
                spy_gap_pct=spy_gap_pct,
            )
            if not x:
                continue
            if (
                x["pm_gap_pct"] < min_pm_gap_pct
                or x["pm_volume"] < min_pm_volume
            ):
                continue
            candidates[symbol] = x

        print(
            f"  {len(candidates)} symbols cleared the screen "
            f"(gap >= {min_pm_gap_pct}%, PM vol >= {min_pm_volume:,})."
        )
        if not candidates:
            print("\nPipeline finished: No stocks met the criteria.")
            return

        # --- Stage 3: forward minute bars for survivors only ---
        cand_list = sorted(candidates)
        print(f"\n[3/4] Forward minute bars for {len(cand_list)} candidates...")
        df_fwd = self._fetch_bars_parallel(
            cand_list,
            TimeFrame.Minute,
            target_dt + timedelta(days=1),
            forward_end,
            chunk_size=chunk_size,
            label="forward",
        )

        entry_day = df_min[df_min["symbol"].isin(candidates)]
        if not df_fwd.empty:
            df_fwd["timestamp"] = pd.to_datetime(
                df_fwd["timestamp"]
            ).dt.tz_convert("America/New_York")
            df_fwd["trade_date"] = df_fwd["timestamp"].dt.date
            df_fwd["time"] = df_fwd["timestamp"].dt.time
            df_fwd = pd.concat([entry_day, df_fwd], ignore_index=True)
        else:
            df_fwd = entry_day

        # Restrict the simulation to the regular session by default; thin
        # after-hours prints otherwise trigger stops that were not reachable.
        session_end = dt.time(20, 0) if include_extended_hours else MARKET_CLOSE
        fwd_times = df_fwd["time"].values
        df_fwd = df_fwd[
            (fwd_times >= MARKET_OPEN) & (fwd_times < session_end)
        ].sort_values(["symbol", "timestamp"])

        # --- Stage 4: news + fundamentals for survivors only ---
        print(f"\n[4/4] News and fundamentals for {len(cand_list)} candidates...")
        news_map = self._fetch_news(cand_list, target_dt)
        if fundamentals_lookup is None:
            fundamentals_lookup = fetch_yfinance_fundamentals(
                cand_list, db_path=self.db_path
            )

        dataset_records = []
        for symbol, forward_bars in df_fwd.groupby("symbol", sort=False):
            if forward_bars.empty:
                continue

            x_features = dict(candidates[symbol])
            articles = news_map.get(symbol, [])
            headline_text = ""
            if articles:
                first = articles[0]
                headline_text = getattr(first, "headline", None) or str(first)

            x_features["has_news"] = bool(articles)
            x_features["headline"] = headline_text
            x_features["has_active_offering"] = bool(
                re.search(OFFERING_KEYWORDS, headline_text, re.IGNORECASE)
            )

            fund = fundamentals_lookup.get(symbol) or {}
            shares_float = fund.get("shares_float")
            x_features["shares_float"] = shares_float
            x_features["short_interest_pct"] = fund.get("short_interest_pct")
            x_features["market_cap"] = fund.get("market_cap")
            x_features["float_turnover"] = (
                round(x_features["pm_volume"] / shares_float, 4)
                if shares_float and shares_float > 0
                else None
            )

            y_targets = self.calculate_y_targets(
                open_price=x_features["open_price"],
                forward_bars=forward_bars,
                capital_per_trade=capital_per_trade,
                stop_loss_pct=stop_loss_pct,
                take_profit_pct=take_profit_pct,
                max_holding_days=max_holding_days,
            )

            dataset_records.append({**x_features, **y_targets})
            print(
                f" -> {symbol} | PM Gap: {x_features['pm_gap_pct']}% | "
                f"Float Turnover: {x_features['float_turnover']} | "
                f"Winner: {y_targets['y_is_winner']}"
            )

        if not dataset_records:
            print("\nPipeline finished: No stocks met the criteria.")
            return

        df_dataset = pd.DataFrame(dataset_records).reindex(
            columns=SCHEMA_COLUMNS
        )

        conn = duckdb.connect(self.db_path)
        conn.register("df_dataset", df_dataset)
        # Insert explicitly BY NAME to avoid positional index mismatch
        conn.execute(
            "INSERT OR REPLACE INTO quantitative_dataset BY NAME "
            "SELECT * FROM df_dataset"
        )
        conn.close()

        csv_filename = f"./data/quantitative_dataset_{start_date}.csv"
        df_dataset.to_csv(csv_filename, index=False)

        print("\n=======================================================")
        print(f"SUCCESS: Stored {len(df_dataset)} rows in DuckDB ({self.db_path})")
        print(f"SUCCESS: Exported DataFrame to CSV ({csv_filename})")
        print(f"Elapsed: {time.time() - t_start:.1f}s")
        print("=======================================================")


# =====================================================================
# 5. MAIN DRIVER BLOCK
# =====================================================================

if __name__ == "__main__":
    env_path = Path("./.env")
    if not env_path.exists():
        raise FileNotFoundError(
            "Critical Error: .env file missing. Please create one from .env.example."
        )
    load_dotenv(dotenv_path=env_path)
    API_KEY = os.getenv("APCA_API_KEY_ID")
    SECRET_KEY = os.getenv("APCA_API_SECRET_KEY")

    trading_client = TradingClient(API_KEY, SECRET_KEY)

    try:
        assets = trading_client.get_all_assets(
            GetAssetsRequest(
                asset_class=AssetClass.US_EQUITY, status=AssetStatus.ACTIVE
            )
        )
        major_exchanges = {
            AssetExchange.NASDAQ,
            AssetExchange.NYSE,
            AssetExchange.AMEX,
        }

        all_symbols = [
            a.symbol
            for a in assets
            if a.tradable and a.exchange in major_exchanges and a.symbol.isalpha()
        ]
        random.seed(42)
        # Uncomment if we want a random sample of all symbols
        #target_symbols = random.sample(all_symbols, min(2000, len(all_symbols)))
        target_symbols = all_symbols

    except Exception as e:
        print(f"Warning: asset listing failed ({e}); using fallback universe.")
        target_symbols = [
            "AAPL",
            "TSLA",
            "NVDA",
            "AMD",
            "MSFT",
            "AMZN",
            "META",
            "GOOGL",
        ]

    pipeline = QuantPipeline(API_KEY, SECRET_KEY, db_path="quant_dataset.duckdb")

    def increment_date_str(date_str: str) -> str:
        # Parse the string into a date object
        current_date = date.fromisoformat(date_str)
        # Increment by 1 day
        next_date = current_date + timedelta(days=1)
        # Return formatted back as YYYY-MM-DD string
        return next_date.isoformat()

    startDate = "2026-01-01"
    endDate = "2026-09-20"

    for i in range(365):
        pipeline.process_and_save_batch(
            symbols=target_symbols,
            start_date=startDate,
            max_holding_days=14,
            min_pm_gap_pct=3.0,
            min_pm_volume=1000,
            chunk_size=50,
            capital_per_trade=1000.0,
            stop_loss_pct=5.0,
            take_profit_pct=10.0,
        )
        startDate = increment_date_str(startDate)
        if (startDate == endDate):
            break
