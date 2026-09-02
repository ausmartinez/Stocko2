package main

import (
	"math"
	"testing"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
)

func day(n int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
}

func TestAverageTrueRange(t *testing.T) {
	bars := []marketdata.Bar{
		{Timestamp: day(0), Open: 10, High: 11, Low: 9, Close: 10},
		// range 2, |12-10|=2, |10-10|=0 -> TR 2
		{Timestamp: day(1), Open: 10, High: 12, Low: 10, Close: 11},
		// range 1, |20-11|=9, |19-11|=8 -> TR 9 (the gap dominates)
		{Timestamp: day(2), Open: 19, High: 20, Low: 19, Close: 20},
	}

	atr, samples := AverageTrueRange(bars, 14)
	if samples != 2 {
		t.Fatalf("samples = %d, want 2", samples)
	}
	if want := 5.5; math.Abs(atr-want) > 1e-9 {
		t.Errorf("atr = %v, want %v", atr, want)
	}
}

func TestAverageTrueRangeRespectsPeriod(t *testing.T) {
	bars := []marketdata.Bar{
		{Timestamp: day(0), High: 11, Low: 9, Close: 10},
		{Timestamp: day(1), High: 40, Low: 10, Close: 20}, // TR 30, must fall out
		{Timestamp: day(2), High: 21, Low: 20, Close: 21}, // TR 1
	}

	atr, samples := AverageTrueRange(bars, 1)
	if samples != 1 {
		t.Fatalf("samples = %d, want 1", samples)
	}
	if want := 1.0; math.Abs(atr-want) > 1e-9 {
		t.Errorf("atr = %v, want %v", atr, want)
	}
}

func TestAverageTrueRangeInsufficientData(t *testing.T) {
	if atr, samples := AverageTrueRange(nil, 14); atr != 0 || samples != 0 {
		t.Errorf("got (%v, %d), want (0, 0)", atr, samples)
	}
	one := []marketdata.Bar{{High: 11, Low: 9, Close: 10}}
	if atr, samples := AverageTrueRange(one, 14); atr != 0 || samples != 0 {
		t.Errorf("got (%v, %d), want (0, 0)", atr, samples)
	}
}

func TestOvernightStats(t *testing.T) {
	// Overnight returns: +10%, -10%, +10% -> mean 10/3 %, sample stdev ~11.547%
	bars := []marketdata.Bar{
		{Timestamp: day(0), Open: 100, Close: 100},
		{Timestamp: day(1), Open: 110, Close: 100},
		{Timestamp: day(2), Open: 90, Close: 100},
		{Timestamp: day(3), Open: 110, Close: 100},
	}

	mean, stdev, samples := OvernightStats(bars)
	if samples != 3 {
		t.Fatalf("samples = %d, want 3", samples)
	}
	if want := 0.1 / 3; math.Abs(mean-want) > 1e-9 {
		t.Errorf("mean = %v, want %v", mean, want)
	}
	if want := 0.11547005; math.Abs(stdev-want) > 1e-6 {
		t.Errorf("stdev = %v, want %v", stdev, want)
	}

	// A 25% gap should sit a long way out on that distribution.
	sigma := (0.25 - mean) / stdev
	if sigma < 1.8 || sigma > 1.9 {
		t.Errorf("sigma = %v, want ~1.88", sigma)
	}
}

func TestOvernightStatsSkipsBadBars(t *testing.T) {
	bars := []marketdata.Bar{
		{Timestamp: day(0), Open: 100, Close: 0}, // unusable prev close
		{Timestamp: day(1), Open: 110, Close: 100},
		{Timestamp: day(2), Open: 110, Close: 100},
		{Timestamp: day(3), Open: 110, Close: 100},
	}

	_, _, samples := OvernightStats(bars)
	if samples != 2 {
		t.Fatalf("samples = %d, want 2", samples)
	}
}

func TestResolveDailyBars(t *testing.T) {
	loc, err := easternLocation()
	if err != nil {
		t.Fatal(err)
	}
	et := func(y, m, d int) time.Time {
		return time.Date(y, time.Month(m), d, 0, 0, 0, 0, loc)
	}

	// Symbol with pre-market prints: DailyBar has already rolled to today.
	rolled := &marketdata.Snapshot{
		DailyBar:     &marketdata.Bar{Timestamp: et(2026, 3, 10), Close: 55},
		PrevDailyBar: &marketdata.Bar{Timestamp: et(2026, 3, 9), Close: 50},
	}
	prev, today := resolveDailyBars(rolled, "2026-03-10", loc)
	if prev == nil || prev.Close != 50 {
		t.Errorf("prev = %+v, want close 50", prev)
	}
	if today == nil || today.Close != 55 {
		t.Errorf("today = %+v, want close 55", today)
	}

	// Quiet symbol: DailyBar is still the last completed session.
	quiet := &marketdata.Snapshot{
		DailyBar:     &marketdata.Bar{Timestamp: et(2026, 3, 9), Close: 50},
		PrevDailyBar: &marketdata.Bar{Timestamp: et(2026, 3, 6), Close: 48},
	}
	prev, today = resolveDailyBars(quiet, "2026-03-10", loc)
	if prev == nil || prev.Close != 50 {
		t.Errorf("prev = %+v, want close 50 (not the day before)", prev)
	}
	if today != nil {
		t.Errorf("today = %+v, want nil", today)
	}
}

