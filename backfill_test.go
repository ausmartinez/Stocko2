package main

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
)

// backfillConfig loosens the gates so a test controls exactly which bars
// qualify, and keeps ATR/sigma measurable from a short history.
func backfillConfig(t *testing.T, dataDir string) ScannerConfig {
	t.Helper()

	cfg := DefaultScannerConfig()
	cfg.DataDir = dataDir
	cfg.MinPrice = 1
	cfg.MaxPrice = 10000
	cfg.MinPrevVolume = 0
	cfg.PrescreenGapPct = 2.0
	cfg.HistoryDays = 120
	cfg.MinSigmaSamples = 3
	cfg.Thresholds = GapThresholds{GapPct: 4.0, ATRMult: 1.0, Sigma: 2.0}
	return cfg
}

// flatSeries builds n quiet weekday bars at `price`, so ATR and overnight sigma
// stay small and a later gap stands out against them.
func flatSeries(t *testing.T, n int, price float64) []marketdata.Bar {
	t.Helper()

	loc, err := easternLocation()
	if err != nil {
		t.Fatal(err)
	}

	bars := make([]marketdata.Bar, 0, n)
	for day := time.Date(2026, 6, 1, 0, 0, 0, 0, loc); len(bars) < n; day = day.AddDate(0, 0, 1) {
		if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		bars = append(bars, marketdata.Bar{
			Timestamp: day,
			Open:      price,
			High:      price * 1.001,
			Low:       price * 0.999,
			Close:     price,
			Volume:    1_000_000,
		})
	}
	return bars
}

// gapAt turns the last bar of a flat series into a gapper and returns its date
// and index.
func gapAt(t *testing.T, s *Scanner, bars []marketdata.Bar, open, high, low, close float64) (string, int) {
	t.Helper()

	i := len(bars) - 1
	bars[i].Open, bars[i].High, bars[i].Low, bars[i].Close = open, high, low, close
	return sessionDateOf(bars[i].Timestamp, s.loc), i
}

func TestBackfillOpenCandidateMeasuresTheGapAgainstTheOpen(t *testing.T) {
	s := testScanner(t, backfillConfig(t, t.TempDir()))

	bars := flatSeries(t, 30, 100)
	_, i := gapAt(t, s, bars, 110, 112, 109, 111)

	c, ok := s.backfillOpenCandidate("AAA", bars, i, false)
	if !ok {
		t.Fatal("candidate rejected")
	}

	if math.Abs(c.GapPct-10) > 1e-9 {
		t.Errorf("gap = %v%%, want 10%% measured to the open", c.GapPct)
	}
	if math.Abs(c.RefPrice-110) > 1e-9 {
		t.Errorf("ref_price = %v, want the session open of 110", c.RefPrice)
	}
	if c.RefSource != BackfillRefSource {
		t.Errorf("ref_source = %q, want %q", c.RefSource, BackfillRefSource)
	}
	if math.Abs(c.PrevClose-100) > 1e-9 {
		t.Errorf("prev_close = %v, want 100", c.PrevClose)
	}
	// No historical quote is fetched, so the cost model must fall back.
	if c.SpreadPct != 0 || c.Bid != 0 || c.Ask != 0 {
		t.Errorf("quote fields should be zero: bid=%v ask=%v spread=%v", c.Bid, c.Ask, c.SpreadPct)
	}
	// The three measurements still come from the daily history.
	if c.ATR <= 0 || c.GapATR <= 0 {
		t.Errorf("ATR not measured: atr=%v gap_atr=%v", c.ATR, c.GapATR)
	}
}

func TestBackfillOpenCandidateSkipsBelowThePrescreen(t *testing.T) {
	s := testScanner(t, backfillConfig(t, t.TempDir()))

	bars := flatSeries(t, 30, 100)
	if _, ok := s.backfillOpenCandidate("AAA", bars, len(bars)-1, false); ok {
		t.Error("a flat series produced a candidate")
	}
}

func TestBackfillOpenCandidateAppliesUniverseFilters(t *testing.T) {
	cfg := backfillConfig(t, t.TempDir())
	cfg.MinPrevVolume = 10_000_000 // above the 1M the helper sets
	s := testScanner(t, cfg)

	bars := flatSeries(t, 30, 100)
	_, i := gapAt(t, s, bars, 110, 112, 109, 111)

	if _, ok := s.backfillOpenCandidate("AAA", bars, i, false); ok {
		t.Error("the volume gate should have rejected it")
	}
}

