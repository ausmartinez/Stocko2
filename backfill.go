package main

import (
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
)

// PhaseBackfill marks a scan reconstructed from daily bars rather than observed
// live. Recorded on the row so a model can control for the difference: these
// have no pre-market figures and no captured quote.
const PhaseBackfill = "backfill"

// BackfillRefSource is what the gap was measured against.
const BackfillRefSource = "backfill_daily_open"

// PhaseBackfillPremarket marks a reconstructed pre-market pass. Kept distinct
// from PhaseBackfill so a row says which of the two runs produced it.
const PhaseBackfillPremarket = "backfill_premarket"

// BackfillPremarketRefSource is what a reconstructed pre-market gap measures to.
const BackfillPremarketRefSource = "backfill_premarket_last"

// premarketAgg is the pre-market state a symbol had reached by the scan hour,
// rebuilt from minute bars the way attachPremarket does live.
type premarketAgg struct {
	last   float64
	high   float64
	low    float64
	volume uint64
	bars   int
}

// aggregatePremarket folds a symbol's extended-hours minute bars into the
// figures the live scan reads from a snapshot plus attachPremarket.
func aggregatePremarket(bars []marketdata.Bar, cutoff time.Time) premarketAgg {
	var agg premarketAgg
	for _, b := range bars {
		// End is inclusive on the API, so drop anything at or past the cutoff.
		if !b.Timestamp.Before(cutoff) {
			continue
		}
		agg.volume += b.Volume
		agg.bars++
		agg.last = b.Close
		if agg.high == 0 || b.High > agg.high {
			agg.high = b.High
		}
		if agg.low == 0 || b.Low < agg.low {
			agg.low = b.Low
		}
	}
	return agg
}

// BackfillResult summarises one backfill run.
type BackfillResult struct {
	From            string `json:"from"`
	To              string `json:"to"`
	TwoPhase        bool   `json:"two_phase"`
	TradingDays     int    `json:"trading_days"`
	UniverseSize    int    `json:"universe_size"`
	SymbolsWithData int    `json:"symbols_with_data"`
	SessionsWritten int    `json:"sessions_written"`
	SessionsSkipped int    `json:"sessions_skipped"`
	Candidates      int    `json:"candidates"`

	// Only populated with -premarket: the lineage the second pass recovers.
	PremarketCandidates int `json:"premarket_candidates"`
	Faded               int `json:"faded"`
}

// premarketForDate pulls extended-hours minute bars for one date and folds them
// into per-symbol pre-market state as of the scan hour.
//
// This is the expensive pass and it cannot be narrowed to symbols that gapped
// at the open: a faded symbol is precisely one whose open gap is small, so
// filtering on that would drop the rows the whole exercise exists to capture.
func (s *Scanner) premarketForDate(
	symbols []string, date string, scanAt string,
) (map[string]premarketAgg, error) {
	start, err := parseETTime(s.loc, date, s.cfg.PremarketStartET)
	if err != nil {
		return nil, err
	}
	cutoff, err := parseETTime(s.loc, date, scanAt)
	if err != nil {
		return nil, err
	}

	out := make(map[string]premarketAgg, len(symbols))
	var mu sync.Mutex

	err = s.forEachBatch(symbols, s.cfg.HistoryBatchSize, func(batch []string) error {
		bars, err := s.data.GetMultiBars(batch, marketdata.GetBarsRequest{
			TimeFrame: marketdata.OneMin,
			Start:     start,
			End:       cutoff,
			Feed:      s.cfg.Feed,
		})
		if err != nil {
			return fmt.Errorf("getting pre-market bars for %s: %w", date, err)
		}

		local := make(map[string]premarketAgg, len(bars))
		for symbol, series := range bars {
			if agg := aggregatePremarket(series, cutoff); agg.bars > 0 && agg.last > 0 {
				local[symbol] = agg
			}
		}

		mu.Lock()
		defer mu.Unlock()
		for symbol, agg := range local {
			out[symbol] = agg
		}
		return nil
	})
	return out, err
}

