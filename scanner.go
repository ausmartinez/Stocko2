package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/civil"
	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
)

// The three ways of measuring a gap, in the order they are reported.
const (
	MethodPercent = "percent"
	MethodATR     = "atr"
	MethodSigma   = "sigma"
)

// The two daily runs. PhasePremarket projects the gap from pre-market prints;
// PhaseOpen measures it against the real open.
const (
	PhasePremarket = "premarket"
	PhaseOpen      = "open"
	// DiscoveredUnknown is used when no pre-open scan exists to compare
	// against, so lineage is genuinely unknown rather than "new at open".
	DiscoveredUnknown = "unknown"
)

// GapCandidate is one symbol that cleared at least one measurement.
type GapCandidate struct {
	Symbol    string `json:"symbol"`
	Direction string `json:"direction"` // "up" or "down"

	PrevClose  float64 `json:"prev_close"`
	PrevDate   string  `json:"prev_session_date"`
	PrevVolume uint64  `json:"prev_volume"`

	// RefSource records where RefPrice came from, so a projected gap is never
	// mistaken for a confirmed one.
	RefPrice  float64 `json:"ref_price"`
	RefSource string  `json:"ref_source"`

	// Method 1: percentage move off the previous close. Signed.
	GapPct float64 `json:"gap_pct"`

	// Method 2: the move in average true ranges. GapATR is a magnitude.
	ATR        float64 `json:"atr"`
	ATRSamples int     `json:"atr_samples"`
	GapATR     float64 `json:"gap_atr"`

	// Method 3: the move in stdevs of this symbol's overnight returns. Signed.
	OvernightMeanPct  float64 `json:"overnight_mean_pct"`
	OvernightStdevPct float64 `json:"overnight_stdev_pct"`
	GapSigma          float64 `json:"gap_sigma"`
	SigmaSamples      int     `json:"sigma_samples"`

	// The two-sided quote at scan time. This is the dominant trading cost and
	// it is indicative only: the spread actually paid is whatever is quoted at
	// entry, which on a gapper minutes after the bell can be far wider.
	Bid       float64 `json:"bid"`
	Ask       float64 `json:"ask"`
	SpreadPct float64 `json:"spread_pct"`

	PremarketVolume uint64  `json:"premarket_volume"`
	PremarketHigh   float64 `json:"premarket_high"`
	PremarketLow    float64 `json:"premarket_low"`
	PremarketLast   float64 `json:"premarket_last"`
	PremarketBars   int     `json:"premarket_bars"`

	// Context features for modelling.
	AvgVolume            uint64  `json:"avg_volume"`
	PrevVolumeRatio      float64 `json:"prev_volume_ratio"`
	PrevDayReturnPct     float64 `json:"prev_day_return_pct"`
	PrevDayRangePct      float64 `json:"prev_day_range_pct"`
	PremarketRangePct    float64 `json:"premarket_range_pct"`
	PremarketVolumeRatio float64 `json:"premarket_volume_ratio"`

	// Lineage across the two runs.
	SeenPremarket bool   `json:"seen_premarket"`
	DiscoveredAt  string `json:"discovered_at"` // premarket, open or unknown
	// Faded marks a pre-open candidate whose gap no longer clears any
	// threshold at the open. Kept deliberately as a negative example.
	Faded bool `json:"faded"`

	// Set on the open run for symbols carried over from the pre-open list.
	ProjectedGapPct   float64 `json:"projected_gap_pct,omitempty"`
	ProjectedRefPrice float64 `json:"projected_ref_price,omitempty"`
	GapDeltaPct       float64 `json:"gap_delta_pct,omitempty"`

	// Methods lists which measurements flagged this symbol.
	Methods []string `json:"methods"`
}

// ScanResult is the watchlist written to disk.
type ScanResult struct {
	GeneratedAt       string        `json:"generated_at"`
	SessionDate       string        `json:"session_date"`
	SessionOpen       string        `json:"session_open"`
	Phase             string        `json:"phase"`
	Mode              string        `json:"mode"`
	Feed              string        `json:"feed"`
	Thresholds        GapThresholds `json:"thresholds"`
	RequireAllMethods bool          `json:"require_all_methods"`

	// Lineage summary for the open run.
	PremarketScanFound  bool `json:"premarket_scan_found"`
	PremarketCandidates int  `json:"premarket_candidates"`
	NewAtOpen           int  `json:"new_at_open"`
	FadedCount          int  `json:"faded_count"`

	UniverseSize      int `json:"universe_size"`
	SnapshotsWithData int `json:"snapshots_with_data"`
	// NotYetTraded is the pre-open blind spot: symbols with no print to screen
	// on, which can still gap hard in the opening auction.
	NotYetTraded   int            `json:"not_yet_traded"`
	Prescreened    int            `json:"prescreened"`
	HistoryFetched int            `json:"history_fetched"`
	Candidates     []GapCandidate `json:"candidates"`
}

