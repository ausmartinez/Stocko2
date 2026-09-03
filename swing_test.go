package main

import (
	"math"
	"testing"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
)

// swingTestConfig sizes a $100 stock with an ATR of 4, costs and the cost floor
// switched off, so target and stop arithmetic is exact.
func swingTestConfig() ScannerConfig {
	cfg := DefaultScannerConfig()
	cfg.ExitReference = ExitRefEntry
	cfg.MinTargetCostMult = 0
	cfg.Costs = CostModel{Disabled: true}
	cfg.SwingTargetATR = 2.0 // 100 + 2*4 = 108
	cfg.SwingStopATR = 1.0   // 100 - 1*4 = 96
	cfg.SwingMaxHoldDays = 10
	cfg.SwingHorizonDays = []int{1, 2, 3, 5, 10}
	return cfg
}

// swingBars builds a forward path from (high, low, close) triples, one per day.
func swingBars(triples ...[3]float64) []SwingBar {
	out := make([]SwingBar, 0, len(triples))
	for i, t := range triples {
		out = append(out, SwingBar{
			Date:  time.Date(2026, 9, 4+i, 0, 0, 0, 0, time.UTC).Format("2006-01-02"),
			Day:   i + 1,
			Open:  t[2],
			High:  t[0],
			Low:   t[1],
			Close: t[2],
		})
	}
	return out
}

// dailyBar builds an Alpaca daily bar on an ET calendar date.
func dailyBar(t *testing.T, date string, open, high, low, close float64) marketdata.Bar {
	t.Helper()

	loc, err := easternLocation()
	if err != nil {
		t.Fatal(err)
	}
	ts, err := time.ParseInLocation("2006-01-02 15:04", date+" 00:00", loc)
	if err != nil {
		t.Fatal(err)
	}
	return marketdata.Bar{Timestamp: ts, Open: open, High: high, Low: low, Close: close}
}

func swingCandidate() GapCandidate {
	return GapCandidate{Symbol: "AAA", Direction: "up", PrevClose: 100, ATR: 4}
}

func TestLooksLikeEarnings(t *testing.T) {
	yes := []string{
		"Acme Reports Q3 Earnings Beat",
		"Widget Co Q1 revenue tops estimates",
		"Foo Inc raises full-year guidance",
		"Bar Corp EPS of $1.20 vs $1.05 expected",
		"Baz quarterly results disappoint",
	}
	for _, h := range yes {
		if !looksLikeEarnings(h) {
			t.Errorf("%q should look like earnings", h)
		}
	}

	no := []string{
		"Acme announces CEO transition",
		"FDA grants approval to Widget therapy",
		"Foo Inc to acquire Bar for $2B",
		"Analyst upgrades Baz to Buy",
	}
	for _, h := range no {
		if looksLikeEarnings(h) {
			t.Errorf("%q should not look like earnings", h)
		}
	}
}

func TestSwingBracketSizesOffATR(t *testing.T) {
	cfg := swingTestConfig()

	target, targetPct, basis, stop, stopPct := cfg.swingBracket(swingCandidate(), 100)

	if math.Abs(target-108) > 1e-9 {
		t.Errorf("target = %v, want 108 (entry + 2 x ATR)", target)
	}
	if math.Abs(targetPct-8) > 1e-9 {
		t.Errorf("target pct = %v, want 8", targetPct)
	}
	if basis != TargetBasisATR {
		t.Errorf("basis = %q, want %q", basis, TargetBasisATR)
	}
	if math.Abs(stop-96) > 1e-9 {
		t.Errorf("stop = %v, want 96 (entry - 1 x ATR)", stop)
	}
	if math.Abs(stopPct-(-4)) > 1e-9 {
		t.Errorf("stop pct = %v, want -4", stopPct)
	}
}

func TestSimulateSwingBracketHitsTarget(t *testing.T) {
	cfg := swingTestConfig()
	o := SwingOutcome{EntryPrice: 100, ReturnPct: map[string]float64{}}

	// Day 3 trades through 108.
	cfg.simulateSwingBracket(&o, swingCandidate(), swingBars(
		[3]float64{102, 99, 101},
		[3]float64{105, 100, 104},
		[3]float64{109, 103, 107},
	))

	if o.ExitReason != ExitReasonTarget {
		t.Fatalf("exit = %q, want target", o.ExitReason)
	}
	if o.ExitDay != 3 {
		t.Errorf("exit day = %d, want 3", o.ExitDay)
	}
	if math.Abs(o.StrategyReturnPct-8) > 1e-9 {
		t.Errorf("return = %v%%, want 8%%", o.StrategyReturnPct)
	}
	if o.Ambiguous {
		t.Error("exit should not be ambiguous")
	}
}

