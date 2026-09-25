import os
import time
import random
import pickle
import pandas as pd
import datetime as dt
from pathlib import Path
from dotenv import load_dotenv
from functools import wraps

from alpaca.data.historical import StockHistoricalDataClient
from alpaca.data.historical.news import NewsClient
from alpaca.trading.requests import GetAssetsRequest
from alpaca.data.requests import StockBarsRequest, NewsRequest
from alpaca.trading.enums import AssetClass, AssetStatus, AssetExchange
from alpaca.data.timeframe import TimeFrame
from alpaca.trading.client import TradingClient

env_path = Path("./.env")
if not env_path.exists():
    raise FileNotFoundError("Critical Error: .env file missing.")
load_dotenv(dotenv_path=env_path)

API_KEY = os.getenv("APCA_API_KEY_ID")
SECRET_KEY = os.getenv("APCA_API_SECRET_KEY")

CACHE_FILE = "cached_gapper_data.pkl"

def timer(func):
    @wraps(func)
    def wrapper(*args, **kwargs):
        start = time.perf_counter()
        result = func(*args, **kwargs)
        end = time.perf_counter()
        print(f"[Timer] '{func.__name__}' completed in {end - start:.4f} seconds")
        return result
    return wrapper

# ----------------------------------------------------------------------
# PHASE 1: Fetch and Cache Candidate Trades & Bar Data (Runs Once)
# ----------------------------------------------------------------------
@timer
def fetch_and_cache_gapper_data(
    symbols: list[str],
    start_date: str,
    max_holding_days: int = 30,  # Pre-fetch enough forward days for any test
    min_pm_gap_pct: float = 5.0,
    min_pm_volume: int = 20000,
    chunk_size: int = 10,
    api_key: str = "YOUR_API_KEY",
    secret_key: str = "YOUR_SECRET_KEY"
) -> list[dict]:
    """
    Fetches raw price/news data and extracts candidate gappers along with
    their forward Regular Trading Hours (RTH) price series.
    """
    data_client = StockHistoricalDataClient(api_key, secret_key)
    news_client = NewsClient(api_key, secret_key)
    
    target_date_obj = pd.to_datetime(start_date).date()
    start_dt = pd.to_datetime(start_date, utc=True) - pd.Timedelta(days=5)
    end_dt = pd.to_datetime(start_date, utc=True) + pd.Timedelta(days=max_holding_days + 7)
    
    cached_candidates = []

    total_chunks = (len(symbols) + chunk_size - 1) // chunk_size
    print(f"Fetching market data for {len(symbols)} symbols...")
    for i in range(0, len(symbols), chunk_size):
        chunk_idx = (i // chunk_size) + 1
        print(f"--- Chunk {chunk_idx}/{total_chunks} ---")
        chunk = symbols[i:i + chunk_size]
        try:
            request = StockBarsRequest(
                symbol_or_symbols=chunk,
                timeframe=TimeFrame.Minute,
                start=start_dt,
                end=end_dt,
                feed="sip",
                extended_hours=True
            )
            bars_df = data_client.get_stock_bars(request).df
            if bars_df.empty:
                continue
                
            bars_df = bars_df.reset_index()
            if "symbol" not in bars_df.columns:
                bars_df["symbol"] = chunk[0]
                
            bars_df["timestamp"] = pd.to_datetime(bars_df["timestamp"]).dt.tz_convert("America/New_York")
            bars_df["trade_date"] = bars_df["timestamp"].dt.date
            bars_df["time"] = bars_df["timestamp"].dt.time
            
            for symbol, sym_group in bars_df.groupby("symbol"):
                unique_dates = sorted(sym_group["trade_date"].unique())
                if target_date_obj not in unique_dates:
                    continue
                
                target_idx = unique_dates.index(target_date_obj)
                if target_idx == 0:
                    continue  
                
                prev_date = unique_dates[target_idx - 1]
                prev_day_bars = sym_group[sym_group["trade_date"] == prev_date]
                curr_day_bars = sym_group[sym_group["trade_date"] == target_date_obj]
                
                rth_prev = prev_day_bars[prev_day_bars["time"] <= dt.time(16, 0)]
                if rth_prev.empty:
                    continue
                prev_close = rth_prev.iloc[-1]["close"]
                
                pm_bars = curr_day_bars[(curr_day_bars["time"] >= dt.time(4, 0)) & (curr_day_bars["time"] < dt.time(9, 30))]
                if pm_bars.empty:
                    continue
                    
                pm_last = pm_bars.iloc[-1]["close"]
                pm_volume = pm_bars["volume"].sum()
                pm_gap_pct = ((pm_last - prev_close) / prev_close) * 100.0

                if abs(pm_gap_pct) < min_pm_gap_pct or pm_volume < min_pm_volume:
                    continue

                open_bars = curr_day_bars[curr_day_bars["time"] >= dt.time(9, 30)]
                if open_bars.empty:
                    continue
                
                buy_price = open_bars.iloc[0]["open"]
                open_gap_pct = ((buy_price - prev_close) / prev_close) * 100.0

                # Slice forward dates for trade evaluation
                forward_dates = unique_dates[target_idx : target_idx + max_holding_days]
                multi_day_bars = sym_group[sym_group["trade_date"].isin(forward_dates)]
                
                rth_multi_day = multi_day_bars[
                    (multi_day_bars["time"] >= dt.time(9, 30)) & 
                    (multi_day_bars["time"] <= dt.time(16, 0))
                ].copy()
                
                if rth_multi_day.empty:
                    continue

                # Fetch news metadata once
                news_start = pd.to_datetime(prev_date).tz_localize("America/New_York").replace(hour=16, minute=0)
                news_end = pd.to_datetime(target_date_obj).tz_localize("America/New_York").replace(hour=9, minute=30)
                has_news, headline = False, None
                try:
                    news_req = NewsRequest(symbols=symbol, start=news_start, end=news_end, limit=1)
                    news_res = news_client.get_news(news_req)
                    articles = news_res.news if hasattr(news_res, "news") else []
                    has_news = len(articles) > 0
                    headline = articles[0].headline if has_news else None
                except Exception:
                    pass

                cached_candidates.append({
                    "symbol": symbol,
                    "target_date_obj": target_date_obj,
                    "prev_close": prev_close,
                    "pm_gap_pct": round(pm_gap_pct, 2),
                    "pm_volume": pm_volume,
                    "open_gap_pct": round(open_gap_pct, 2),
                    "buy_price": buy_price,
                    "has_news": has_news,
                    "headline": headline,
                    "forward_dates": forward_dates,
                    "rth_bars": rth_multi_day[["timestamp", "trade_date", "time", "open", "high", "low", "close"]].to_dict("records")
                })

        except Exception as e:
            print(f"  [!] Error pre-fetching chunk {chunk}: {e}")
            continue

    print(f"Cached {len(cached_candidates)} gapper candidates.")
    return cached_candidates


# ----------------------------------------------------------------------
# PHASE 2: Evaluate Trades from Cached Data (Runs Pure In-Memory)
# ----------------------------------------------------------------------
def simulate_from_cache(
    cached_candidates: list[dict],
    holding_days: int = 10,
    capital_per_trade: float = 1000.0,
    max_capital_traded: float | None = None,
    take_profit_pct: float = 10.0,
    stop_loss_pct: float | None = 5.0
) -> pd.DataFrame:
    """
    Evaluates trades on pre-fetched candidate data in memory without API calls.
    """
    all_results = []
    total_capital_deployed = 0.0

    for cand in cached_candidates:
        if max_capital_traded is not None and total_capital_deployed >= max_capital_traded:
            break

        if max_capital_traded is not None:
            remaining_capital = max_capital_traded - total_capital_deployed
            if remaining_capital <= 0:
                break
            actual_trade_capital = min(capital_per_trade, remaining_capital)
        else:
            actual_trade_capital = capital_per_trade

        buy_price = cand["buy_price"]
        shares = actual_trade_capital / buy_price
        forward_dates = cand["forward_dates"][:holding_days]
        
        # Filter bars to selected holding period
        rth_bars = [b for b in cand["rth_bars"] if b["trade_date"] in forward_dates]
        if not rth_bars:
            continue

        # Calculate target exit prices if parameters are provided
        tp_target_price = (
            buy_price * (1.0 + (take_profit_pct / 100.0))
            if take_profit_pct is not None
            else None
        )
        sl_target_price = (
            buy_price * (1.0 - (stop_loss_pct / 100.0))
            if stop_loss_pct is not None
            else None
        )

        exit_price, exit_date, exit_timestamp, exit_reason, days_held = None, None, None, None, None

        for bar in rth_bars:
            # Check Stop Loss
            if sl_target_price is not None and bar["low"] <= sl_target_price:
                exit_price = min(bar["open"], sl_target_price)
                exit_date = bar["trade_date"]
                exit_timestamp = bar["timestamp"]
                exit_reason = "STOP_LOSS"
                break

            # Check Take Profit
            if tp_target_price is not None and bar["high"] >= tp_target_price:
                exit_price = max(bar["open"], tp_target_price)
                exit_date = bar["trade_date"]
                exit_timestamp = bar["timestamp"]
                exit_reason = "TAKE_PROFIT"
                break

        if exit_reason is None:
            last_bar = rth_bars[-1]
            exit_price = last_bar["close"]
            exit_date = last_bar["trade_date"]
            exit_timestamp = last_bar["timestamp"]
            exit_reason = "TIMEOUT"
            days_held = forward_dates.index(exit_date) if exit_date in forward_dates else len(forward_dates) - 1

        realized_pnl_pct = ((exit_price - buy_price) / buy_price) * 100.0
        realized_pnl_usd = (exit_price - buy_price) * shares

        total_capital_deployed += actual_trade_capital

        all_results.append({
            "symbol": cand["symbol"],
            "buy_date": cand["target_date_obj"],
            "capital_deployed": round(actual_trade_capital, 2),
            "buy_price": buy_price,
            "exit_price": round(exit_price, 4),
            "exit_reason": exit_reason,
            "days_held": days_held,
            "realized_pnl_pct": round(realized_pnl_pct, 2),
            "realized_pnl_usd": round(realized_pnl_usd, 2)
        })

    return pd.DataFrame(all_results)


# ----------------------------------------------------------------------
# PHASE 3: Single-Parameter Fast Sweep Controller
# ----------------------------------------------------------------------
def run_fast_single_param_sweep(
    param_name: str,
    param_values: list,
    cached_candidates: list[dict],
    base_trade_kwargs: dict
) -> pd.DataFrame:
    """
    Loops through parameter values using cached data in milliseconds.
    """
    summary = []
    
    start_sweep = time.perf_counter()
    for val in param_values:
        kwargs = base_trade_kwargs.copy()
        kwargs[param_name] = val
        
        sim_df = simulate_from_cache(cached_candidates, **kwargs)
        
        if sim_df.empty:
            summary.append({
                param_name: val, "trades": 0, "win_rate_pct": 0.0, 
                "total_pnl_usd": 0.0, "avg_pnl_usd": 0.0, "avg_pnl_pct": 0.0
            })
            continue

        total_trades = len(sim_df)
        wins = len(sim_df[sim_df["realized_pnl_usd"] > 0])
        win_rate = (wins / total_trades) * 100.0

        summary.append({
            param_name: val,
            "trades": total_trades,
            "win_rate_pct": round(win_rate, 2),
            "total_pnl_usd": round(sim_df["realized_pnl_usd"].sum(), 2),
            "avg_pnl_usd": round(sim_df["realized_pnl_usd"].mean(), 2),
            "avg_pnl_pct": round(sim_df["realized_pnl_pct"].mean(), 2),
            "stop_losses_hit": len(sim_df[sim_df["exit_reason"] == "STOP_LOSS"]),
            "take_profits_hit": len(sim_df[sim_df["exit_reason"] == "TAKE_PROFIT"]),
            "timeouts_hit": len(sim_df[sim_df["exit_reason"] == "TIMEOUT"])
        })

    elapsed = time.perf_counter() - start_sweep
    print(f"\n[+] Swept {len(param_values)} parameter options in {elapsed:.4f} seconds!")
    return pd.DataFrame(summary)


# ----------------------------------------------------------------------
# EXECUTION SCRIPT
# ----------------------------------------------------------------------
if __name__ == "__main__":
    trading_client = TradingClient(API_KEY, SECRET_KEY)
    assets = trading_client.get_all_assets(GetAssetsRequest(asset_class=AssetClass.US_EQUITY, status=AssetStatus.ACTIVE))
    major_exchanges = {AssetExchange.NASDAQ, AssetExchange.NYSE, AssetExchange.AMEX}
    
    symbols = [
        a.symbol for a in assets 
        if a.tradable and a.exchange in major_exchanges and a.symbol.isalpha()
    ]
    
    random.seed(42)
    sample_symbols = random.sample(symbols, min(2000, len(symbols)))

    if os.path.exists(CACHE_FILE):
        print("Loading data from pickle cache...")
        with open(CACHE_FILE, "rb") as f:
            cached_data = pickle.load(f)
    else:
        print("Fetching new data...")
        cached_data = fetch_and_cache_gapper_data(
            symbols=sample_symbols,
            start_date="2026-08-03",
            max_holding_days=30,      # Allows testing holding_days from 1 to 30
            min_pm_gap_pct=5.0,
            min_pm_volume=20000,
            chunk_size=10,
            api_key=API_KEY,
            secret_key=SECRET_KEY
        )
        with open(CACHE_FILE, "wb") as f:
            pickle.dump(cached_data, f)

    # Base trade management arguments
    base_trade_config = {
        "holding_days": 20,
        "capital_per_trade": 100.0,
        "max_capital_traded": 1500.0,
        "take_profit_pct": 10.0,
        "stop_loss_pct": 40.0
    }

    # Step 2: Sweep Stop Loss (e.g., test 1% through 20%, plus None)
    stop_loss_values = [1.0, 2.0, 3.0, 4.0, 5.0, 7.5, 10.0, 12.5, 15.0, 20.0, 40.0, 60.0, None]
    
    results_df = run_fast_single_param_sweep(
        param_name="take_profit_pct",
        param_values=stop_loss_values,
        cached_candidates=cached_data,
        base_trade_kwargs=base_trade_config
    )

    print("\n=== Stop Loss Optimization Results ===")
    print(results_df.to_string(index=False))