// Scanner finds gap candidates for a single trading session.
type Scanner struct {
	trading *alpaca.Client
	data    *marketdata.Client
	cfg     ScannerConfig
	loc     *time.Location
	limiter *rateLimiter
}

// NewScanner wires a scanner to an existing trading client. Market data is a
// separate service, so it gets its own client.
func NewScanner(trading *alpaca.Client, cfg ScannerConfig) (*Scanner, error) {
	loc, err := easternLocation()
	if err != nil {
		return nil, err
	}

	return &Scanner{
		trading: trading,
		data: marketdata.NewClient(marketdata.ClientOpts{
			APIKey:    os.Getenv("APCA_API_KEY_ID"),
			APISecret: os.Getenv("APCA_API_SECRET_KEY"),
			Feed:      cfg.Feed,
		}),
		cfg:     cfg,
		loc:     loc,
		limiter: newRateLimiter(cfg.RequestsPerMinute),
	}, nil
}

// Close releases the scanner's rate limiter.
func (s *Scanner) Close() {
	s.limiter.close()
}

// prescreenStats records why symbols fell out of the first pass.
type prescreenStats struct {
	withData         int
	noReferencePrice int
}

// rawGap is the cheap first-pass view of a symbol, before any history is pulled.
type rawGap struct {
	symbol     string
	prevClose  float64
	prevDate   string
	prevVolume uint64
	refPrice   float64
	refSource  string
	gapPct     float64
	bid        float64
	ask        float64
	spreadPct  float64
}

// Scan runs the full pipeline for the given session and returns the watchlist.
func (s *Scanner) Scan(session TradingSession) (*ScanResult, error) {
	phase := PhaseOpen
	if session.BeforeOpen {
		phase = PhasePremarket
	}

	// On the open run, load the pre-open list so candidates can be marked as
	// carried over or new, and carry its symbols forward unconditionally so we
	// still record what happened to gaps that evaporated.
	baseline, err := s.loadBaseline(session, phase)
	if err != nil {
		return nil, err
	}
	carry := make(map[string]bool)
	if baseline != nil {
		for _, c := range baseline.Candidates {
			carry[c.Symbol] = true
		}
	}

	symbols, err := s.Universe()
	if err != nil {
		return nil, err
	}
	log.Printf("scanner: universe of %d symbols, session %s, phase %s", len(symbols), session.Date, phase)

	raws, stats, err := s.prescreen(symbols, session, carry)
	if err != nil {
		return nil, err
	}
	log.Printf("scanner: %d symbols returned usable snapshots, %d cleared the %.2f%% prescreen",
		stats.withData, len(raws), s.cfg.PrescreenGapPct)
	if stats.noReferencePrice > 0 {
		log.Printf("scanner: %d symbols had not traded yet and could not be screened; the open run catches them",
			stats.noReferencePrice)
	}

	raws = s.rankAndCap(raws, carry)

	if !session.BeforeOpen && s.cfg.UseAuctionOpen {
		if err := s.applyAuctionOpens(raws, session); err != nil {
			// The auctions endpoint is SIP-only, so a failure here is a
			// subscription problem rather than a reason to abandon the scan.
			log.Printf("scanner: auction open lookup failed, falling back to daily bar opens: %v", err)
		}
	}

	candidates, err := s.measure(raws, session)
	if err != nil {
		return nil, err
	}

	// Keep anything that flagged, plus every carried-forward symbol whether it
	// flagged or not.
	kept := make([]GapCandidate, 0, len(candidates))
	for _, c := range candidates {
		if len(c.Methods) > 0 || carry[c.Symbol] {
			kept = append(kept, c)
		}
	}
	log.Printf("scanner: %d symbols flagged, %d rows kept including carry-forwards",
		countFlagged(kept), len(kept))

	if err := s.attachPremarket(kept, session); err != nil {
		log.Printf("scanner: pre-market bar lookup failed, continuing without it: %v", err)
	}

	if s.cfg.MinPremarketVolume > 0 {
		filtered := make([]GapCandidate, 0, len(kept))
		for _, c := range kept {
			// Never drop a carry-forward: its absence is the data point.
			if carry[c.Symbol] || c.PremarketVolume >= s.cfg.MinPremarketVolume {
				filtered = append(filtered, c)
			}
		}
		kept = filtered
	}

	applyLineage(kept, phase, baseline)
	sortCandidates(kept)
	kept = s.capCandidates(kept)

	result := &ScanResult{
		GeneratedAt:         session.Now.Format(time.RFC3339),
		SessionDate:         session.Date,
		SessionOpen:         session.Open.Format(time.RFC3339),
		Phase:               phase,
		Mode:                session.Mode(),
		Feed:                s.cfg.Feed,
		Thresholds:          s.cfg.Thresholds,
		RequireAllMethods:   s.cfg.RequireAllMethods,
		PremarketScanFound:  baseline != nil,
		PremarketCandidates: len(carry),
		UniverseSize:        len(symbols),
		SnapshotsWithData:   stats.withData,
		NotYetTraded:        stats.noReferencePrice,
		Prescreened:         len(raws),
		HistoryFetched:      len(candidates),
		Candidates:          kept,
	}
	for _, c := range kept {
		if c.Faded {
			result.FadedCount++
		}
		if c.DiscoveredAt == PhaseOpen {
			result.NewAtOpen++
		}
	}
	if phase == PhaseOpen {
		log.Printf("scanner: %d carried from pre-open, %d new at open, %d faded",
			len(carry), result.NewAtOpen, result.FadedCount)
	}
	return result, nil
}