func TestReferencePrice(t *testing.T) {
	loc, err := easternLocation()
	if err != nil {
		t.Fatal(err)
	}
	premarket := time.Date(2026, 3, 10, 7, 15, 0, 0, loc)
	session := TradingSession{
		Date:       "2026-03-10",
		Open:       time.Date(2026, 3, 10, 9, 30, 0, 0, loc),
		BeforeOpen: true,
	}

	snap := &marketdata.Snapshot{
		LatestTrade: &marketdata.Trade{Timestamp: premarket, Price: 55},
	}
	price, source, ok := referencePrice(snap, nil, session, loc)
	if !ok || price != 55 || source != "premarket_last_trade" {
		t.Errorf("got (%v, %q, %v), want (55, premarket_last_trade, true)", price, source, ok)
	}

	// A stale trade from a previous session must not be treated as pre-market.
	stale := &marketdata.Snapshot{
		LatestTrade: &marketdata.Trade{Timestamp: time.Date(2026, 3, 9, 15, 59, 0, 0, loc), Price: 50},
	}
	if _, _, ok := referencePrice(stale, nil, session, loc); ok {
		t.Error("stale trade accepted as a pre-market reference price")
	}

	// After the bell the session open is what counts.
	session.BeforeOpen = false
	todayBar := &marketdata.Bar{Timestamp: time.Date(2026, 3, 10, 0, 0, 0, 0, loc), Open: 57, Close: 56}
	price, source, ok = referencePrice(stale, todayBar, session, loc)
	if !ok || price != 57 || source != "session_open_daily_bar" {
		t.Errorf("got (%v, %q, %v), want (57, session_open_daily_bar, true)", price, source, ok)
	}

	// A symbol that has not opened yet has no bar for the session and only a
	// stale trade. Reporting that as a 0% gap would be worse than reporting
	// nothing, so it must come back not-ok.
	if p, src, ok := referencePrice(stale, nil, session, loc); ok {
		t.Errorf("got (%v, %q, true) for an unopened symbol, want not-ok", p, src)
	}

	// A trade from this session is a valid fallback when the bar is missing.
	fresh := &marketdata.Snapshot{
		LatestTrade: &marketdata.Trade{Timestamp: time.Date(2026, 3, 10, 9, 31, 0, 0, loc), Price: 58},
	}
	price, source, ok = referencePrice(fresh, nil, session, loc)
	if !ok || price != 58 || source != "latest_trade" {
		t.Errorf("got (%v, %q, %v), want (58, latest_trade, true)", price, source, ok)
	}
}

func TestBuildCandidateFlagsEachMethod(t *testing.T) {
	loc, err := easternLocation()
	if err != nil {
		t.Fatal(err)
	}

	// A calm symbol: ~1% daily range, tiny overnight moves.
	history := make([]marketdata.Bar, 0, 60)
	for i := 0; i < 60; i++ {
		open := 100.0
		if i%2 == 0 {
			open = 100.2
		}
		history = append(history, marketdata.Bar{
			Timestamp: day(i), Open: open, High: 100.5, Low: 99.5, Close: 100,
		})
	}

	s := &Scanner{
		loc: loc,
		cfg: ScannerConfig{
			ATRPeriod:       14,
			MinSigmaSamples: 30,
			Thresholds:      GapThresholds{GapPct: 4, ATRMult: 1, Sigma: 2},
		},
	}

	// 8% gap on a symbol with a ~1 point ATR: all three should fire.
	c := s.buildCandidate(rawGap{
		symbol: "CALM", prevClose: 100, refPrice: 108, gapPct: 8,
	}, history)

	if len(c.Methods) != 3 {
		t.Fatalf("methods = %v, want all three", c.Methods)
	}
	if c.Direction != "up" {
		t.Errorf("direction = %q, want up", c.Direction)
	}
	if c.GapATR < 5 {
		t.Errorf("gapATR = %v, want a large multiple of a ~1 point ATR", c.GapATR)
	}

	// A 2% gap clears neither the percent nor the sigma bar here.
	small := s.buildCandidate(rawGap{
		symbol: "CALM", prevClose: 100, refPrice: 102, gapPct: 2,
	}, history)
	for _, m := range small.Methods {
		if m == MethodPercent {
			t.Error("2% gap should not trip a 4% percent threshold")
		}
	}

	// Same 2% gap must still trip the ATR method, since 2 points is twice
	// this symbol's normal daily range.
	if small.GapATR < 1 {
		t.Errorf("gapATR = %v, want >= 1", small.GapATR)
	}

	// require_all_methods should suppress a partial match.
	s.cfg.RequireAllMethods = true
	if got := s.buildCandidate(rawGap{
		symbol: "CALM", prevClose: 100, refPrice: 102, gapPct: 2,
	}, history); len(got.Methods) != 0 {
		t.Errorf("methods = %v, want none under require_all_methods", got.Methods)
	}
}