// backfillPremarketCandidates measures the reconstructed pre-market gap for one
// date, mirroring what the 09:00 live scan would have flagged.
func (s *Scanner) backfillPremarketCandidates(
	date string, daily map[string][]marketdata.Bar, aggs map[string]premarketAgg,
) []GapCandidate {
	var out []GapCandidate

	for symbol, agg := range aggs {
		bars := daily[symbol]
		i := barIndexForDate(bars, date, s.loc)
		if i < 1 {
			continue
		}
		prev := bars[i-1]
		if prev.Close <= 0 || !s.passesUniverseFilters(&prev) {
			continue
		}

		gapPct := (agg.last - prev.Close) / prev.Close * 100
		if math.Abs(gapPct) < s.cfg.PrescreenGapPct {
			continue
		}

		start := max(0, i-s.cfg.HistoryDays)
		c := s.buildCandidate(rawGap{
			symbol:     symbol,
			prevClose:  prev.Close,
			prevDate:   sessionDateOf(prev.Timestamp, s.loc),
			prevVolume: prev.Volume,
			refPrice:   agg.last,
			refSource:  BackfillPremarketRefSource,
			gapPct:     gapPct,
		}, bars[start:i])
		if len(c.Methods) == 0 {
			continue
		}

		c.PremarketLast = agg.last
		c.PremarketHigh = agg.high
		c.PremarketLow = agg.low
		c.PremarketVolume = agg.volume
		c.PremarketBars = agg.bars
		// Same derived ratios attachPremarket computes once AvgVolume is known.
		if c.PrevClose > 0 && c.PremarketHigh > 0 {
			c.PremarketRangePct = (c.PremarketHigh - c.PremarketLow) / c.PrevClose * 100
		}
		if c.AvgVolume > 0 {
			c.PremarketVolumeRatio = float64(c.PremarketVolume) / float64(c.AvgVolume)
		}
		out = append(out, c)
	}
	return out
}

// barIndexForDate finds a symbol's bar for an ET calendar date, or -1.
func barIndexForDate(bars []marketdata.Bar, date string, loc *time.Location) int {
	for i, b := range bars {
		if sessionDateOf(b.Timestamp, loc) == date {
			return i
		}
	}
	return -1
}

// backfillScanAtET is the hour the reconstruction pretends to scan at. It must
// match the pre-market cron entry, or the rebuilt reference price reflects a
// different moment than the live one would have.
const backfillScanAtET = "09:00"

// backfillOpenCandidate measures one symbol's open-phase gap at one bar index.
//
// forced mirrors the live carry set: a symbol carried from the pre-market pass
// bypasses the gates and is kept even when it flags nothing, because its
// absence at the open is precisely the data point.
func (s *Scanner) backfillOpenCandidate(
	symbol string, bars []marketdata.Bar, i int, forced bool,
) (GapCandidate, bool) {
	if i < 1 || i >= len(bars) {
		return GapCandidate{}, false
	}
	prev, cur := bars[i-1], bars[i]
	if prev.Close <= 0 || cur.Open <= 0 {
		return GapCandidate{}, false
	}
	// Same price and liquidity gate the live prescreen applies, against the
	// same bar: the previous session.
	if !forced && !s.passesUniverseFilters(&prev) {
		return GapCandidate{}, false
	}

	gapPct := (cur.Open - prev.Close) / prev.Close * 100
	if !forced && math.Abs(gapPct) < s.cfg.PrescreenGapPct {
		return GapCandidate{}, false
	}

	// History strictly before this session, so the gap is never measured
	// against a baseline that already contains it.
	start := max(0, i-s.cfg.HistoryDays)
	c := s.buildCandidate(rawGap{
		symbol:     symbol,
		prevClose:  prev.Close,
		prevDate:   sessionDateOf(prev.Timestamp, s.loc),
		prevVolume: prev.Volume,
		refPrice:   cur.Open,
		refSource:  BackfillRefSource,
		gapPct:     gapPct,
		// No historical quote is pulled, so SpreadPct stays zero and the cost
		// model falls back to AssumedSpreadPct.
	}, bars[start:i])

	if !forced && len(c.Methods) == 0 {
		return GapCandidate{}, false
	}
	return c, true
}

