package main

// Position represents a single stock holding
type Position struct {
	Symbol   string  `json:"symbol"`
	Shares   float64 `json:"shares"`
	BuyPrice float64 `json:"buy_price"`
	BoughtAt string  `json:"bought_at"`
}

// GapThresholds is the cutoff for each measurement. All are compared against
// the absolute size of the gap, so up and down gaps are treated alike.
type GapThresholds struct {
	GapPct  float64 `json:"gap_pct"`  // percent move, e.g. 4.0
	ATRMult float64 `json:"atr_mult"` // multiples of ATR, e.g. 1.0
	Sigma   float64 `json:"sigma"`    // stdevs of overnight returns, e.g. 2.0
}

// ScannerConfig controls the pre-market gapper scan.
type ScannerConfig struct {
	Enabled bool `json:"enabled"`

	// "iex" (free), "sip" (paid) or "delayed_sip". IEX misses most pre-market.
	Feed string `json:"feed"`

	// SymbolsOverride skips universe discovery and scans exactly these symbols.
	SymbolsOverride       []string `json:"symbols_override"`
	Exchanges             []string `json:"exchanges"` // empty means all
	ExcludeComplexSymbols bool     `json:"exclude_complex_symbols"`

	// Price and liquidity gates, applied to the previous close.
	MinPrice      float64 `json:"min_price"`
	MaxPrice      float64 `json:"max_price"`
	MinPrevVolume uint64  `json:"min_prev_volume"`

	// PrescreenGapPct is the first-pass gate; keep it at or below the smallest
	// threshold you care about, since only symbols clearing it get history.
	PrescreenGapPct   float64 `json:"prescreen_gap_pct"`
	MaxHistoryLookups int     `json:"max_history_lookups"`
	MaxCandidates     int     `json:"max_candidates"` // 0 means unlimited

	HistoryDays     int `json:"history_days"`
	ATRPeriod       int `json:"atr_period"`
	MinSigmaSamples int `json:"min_sigma_samples"`

	PremarketStartET   string `json:"premarket_start_et"` // ET, "HH:MM"
	MinPremarketVolume uint64 `json:"min_premarket_volume"`

	// UseAuctionOpen prefers the official opening auction print. Post-open
	// only, and the auctions endpoint requires the SIP feed.
	UseAuctionOpen bool `json:"use_auction_open"`

	// RequireAllMethods demands all three measurements fire, not just one.
	RequireAllMethods bool          `json:"require_all_methods"`
	Thresholds        GapThresholds `json:"thresholds"`

	SnapshotBatchSize int `json:"snapshot_batch_size"`
	HistoryBatchSize  int `json:"history_batch_size"`
	MaxConcurrency    int `json:"max_concurrency"`
	RequestsPerMinute int `json:"requests_per_minute"`

	// Outcome collection, run after the close to label the day's candidates.
	OutcomeIntervalMinutes int   `json:"outcome_interval_minutes"`
	EntryMinutesFromOpen   int   `json:"entry_minutes_from_open"`
	OutcomeHorizonsMinutes []int `json:"outcome_horizons_minutes"`

	// Strategy simulation: sell as soon as price clears the target, otherwise
	// hold to the close. ExitReference is "entry" (any profit on the trade) or
	// "prev_close" (the gap has filled and gone green, which only makes sense
	// for down gappers).
	ExitReference string `json:"exit_reference"`

	// ExitTargetMode is "atr" (ExitTargetATR multiples of the symbol's daily
	// range) or "pct" (a flat ExitTargetPct). ATR mode is the default because
	// one flat percentage cannot suit both a $2 stock and a $500 one.
	// ExitTargetPct still applies as the fallback when a symbol has no ATR.
	ExitTargetMode string  `json:"exit_target_mode"`
	ExitTargetATR  float64 `json:"exit_target_atr"`
	ExitTargetPct  float64 `json:"exit_target_pct"`

	// MinTargetCostMult floors every target at this multiple of its own
	// round-trip cost, so a fill is never a loss. At 2.0 half the move is kept.
	MinTargetCostMult float64 `json:"min_target_cost_mult"`

	// SweepATRMultiples is the grid -sweep replays collected trades against.
	// Empty uses the built-in range.
	SweepATRMultiples []float64 `json:"sweep_atr_multiples"`

	// Swing simulation: multi-day holds scored from daily bars only, so it
	// needs no real-time subscription and runs alongside the intraday path
	// rather than replacing it.
	//
	// SwingEntry is "session_close" (buy the gap day's close) or "next_open".
	SwingEntry       string  `json:"swing_entry"`
	SwingHorizonDays []int   `json:"swing_horizon_days"`
	SwingTargetATR   float64 `json:"swing_target_atr"`
	SwingStopATR     float64 `json:"swing_stop_atr"`
	SwingMaxHoldDays int     `json:"swing_max_hold_days"`

	// NewsLookbackHours is the window before the open searched for the story
	// that caused the gap. News-driven gaps and quiet ones behave differently,
	// so this is the tag that separates them.
	NewsLookbackHours int `json:"news_lookback_hours"`

	// Grid for -swing-sweep: holding periods against target multiples.
	SwingSweepDays      []int     `json:"swing_sweep_days"`
	SwingSweepTargetATR []float64 `json:"swing_sweep_target_atr"`

	// Costs charged against every simulated round trip, so the reported
	// return is net of the friction a real fill would have paid.
	Costs CostModel `json:"costs"`

	// Live paper tracking.
	PositionNotional           float64 `json:"position_notional"`
	CloseAllMinutesBeforeClose int     `json:"close_all_minutes_before_close"`
	ExportDir                  string  `json:"export_dir"`

	// WatchlistFile is the latest run; DataDir holds the per-session phase
	// files and the append-only logs used for modelling.
	WatchlistFile string `json:"watchlist_file"`
	DataDir       string `json:"data_dir"`
}