func TestSimulateSwingBracketHitsStop(t *testing.T) {
	cfg := swingTestConfig()
	o := SwingOutcome{EntryPrice: 100, ReturnPct: map[string]float64{}}

	// Day 2 breaks 96 without ever reaching 108.
	cfg.simulateSwingBracket(&o, swingCandidate(), swingBars(
		[3]float64{101, 98, 99},
		[3]float64{100, 95, 96},
		[3]float64{110, 96, 109},
	))

	if o.ExitReason != SwingExitStop {
		t.Fatalf("exit = %q, want stop", o.ExitReason)
	}
	if o.ExitDay != 2 {
		t.Errorf("exit day = %d, want 2", o.ExitDay)
	}
	if math.Abs(o.StrategyReturnPct-(-4)) > 1e-9 {
		t.Errorf("return = %v%%, want -4%%", o.StrategyReturnPct)
	}
}

func TestSimulateSwingBracketTimeStop(t *testing.T) {
	cfg := swingTestConfig()
	cfg.SwingMaxHoldDays = 3
	o := SwingOutcome{EntryPrice: 100, ReturnPct: map[string]float64{}}

	// Never reaches 108 or 96, so it is closed at day 3's close.
	cfg.simulateSwingBracket(&o, swingCandidate(), swingBars(
		[3]float64{101, 99, 100},
		[3]float64{103, 98, 102},
		[3]float64{104, 100, 103},
		[3]float64{120, 80, 110},
	))

	if o.ExitReason != SwingExitTime {
		t.Fatalf("exit = %q, want time_stop", o.ExitReason)
	}
	if o.ExitDay != 3 {
		t.Errorf("exit day = %d, want 3", o.ExitDay)
	}
	// Day 4's wild range must not be reached.
	if math.Abs(o.StrategyReturnPct-3) > 1e-9 {
		t.Errorf("return = %v%%, want 3%% (day 3 close)", o.StrategyReturnPct)
	}
}

// A day whose range covers both levels cannot be ordered from a daily bar.
// Resolving to the target would be the optimistic bias that makes a backtest
// look tradeable, so the stop must win and the row must be flagged.
func TestSimulateSwingBracketResolvesAmbiguityToTheStop(t *testing.T) {
	cfg := swingTestConfig()
	o := SwingOutcome{EntryPrice: 100, ReturnPct: map[string]float64{}}

	cfg.simulateSwingBracket(&o, swingCandidate(), swingBars(
		[3]float64{109, 95, 104}, // straddles both 108 and 96
	))

	if !o.Ambiguous {
		t.Error("same-day target and stop should be flagged ambiguous")
	}
	if o.ExitReason != SwingExitStop {
		t.Errorf("exit = %q, want stop (the conservative resolution)", o.ExitReason)
	}
	if math.Abs(o.StrategyReturnPct-(-4)) > 1e-9 {
		t.Errorf("return = %v%%, want -4%%", o.StrategyReturnPct)
	}
}

// A session too recent to have run its course is not a finished trade.
func TestSimulateSwingBracketLeavesRecentTradesOpen(t *testing.T) {
	cfg := swingTestConfig()
	o := SwingOutcome{EntryPrice: 100, ReturnPct: map[string]float64{}}

	cfg.simulateSwingBracket(&o, swingCandidate(), swingBars(
		[3]float64{101, 99, 100},
		[3]float64{102, 99, 101},
	))

	if o.ExitReason != SwingExitOpen {
		t.Fatalf("exit = %q, want open", o.ExitReason)
	}
	if o.Mature() {
		t.Error("an open trade must not count as mature")
	}
	// An unfinished trade must contribute no return, or recent sessions read
	// as early time stops.
	if o.StrategyReturnPct != 0 || o.NetStrategyReturnPct != 0 || o.CostPct != 0 {
		t.Errorf("open trade carries figures: gross=%v net=%v cost=%v",
			o.StrategyReturnPct, o.NetStrategyReturnPct, o.CostPct)
	}
}

func TestSwingCrossingsByExit(t *testing.T) {
	// A target fills a resting limit, so only the entry crosses the spread.
	if got := swingCrossings(ExitReasonTarget); got != 1 {
		t.Errorf("target crossings = %v, want 1", got)
	}
	// A stop and a time stop are both market orders.
	for _, reason := range []string{SwingExitStop, SwingExitTime} {
		if got := swingCrossings(reason); got != 2 {
			t.Errorf("%s crossings = %v, want 2", reason, got)
		}
	}
}