// loadBaseline reads the pre-open result for this session, which only exists on
// the open run.
func (s *Scanner) loadBaseline(session TradingSession, phase string) (*ScanResult, error) {
	if phase != PhaseOpen {
		return nil, nil
	}

	path := PhaseFile(s.cfg.DataDir, session.Date, PhasePremarket)
	baseline, err := LoadScanResult(path)
	if err != nil {
		return nil, err
	}
	if baseline == nil {
		log.Printf("scanner: no pre-open scan at %s, so lineage is unknown for this session", path)
		return nil, nil
	}
	log.Printf("scanner: loaded %d pre-open candidates from %s", len(baseline.Candidates), path)
	return baseline, nil
}

// rankAndCap keeps the largest raw moves, never dropping a carried-forward
// symbol since the history pass is what gives it its open measurement.
func (s *Scanner) rankAndCap(raws []rawGap, carry map[string]bool) []rawGap {
	var carried, rest []rawGap
	for _, r := range raws {
		if carry[r.symbol] {
			carried = append(carried, r)
		} else {
			rest = append(rest, r)
		}
	}

	sort.Slice(rest, func(i, j int) bool {
		return math.Abs(rest[i].gapPct) > math.Abs(rest[j].gapPct)
	})
	if len(rest) > s.cfg.MaxHistoryLookups {
		rest = rest[:s.cfg.MaxHistoryLookups]
	}
	return append(carried, rest...)
}

// capCandidates applies MaxCandidates to freshly flagged rows only, so
// carry-forward records are never truncated away.
func (s *Scanner) capCandidates(candidates []GapCandidate) []GapCandidate {
	if s.cfg.MaxCandidates <= 0 {
		return candidates
	}

	out := make([]GapCandidate, 0, len(candidates))
	flagged := 0
	for _, c := range candidates {
		if len(c.Methods) == 0 {
			out = append(out, c)
			continue
		}
		if flagged < s.cfg.MaxCandidates {
			out = append(out, c)
			flagged++
		}
	}
	return out
}