// Config represents the structure of our JSON file
type Config struct {
	AppName     string        `json:"app_name"`
	Version     string        `json:"version"`
	Debug       bool          `json:"debug"`
	MaxDaySpend int           `json:"max_day_spend"`
	CashFloor   int           `json:"CashFloor"`
	Positions   []Position    `json:"positions"`
	Scanner     ScannerConfig `json:"scanner"`
}

// DefaultScannerConfig returns settings tuned for a broad daily pre-market
// scan of liquid US equities.
func DefaultScannerConfig() ScannerConfig {
	return ScannerConfig{
		Enabled:               true,
		Feed:                  "iex",
		Exchanges:             []string{"NASDAQ", "NYSE", "ARCA", "AMEX", "BATS"},
		ExcludeComplexSymbols: true,
		MinPrice:              1.0,
		MaxPrice:              1000.0,
		MinPrevVolume:         200_000,
		PrescreenGapPct:       2.0,
		MaxHistoryLookups:     600,
		MaxCandidates:         50,
		HistoryDays:           120,
		ATRPeriod:             14,
		MinSigmaSamples:       30,
		PremarketStartET:      "04:00",
		MinPremarketVolume:    0,
		UseAuctionOpen:        false,
		RequireAllMethods:     false,
		Thresholds: GapThresholds{
			GapPct:  4.0,
			ATRMult: 1.0,
			Sigma:   2.0,
		},
		SnapshotBatchSize: 400,
		HistoryBatchSize:  100,
		MaxConcurrency:    4,
		RequestsPerMinute: 180,
		WatchlistFile:     "watchlist.json",
		DataDir:           "data",

		OutcomeIntervalMinutes: 5,
		EntryMinutesFromOpen:   5,
		OutcomeHorizonsMinutes: []int{5, 15, 30, 60, 120, 240},
		ExitReference:          ExitRefEntry,
		ExitTargetMode:         ExitTargetModeATR,
		ExitTargetATR:          0.25,
		ExitTargetPct:          0.1,
		MinTargetCostMult:      2.0,
		Costs:                  DefaultCostModel(),

		SwingEntry:          SwingEntrySessionClose,
		SwingHorizonDays:    []int{1, 2, 3, 5, 10, 20},
		SwingTargetATR:      2.0,
		SwingStopATR:        1.0,
		SwingMaxHoldDays:    10,
		NewsLookbackHours:   24,
		SwingSweepDays:      []int{1, 2, 3, 5, 10, 20},
		SwingSweepTargetATR: []float64{0.5, 1.0, 1.5, 2.0, 3.0, 4.0},

		PositionNotional:           1000,
		CloseAllMinutesBeforeClose: 5,
		ExportDir:                  "data/export",
	}
}

