package main

import (
	"fmt"
	"math"
	"testing"
)

// trackFrom builds a 5-minute track where each bar is flat at the given price,
// so excursion maths is easy to reason about.
func trackFrom(closes ...float64) []IntradayBar {
	track := make([]IntradayBar, 0, len(closes))
	for i, c := range closes {
		minutes := (i + 1) * 5
		track = append(track, IntradayBar{
			Time:            fmt.Sprintf("2026-09-02T09:%02d:00-04:00", 30+minutes),
			MinutesFromOpen: minutes,
			Open:            c,
			High:            c,
			Low:             c,
			Close:           c,
		})
	}
	return track
}

func TestBarIndexAt(t *testing.T) {
	track := trackFrom(10, 11, 12)

	if i, ok := barIndexAt(track, 5); !ok || i != 0 {
		t.Errorf("at 5m got (%d, %t), want (0, true)", i, ok)
	}
	if i, ok := barIndexAt(track, 15); !ok || i != 2 {
		t.Errorf("at 15m got (%d, %t), want (2, true)", i, ok)
	}
	// Not an exact multiple: take the first bar at or after the horizon.
	if i, ok := barIndexAt(track, 7); !ok || i != 1 {
		t.Errorf("at 7m got (%d, %t), want (1, true)", i, ok)
	}
	// Past the end of the recorded session.
	if _, ok := barIndexAt(track, 400); ok {
		t.Error("at 400m got ok, want not-ok")
	}
}

func TestScoreCandidate(t *testing.T) {
	s := &Scanner{cfg: ScannerConfig{
		EntryMinutesFromOpen:   5,
		OutcomeHorizonsMinutes: []int{5, 15},
	}}
	session := TradingSession{Date: "2026-09-02"}
	scan := &ScanResult{Phase: PhaseOpen}

	c := GapCandidate{
		Symbol:       "AAA",
		Direction:    "up",
		PrevClose:    90,
		GapPct:       11.1,
		DiscoveredAt: PhaseOpen,
		Methods:      []string{MethodPercent},
	}

	o := s.scoreCandidate(session, scan, c, trackFrom(100, 102, 104, 103))

	if o.EntryPrice != 100 {
		t.Errorf("entry_price = %v, want the close of the first bar (100)", o.EntryPrice)
	}
	if o.EntryMinutes != 5 {
		t.Errorf("entry_minutes = %d, want 5", o.EntryMinutes)
	}
	if got := o.ReturnPct["5m"]; math.Abs(got) > 1e-9 {
		t.Errorf("5m return = %v, want 0 (entry is the 5m bar)", got)
	}
	if got := o.ReturnPct["15m"]; math.Abs(got-4.0) > 1e-9 {
		t.Errorf("15m return = %v, want 4.0", got)
	}
	if math.Abs(o.CloseReturnPct-3.0) > 1e-9 {
		t.Errorf("close return = %v, want 3.0", o.CloseReturnPct)
	}
	if got := o.ReturnPct[HorizonClose]; math.Abs(got-3.0) > 1e-9 {
		t.Errorf("return_pct[close] = %v, want 3.0", got)
	}
	if math.Abs(o.MaxFavourablePct-4.0) > 1e-9 {
		t.Errorf("MFE = %v, want 4.0", o.MaxFavourablePct)
	}
	if math.Abs(o.MaxAdversePct) > 1e-9 {
		t.Errorf("MAE = %v, want 0 (price never went below entry)", o.MaxAdversePct)
	}
	if !o.Flagged {
		t.Error("flagged = false for a candidate with a method")
	}
	if o.SessionOpenPrice != 100 {
		t.Errorf("session_open_price = %v, want 100", o.SessionOpenPrice)
	}
	if o.Bars != 4 {
		t.Errorf("bars = %d, want 4", o.Bars)
	}
}