func TestBackfillOpenCandidateNeedsAPreviousBar(t *testing.T) {
	s := testScanner(t, backfillConfig(t, t.TempDir()))

	if _, ok := s.backfillOpenCandidate("AAA", flatSeries(t, 1, 100), 0, false); ok {
		t.Error("index 0 has no previous close to gap from")
	}
}

// The carry set is what makes `faded` possible: a symbol that gapped pre-market
// must be measured at the open even when it now clears nothing, because its
// absence is the data point.
func TestBackfillOpenCandidateForcedBypassesEveryGate(t *testing.T) {
	cfg := backfillConfig(t, t.TempDir())
	cfg.MinPrevVolume = 10_000_000 // would normally reject
	s := testScanner(t, cfg)

	bars := flatSeries(t, 30, 100)
	// Opens dead flat, so it clears neither the prescreen nor any threshold.
	_, i := gapAt(t, s, bars, 100, 100.1, 99.9, 100)

	if _, ok := s.backfillOpenCandidate("AAA", bars, i, false); ok {
		t.Fatal("unforced, a flat low-volume bar should be rejected")
	}

	c, ok := s.backfillOpenCandidate("AAA", bars, i, true)
	if !ok {
		t.Fatal("forced candidate was dropped; faded rows would be lost")
	}
	if len(c.Methods) != 0 {
		t.Errorf("methods = %v, want empty so applyLineage marks it faded", c.Methods)
	}
}

func TestAggregatePremarketFoldsMinuteBars(t *testing.T) {
	loc, err := easternLocation()
	if err != nil {
		t.Fatal(err)
	}
	at := func(hhmm string) time.Time {
		ts, err := parseETTime(loc, "2026-09-11", hhmm)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}

	bars := []marketdata.Bar{
		{Timestamp: at("07:00"), High: 101, Low: 99, Close: 100, Volume: 500},
		{Timestamp: at("08:00"), High: 106, Low: 104, Close: 105, Volume: 1500},
		{Timestamp: at("08:59"), High: 104, Low: 102, Close: 103, Volume: 1000},
		// At and past the cutoff: must not be counted.
		{Timestamp: at("09:00"), High: 200, Low: 1, Close: 150, Volume: 9999},
		{Timestamp: at("09:30"), High: 300, Low: 1, Close: 250, Volume: 9999},
	}

	agg := aggregatePremarket(bars, at("09:00"))

	if agg.bars != 3 {
		t.Errorf("bars = %d, want 3 before the cutoff", agg.bars)
	}
	if agg.last != 103 {
		t.Errorf("last = %v, want the 08:59 close of 103", agg.last)
	}
	if agg.high != 106 || agg.low != 99 {
		t.Errorf("range = %v..%v, want 99..106", agg.low, agg.high)
	}
	if agg.volume != 3000 {
		t.Errorf("volume = %d, want 3000", agg.volume)
	}
}

func TestAggregatePremarketWithNoBars(t *testing.T) {
	loc, _ := easternLocation()
	cutoff, _ := parseETTime(loc, "2026-09-11", "09:00")

	if agg := aggregatePremarket(nil, cutoff); agg.bars != 0 || agg.last != 0 {
		t.Errorf("empty input gave %+v", agg)
	}
}

func TestKeepForBackfillNeedsAGatePassInWindow(t *testing.T) {
	cfg := backfillConfig(t, t.TempDir())
	cfg.MinPrevVolume = 500_000
	s := testScanner(t, cfg)

	bars := flatSeries(t, 10, 100)
	from := sessionDateOf(bars[1].Timestamp, s.loc)
	to := sessionDateOf(bars[9].Timestamp, s.loc)

	if !s.keepForBackfill(bars, from, to) {
		t.Error("1M volume should clear a 500k gate")
	}

	// Same series, but too thin to matter.
	for i := range bars {
		bars[i].Volume = 1000
	}
	if s.keepForBackfill(bars, from, to) {
		t.Error("a 1k-volume symbol should not be retained for the minute-bar pass")
	}
}

func TestBackfillDatesAreSortedAndWindowed(t *testing.T) {
	s := testScanner(t, backfillConfig(t, t.TempDir()))

	bars := flatSeries(t, 10, 100)
	daily := map[string][]marketdata.Bar{"AAA": bars, "BBB": bars}

	from := sessionDateOf(bars[2].Timestamp, s.loc)
	to := sessionDateOf(bars[5].Timestamp, s.loc)
	dates := backfillDates(daily, from, to, s.loc)

	if len(dates) != 4 {
		t.Fatalf("dates = %v, want the 4 in the window", dates)
	}
	for i := 1; i < len(dates); i++ {
		if dates[i] <= dates[i-1] {
			t.Errorf("dates not sorted or deduplicated: %v", dates)
			break
		}
	}
	if dates[0] != from || dates[len(dates)-1] != to {
		t.Errorf("window ends wrong: %v", dates)
	}
}