// applyScannerDefaults fills in any field left at its zero value so that an
// older or hand-trimmed config.json still produces a working scan.
func applyScannerDefaults(cfg *ScannerConfig) {
	d := DefaultScannerConfig()

	if cfg.Feed == "" {
		cfg.Feed = d.Feed
	}
	if len(cfg.Exchanges) == 0 {
		cfg.Exchanges = d.Exchanges
	}
	if cfg.MinPrice <= 0 {
		cfg.MinPrice = d.MinPrice
	}
	if cfg.MaxPrice <= 0 {
		cfg.MaxPrice = d.MaxPrice
	}
	if cfg.PrescreenGapPct <= 0 {
		cfg.PrescreenGapPct = d.PrescreenGapPct
	}
	if cfg.MaxHistoryLookups <= 0 {
		cfg.MaxHistoryLookups = d.MaxHistoryLookups
	}
	if cfg.HistoryDays <= 0 {
		cfg.HistoryDays = d.HistoryDays
	}
	if cfg.ATRPeriod <= 0 {
		cfg.ATRPeriod = d.ATRPeriod
	}
	if cfg.MinSigmaSamples <= 0 {
		cfg.MinSigmaSamples = d.MinSigmaSamples
	}
	if cfg.PremarketStartET == "" {
		cfg.PremarketStartET = d.PremarketStartET
	}
	if cfg.Thresholds.GapPct <= 0 {
		cfg.Thresholds.GapPct = d.Thresholds.GapPct
	}
	if cfg.Thresholds.ATRMult <= 0 {
		cfg.Thresholds.ATRMult = d.Thresholds.ATRMult
	}
	if cfg.Thresholds.Sigma <= 0 {
		cfg.Thresholds.Sigma = d.Thresholds.Sigma
	}
	if cfg.SnapshotBatchSize <= 0 {
		cfg.SnapshotBatchSize = d.SnapshotBatchSize
	}
	if cfg.HistoryBatchSize <= 0 {
		cfg.HistoryBatchSize = d.HistoryBatchSize
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = d.MaxConcurrency
	}
	if cfg.RequestsPerMinute <= 0 {
		cfg.RequestsPerMinute = d.RequestsPerMinute
	}
	if cfg.WatchlistFile == "" {
		cfg.WatchlistFile = d.WatchlistFile
	}
	if cfg.DataDir == "" {
		cfg.DataDir = d.DataDir
	}
	if cfg.OutcomeIntervalMinutes <= 0 {
		cfg.OutcomeIntervalMinutes = d.OutcomeIntervalMinutes
	}
	if cfg.EntryMinutesFromOpen <= 0 {
		cfg.EntryMinutesFromOpen = d.EntryMinutesFromOpen
	}
	if len(cfg.OutcomeHorizonsMinutes) == 0 {
		cfg.OutcomeHorizonsMinutes = d.OutcomeHorizonsMinutes
	}
	if cfg.ExitReference == "" {
		cfg.ExitReference = d.ExitReference
	}
	if cfg.ExitTargetMode == "" {
		cfg.ExitTargetMode = d.ExitTargetMode
	}
	if cfg.ExitTargetATR <= 0 {
		cfg.ExitTargetATR = d.ExitTargetATR
	}
	if cfg.ExitTargetPct <= 0 {
		cfg.ExitTargetPct = d.ExitTargetPct
	}
	if cfg.MinTargetCostMult <= 0 {
		cfg.MinTargetCostMult = d.MinTargetCostMult
	}
	// SlippagePct and AssumedSpreadPct are deliberately not filled: zero is a
	// meaningful choice for both.
	if cfg.Costs.PerShareFee <= 0 {
		cfg.Costs.PerShareFee = d.Costs.PerShareFee
	}
	if cfg.Costs.SellNotionalPct <= 0 {
		cfg.Costs.SellNotionalPct = d.Costs.SellNotionalPct
	}
	if cfg.PositionNotional <= 0 {
		cfg.PositionNotional = d.PositionNotional
	}
	if cfg.CloseAllMinutesBeforeClose <= 0 {
		cfg.CloseAllMinutesBeforeClose = d.CloseAllMinutesBeforeClose
	}
	if cfg.ExportDir == "" {
		cfg.ExportDir = d.ExportDir
	}
	if cfg.SwingEntry == "" {
		cfg.SwingEntry = d.SwingEntry
	}
	if len(cfg.SwingHorizonDays) == 0 {
		cfg.SwingHorizonDays = d.SwingHorizonDays
	}
	if cfg.SwingTargetATR <= 0 {
		cfg.SwingTargetATR = d.SwingTargetATR
	}
	if cfg.SwingStopATR <= 0 {
		cfg.SwingStopATR = d.SwingStopATR
	}
	if cfg.SwingMaxHoldDays <= 0 {
		cfg.SwingMaxHoldDays = d.SwingMaxHoldDays
	}
	if cfg.NewsLookbackHours <= 0 {
		cfg.NewsLookbackHours = d.NewsLookbackHours
	}
	if len(cfg.SwingSweepDays) == 0 {
		cfg.SwingSweepDays = d.SwingSweepDays
	}
	if len(cfg.SwingSweepTargetATR) == 0 {
		cfg.SwingSweepTargetATR = d.SwingSweepTargetATR
	}
}
