from alpaca.data.historical import StockHistoricalDataClient
from alpaca.data.requests import StockBarsRequest, StockTradesRequest, StockQuotesRequest
from alpaca.data.timeframe import TimeFrame
from datetime import datetime, timezone

# 1. Initialize the client (Replace with your actual API keys)
client = StockHistoricalDataClient("PKQA4AUTTFNFNFC6GW546VLEBO", "EvVQ2agnsufstd54n76G4uSsnuuoKQsoPibuUZj2hdmv")

# 2. Define the exact time you want to query
# Alpaca expects timezone-aware datetime objects (UTC is recommended)
query_time = datetime(2025, 8, 15, 14, 30, 0, tzinfo=timezone.utc)
symbol = "AAPL"

# 3. Request Minute Bars (aggregated candlestick data)
bar_request = StockBarsRequest(
    symbol_or_symbols=symbol,
    timeframe=TimeFrame.Minute,
    start=query_time,
    end=query_time
)
bars = client.get_stock_bars(bar_request)

# 4. Request Trades (tick-by-tick transaction data)
trade_request = StockTradesRequest(
    symbol_or_symbols=symbol,
    start=query_time,
    end=query_time
)
trades = client.get_stock_trades(trade_request)

# 5. Request Quotes (historical bid/ask pricing)
quote_request = StockQuotesRequest(
    symbol_or_symbols=symbol,
    start=query_time,
    end=query_time
)
quotes = client.get_stock_quotes(quote_request)

# 6. Display the data as Pandas DataFrames
print("--- Minute Bar ---")
print(bars.df if not bars.df.empty else "No bar data for this exact minute.")

print("\n--- Trades ---")
print(trades.df if not trades.df.empty else "No trade data for this exact second.")

print("\n--- Quotes ---")
print(quotes.df if not quotes.df.empty else "No quote data for this exact second.")