func TestSimulateSwingBracketChargesCosts(t *testing.T) {
	cfg := swingTestConfig()
	cfg.Costs = CostModel{AssumedSpreadPct: 0.2}
	c := swingCandidate()
	c.SpreadPct = 0.2

	// Target: one crossing of a 0.2% spread, so 0.1% of an 8% move.
	hit := SwingOutcome{EntryPrice: 100, ReturnPct: map[string]float64{}}
	cfg.simulateSwingBracket(&hit, c, swingBars([3]float64{109, 100, 108}))
	if math.Abs(hit.CostPct-0.1) > 1e-9 {
		t.Errorf("target cost = %v%%, want 0.1%%", hit.CostPct)
	}
	if math.Abs(hit.NetStrategyReturnPct-7.9) > 1e-9 {
		t.Errorf("target net = %v%%, want 7.9%%", hit.NetStrategyReturnPct)
	}
	// Friction is now a rounding error against the target, which is the whole
	// argument for the longer horizon.
	if hit.CostPct/hit.StrategyReturnPct > 0.02 {
		t.Errorf("cost is %.1f%% of the target; swing friction should be negligible",
			hit.CostPct/hit.StrategyReturnPct*100)
	}

	// Stop: a market order, so two crossings.
	stopped := SwingOutcome{EntryPrice: 100, ReturnPct: map[string]float64{}}
	cfg.simulateSwingBracket(&stopped, c, swingBars([3]float64{100, 95, 96}))
	if math.Abs(stopped.CostPct-0.2) > 1e-9 {
		t.Errorf("stop cost = %v%%, want 0.2%%", stopped.CostPct)
	}
}

func TestScoreSwingEntryBases(t *testing.T) {
	// testSession is dated 2026-09-02, which must be the gap session here.
	bars := []marketdata.Bar{
		dailyBar(t, "2026-09-02", 95, 101, 94, 100), // the gap session
		dailyBar(t, "2026-09-03", 102, 106, 101, 105),
		dailyBar(t, "2026-09-04", 105, 110, 104, 109),
	}
	session := testSession(t, "16:30")
	scan := &ScanResult{Phase: PhaseOpen}

	t.Run("session_close", func(t *testing.T) {
		cfg := swingTestConfig()
		cfg.SwingEntry = SwingEntrySessionClose
		o, track := testScanner(t, cfg).scoreSwing(session, scan, swingCandidate(), bars)

		if math.Abs(o.EntryPrice-100) > 1e-9 {
			t.Errorf("entry = %v, want the gap day's close of 100", o.EntryPrice)
		}
		if o.EntryDate != "2026-09-02" {
			t.Errorf("entry date = %q, want the gap date", o.EntryDate)
		}
		if len(track) != 2 {
			t.Fatalf("forward bars = %d, want 2", len(track))
		}
		if math.Abs(o.ReturnPct["1d"]-5) > 1e-9 {
			t.Errorf("1d return = %v%%, want 5%%", o.ReturnPct["1d"])
		}
		if math.Abs(o.ReturnPct["2d"]-9) > 1e-9 {
			t.Errorf("2d return = %v%%, want 9%%", o.ReturnPct["2d"])
		}
		// Horizons the data has not reached must be absent, not zero.
		if _, ok := o.ReturnPct["5d"]; ok {
			t.Error("5d return present with only 2 forward bars")
		}
	})

	t.Run("next_open", func(t *testing.T) {
		cfg := swingTestConfig()
		cfg.SwingEntry = SwingEntryNextOpen
		o, track := testScanner(t, cfg).scoreSwing(session, scan, swingCandidate(), bars)

		if math.Abs(o.EntryPrice-102) > 1e-9 {
			t.Errorf("entry = %v, want the next open of 102", o.EntryPrice)
		}
		if o.EntryDate != "2026-09-03" {
			t.Errorf("entry date = %q, want the next session", o.EntryDate)
		}
		// Buying that day's OPEN means its whole range is still ahead of the
		// position, so the entry day counts as forward day 1. That is the
		// opposite of session_close, where the day's range already happened.
		if len(track) != 2 {
			t.Fatalf("forward bars = %d, want 2", len(track))
		}
		if track[0].Date != "2026-09-03" {
			t.Errorf("day 1 = %q, want the entry day itself", track[0].Date)
		}
		// Excursions run from the 102 entry across both forward days, peaking
		// at day 2's high of 110.
		if math.Abs(o.MaxFavourablePct-pctChange(102, 110)) > 1e-9 {
			t.Errorf("MFE = %v%%, want %v%%", o.MaxFavourablePct, pctChange(102, 110))
		}
	})
}

