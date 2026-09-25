import re
import os
import math
import random
import datetime as dt
from datetime import timedelta
import numpy as np
import pandas as pd
import duckdb
from pathlib import Path
from dotenv import load_dotenv

from alpaca.trading.client import TradingClient
from alpaca.trading.requests import GetAssetsRequest
from alpaca.trading.enums import AssetClass, AssetStatus, AssetExchange
from alpaca.data.historical import StockHistoricalDataClient
from alpaca.data.historical.news import NewsClient
from alpaca.data.requests import StockBarsRequest, NewsRequest
from alpaca.data.timeframe import TimeFrame


# =====================================================================
# 1. DATABASE SCHEMA INITIALIZATION
# =====================================================================

def _init_db(db_path: str):
    """Initializes DuckDB storage schema for X features and Y targets."""
    conn = duckdb.connect(db_path)
    conn.execute("""
        CREATE TABLE IF NOT EXISTS quantitative_dataset (
            -- Identifiers
            symbol VARCHAR,
            trade_date DATE,

            -- Original Baseline X Features
            prev_close DOUBLE,
            open_price DOUBLE,
            pm_gap_pct DOUBLE,
            open_gap_pct DOUBLE,
            pm_volume BIGINT,
            atr_14d DOUBLE,
            rvol_5d DOUBLE,
            has_news BOOLEAN,
            headline VARCHAR,

            -- New Advanced X Features
            pm_high_retention DOUBLE,
            pm_vwap_distance_pct DOUBLE,
            rvol_pm DOUBLE,
            gap_atr_ratio DOUBLE,
            float_turnover DOUBLE,
            is_52w_high_breakout BOOLEAN,
            has_active_offering BOOLEAN,

            -- Execution Setup
            capital_allocated_usd DOUBLE,
            shares_bought DOUBLE,

            -- Y Targets (Forward Outcome Metrics)
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
    conn.close()


# =====================================================================
# 2. FEATURE EXTRACTION LOGIC
# =====================================================================

def extract_advanced_x_features(
    symbol: str,
    target_date: dt.date,
    sym_group: pd.DataFrame,
    prior_dates: list,
    news_articles: list,
    share_float: float | None = None
) -> dict | None:
    """Calculates all baseline and advanced X features for a single symbol and date."""
    
    if not prior_dates:
        return None

    prev_date = prior_dates[-1]
    prev_day_bars = sym_group[sym_group["trade_date"] == prev_date]
    curr_day_bars = sym_group[sym_group["trade_date"] == target_date]

    if prev_day_bars.empty or curr_day_bars.empty:
        return None

    # 1. Previous Day Close
    rth_prev = prev_day_bars[prev_day_bars["time"] <= dt.time(16, 0)]
    if rth_prev.empty:
        return None
    prev_close = float(rth_prev.iloc[-1]["close"])

    # 2. Pre-Market Slicing (04:00 - 09:29:59)
    pm_bars = curr_day_bars[(curr_day_bars["time"] >= dt.time(4, 0)) & (curr_day_bars["time"] < dt.time(9, 30))]
    if pm_bars.empty:
        return None

    pm_last = float(pm_bars.iloc[-1]["close"])
    pm_high = float(pm_bars["high"].max())
    pm_volume = int(pm_bars["volume"].sum())
    
    # Baseline Feature: Pre-Market Gap %
    pm_gap_pct = ((pm_last - prev_close) / prev_close) * 100.0 if prev_close > 0 else 0.0
    has_news = bool(news_articles)

    # Advanced Feature 1: Pre-Market High Retention
    pm_high_retention = pm_last / pm_high if pm_high > 0 else 0.0

    # Advanced Feature 2: Pre-Market VWAP Distance %
    if "vwap" in pm_bars.columns and not pm_bars["vwap"].isnull().all():
        pm_vwap = (pm_bars["vwap"] * pm_bars["volume"]).sum() / pm_volume if pm_volume > 0 else pm_last
    else:
        pm_vwap = ((pm_bars["close"] * pm_bars["volume"]).sum() / pm_volume) if pm_volume > 0 else pm_last
    
    pm_vwap_distance_pct = ((pm_last - pm_vwap) / pm_vwap) * 100.0 if pm_vwap > 0 else 0.0

    # 3. Market Open Price (09:30 AM)
    open_bars = curr_day_bars[curr_day_bars["time"] >= dt.time(9, 30)]
    if open_bars.empty:
        return None
    open_price = float(open_bars.iloc[0]["open"])
    
    # Baseline Feature: Open Gap %
    open_gap_pct = ((open_price - prev_close) / prev_close) * 100.0 if prev_close > 0 else 0.0

    # 4. Historical Daily Aggregation (20-day ADV & 14-day ATR)
    daily_summary = sym_group[sym_group["trade_date"].isin(prior_dates)].groupby("trade_date").agg({
        "open": "first", "high": "max", "low": "min", "close": "last", "volume": "sum"
    }).reset_index()

    if daily_summary.empty:
        return None

    # Advanced Feature 3: RVOL PM relative to ADV20
    adv_20 = float(daily_summary["volume"].tail(20).mean()) if len(daily_summary) >= 20 else float(daily_summary["volume"].mean())
    rvol_pm = pm_volume / adv_20 if adv_20 > 0 else 0.0
    
    # Baseline Feature: 5-Day RVOL
    adv_5 = float(daily_summary["volume"].tail(5).mean()) if len(daily_summary) >= 5 else float(daily_summary["volume"].mean())
    rvol_5d = pm_volume / adv_5 if adv_5 > 0 else 0.0

    # Advanced Feature 4: Gap to ATR Ratio
    df_atr = daily_summary.tail(15).copy()
    df_atr["tr"] = np.maximum(
        df_atr["high"] - df_atr["low"],
        np.maximum(
            (df_atr["high"] - df_atr["close"].shift(1)).abs(),
            (df_atr["low"] - df_atr["close"].shift(1)).abs()
        )
    )
    atr_14d = float(df_atr["tr"].rolling(14).mean().iloc[-1]) if len(df_atr) >= 14 else 1.0
    gap_atr_ratio = (open_price - prev_close) / atr_14d if atr_14d > 0 else 0.0

    # Advanced Feature 5: Float Turnover
    float_turnover = (pm_volume / share_float) if share_float and share_float > 0 else None

    # Advanced Feature 6: 52-Week High Breakout
    high_52w = float(daily_summary["high"].max())
    is_52w_high_breakout = bool(open_price > high_52w)

    # Advanced Feature 7: Active Offering NLP Flag from Headlines
    offering_keywords = r"\b(offering|direct offering|underwritten|s-1|s-3|prospectus|dilution|registered direct)\b"
    has_active_offering = False
    headline_text = ""

    if news_articles:
        headline = news_articles[0].headline if hasattr(news_articles[0], 'headline') else str(news_articles[0])
        headline_text = headline
        has_active_offering = bool(re.search(offering_keywords, headline_text, re.IGNORECASE))

    return {
        "symbol": symbol,
        "trade_date": target_date,
        
        # Original Baseline Features
        "prev_close": round(prev_close, 2),
        "open_price": round(open_price, 2),
        "pm_gap_pct": round(pm_gap_pct, 2),
        "open_gap_pct": round(open_gap_pct, 2),
        "pm_volume": pm_volume,
        "atr_14d": round(atr_14d, 4),
        "rvol_5d": round(rvol_5d, 4),
        "has_news": has_news,
        "headline": headline_text,
        
        # Advanced Features
        "pm_high_retention": round(pm_high_retention, 4),
        "pm_vwap_distance_pct": round(pm_vwap_distance_pct, 2),
        "rvol_pm": round(rvol_pm, 4),
        "gap_atr_ratio": round(gap_atr_ratio, 2),
        "float_turnover": round(float_turnover, 4) if float_turnover else None,
        "is_52w_high_breakout": is_52w_high_breakout,
        "has_active_offering": has_active_offering
    }


# =====================================================================
# 3. PIPELINE ORCHESTRATION CLASS
# =====================================================================

class QuantPipeline:
    def __init__(self, api_key: str, secret_key: str, db_path: str = "quant_dataset.duckdb"):
        self.api_key = api_key
        self.secret_key = secret_key
        self.db_path = db_path
        
        self.data_client = StockHistoricalDataClient(api_key, secret_key)
        try:
            self.news_client = StockNewsClient(api_key, secret_key)
        except Exception:
            self.news_client = None
            
        _init_db(self.db_path)

    def calculate_y_targets(
        self,
        open_price: float,
        forward_bars: pd.DataFrame,
        capital_per_trade: float,
        stop_loss_pct: float,
        take_profit_pct: float,
        max_holding_days: int
    ) -> dict:
        """Simulates trade execution from open_price over forward bars to build outcome metrics."""
        
        shares_bought = capital_per_trade / open_price if open_price > 0 else 0.0
        stop_price = open_price * (1.0 - stop_loss_pct / 100.0)
        tp_price = open_price * (1.0 + take_profit_pct / 100.0)

        exit_reason = "MAX_HOLD_EXPIRATION"
        exit_price = open_price
        exit_date = forward_bars["trade_date"].iloc[-1] if not forward_bars.empty else dt.date.today()
        days_held = 0

        max_high = open_price
        min_low = open_price

        trading_days = forward_bars["trade_date"].unique()
        best_day_idx = 0
        worst_day_idx = 0

        for day_idx, t_date in enumerate(trading_days[:max_holding_days]):
            day_bars = forward_bars[forward_bars["trade_date"] == t_date]
            
            for _, bar in day_bars.iterrows():
                high = float(bar["high"])
                low = float(bar["low"])

                if high > max_high:
                    max_high = high
                    best_day_idx = day_idx
                if low < min_low:
                    min_low = low
                    worst_day_idx = day_idx

                # Check Stop Loss
                if low <= stop_price:
                    exit_reason = "STOP_LOSS"
                    exit_price = min(stop_price, float(bar["open"]))  # Account for gap downs
                    exit_date = t_date
                    days_held = day_idx + 1
                    break
                
                # Check Take Profit
                if high >= tp_price:
                    exit_reason = "TAKE_PROFIT"
                    exit_price = max(tp_price, float(bar["open"]))  # Account for gap ups
                    exit_date = t_date
                    days_held = day_idx + 1
                    break

            if exit_reason != "MAX_HOLD_EXPIRATION":
                break

        if exit_reason == "MAX_HOLD_EXPIRATION" and not forward_bars.empty:
            exit_price = float(forward_bars.iloc[-1]["close"])
            days_held = min(len(trading_days), max_holding_days)

        # Performance Calculations
        pnl_usd = (exit_price - open_price) * shares_bought
        pnl_pct = ((exit_price - open_price) / open_price) * 100.0 if open_price > 0 else 0.0
        max_drawup_usd = (max_high - open_price) * shares_bought
        max_drawup_pct = ((max_high - open_price) / open_price) * 100.0 if open_price > 0 else 0.0
        max_drawdown_usd = (min_low - open_price) * shares_bought
        max_drawdown_pct = ((min_low - open_price) / open_price) * 100.0 if open_price > 0 else 0.0

        return {
            "capital_allocated_usd": round(capital_per_trade, 2),
            "shares_bought": round(shares_bought, 4),
            "y_exit_reason": exit_reason,
            "y_exit_price": round(exit_price, 2),
            "y_exit_date": exit_date,
            "y_days_held": days_held,
            "y_realized_pnl_usd": round(pnl_usd, 2),
            "y_realized_pnl_pct": round(pnl_pct, 2),
            "y_max_drawup_usd": round(max_drawup_usd, 2),
            "y_max_drawup_pct": round(max_drawup_pct, 2),
            "y_max_drawdown_usd": round(max_drawdown_usd, 2),
            "y_max_drawdown_pct": round(max_drawdown_pct, 2),
            "y_best_day_idx": best_day_idx,
            "y_worst_day_idx": worst_day_idx,
            "y_is_winner": bool(pnl_usd > 0)
        }

    def process_and_save_batch(
        self,
        symbols: list,
        start_date: str,
        max_holding_days: int = 10,
        min_pm_gap_pct: float = 5.0,
        min_pm_volume: int = 20000,
        chunk_size: int = 25,
        capital_per_trade: float = 1000.0,
        stop_loss_pct: float = 5.0,
        take_profit_pct: float = 10.0
    ):
        """Fetches historical market data, extracts features, calculates Y targets, and stores to DuckDB and CSV."""
        
        target_dt = dt.datetime.strptime(start_date, "%Y-%m-%d").date()
        lookback_start = target_dt - timedelta(days=365)  # 1 year prior for 52w high, ADV, ATR
        forward_end = target_dt + timedelta(days=max_holding_days + 15)  # Buffer for weekends/holidays

        total_chunks = math.ceil(len(symbols) / chunk_size)
        print(f"Starting pipeline run for target date: {target_dt}")
        print(f"Processing {len(symbols)} symbols in {total_chunks} chunks...")

        dataset_records = []

        for c_idx in range(total_chunks):
            chunk = symbols[c_idx * chunk_size : (c_idx + 1) * chunk_size]
            print(f"\n--- Processing Chunk {c_idx + 1}/{total_chunks} ({len(chunk)} symbols) ---")

            try:
                # 1. Fetch Historical Minute Bars for target_date + holding period
                minute_req = StockBarsRequest(
                    symbol_or_symbols=chunk,
                    timeframe=TimeFrame.Minute,
                    start=target_dt - timedelta(days=4), # Include prior trading day minute bars
                    end=forward_end
                )
                min_bars_res = self.data_client.get_stock_bars(minute_req)
                if not min_bars_res.data:
                    continue

                df_min = min_bars_res.df.reset_index()
                
                # Format Timestamps to US/Eastern
                df_min["timestamp"] = pd.to_datetime(df_min["timestamp"]).dt.tz_convert("America/New_York")
                df_min["trade_date"] = df_min["timestamp"].dt.date
                df_min["time"] = df_min["timestamp"].dt.time

                for symbol in chunk:
                    sym_min_bars = df_min[df_min["symbol"] == symbol].sort_values("timestamp")
                    if sym_min_bars.empty:
                        continue

                    prior_dates = sorted([d for d in sym_min_bars["trade_date"].unique() if d < target_dt])
                    if not prior_dates:
                        continue

                    # Fetch News if available
                    news_articles = []
                    if self.news_client:
                        try:
                            news_req = StockNewsRequest(
                                symbols=symbol,
                                start=target_dt - timedelta(days=1),
                                end=target_dt + timedelta(days=1),
                                limit=5
                            )
                            news_res = self.news_client.get_news(news_req)
                            news_articles = news_res.news if hasattr(news_res, 'news') else []
                        except Exception:
                            news_articles = []

                    # 2. Extract X Features
                    x_features = extract_advanced_x_features(
                        symbol=symbol,
                        target_date=target_dt,
                        sym_group=sym_min_bars,
                        prior_dates=prior_dates,
                        news_articles=news_articles,
                        share_float=None  # Can be populated if float provider is connected
                    )

                    if not x_features:
                        continue

                    # Filter thresholds
                    if x_features["pm_gap_pct"] < min_pm_gap_pct or x_features["pm_volume"] < min_pm_volume:
                        continue

                    # 3. Calculate Forward Y Targets
                    forward_bars = sym_min_bars[
                        (sym_min_bars["trade_date"] >= target_dt) & 
                        (sym_min_bars["time"] >= dt.time(9, 30))
                    ]
                    
                    if forward_bars.empty:
                        continue

                    y_targets = self.calculate_y_targets(
                        open_price=x_features["open_price"],
                        forward_bars=forward_bars,
                        capital_per_trade=capital_per_trade,
                        stop_loss_pct=stop_loss_pct,
                        take_profit_pct=take_profit_pct,
                        max_holding_days=max_holding_days
                    )

                    # Merge X and Y
                    combined_row = {**x_features, **y_targets}
                    dataset_records.append(combined_row)
                    print(f" -> Extracted features for {symbol} | PM Gap: {x_features['pm_gap_pct']}% | Winner: {y_targets['y_is_winner']}")

            except Exception as e:
                print(f"Error processing chunk {c_idx + 1}: {e}")

        # 4. Save to DuckDB and Export to CSV
        if dataset_records:
            df_dataset = pd.DataFrame(dataset_records)
            
            conn = duckdb.connect(self.db_path)
            conn.execute("INSERT OR REPLACE INTO quantitative_dataset SELECT * FROM df_dataset")
            conn.close()

            csv_filename = f"quantitative_dataset_{start_date}.csv"
            df_dataset.to_csv(csv_filename, index=False)
            
            print("\n=======================================================")
            print(f"SUCCESS: Stored {len(df_dataset)} rows in DuckDB ({self.db_path})")
            print(f"SUCCESS: Exported DataFrame to CSV ({csv_filename})")
            print("=======================================================")
        else:
            print("\nPipeline finished: No stocks met the pre-market gap/volume criteria.")


# =====================================================================
# 4. MAIN DRIVER BLOCK
# =====================================================================

if __name__ == "__main__":
    env_path = Path("./.env")
    if not env_path.exists():
        raise FileNotFoundError("Critical Error: .env file missing. Please create one from .env.example.")
    load_dotenv(dotenv_path=env_path)
    API_KEY = os.getenv("APCA_API_KEY_ID")
    SECRET_KEY = os.getenv("APCA_API_SECRET_KEY") 

    # 1. Fetch All Active Tradable US Equities from Alpaca
    print("Connecting to Alpaca Trading Client to fetch symbol universe...")
    trading_client = TradingClient(API_KEY, SECRET_KEY)
    
    try:
        assets = trading_client.get_all_assets(
            GetAssetsRequest(asset_class=AssetClass.US_EQUITY, status=AssetStatus.ACTIVE)
        )
        major_exchanges = {AssetExchange.NASDAQ, AssetExchange.NYSE, AssetExchange.AMEX}

        all_symbols = [
            a.symbol for a in assets
            if a.tradable and a.exchange in major_exchanges and a.symbol.isalpha()
        ]
        print(f"Retrieved {len(all_symbols)} active tradable US equity symbols.")

        # Sample a manageable subset or use full universe
        random.seed(42)
        target_symbols = random.sample(all_symbols, min(4000, len(all_symbols)))

    except Exception as e:
        print(f"Warning: Could not fetch asset universe from Alpaca ({e}). Falling back to default list.")
        target_symbols = ["AAPL", "TSLA", "NVDA", "AMD", "MSFT", "AMZN", "META", "GOOGL"]

    # 2. Instantiate and Execute Pipeline
    pipeline = QuantPipeline(API_KEY, SECRET_KEY, db_path="quant_dataset.duckdb")

    # Set start date (e.g. recent trading day)
    pipeline.process_and_save_batch(
        symbols=target_symbols,
        start_date="2026-09-01",
        max_holding_days=10,
        min_pm_gap_pct=3.0,
        min_pm_volume=10000,
        chunk_size=20,
        capital_per_trade=1000.0,
        stop_loss_pct=5.0,
        take_profit_pct=10.0
    )