// A track too short to reach the entry horizon must not invent a return.
func TestScoreCandidateWithoutEnoughBars(t *testing.T) {
	s := &Scanner{cfg: ScannerConfig{
		EntryMinutesFromOpen:   30,
		OutcomeHorizonsMinutes: []int{60},
	}}

	o := s.scoreCandidate(TradingSession{}, &ScanResult{}, GapCandidate{Symbol: "AAA"}, trackFrom(100, 101))

	if o.EntryPrice != 0 {
		t.Errorf("entry_price = %v, want 0 when the entry bar is missing", o.EntryPrice)
	}
	if len(o.ReturnPct) != 0 {
		t.Errorf("return_pct = %v, want empty", o.ReturnPct)
	}
	if o.CloseReturnPct != 0 {
		t.Errorf("close return = %v, want 0", o.CloseReturnPct)
	}
}

func TestGapFill(t *testing.T) {
	// Up gap from 101: price never trades back down to it.
	filled, when := gapFill(trackFrom(103, 104, 105), GapCandidate{Direction: "up", PrevClose: 101})
	if filled {
		t.Errorf("up gap reported filled at %dm, want unfilled", when)
	}

	// Up gap from 104: the 10-minute bar trades through it.
	filled, when = gapFill(trackFrom(105, 103, 106), GapCandidate{Direction: "up", PrevClose: 104})
	if !filled || when != 10 {
		t.Errorf("got (%t, %dm), want (true, 10m)", filled, when)
	}

	// Down gap from 100: price rallies back up through the previous close.
	filled, when = gapFill(trackFrom(95, 97, 101), GapCandidate{Direction: "down", PrevClose: 100})
	if !filled || when != 15 {
		t.Errorf("got (%t, %dm), want (true, 15m)", filled, when)
	}

	// No previous close means the question is unanswerable.
	if filled, _ := gapFill(trackFrom(100), GapCandidate{Direction: "up"}); filled {
		t.Error("reported a fill with no previous close")
	}
}

func TestBuildPortfolio(t *testing.T) {
	cfg := ScannerConfig{EntryMinutesFromOpen: 5, OutcomeHorizonsMinutes: []int{30}}
	outcomes := []Outcome{
		{Symbol: "U1", Direction: "up", Flagged: true, ReturnPct: map[string]float64{"30m": 4, HorizonClose: 6}},
		{Symbol: "U2", Direction: "up", Flagged: true, ReturnPct: map[string]float64{"30m": -2, HorizonClose: 2}},
		{Symbol: "D1", Direction: "down", Flagged: true, ReturnPct: map[string]float64{"30m": 1, HorizonClose: -4}},
		// Faded rows are not something you would have bought.
		{Symbol: "F1", Direction: "up", Faded: true, ReturnPct: map[string]float64{"30m": 99, HorizonClose: 99}},
	}

	p := BuildPortfolio(TradingSession{Date: "2026-09-02"}, &ScanResult{Phase: PhaseOpen}, outcomes, cfg)

	if p.Flagged != 3 || p.Faded != 1 {
		t.Fatalf("flagged=%d faded=%d, want 3 and 1", p.Flagged, p.Faded)
	}

	all30 := p.All["30m"]
	if all30.Count != 3 {
		t.Errorf("30m count = %d, want 3 (faded excluded)", all30.Count)
	}
	if want := (4.0 - 2.0 + 1.0) / 3; math.Abs(all30.MeanPct-want) > 1e-9 {
		t.Errorf("30m mean = %v, want %v", all30.MeanPct, want)
	}
	if math.Abs(all30.MedianPct-1.0) > 1e-9 {
		t.Errorf("30m median = %v, want 1.0", all30.MedianPct)
	}
	if math.Abs(all30.WinRate-200.0/3) > 1e-9 {
		t.Errorf("30m win rate = %v, want 2 of 3", all30.WinRate)
	}
	if all30.BestPct != 4 || all30.WorstPct != -2 {
		t.Errorf("30m best/worst = %v/%v, want 4/-2", all30.BestPct, all30.WorstPct)
	}

	// The faded 99% return must not leak into any bucket.
	if p.All[HorizonClose].BestPct == 99 || p.Up[HorizonClose].BestPct == 99 {
		t.Error("a faded candidate's return leaked into the portfolio")
	}

	if got := p.Up["30m"].Count; got != 2 {
		t.Errorf("up count = %d, want 2", got)
	}
	if got := p.Down["30m"].Count; got != 1 {
		t.Errorf("down count = %d, want 1", got)
	}
	if want := -4.0; math.Abs(p.Down[HorizonClose].MeanPct-want) > 1e-9 {
		t.Errorf("down close mean = %v, want %v", p.Down[HorizonClose].MeanPct, want)
	}

	if len(p.Horizons) != 2 || p.Horizons[0] != "30m" || p.Horizons[1] != HorizonClose {
		t.Errorf("horizons = %v, want [30m close]", p.Horizons)
	}
}

