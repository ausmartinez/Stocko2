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

	// Strategy simulation: sell as soon as price clears the reference by
	// ExitTargetPct, otherwise hold to the close. ExitReference is "entry"
	// (any profit on the trade) or "prev_close" (the gap has filled and gone
	// green, which only makes sense for down gappers).
	ExitReference string  `json:"exit_reference"`
	ExitTargetPct float64 `json:"exit_target_pct"`

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
		ExitTargetPct:          0.1,

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
	if cfg.ExitTargetPct <= 0 {
		cfg.ExitTargetPct = d.ExitTargetPct
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
}
