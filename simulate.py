import os
import time
import random
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
    raise FileNotFoundError("Critical Error: .env file missing. Please create one from .env.example.")
load_dotenv(dotenv_path=env_path)

API_KEY = os.getenv("APCA_API_KEY_ID")
SECRET_KEY = os.getenv("APCA_API_SECRET_KEY")

def timer(func):
    @wraps(func)
    def wrapper(*args, **kwargs):
        start = time.perf_counter()
        result = func(*args, **kwargs)
        end = time.perf_counter()
        print(f"[Timer] '{func.__name__}' completed in {end - start:.4f} seconds")
        return result
    return wrapper

@timer
def run_gapper_trading_simulation(
    symbols: list[str],
    start_date: str,               
    holding_days: int = 10,        
    capital_per_trade: float = 1000.0, 
    max_capital_traded: float | None = None,  # NEW: Maximum total capital cap
    take_profit_pct: float = 5.0,  
    stop_loss_pct: float | None = None, 
    min_pm_gap_pct: float = 3.0,   
    min_pm_volume: int = 20000,    
    chunk_size: int = 5,
    api_key: str = "YOUR_API_KEY",
    secret_key: str = "YOUR_SECRET_KEY"
) -> pd.DataFrame:
    data_client = StockHistoricalDataClient(api_key, secret_key)
    news_client = NewsClient(api_key, secret_key)
    
    target_date_obj = pd.to_datetime(start_date).date()
    start_dt = pd.to_datetime(start_date, utc=True) - pd.Timedelta(days=5)
    end_dt = pd.to_datetime(start_date, utc=True) + pd.Timedelta(days=holding_days + 7)
    
    all_results = []
    total_capital_deployed = 0.0  # Track accumulated capital deployed

    for i in range(0, len(symbols), chunk_size):
        # Stop overall loop if max capital limit has been hit
        if max_capital_traded is not None and total_capital_deployed >= max_capital_traded:
            print(f"[!] Reached maximum capital limit (${max_capital_traded:,.2f}). Stopping simulation.")
            break

        chunk = symbols[i:i + chunk_size]
        print(f"--- Processing Chunk {i//chunk_size + 1}/{int(len(symbols)/chunk_size)}: {chunk} ---")
        
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
                # Enforce capital budget check per candidate trade
                if max_capital_traded is not None:
                    remaining_capital = max_capital_traded - total_capital_deployed
                    if remaining_capital <= 0:
                        break
                    actual_trade_capital = min(capital_per_trade, remaining_capital)
                else:
                    actual_trade_capital = capital_per_trade

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
                pm_gap_pct = ((pm_last - prev_close) / prev_close) * 100

                if abs(pm_gap_pct) < min_pm_gap_pct or pm_volume < min_pm_volume:
                    continue

                time.sleep(0.3)
                news_start = pd.to_datetime(prev_date).tz_localize("America/New_York").replace(hour=16, minute=0)
                news_end = pd.to_datetime(target_date_obj).tz_localize("America/New_York").replace(hour=9, minute=30)
                
                try:
                    news_req = NewsRequest(symbols=symbol, start=news_start, end=news_end, limit=1)
                    news_res = news_client.get_news(news_req)
                    articles = news_res.news if hasattr(news_res, "news") else []
                    has_news = len(articles) > 0
                    headline = articles[0].headline if has_news else None
                except Exception:
                    has_news = False
                    headline = None

                open_bars = curr_day_bars[curr_day_bars["time"] >= dt.time(9, 30)]
                if open_bars.empty:
                    continue
                
                buy_price = open_bars.iloc[0]["open"]
                open_gap_pct = ((buy_price - prev_close) / prev_close) * 100
                
                # Calculate positional shares using actual allocated capital
                shares = actual_trade_capital / buy_price

                forward_dates = unique_dates[target_idx : target_idx + holding_days]
                multi_day_bars = sym_group[sym_group["trade_date"].isin(forward_dates)]
                
                rth_multi_day = multi_day_bars[
                    (multi_day_bars["time"] >= dt.time(9, 30)) & 
                    (multi_day_bars["time"] <= dt.time(16, 0))
                ]
                
                if rth_multi_day.empty:
                    continue

                peak_idx = rth_multi_day["high"].idxmax()
                trough_idx = rth_multi_day["low"].idxmin()

                peak_bar = rth_multi_day.loc[peak_idx]
                trough_bar = rth_multi_day.loc[trough_idx]

                best_price = peak_bar["high"]
                best_date = peak_bar["trade_date"]
                best_timestamp = peak_bar["timestamp"]
                days_to_best = forward_dates.index(best_date)
                max_profit_pct = ((best_price - buy_price) / buy_price) * 100
                max_profit_usd = (best_price - buy_price) * shares

                worst_price = trough_bar["low"]
                worst_date = trough_bar["trade_date"]
                worst_timestamp = trough_bar["timestamp"]
                days_to_worst = forward_dates.index(worst_date)
                max_loss_pct = ((worst_price - buy_price) / buy_price) * 100
                max_loss_usd = (worst_price - buy_price) * shares

                tp_target_price = buy_price * (1.0 + (take_profit_pct / 100.0))
                sl_target_price = buy_price * (1.0 - (stop_loss_pct / 100.0)) if stop_loss_pct is not None else None

                exit_price = None
                exit_date = None
                exit_timestamp = None
                exit_reason = None
                days_held = None

                for _, bar in rth_multi_day.iterrows():
                    if sl_target_price is not None and bar["low"] <= sl_target_price:
                        exit_price = min(bar["open"], sl_target_price)
                        exit_date = bar["trade_date"]
                        exit_timestamp = bar["timestamp"]
                        exit_reason = "STOP_LOSS"
                        days_held = forward_dates.index(exit_date)
                        break

                    if bar["high"] >= tp_target_price:
                        exit_price = max(bar["open"], tp_target_price)
                        exit_date = bar["trade_date"]
                        exit_timestamp = bar["timestamp"]
                        exit_reason = "TAKE_PROFIT"
                        days_held = forward_dates.index(exit_date)
                        break

                if exit_reason is None:
                    last_bar = rth_multi_day.iloc[-1]
                    exit_price = last_bar["close"]
                    exit_date = last_bar["trade_date"]
                    exit_timestamp = last_bar["timestamp"]
                    exit_reason = "TIMEOUT"
                    days_held = forward_dates.index(exit_date)

                realized_pnl_pct = ((exit_price - buy_price) / buy_price) * 100.0
                realized_pnl_usd = (exit_price - buy_price) * shares

                # Accumulate total capital used
                total_capital_deployed += actual_trade_capital

                all_results.append({
                    "symbol": symbol,
                    "buy_date": target_date_obj,
                    "prev_close": prev_close,
                    "pm_gap_pct": round(pm_gap_pct, 2),
                    "pm_volume": pm_volume,
                    "has_news": has_news,
                    "headline": headline,
                    "buy_price": buy_price,
                    "shares": round(shares, 4),
                    "capital_deployed": round(actual_trade_capital, 2),
                    "open_gap_pct": round(open_gap_pct, 2),
                    
                    "target_tp_pct": take_profit_pct,
                    "target_sl_pct": stop_loss_pct if stop_loss_pct is not None else 0.0,
                    "exit_price": round(exit_price, 4),
                    "exit_date": exit_date,
                    "exit_timestamp": exit_timestamp,
                    "exit_reason": exit_reason,
                    "days_held": days_held,
                    "realized_pnl_pct": round(realized_pnl_pct, 2),
                    "realized_pnl_usd": round(realized_pnl_usd, 2),

                    "best_price": best_price,
                    "best_date": best_date,
                    "best_timestamp": best_timestamp,
                    "days_to_best": days_to_best,
                    "max_profit_pct": round(max_profit_pct, 2),
                    "max_profit_usd": round(max_profit_usd, 2),
                    
                    "worst_price": worst_price,
                    "worst_date": worst_date,
                    "worst_timestamp": worst_timestamp,
                    "days_to_worst": days_to_worst,
                    "max_loss_pct": round(max_loss_pct, 2),
                    "max_loss_usd": round(max_loss_usd, 2)
                })

        except Exception as e:
            print(f"  [!] Error processing chunk {chunk}: {e}")
            continue

    return pd.DataFrame(all_results)

if (__name__ == "__main__"):
    trading_client = TradingClient(API_KEY, SECRET_KEY)
    assets = trading_client.get_all_assets(GetAssetsRequest(asset_class=AssetClass.US_EQUITY, status=AssetStatus.ACTIVE))
    major_exchanges = {AssetExchange.NASDAQ, AssetExchange.NYSE, AssetExchange.AMEX}
    symbols = [
        a.symbol for a in assets 
        if a.tradable and a.exchange in major_exchanges and a.symbol.isalpha()
    ]
    random.seed(42)
    sample_size = min(800, len(symbols))
    sampled_symbols = random.sample(symbols, sample_size)
    df = run_gapper_trading_simulation(
        sampled_symbols,
        start_date="2026-05-01",     
        holding_days=20, 
        capital_per_trade=100.0,        
        max_capital_traded=1500.0,     
        take_profit_pct=7.0,         
        stop_loss_pct=20.0,          
        min_pm_gap_pct=3.0,          
        min_pm_volume=20000,         
        chunk_size=10,               
        api_key=API_KEY,
        secret_key=SECRET_KEY,
    )
    df.to_csv("simulated_gappers_results.csv", index=False)
