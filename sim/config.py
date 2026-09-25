from sim.market import ROOT

DATA_DIR = ROOT / "data" / "sim"
DATA_DIR.mkdir(parents=True, exist_ok=True)
DB_PATH = DATA_DIR / "sim.duckdb"
LOCK_PATH = DATA_DIR / ".trader.lock"

# --- Entry timing ---
ENTRY_DELAY_MIN = 1  # wait for the opening auction to print before quoting
ENTRY_WINDOW_MIN = 15  # no new entries after open + this
SKIP_LATE_OPEN = True  # no new entries on sessions that don't open at 9:30 ET

# --- Candidate selection ---
MAX_NEW_POSITIONS = 10
MIN_SCORE = 0.25  # floor on the aggregated news score
MIN_CONFIDENCE = 0.30
POPULATION_LOOKBACK_DAYS = 30
POPULATION_PERCENTILE = 0.80  # candidate strength must beat this share of all scored articles
MIN_POPULATION = 100  # below this many articles only the fixed floors apply
EVENT_TYPES = {"fda_pdufa", "fda_adcom", "data_readout", "earnings", "product_launch", "investor_conference", "deal_close"}
EVENT_MIN_SCORE = 0.0  # event plays need non-negative sentiment on the article that announced them

# --- Market filters at entry ---
MIN_PRICE = 1.00
MAX_SPREAD_PCT = 3.0
MAX_QUOTE_AGE_SEC = 120
MIN_GAP_PCT = None  # e.g. 2.0 to require the stock to be gapping up vs prior close

# --- Exits ---
TAKE_PROFIT_PCT = 6.0
STOP_LOSS_PCT = 4.0
TRAIL_ACTIVATE_PCT = 3.0  # trailing stop arms once the position was up this much
TRAIL_PCT = 2.0  # then exits on this much pullback from the peak
CLIMAX_VOLUME_MULT = 4.0  # last-minute volume vs session median minute volume
CLIMAX_BUY_RATIO = 0.65  # share of recent volume on upticks
CLIMAX_MIN_PNL_PCT = 1.5  # only sell into a buying climax when already in profit
MAX_HOLD_DAYS = 3  # trading days, entry day counts as day 1
EOD_EXIT_MIN = 10  # max-hold exits happen in the last N minutes of the final day
TRADE_LOOKBACK_MIN = 3  # window of tick data used for order-flow metrics