// backfillOpenCandidates measures every retained symbol on one date.
func (s *Scanner) backfillOpenCandidates(
	date string, daily map[string][]marketdata.Bar, carry map[string]bool,
) []GapCandidate {
	var out []GapCandidate
	for symbol, bars := range daily {
		i := barIndexForDate(bars, date, s.loc)
		if i < 1 {
			continue
		}
		if c, ok := s.backfillOpenCandidate(symbol, bars, i, carry[symbol]); ok {
			out = append(out, c)
		}
	}
	return out
}

// keepForBackfill reports whether a symbol could matter in the window. It needs
// two bars and must clear the price and liquidity gate on at least one date, so
// the expensive minute-bar pass never runs over the whole tradable universe.
func (s *Scanner) keepForBackfill(bars []marketdata.Bar, from, to string) bool {
	for i := 1; i < len(bars); i++ {
		date := sessionDateOf(bars[i].Timestamp, s.loc)
		if date < from || date > to {
			continue
		}
		if s.passesUniverseFilters(&bars[i-1]) {
			return true
		}
	}
	return false
}

// backfillDailyBars pulls the daily history once and keeps only the series the
// later passes can use, so memory stays bounded by the gated universe rather
// than by the length of the window.
func (s *Scanner) backfillDailyBars(
	symbols []string, from, to string,
) (map[string][]marketdata.Bar, error) {
	start, err := time.ParseInLocation("2006-01-02", from, s.loc)
	if err != nil {
		return nil, fmt.Errorf("parsing from %q: %w", from, err)
	}
	end, err := time.ParseInLocation("2006-01-02", to, s.loc)
	if err != nil {
		return nil, fmt.Errorf("parsing to %q: %w", to, err)
	}
	// Enough calendar days to cover HistoryDays trading days of lookback.
	barsFrom := start.AddDate(0, 0, -(s.cfg.HistoryDays*2 + 10))
	barsTo := end.AddDate(0, 0, 1)
	if cutoff := time.Now().Add(-16 * time.Minute); barsTo.After(cutoff) {
		barsTo = cutoff
	}

	out := make(map[string][]marketdata.Bar)
	var mu sync.Mutex

	err = s.forEachBatch(symbols, s.cfg.HistoryBatchSize, func(batch []string) error {
		bars, err := s.data.GetMultiBars(batch, marketdata.GetBarsRequest{
			TimeFrame: marketdata.OneDay,
			// Without this a split inside the window reads as a huge gap.
			Adjustment: marketdata.AdjustmentAll,
			Start:      barsFrom,
			End:        barsTo,
			Feed:       s.cfg.Feed,
		})
		if err != nil {
			return fmt.Errorf("getting daily bars for backfill: %w", err)
		}

		local := make(map[string][]marketdata.Bar, len(bars))
		for symbol, series := range bars {
			if len(series) >= 2 && s.keepForBackfill(series, from, to) {
				local[symbol] = series
			}
		}

		mu.Lock()
		defer mu.Unlock()
		for symbol, series := range local {
			out[symbol] = series
		}
		return nil
	})
	return out, err
}