func TestSimulateExitHitsTarget(t *testing.T) {
	s := &Scanner{cfg: ScannerConfig{
		EntryMinutesFromOpen: 5,
		ExitReference:        ExitRefEntry,
		ExitTargetPct:        1.0,
	}}

	// Entry 100 at 5m, dips to 98, then trades through 101.
	o := s.scoreCandidate(TradingSession{}, &ScanResult{},
		GapCandidate{Symbol: "AAA", Direction: "down", PrevClose: 110},
		trackFrom(100, 98, 101, 99))

	if o.ExitReason != ExitReasonTarget {
		t.Fatalf("exit_reason = %q, want target", o.ExitReason)
	}
	if o.ExitMinutes != 15 {
		t.Errorf("exit_minutes = %d, want 15", o.ExitMinutes)
	}
	if math.Abs(o.ExitTargetPrice-101) > 1e-9 {
		t.Errorf("exit_target_price = %v, want 101", o.ExitTargetPrice)
	}
	if math.Abs(o.StrategyReturnPct-1.0) > 1e-9 {
		t.Errorf("strategy_return_pct = %v, want 1.0", o.StrategyReturnPct)
	}
	// The -2% dip happened before the target fired and must be recorded.
	if math.Abs(o.AdverseBeforeExitPct-(-2.0)) > 1e-9 {
		t.Errorf("adverse_before_exit_pct = %v, want -2.0", o.AdverseBeforeExitPct)
	}
}

func TestSimulateExitHoldsToClose(t *testing.T) {
	s := &Scanner{cfg: ScannerConfig{
		EntryMinutesFromOpen: 5,
		ExitReference:        ExitRefEntry,
		ExitTargetPct:        5.0,
	}}

	// Never reaches entry +5%, so it is carried into the closing bell.
	o := s.scoreCandidate(TradingSession{}, &ScanResult{},
		GapCandidate{Symbol: "AAA", Direction: "down", PrevClose: 110},
		trackFrom(100, 97, 102, 96))

	if o.ExitReason != ExitReasonClose {
		t.Fatalf("exit_reason = %q, want session_close", o.ExitReason)
	}
	if math.Abs(o.StrategyReturnPct-(-4.0)) > 1e-9 {
		t.Errorf("strategy_return_pct = %v, want -4.0", o.StrategyReturnPct)
	}
	if math.Abs(o.AdverseBeforeExitPct-(-4.0)) > 1e-9 {
		t.Errorf("adverse_before_exit_pct = %v, want -4.0", o.AdverseBeforeExitPct)
	}
}