// applyLineage marks each candidate as a pre-open carry-over or a new find, and
// records how far the open drifted from the pre-market projection.
func applyLineage(candidates []GapCandidate, phase string, baseline *ScanResult) {
	if phase == PhasePremarket {
		for i := range candidates {
			candidates[i].SeenPremarket = true
			candidates[i].DiscoveredAt = PhasePremarket
		}
		return
	}

	// Without a pre-open scan we cannot tell a new gapper from a carried one.
	if baseline == nil {
		for i := range candidates {
			candidates[i].DiscoveredAt = DiscoveredUnknown
		}
		return
	}

	prior := make(map[string]GapCandidate, len(baseline.Candidates))
	for _, c := range baseline.Candidates {
		prior[c.Symbol] = c
	}

	for i := range candidates {
		c := &candidates[i]
		p, seen := prior[c.Symbol]
		if !seen {
			c.DiscoveredAt = PhaseOpen
			continue
		}

		c.SeenPremarket = true
		c.DiscoveredAt = PhasePremarket
		c.ProjectedGapPct = p.GapPct
		c.ProjectedRefPrice = p.RefPrice
		c.GapDeltaPct = c.GapPct - p.GapPct
		c.Faded = len(c.Methods) == 0
	}
}

func countFlagged(candidates []GapCandidate) int {
	n := 0
	for _, c := range candidates {
		if len(c.Methods) > 0 {
			n++
		}
	}
	return n
}

// Universe returns the tradable symbols to scan.
func (s *Scanner) Universe() ([]string, error) {
	if len(s.cfg.SymbolsOverride) > 0 {
		out := make([]string, 0, len(s.cfg.SymbolsOverride))
		for _, sym := range s.cfg.SymbolsOverride {
			out = append(out, strings.ToUpper(strings.TrimSpace(sym)))
		}
		return out, nil
	}

	assets, err := s.trading.GetAssets(alpaca.GetAssetsRequest{
		Status:     string(alpaca.AssetActive),
		AssetClass: string(alpaca.USEquity),
	})
	if err != nil {
		return nil, fmt.Errorf("listing assets: %w", err)
	}

	allowed := make(map[string]bool, len(s.cfg.Exchanges))
	for _, ex := range s.cfg.Exchanges {
		allowed[strings.ToUpper(ex)] = true
	}

	symbols := make([]string, 0, len(assets))
	for _, a := range assets {
		if !a.Tradable {
			continue
		}
		if len(allowed) > 0 && !allowed[strings.ToUpper(a.Exchange)] {
			continue
		}
		if s.cfg.ExcludeComplexSymbols && !isSimpleSymbol(a.Symbol) {
			continue
		}
		symbols = append(symbols, a.Symbol)
	}
	sort.Strings(symbols)
	return symbols, nil
}