// backfillDates lists the trading days the pulled bars actually cover. Derived
// from the bars themselves, so half days and holidays need no calendar call.
func backfillDates(daily map[string][]marketdata.Bar, from, to string, loc *time.Location) []string {
	seen := make(map[string]bool)
	for _, bars := range daily {
		for _, b := range bars {
			if date := sessionDateOf(b.Timestamp, loc); date >= from && date <= to {
				seen[date] = true
			}
		}
	}

	dates := make([]string, 0, len(seen))
	for date := range seen {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	return dates
}

// backfillScanResult wraps reconstructed candidates in the same shape a live
// run writes, so every existing loader reads them without special-casing.
func (s *Scanner) backfillScanResult(date, phase string, candidates []GapCandidate) *ScanResult {
	sortCandidates(candidates)
	candidates = s.capCandidates(candidates)

	result := &ScanResult{
		GeneratedAt:       time.Now().Format(time.RFC3339),
		SessionDate:       date,
		SessionOpen:       date + "T09:30:00-04:00",
		Phase:             phase,
		Mode:              "reconstructed_daily",
		Feed:              s.cfg.Feed,
		Thresholds:        s.cfg.Thresholds,
		RequireAllMethods: s.cfg.RequireAllMethods,
		Prescreened:       len(candidates),
		HistoryFetched:    len(candidates),
		Candidates:        candidates,
	}
	for _, c := range candidates {
		if c.Faded {
			result.FadedCount++
		}
		if c.DiscoveredAt == PhaseOpen {
			result.NewAtOpen++
		}
		if c.SeenPremarket {
			result.PremarketCandidates++
		}
	}
	result.PremarketScanFound = result.PremarketCandidates > 0
	return result
}

// Backfill reconstructs past sessions so the scorers have history to work with
// without waiting weeks for live collection.
//
// One bulk pull of daily bars covers every date at once, so that pass costs the
// same whatever the window. With twoPhase the pre-market pass adds one
// minute-bar sweep per date, which is where the time actually goes.
func (s *Scanner) Backfill(from, to string, force, twoPhase bool) (*BackfillResult, error) {
	if from > to {
		return nil, fmt.Errorf("from %s is after to %s", from, to)
	}

	symbols, err := s.Universe()
	if err != nil {
		return nil, err
	}
	result := &BackfillResult{From: from, To: to, UniverseSize: len(symbols), TwoPhase: twoPhase}
	log.Printf("backfill: %d symbols, window %s to %s, two-phase=%t", len(symbols), from, to, twoPhase)

	daily, err := s.backfillDailyBars(symbols, from, to)
	if err != nil {
		return nil, err
	}
	result.SymbolsWithData = len(daily)

	kept := make([]string, 0, len(daily))
	for symbol := range daily {
		kept = append(kept, symbol)
	}
	sort.Strings(kept)

	dates := backfillDates(daily, from, to, s.loc)
	result.TradingDays = len(dates)
	log.Printf("backfill: %d symbols cleared the gates, %d trading days to rebuild",
		len(kept), len(dates))

	for _, date := range dates {
		var baseline *ScanResult

		if twoPhase {
			aggs, err := s.premarketForDate(kept, date, backfillScanAtET)
			if err != nil {
				return nil, err
			}
			premarket := s.backfillPremarketCandidates(date, daily, aggs)
			applyLineage(premarket, PhasePremarket, nil)
			baseline = s.backfillScanResult(date, PhaseBackfillPremarket, premarket)

			written, err := s.writeBackfillPhase(
				PhaseFile(s.cfg.DataDir, date, PhasePremarket), baseline, force)
			if err != nil {
				return nil, err
			}
			if written {
				result.PremarketCandidates += len(baseline.Candidates)
			}
		}

		carry := make(map[string]bool)
		if baseline != nil {
			for _, c := range baseline.Candidates {
				carry[c.Symbol] = true
			}
		}

		open := s.backfillOpenCandidates(date, daily, carry)
		applyLineage(open, PhaseOpen, baseline)
		scan := s.backfillScanResult(date, PhaseBackfill, open)

		written, err := s.writeBackfillPhase(
			PhaseFile(s.cfg.DataDir, date, PhaseOpen), scan, force)
		if err != nil {
			return nil, err
		}
		if !written {
			result.SessionsSkipped++
			continue
		}
		result.SessionsWritten++
		result.Candidates += len(scan.Candidates)
		result.Faded += scan.FadedCount
	}
	return result, nil
}

// writeBackfillPhase persists one reconstructed phase, refusing to clobber a
// scan that was observed live unless forced.
func (s *Scanner) writeBackfillPhase(path string, scan *ScanResult, force bool) (bool, error) {
	if !force {
		existing, err := LoadScanResult(path)
		if err != nil {
			return false, err
		}
		if existing != nil {
			// A live scan carries real quotes and a real snapshot that a
			// reconstruction cannot, so it always outranks one.
			log.Printf("backfill: %s already has a %s scan, leaving it alone",
				scan.SessionDate, existing.Phase)
			return false, nil
		}
	}
	return true, SaveScanResult(path, scan)
}

// resolveBackfillWindow turns the flags into a concrete date range. Explicit
// -from wins; otherwise -days, or the configured BackfillDays, counts back from
// the end of the window.
func resolveBackfillWindow(
	client *alpaca.Client, cfg ScannerConfig, from, to string, days int,
) (string, string, error) {
	loc, err := easternLocation()
	if err != nil {
		return "", "", err
	}

	if to == "" {
		session, err := ResolveLastCompletedSession(client, cfg.PremarketStartET)
		if err != nil {
			return "", "", err
		}
		to = session.Date
	}
	if from != "" {
		return from, to, nil
	}

	if days <= 0 {
		days = cfg.BackfillDays
	}
	end, err := time.ParseInLocation("2006-01-02", to, loc)
	if err != nil {
		return "", "", fmt.Errorf("parsing to %q: %w", to, err)
	}
	return end.AddDate(0, 0, -days).Format("2006-01-02"), to, nil
}

// runBackfill reconstructs past sessions from historical bars.
func runBackfill(
	client *alpaca.Client, cfg ScannerConfig, from, to string, days int, force, twoPhase bool,
) error {
	from, to, err := resolveBackfillWindow(client, cfg, from, to, days)
	if err != nil {
		return err
	}
	log.Printf("backfill: window resolved to %s .. %s", from, to)

	scanner, err := NewScanner(client, cfg)
	if err != nil {
		return err
	}
	defer scanner.Close()

	result, err := scanner.Backfill(from, to, force, twoPhase)
	if err != nil {
		return err
	}

	log.Printf("backfill: wrote %d sessions (%d candidates, %d faded), skipped %d existing",
		result.SessionsWritten, result.Candidates, result.Faded, result.SessionsSkipped)
	PrintBackfill(result)
	return nil
}

// PrintBackfill writes the run summary to stdout.
func PrintBackfill(r *BackfillResult) {
	fmt.Printf("\nBackfill  %s to %s  (%d trading days)\n", r.From, r.To, r.TradingDays)
	fmt.Printf("universe=%d  cleared the gates=%d\n", r.UniverseSize, r.SymbolsWithData)
	fmt.Printf("sessions written=%d  skipped (already scanned)=%d  candidates=%d\n",
		r.SessionsWritten, r.SessionsSkipped, r.Candidates)

	if r.TwoPhase {
		fmt.Printf("pre-market rows=%d  faded=%d\n", r.PremarketCandidates, r.Faded)
	}
	if r.SessionsWritten == 0 {
		fmt.Println("\nnothing written — either the window has no trading days, or every " +
			"session already has a scan (use -force to overwrite)")
		return
	}

	fmt.Println("\nHow these differ from live rows, all recorded on the row itself:")
	fmt.Printf("  scan_phase=%q\n", PhaseBackfill)
	if !r.TwoPhase {
		fmt.Printf("  discovered_at=%q, no faded rows, no gap_delta_pct — add -premarket for those\n",
			DiscoveredUnknown)
		fmt.Println("  premarket_* are zero: daily bars carry no pre-market activity")
	}
	fmt.Println("  spread_pct is zero: no historical quote, so costs use assumed_spread_pct")
	fmt.Printf("\nThe universe is today's tradable assets. Measured against Alpaca's inactive\n" +
		"list, that omits roughly 0.2%% of symbols per quarter — negligible here, but it\n" +
		"grows if you extend the window to years.\n")
	fmt.Printf("\nNext: ./stocko2 -swing    then ./stocko2 -swing-sweep\n\n")
}