// With prev_close as the reference, a down gapper must climb all the way back
// through yesterday's close before the exit fires.
func TestSimulateExitPrevCloseReference(t *testing.T) {
	s := &Scanner{cfg: ScannerConfig{
		EntryMinutesFromOpen: 5,
		ExitReference:        ExitRefPrevClose,
		ExitTargetPct:        0,
	}}
	c := GapCandidate{Symbol: "AAA", Direction: "down", PrevClose: 110}

	// Peaks at 105, short of the 110 previous close.
	o := s.scoreCandidate(TradingSession{}, &ScanResult{}, c, trackFrom(100, 103, 105, 104))
	if o.ExitReason != ExitReasonClose {
		t.Errorf("exit_reason = %q, want session_close (never filled the gap)", o.ExitReason)
	}
	if math.Abs(o.ExitTargetPrice-110) > 1e-9 {
		t.Errorf("exit_target_price = %v, want the previous close of 110", o.ExitTargetPrice)
	}

	// Now it does trade back above 110.
	o = s.scoreCandidate(TradingSession{}, &ScanResult{}, c, trackFrom(100, 103, 111))
	if o.ExitReason != ExitReasonTarget {
		t.Errorf("exit_reason = %q, want target", o.ExitReason)
	}
	if math.Abs(o.StrategyReturnPct-10.0) > 1e-9 {
		t.Errorf("strategy_return_pct = %v, want 10.0", o.StrategyReturnPct)
	}
}

func TestSummariseStrategy(t *testing.T) {
	cfg := ScannerConfig{ExitReference: ExitRefEntry, ExitTargetPct: 0.1}
	outcomes := []Outcome{
		{ExitReason: ExitReasonTarget, StrategyReturnPct: 0.1, ExitMinutes: 10, AdverseBeforeExitPct: -0.5},
		{ExitReason: ExitReasonTarget, StrategyReturnPct: 0.1, ExitMinutes: 30, AdverseBeforeExitPct: -3.0},
		{ExitReason: ExitReasonClose, StrategyReturnPct: -9.0, ExitMinutes: 390, AdverseBeforeExitPct: -9.5},
		{ExitReason: ExitReasonNoEntry},
	}

	sum := summariseStrategy(outcomes, cfg)

	if sum.Count != 3 {
		t.Fatalf("count = %d, want 3 (no-entry excluded)", sum.Count)
	}
	if sum.HitTarget != 2 || sum.HeldToClose != 1 {
		t.Errorf("hit=%d held=%d, want 2 and 1", sum.HitTarget, sum.HeldToClose)
	}
	// Two tiny wins do not pay for one large loss: this is the whole risk of
	// the strategy and the summary must show it.
	if want := (0.1 + 0.1 - 9.0) / 3; math.Abs(sum.MeanReturnPct-want) > 1e-9 {
		t.Errorf("mean = %v, want %v", sum.MeanReturnPct, want)
	}
	if math.Abs(sum.WinRate-200.0/3) > 1e-9 {
		t.Errorf("win rate = %v, want 2 of 3", sum.WinRate)
	}
	if math.Abs(sum.WorstAdverseBeforeExitPct-(-9.5)) > 1e-9 {
		t.Errorf("worst drawdown = %v, want -9.5", sum.WorstAdverseBeforeExitPct)
	}
	if math.Abs(sum.MeanExitMinutes-(10+30+390)/3.0) > 1e-9 {
		t.Errorf("mean exit minutes = %v", sum.MeanExitMinutes)
	}
}

func TestStatsForEmpty(t *testing.T) {
	if got := statsFor(nil, "30m"); got.Count != 0 {
		t.Errorf("got %+v, want a zero value", got)
	}
	// Outcomes that never reached the horizon should not be counted.
	only5 := []Outcome{{ReturnPct: map[string]float64{"5m": 3}}}
	if got := statsFor(only5, "60m"); got.Count != 0 {
		t.Errorf("60m count = %d, want 0", got.Count)
	}
}

func TestPctChange(t *testing.T) {
	if got := pctChange(100, 110); math.Abs(got-10) > 1e-9 {
		t.Errorf("got %v, want 10", got)
	}
	if got := pctChange(0, 110); got != 0 {
		t.Errorf("got %v, want 0 for a zero base", got)
	}
}