// isSimpleSymbol keeps plain 1-5 letter tickers, which filters out most
// warrants, units, rights and preferred share classes.
func isSimpleSymbol(symbol string) bool {
	if len(symbol) == 0 || len(symbol) > 5 {
		return false
	}
	for _, r := range symbol {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// prescreen snapshots the whole universe and keeps the symbols whose raw move
// is large enough to be worth a history lookup.
// Symbols in carry bypass the gates so the open run always measures them.
func (s *Scanner) prescreen(
	symbols []string, session TradingSession, carry map[string]bool,
) ([]rawGap, prescreenStats, error) {
	var (
		mu    sync.Mutex
		raws  []rawGap
		stats prescreenStats
	)

	err := s.forEachBatch(symbols, s.cfg.SnapshotBatchSize, func(batch []string) error {
		snaps, err := s.data.GetSnapshots(batch, marketdata.GetSnapshotRequest{Feed: s.cfg.Feed})
		if err != nil {
			return fmt.Errorf("getting snapshots: %w", err)
		}

		local := make([]rawGap, 0, len(snaps))
		var batchStats prescreenStats
		for symbol, snap := range snaps {
			prevBar, todayBar := resolveDailyBars(snap, session.Date, s.loc)
			if prevBar == nil || prevBar.Close <= 0 {
				continue
			}
			batchStats.withData++

			ref, source, ok := referencePrice(snap, todayBar, session, s.loc)
			if !ok {
				// Nothing has traded yet, so this symbol cannot be screened
				// pre-open however big its gap turns out to be.
				batchStats.noReferencePrice++
				continue
			}
			forced := carry[symbol]
			if !forced && !s.passesUniverseFilters(prevBar) {
				continue
			}

			gapPct := (ref - prevBar.Close) / prevBar.Close * 100
			if !forced && math.Abs(gapPct) < s.cfg.PrescreenGapPct {
				continue
			}

			// The snapshot already carries the quote, so recording the spread
			// costs no extra request.
			var bidPrice, askPrice float64
			if snap.LatestQuote != nil {
				bidPrice, askPrice = snap.LatestQuote.BidPrice, snap.LatestQuote.AskPrice
			}
			bid, ask, spreadPct := quoteSpread(bidPrice, askPrice)

			local = append(local, rawGap{
				symbol:     symbol,
				prevClose:  prevBar.Close,
				prevDate:   sessionDateOf(prevBar.Timestamp, s.loc),
				prevVolume: prevBar.Volume,
				refPrice:   ref,
				refSource:  source,
				gapPct:     gapPct,
				bid:        bid,
				ask:        ask,
				spreadPct:  spreadPct,
			})
		}

		mu.Lock()
		raws = append(raws, local...)
		stats.withData += batchStats.withData
		stats.noReferencePrice += batchStats.noReferencePrice
		mu.Unlock()
		return nil
	})

	return raws, stats, err
}

// passesUniverseFilters gates on price and liquidity. Reads the previous close
// rather than the post-gap price so the band doesn't favour a gap direction.
func (s *Scanner) passesUniverseFilters(prevBar *marketdata.Bar) bool {
	return prevBar.Close >= s.cfg.MinPrice &&
		prevBar.Close <= s.cfg.MaxPrice &&
		prevBar.Volume >= s.cfg.MinPrevVolume
}

// resolveDailyBars picks the bar for the last completed session. Alpaca rolls
// DailyBar over as soon as a symbol has any print, including pre-market, so
// before the bell it holds today only for symbols already trading.
func resolveDailyBars(snap *marketdata.Snapshot, sessionDate string, loc *time.Location) (prev, today *marketdata.Bar) {
	if snap == nil {
		return nil, nil
	}
	if snap.DailyBar != nil && sessionDateOf(snap.DailyBar.Timestamp, loc) == sessionDate {
		return snap.PrevDailyBar, snap.DailyBar
	}
	return snap.DailyBar, nil
}

// referencePrice picks the price the gap is measured to: the latest pre-market
// print before the bell, the session open afterwards.
func referencePrice(
	snap *marketdata.Snapshot, todayBar *marketdata.Bar, session TradingSession, loc *time.Location,
) (price float64, source string, ok bool) {
	if session.BeforeOpen {
		if snap.LatestTrade != nil && snap.LatestTrade.Price > 0 &&
			sessionDateOf(snap.LatestTrade.Timestamp, loc) == session.Date {
			return snap.LatestTrade.Price, "premarket_last_trade", true
		}
		if todayBar != nil && todayBar.Close > 0 {
			return todayBar.Close, "premarket_daily_bar", true
		}
		// No pre-market print at all, so there is nothing to project from.
		return 0, "", false
	}

	if todayBar != nil && todayBar.Open > 0 {
		return todayBar.Open, "session_open_daily_bar", true
	}
	// Fall back to the last trade only if it belongs to this session. A symbol
	// that has not opened yet still carries the previous session's trade, which
	// would otherwise be measured as a 0% gap instead of reported as missing.
	if snap.LatestTrade != nil && snap.LatestTrade.Price > 0 &&
		sessionDateOf(snap.LatestTrade.Timestamp, loc) == session.Date {
		return snap.LatestTrade.Price, "latest_trade", true
	}
	return 0, "", false
}

// applyAuctionOpens replaces daily-bar opens with the official opening auction
// print, which is the authoritative open. The auctions endpoint is SIP-only.
func (s *Scanner) applyAuctionOpens(raws []rawGap, session TradingSession) error {
	if len(raws) == 0 {
		return nil
	}

	symbols := make([]string, 0, len(raws))
	for _, r := range raws {
		symbols = append(symbols, r.symbol)
	}

	day, err := civil.ParseDate(session.Date)
	if err != nil {
		return fmt.Errorf("parsing session date %q: %w", session.Date, err)
	}

	opens := make(map[string]float64, len(raws))
	var mu sync.Mutex

	err = s.forEachBatch(symbols, s.cfg.HistoryBatchSize, func(batch []string) error {
		res, err := s.data.GetMultiAuctions(batch, marketdata.GetAuctionsRequest{
			Start: session.Open.Add(-time.Hour),
			End:   session.Close,
		})
		if err != nil {
			return fmt.Errorf("getting auctions: %w", err)
		}

		mu.Lock()
		defer mu.Unlock()
		for symbol, daily := range res {
			for _, d := range daily {
				if d.Date != day || len(d.Opening) == 0 {
					continue
				}
				// Take the first opening print of the session; for a primary
				// listing that is the official opening auction.
				if p := d.Opening[0].Price; p > 0 {
					opens[symbol] = p
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	applied := 0
	for i := range raws {
		open, found := opens[raws[i].symbol]
		if !found {
			continue
		}
		raws[i].refPrice = open
		raws[i].refSource = "opening_auction"
		raws[i].gapPct = (open - raws[i].prevClose) / raws[i].prevClose * 100
		applied++
	}
	log.Printf("scanner: applied official opening auction prices to %d/%d symbols", applied, len(raws))
	return nil
}

// measure pulls daily history for the prescreened symbols and computes all
// three measurements, flagging each symbol against the configured thresholds.
func (s *Scanner) measure(raws []rawGap, session TradingSession) ([]GapCandidate, error) {
	if len(raws) == 0 {
		return nil, nil
	}

	bySymbol := make(map[string]rawGap, len(raws))
	symbols := make([]string, 0, len(raws))
	for _, r := range raws {
		bySymbol[r.symbol] = r
		symbols = append(symbols, r.symbol)
	}

	var (
		mu         sync.Mutex
		candidates []GapCandidate
	)

	err := s.forEachBatch(symbols, s.cfg.HistoryBatchSize, func(batch []string) error {
		bars, err := s.data.GetMultiBars(batch, marketdata.GetBarsRequest{
			TimeFrame: marketdata.OneDay,
			// Without this a 4:1 split looks like a -75% gap.
			Adjustment: marketdata.AdjustmentAll,
			Start:      session.Open.AddDate(0, 0, -s.cfg.HistoryDays),
			End:        session.Open,
			Feed:       s.cfg.Feed,
		})
		if err != nil {
			return fmt.Errorf("getting daily bars: %w", err)
		}

		local := make([]GapCandidate, 0, len(batch))
		for symbol, history := range bars {
			raw, found := bySymbol[symbol]
			if !found {
				continue
			}
			local = append(local, s.buildCandidate(raw, s.trimHistory(history, session)))
		}

		mu.Lock()
		candidates = append(candidates, local...)
		mu.Unlock()
		return nil
	})

	return candidates, err
}

// trimHistory drops any bar belonging to the session being scanned so that the
// gap is never measured against a baseline that already contains it.
func (s *Scanner) trimHistory(bars []marketdata.Bar, session TradingSession) []marketdata.Bar {
	out := bars[:0:0]
	for _, b := range bars {
		if sessionDateOf(b.Timestamp, s.loc) >= session.Date {
			continue
		}
		out = append(out, b)
	}
	return out
}

// buildCandidate computes all three measurements and records which ones fire.
func (s *Scanner) buildCandidate(raw rawGap, history []marketdata.Bar) GapCandidate {
	th := s.cfg.Thresholds

	c := GapCandidate{
		Symbol:     raw.symbol,
		Direction:  "up",
		PrevClose:  raw.prevClose,
		PrevDate:   raw.prevDate,
		PrevVolume: raw.prevVolume,
		RefPrice:   raw.refPrice,
		RefSource:  raw.refSource,
		GapPct:     raw.gapPct,
		Bid:        raw.bid,
		Ask:        raw.ask,
		SpreadPct:  raw.spreadPct,
		// Empty rather than nil so faded rows serialise as [] for a dataframe.
		Methods: []string{},
	}
	if raw.gapPct < 0 {
		c.Direction = "down"
	}

	s.attachHistoryFeatures(&c, raw, history)

	// Method 2: gap in ATR units.
	atr, atrSamples := AverageTrueRange(history, s.cfg.ATRPeriod)
	c.ATR = atr
	c.ATRSamples = atrSamples
	if atr > 0 {
		c.GapATR = math.Abs(raw.refPrice-raw.prevClose) / atr
	}

	// Method 3: gap in standard deviations of this symbol's overnight returns.
	mean, stdev, sigmaSamples := OvernightStats(history)
	c.OvernightMeanPct = mean * 100
	c.OvernightStdevPct = stdev * 100
	c.SigmaSamples = sigmaSamples
	if stdev > 0 {
		c.GapSigma = (raw.gapPct/100 - mean) / stdev
	}

	// Method 1: plain percentage.
	percentHit := math.Abs(c.GapPct) >= th.GapPct
	atrHit := atr > 0 && c.GapATR >= th.ATRMult
	sigmaHit := stdev > 0 && sigmaSamples >= s.cfg.MinSigmaSamples && math.Abs(c.GapSigma) >= th.Sigma

	if s.cfg.RequireAllMethods {
		if percentHit && atrHit && sigmaHit {
			c.Methods = []string{MethodPercent, MethodATR, MethodSigma}
		}
		return c
	}

	if percentHit {
		c.Methods = append(c.Methods, MethodPercent)
	}
	if atrHit {
		c.Methods = append(c.Methods, MethodATR)
	}
	if sigmaHit {
		c.Methods = append(c.Methods, MethodSigma)
	}
	return c
}

// attachHistoryFeatures records the prior session's behaviour, which is context
// a model needs to tell a gap on a quiet stock from one already in motion.
func (s *Scanner) attachHistoryFeatures(c *GapCandidate, raw rawGap, history []marketdata.Bar) {
	n := len(history)
	if n == 0 {
		return
	}

	last := history[n-1]
	if last.Close > 0 {
		c.PrevDayRangePct = (last.High - last.Low) / last.Close * 100
	}
	if n > 1 && history[n-2].Close > 0 {
		c.PrevDayReturnPct = (last.Close - history[n-2].Close) / history[n-2].Close * 100
	}

	var total uint64
	for _, b := range history {
		total += b.Volume
	}
	c.AvgVolume = total / uint64(n)
	if c.AvgVolume > 0 {
		c.PrevVolumeRatio = float64(raw.prevVolume) / float64(c.AvgVolume)
	}
}

// attachPremarket fills in pre-market activity for the flagged candidates. It
// runs last so that the expensive minute-bar pull only covers the short list.
func (s *Scanner) attachPremarket(candidates []GapCandidate, session TradingSession) error {
	if len(candidates) == 0 {
		return nil
	}

	index := make(map[string]int, len(candidates))
	symbols := make([]string, 0, len(candidates))
	for i, c := range candidates {
		index[c.Symbol] = i
		symbols = append(symbols, c.Symbol)
	}

	var mu sync.Mutex

	err := s.forEachBatch(symbols, s.cfg.HistoryBatchSize, func(batch []string) error {
		bars, err := s.data.GetMultiBars(batch, marketdata.GetBarsRequest{
			TimeFrame: marketdata.OneMin,
			Start:     session.PremarketStart,
			End:       session.Open,
			Feed:      s.cfg.Feed,
		})
		if err != nil {
			return fmt.Errorf("getting pre-market bars: %w", err)
		}

		mu.Lock()
		defer mu.Unlock()
		for symbol, minutes := range bars {
			i, found := index[symbol]
			if !found {
				continue
			}
			for _, b := range minutes {
				// End is inclusive, so drop the opening bell bar itself.
				if !b.Timestamp.Before(session.Open) {
					continue
				}
				c := &candidates[i]
				c.PremarketVolume += b.Volume
				c.PremarketBars++
				c.PremarketLast = b.Close
				if c.PremarketHigh == 0 || b.High > c.PremarketHigh {
					c.PremarketHigh = b.High
				}
				if c.PremarketLow == 0 || b.Low < c.PremarketLow {
					c.PremarketLow = b.Low
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Derived ratios, once every batch has landed.
	for i := range candidates {
		c := &candidates[i]
		if c.PrevClose > 0 && c.PremarketHigh > 0 {
			c.PremarketRangePct = (c.PremarketHigh - c.PremarketLow) / c.PrevClose * 100
		}
		if c.AvgVolume > 0 {
			c.PremarketVolumeRatio = float64(c.PremarketVolume) / float64(c.AvgVolume)
		}
	}
	return nil
}

// sortCandidates ranks by how many measurements agreed, then by the strength of
// the most context-aware measurement available.
func sortCandidates(candidates []GapCandidate) {
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if len(a.Methods) != len(b.Methods) {
			return len(a.Methods) > len(b.Methods)
		}
		if as, bs := math.Abs(a.GapSigma), math.Abs(b.GapSigma); as != bs {
			return as > bs
		}
		if a.GapATR != b.GapATR {
			return a.GapATR > b.GapATR
		}
		return math.Abs(a.GapPct) > math.Abs(b.GapPct)
	})
}

// forEachBatch runs fn over batches of symbols with bounded concurrency and
// rate limiting, stopping early on the first error.
func (s *Scanner) forEachBatch(items []string, size int, fn func(batch []string) error) error {
	batches := chunk(items, size)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, s.cfg.MaxConcurrency)
	)

	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}

	for _, batch := range batches {
		if failed() {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(batch []string) {
			defer wg.Done()
			defer func() { <-sem }()

			s.limiter.acquire()
			if err := fn(batch); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(batch)
	}

	wg.Wait()
	return firstErr
}

// WriteWatchlist persists the scan result as pretty JSON.
func WriteWatchlist(path string, result *ScanResult) error {
	data, err := json.MarshalIndent(result, "", "    ")
	if err != nil {
		return fmt.Errorf("encoding watchlist: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// PrintWatchlist writes a human-readable table to stdout, since the log output
// is redirected to a file.
func PrintWatchlist(result *ScanResult) {
	fmt.Printf("\nGapper scan  session=%s  phase=%s  feed=%s\n", result.SessionDate, result.Phase, result.Feed)
	fmt.Printf("thresholds: %.2f%% | %.2f x ATR | %.2f sigma  (match=%s)\n",
		result.Thresholds.GapPct, result.Thresholds.ATRMult, result.Thresholds.Sigma,
		map[bool]string{true: "all", false: "any"}[result.RequireAllMethods])
	fmt.Printf("universe=%d  with-data=%d  not-yet-traded=%d  prescreened=%d  rows=%d\n",
		result.UniverseSize, result.SnapshotsWithData, result.NotYetTraded,
		result.Prescreened, len(result.Candidates))
	if result.Phase == PhaseOpen {
		fmt.Printf("pre-open list=%d (found=%t)  new at open=%d  faded=%d\n",
			result.PremarketCandidates, result.PremarketScanFound, result.NewAtOpen, result.FadedCount)
	}
	fmt.Println()

	if len(result.Candidates) == 0 {
		fmt.Println("no candidates cleared the thresholds")
		return
	}

	fmt.Printf("%-8s %-5s %-10s %9s %9s %8s %8s %9s %8s %10s  %s\n",
		"SYMBOL", "DIR", "FOUND", "PREV", "REF", "GAP%", "xATR", "SIGMA", "SPREAD%", "PM VOL", "METHODS")
	for _, c := range result.Candidates {
		found := c.DiscoveredAt
		if c.Faded {
			found = "faded"
		}
		methods := strings.Join(c.Methods, ",")
		if methods == "" {
			methods = "-"
		}
		fmt.Printf("%-8s %-5s %-10s %9.2f %9.2f %7.2f%% %8.2f %9.2f %8s %10d  %s\n",
			c.Symbol, c.Direction, found, c.PrevClose, c.RefPrice, c.GapPct, c.GapATR,
			c.GapSigma, formatSpread(c.SpreadPct), c.PremarketVolume, methods)
	}
	fmt.Println()
}

// formatSpread renders an unmeasured spread as "-" rather than 0.00, which
// would read as a free round trip.
func formatSpread(spreadPct float64) string {
	if spreadPct <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.3f", spreadPct)
}

// rateLimiter is a simple token bucket that refills at a fixed rate, used to
// stay under Alpaca's per-minute request cap.
type rateLimiter struct {
	tokens chan struct{}
	done   chan struct{}
	once   sync.Once
}

func newRateLimiter(perMinute int) *rateLimiter {
	if perMinute <= 0 {
		return nil
	}

	burst := perMinute / 4
	if burst < 1 {
		burst = 1
	}

	rl := &rateLimiter{
		tokens: make(chan struct{}, burst),
		done:   make(chan struct{}),
	}
	for i := 0; i < burst; i++ {
		rl.tokens <- struct{}{}
	}

	go func() {
		ticker := time.NewTicker(time.Minute / time.Duration(perMinute))
		defer ticker.Stop()
		for {
			select {
			case <-rl.done:
				return
			case <-ticker.C:
				select {
				case rl.tokens <- struct{}{}:
				default: // bucket full
				}
			}
		}
	}()
	return rl
}

func (rl *rateLimiter) acquire() {
	if rl == nil {
		return
	}
	<-rl.tokens
}

func (rl *rateLimiter) close() {
	if rl == nil {
		return
	}
	rl.once.Do(func() { close(rl.done) })
}