func TestBuildCandidateWithoutHistory(t *testing.T) {
	s := &Scanner{
		cfg: ScannerConfig{
			ATRPeriod:       14,
			MinSigmaSamples: 30,
			Thresholds:      GapThresholds{GapPct: 4, ATRMult: 1, Sigma: 2},
		},
	}

	// No history means only the percentage method can speak.
	c := s.buildCandidate(rawGap{symbol: "NEW", prevClose: 10, refPrice: 12, gapPct: 20}, nil)
	if len(c.Methods) != 1 || c.Methods[0] != MethodPercent {
		t.Errorf("methods = %v, want [percent] only", c.Methods)
	}
	if c.GapATR != 0 || c.GapSigma != 0 {
		t.Errorf("gapATR = %v, gapSigma = %v, want zero without history", c.GapATR, c.GapSigma)
	}
}

// TestBuildCandidateIsDirectionSymmetric pins down that a down gap is measured
// and flagged exactly as readily as the equivalent up gap.
func TestBuildCandidateIsDirectionSymmetric(t *testing.T) {
	// Overnight returns alternate +1% / -1%. 61 bars yield 60 returns, an even
	// split, so the mean is exactly zero and the sigma measurement is
	// symmetric about the previous close.
	history := make([]marketdata.Bar, 0, 61)
	for i := 0; i < 61; i++ {
		open := 101.0
		if i%2 == 0 {
			open = 99.0
		}
		history = append(history, marketdata.Bar{
			Timestamp: day(i), Open: open, High: 101.5, Low: 98.5, Close: 100,
		})
	}

	s := &Scanner{
		cfg: ScannerConfig{
			ATRPeriod:       14,
			MinSigmaSamples: 30,
			Thresholds:      GapThresholds{GapPct: 4, ATRMult: 1, Sigma: 2},
		},
	}

	up := s.buildCandidate(rawGap{symbol: "SYM", prevClose: 100, refPrice: 106, gapPct: 6}, history)
	down := s.buildCandidate(rawGap{symbol: "SYM", prevClose: 100, refPrice: 94, gapPct: -6}, history)

	if up.Direction != "up" || down.Direction != "down" {
		t.Errorf("directions = %q / %q, want up / down", up.Direction, down.Direction)
	}
	if len(up.Methods) != len(down.Methods) || len(up.Methods) == 0 {
		t.Fatalf("methods differ by direction: up=%v down=%v", up.Methods, down.Methods)
	}
	for i := range up.Methods {
		if up.Methods[i] != down.Methods[i] {
			t.Errorf("methods differ by direction: up=%v down=%v", up.Methods, down.Methods)
		}
	}

	// The ATR measure is a magnitude, so it must be identical either way.
	if math.Abs(up.GapATR-down.GapATR) > 1e-9 {
		t.Errorf("gapATR up=%v down=%v, want equal", up.GapATR, down.GapATR)
	}
	// Sigma keeps its sign but must have equal magnitude on a zero-mean
	// distribution.
	if math.Abs(up.GapSigma+down.GapSigma) > 1e-9 {
		t.Errorf("gapSigma up=%v down=%v, want equal and opposite", up.GapSigma, down.GapSigma)
	}
	if down.GapSigma >= 0 {
		t.Errorf("down gap sigma = %v, want negative", down.GapSigma)
	}
}

// TestPassesUniverseFiltersIgnoresGapDirection guards the price band against
// being re-pointed at the post-gap price, which would quietly drop down gaps
// that fall through min_price.
func TestPassesUniverseFiltersIgnoresGapDirection(t *testing.T) {
	s := &Scanner{cfg: ScannerConfig{MinPrice: 1, MaxPrice: 1000, MinPrevVolume: 100}}

	// Previous close of $1.05: in band, even though a 20% down gap would put
	// the reference price at $0.84, below min_price.
	if !s.passesUniverseFilters(&marketdata.Bar{Close: 1.05, Volume: 500}) {
		t.Error("a $1.05 stock was rejected; the band must read the previous close")
	}
	// Previous close genuinely below the band is still rejected.
	if s.passesUniverseFilters(&marketdata.Bar{Close: 0.80, Volume: 500}) {
		t.Error("a $0.80 stock passed the $1.00 floor")
	}
	if s.passesUniverseFilters(&marketdata.Bar{Close: 1200, Volume: 500}) {
		t.Error("a $1200 stock passed the $1000 ceiling")
	}
	if s.passesUniverseFilters(&marketdata.Bar{Close: 50, Volume: 99}) {
		t.Error("thin volume passed the liquidity gate")
	}
}

func TestChunk(t *testing.T) {
	got := chunk([]string{"A", "B", "C", "D", "E"}, 2)
	if len(got) != 3 || len(got[0]) != 2 || len(got[2]) != 1 {
		t.Errorf("chunk = %v, want 3 batches of 2,2,1", got)
	}
	if len(chunk(nil, 10)) != 0 {
		t.Error("chunking nil should produce no batches")
	}
}