// Excursions must start after the entry, matching the intraday path: the entry
// bar's own extremes may have printed before the position existed.
func TestScoreSwingExcursionsExcludeTheEntryBar(t *testing.T) {
	cfg := swingTestConfig()
	// The gap day swings 80-130 but closes at 100, which is the fill.
	bars := []marketdata.Bar{
		dailyBar(t, "2026-09-02", 90, 130, 80, 100),
		dailyBar(t, "2026-09-03", 100, 104, 98, 102),
	}

	o, _ := testScanner(t, cfg).scoreSwing(
		testSession(t, "16:30"), &ScanResult{Phase: PhaseOpen}, swingCandidate(), bars)

	if math.Abs(o.MaxFavourablePct-4) > 1e-9 {
		t.Errorf("MFE = %v%%, want 4%% from the day after entry, not 30%%", o.MaxFavourablePct)
	}
	if math.Abs(o.MaxAdversePct-(-2)) > 1e-9 {
		t.Errorf("MAE = %v%%, want -2%%, not -20%%", o.MaxAdversePct)
	}
}

func TestScoreSwingWithoutTheGapSessionYieldsNothing(t *testing.T) {
	cfg := swingTestConfig()
	// Bars that skip the session being scored, e.g. a halted symbol.
	bars := []marketdata.Bar{dailyBar(t, "2026-09-05", 100, 101, 99, 100)}

	o, track := testScanner(t, cfg).scoreSwing(
		testSession(t, "16:30"), &ScanResult{Phase: PhaseOpen}, swingCandidate(), bars)

	if o.EntryPrice != 0 || len(track) != 0 {
		t.Errorf("expected no entry, got price=%v bars=%d", o.EntryPrice, len(track))
	}
}

func TestSummariseSwingExcludesImmatureTrades(t *testing.T) {
	finished := func(reason string, net float64, day int) SwingOutcome {
		return SwingOutcome{ExitReason: reason, NetStrategyReturnPct: net, ExitDay: day}
	}

	sum := SummariseSwing([]SwingOutcome{
		finished(ExitReasonTarget, 8, 3),
		finished(SwingExitStop, -4, 2),
		finished(SwingExitTime, 1, 10),
		{ExitReason: SwingExitOpen},
		{ExitReason: SwingExitOpen},
	})

	if sum.Trades != 3 {
		t.Errorf("trades = %d, want 3", sum.Trades)
	}
	if sum.Immature != 2 {
		t.Errorf("immature = %d, want 2", sum.Immature)
	}
	if sum.HitTarget != 1 || sum.HitStop != 1 || sum.TimeStop != 1 {
		t.Errorf("exit mix = target %d, stop %d, time %d; want 1/1/1",
			sum.HitTarget, sum.HitStop, sum.TimeStop)
	}
	if want := (8.0 - 4.0 + 1.0) / 3; math.Abs(sum.MeanNetPct-want) > 1e-9 {
		t.Errorf("mean net = %v%%, want %v%%", sum.MeanNetPct, want)
	}
	if math.Abs(sum.NetWinRate-2.0/3*100) > 1e-9 {
		t.Errorf("net win rate = %v%%, want 66.7%%", sum.NetWinRate)
	}
	if math.Abs(sum.MeanHoldDays-5) > 1e-9 {
		t.Errorf("mean hold = %v days, want 5", sum.MeanHoldDays)
	}
}

func TestSummariseSwingWithNoFinishedTrades(t *testing.T) {
	sum := SummariseSwing([]SwingOutcome{{ExitReason: SwingExitOpen}})

	if sum.Trades != 0 || sum.MeanNetPct != 0 {
		t.Errorf("expected an empty summary, got %+v", sum)
	}
	if sum.Immature != 1 {
		t.Errorf("immature = %d, want 1", sum.Immature)
	}
}

func TestApplyScannerDefaultsFillsSwingSettings(t *testing.T) {
	cfg := ScannerConfig{}
	applyScannerDefaults(&cfg)

	if cfg.SwingEntry != SwingEntrySessionClose {
		t.Errorf("SwingEntry = %q, want %q", cfg.SwingEntry, SwingEntrySessionClose)
	}
	if cfg.SwingTargetATR <= 0 || cfg.SwingStopATR <= 0 || cfg.SwingMaxHoldDays <= 0 {
		t.Errorf("swing sizing left at zero: %+v", cfg)
	}
	if cfg.NewsLookbackHours <= 0 {
		t.Errorf("NewsLookbackHours = %d, want a default window", cfg.NewsLookbackHours)
	}
	if len(cfg.SwingHorizonDays) == 0 || len(cfg.SwingSweepDays) == 0 ||
		len(cfg.SwingSweepTargetATR) == 0 {
		t.Error("swing horizon or sweep grids left empty")
	}
}