func TestWriteBackfillPhaseRefusesToClobberALiveScan(t *testing.T) {
	dir := t.TempDir()
	s := testScanner(t, backfillConfig(t, dir))
	path := PhaseFile(dir, "2026-06-15", PhaseOpen)

	live := &ScanResult{
		SessionDate: "2026-06-15", Phase: PhaseOpen,
		Candidates: []GapCandidate{{Symbol: "LIVE", Methods: []string{MethodPercent}}},
	}
	if err := SaveScanResult(path, live); err != nil {
		t.Fatal(err)
	}

	recon := s.backfillScanResult("2026-06-15",
		PhaseBackfill, []GapCandidate{{Symbol: "RECON", Methods: []string{MethodPercent}}})

	written, err := s.writeBackfillPhase(path, recon, false)
	if err != nil {
		t.Fatal(err)
	}
	if written {
		t.Error("backfill overwrote a live scan without -force")
	}
	after, err := LoadScanResult(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Candidates[0].Symbol != "LIVE" {
		t.Errorf("live scan replaced by %q", after.Candidates[0].Symbol)
	}

	// -force is the escape hatch.
	if written, err = s.writeBackfillPhase(path, recon, true); err != nil || !written {
		t.Fatalf("-force did not overwrite (written=%t err=%v)", written, err)
	}
	if after, _ = LoadScanResult(path); after.Candidates[0].Symbol != "RECON" {
		t.Errorf("forced write left %q", after.Candidates[0].Symbol)
	}
}

func TestBackfillScanResultMarksThePhaseAndSorts(t *testing.T) {
	s := testScanner(t, backfillConfig(t, t.TempDir()))

	got := s.backfillScanResult("2026-06-15", PhaseBackfill, []GapCandidate{
		{Symbol: "AAA", GapPct: 5, Methods: []string{MethodPercent}},
		{Symbol: "BBB", GapPct: 9, Methods: []string{MethodPercent, MethodATR}},
	})

	if got.Phase != PhaseBackfill {
		t.Errorf("phase = %q, want %q so downstream rows carry the provenance", got.Phase, PhaseBackfill)
	}
	// sortCandidates ranks by method count first.
	if got.Candidates[0].Symbol != "BBB" {
		t.Errorf("not sorted: first is %q, want BBB", got.Candidates[0].Symbol)
	}
}

func TestBackfillScanResultCapsLikeTheLiveScan(t *testing.T) {
	cfg := backfillConfig(t, t.TempDir())
	cfg.MaxCandidates = 3
	s := testScanner(t, cfg)

	var many []GapCandidate
	for i := 0; i < 10; i++ {
		many = append(many, GapCandidate{
			Symbol: fmt.Sprintf("S%02d", i), GapPct: float64(10 + i),
			Methods: []string{MethodPercent},
		})
	}

	// Backfilled days must not carry more candidates than live ones would.
	if got := s.backfillScanResult("2026-06-15", PhaseBackfill, many); len(got.Candidates) != 3 {
		t.Errorf("kept %d candidates, want the MaxCandidates cap of 3", len(got.Candidates))
	}
}

func TestBackfillScanResultCountsLineage(t *testing.T) {
	s := testScanner(t, backfillConfig(t, t.TempDir()))

	got := s.backfillScanResult("2026-06-15", PhaseBackfill, []GapCandidate{
		{Symbol: "CARRY", SeenPremarket: true, DiscoveredAt: PhasePremarket, Methods: []string{MethodPercent}},
		{Symbol: "FADED", SeenPremarket: true, DiscoveredAt: PhasePremarket, Faded: true, Methods: []string{}},
		{Symbol: "NEW", DiscoveredAt: PhaseOpen, Methods: []string{MethodPercent}},
	})

	if got.FadedCount != 1 {
		t.Errorf("faded = %d, want 1", got.FadedCount)
	}
	if got.NewAtOpen != 1 {
		t.Errorf("new_at_open = %d, want 1", got.NewAtOpen)
	}
	if got.PremarketCandidates != 2 || !got.PremarketScanFound {
		t.Errorf("premarket lineage = %d found=%t, want 2 and true",
			got.PremarketCandidates, got.PremarketScanFound)
	}
}
