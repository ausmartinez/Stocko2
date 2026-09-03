package main

import (
	"math"
	"testing"
)

// sweepConfig disables costs and the floor so a test sees the target/fill
// arithmetic on its own.
func sweepConfig(t *testing.T, dataDir string) ScannerConfig {
	t.Helper()

	cfg := DefaultScannerConfig()
	cfg.DataDir = dataDir
	cfg.ExitReference = ExitRefEntry
	cfg.MinTargetCostMult = 0
	cfg.Costs = CostModel{Disabled: true}
	return cfg
}

// sweepTrade builds a scored trade at entry 100 with an ATR of 4, so a
// multiple k produces a target of exactly 4k percent.
func sweepTrade(symbol string, mfePct, closePct float64) Outcome {
	return Outcome{
		SessionDate:      "2026-09-02",
		Symbol:           symbol,
		Flagged:          true,
		Direction:        "up",
		PrevClose:        100,
		ATR:              4,
		EntryPrice:       100,
		ExitReason:       ExitReasonClose,
		MaxFavourablePct: mfePct,
		CloseReturnPct:   closePct,
	}
}

// writeSweepSession puts scored outcomes where the sweep looks for them.
func writeSweepSession(t *testing.T, dataDir, date string, outcomes []Outcome) {
	t.Helper()

	if err := SaveJSON(PhaseFile(dataDir, date, PhaseOutcome), outcomes); err != nil {
		t.Fatal(err)
	}
}

// The four trades below are shaped so expectancy rises then falls, giving a
// single unambiguous optimum at 0.50 x ATR.
func sweepFixture() []Outcome {
	return []Outcome{
		sweepTrade("AAA", 1.2, 0.5),
		sweepTrade("BBB", 2.4, -1.0),
		sweepTrade("CCC", 3.6, 1.0),
		sweepTrade("DDD", 6.0, 5.0),
	}
}

func TestSweepFindsTheBestExpectancy(t *testing.T) {
	dir := t.TempDir()
	cfg := sweepConfig(t, dir)
	writeSweepSession(t, dir, "2026-09-02", sweepFixture())

	result, err := Sweep(cfg, "", []float64{0.25, 0.50, 0.75, 1.00})
	if err != nil {
		t.Fatal(err)
	}
	if result.Trades != 4 {
		t.Fatalf("trades = %d, want 4", result.Trades)
	}

	// Worked by hand from the fixture: targets are 1, 2, 3 and 4 percent.
	want := map[float64]struct {
		fills int
		net   float64
	}{
		0.25: {4, 1.000}, // everything fills at +1%
		0.50: {3, 1.625}, // three fill at +2%, the miss still closed +0.5%
		0.75: {2, 1.375}, // two fill at +3%, one miss closed -1%
		1.00: {1, 1.125}, // only the 6% runner fills
	}
	for _, row := range result.Rows {
		exp, ok := want[row.ATRMult]
		if !ok {
			t.Fatalf("unexpected multiple %v", row.ATRMult)
		}
		if row.Filled != exp.fills {
			t.Errorf("k=%.2f: filled = %d, want %d", row.ATRMult, row.Filled, exp.fills)
		}
		if math.Abs(row.MeanNetPct-exp.net) > 1e-9 {
			t.Errorf("k=%.2f: net = %v%%, want %v%%", row.ATRMult, row.MeanNetPct, exp.net)
		}
	}

	if result.Best == nil {
		t.Fatal("no best row identified")
	}
	if result.Best.ATRMult != 0.50 {
		t.Errorf("best = %.2f x ATR, want 0.50", result.Best.ATRMult)
	}
}

func TestSweepMeasuresGivebackAndOvershoot(t *testing.T) {
	dir := t.TempDir()
	cfg := sweepConfig(t, dir)
	writeSweepSession(t, dir, "2026-09-02", sweepFixture())

	result, err := Sweep(cfg, "", []float64{0.50})
	if err != nil {
		t.Fatal(err)
	}
	row := result.Rows[0]

	// The one miss showed 1.2% and closed at 0.5%, so it handed back 0.7%.
	if math.Abs(row.MeanGivebackPct-0.7) > 1e-9 {
		t.Errorf("giveback = %v%%, want 0.7%%", row.MeanGivebackPct)
	}
	// The three fills ran to 2.4, 3.6 and 6.0 past a 2% target: mean 2.0% left
	// on the table, which is the cost of aiming low.
	if math.Abs(row.MeanOvershootPct-2.0) > 1e-9 {
		t.Errorf("overshoot = %v%%, want 2.0%%", row.MeanOvershootPct)
	}
}

func TestSweepSkipsTradesItCannotUse(t *testing.T) {
	dir := t.TempDir()
	cfg := sweepConfig(t, dir)

	faded := sweepTrade("FADE", 5, 1)
	faded.Faded = true
	unflagged := sweepTrade("UNFL", 5, 1)
	unflagged.Flagged = false
	noEntry := sweepTrade("NOEN", 5, 1)
	noEntry.EntryPrice = 0
	noExit := sweepTrade("NOEX", 5, 1)
	noExit.ExitReason = ""
	noATR := sweepTrade("NOAT", 5, 1)
	noATR.ATR = 0

	writeSweepSession(t, dir, "2026-09-02", []Outcome{
		sweepTrade("GOOD", 5, 1), faded, unflagged, noEntry, noExit, noATR,
	})

	result, err := Sweep(cfg, "", []float64{0.50})
	if err != nil {
		t.Fatal(err)
	}

	if result.Trades != 1 {
		t.Errorf("trades = %d, want only the usable one", result.Trades)
	}
	if result.SkippedUnheld != 2 {
		t.Errorf("skipped_not_flagged = %d, want 2 (faded + unflagged)", result.SkippedUnheld)
	}
	if result.SkippedNoFill != 2 {
		t.Errorf("skipped_no_entry = %d, want 2 (no entry + no exit)", result.SkippedNoFill)
	}
	// Without ATR the multiple under test cannot change anything, so the row
	// would silently flatten the sweep.
	if result.SkippedNoATR != 1 {
		t.Errorf("skipped_no_atr = %d, want 1", result.SkippedNoATR)
	}
}

func TestSweepSpansMultipleSessions(t *testing.T) {
	dir := t.TempDir()
	cfg := sweepConfig(t, dir)
	writeSweepSession(t, dir, "2026-09-01", []Outcome{sweepTrade("AAA", 5, 1)})
	writeSweepSession(t, dir, "2026-09-02", []Outcome{sweepTrade("BBB", 5, 1)})

	all, err := Sweep(cfg, "", []float64{0.50})
	if err != nil {
		t.Fatal(err)
	}
	if all.Sessions != 2 || all.Trades != 2 {
		t.Errorf("sessions=%d trades=%d, want 2 and 2", all.Sessions, all.Trades)
	}

	// A date narrows it to one session.
	one, err := Sweep(cfg, "2026-09-02", []float64{0.50})
	if err != nil {
		t.Fatal(err)
	}
	if one.Sessions != 1 || one.Trades != 1 {
		t.Errorf("scoped sessions=%d trades=%d, want 1 and 1", one.Sessions, one.Trades)
	}
}

func TestSweepWithNoDataIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	cfg := sweepConfig(t, dir)
	// A session directory with no outcome file, which is the state before
	// -outcomes has ever run.
	writeSweepSession(t, dir, "2026-09-02", nil)

	result, err := Sweep(cfg, "", defaultSweepMultiples())
	if err != nil {
		t.Fatal(err)
	}
	if result.Trades != 0 {
		t.Errorf("trades = %d, want 0", result.Trades)
	}
	if result.Best != nil {
		t.Errorf("best = %+v, want nil with no trades", result.Best)
	}
	if len(result.Rows) != 0 {
		t.Errorf("rows = %d, want none scored", len(result.Rows))
	}
}

// The cost floor overrides the multiple on cheap or wide-spread symbols, which
// makes the sweep flat. That is a finding, and sweepIsInformative reports it.
func TestSweepFlatWhenCostFloorDominates(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultScannerConfig()
	cfg.DataDir = dir
	cfg.ExitReference = ExitRefEntry

	// A $1 stock with a one-cent spread: breakeven is far above any small
	// multiple of its tiny ATR.
	cheap := Outcome{
		SessionDate: "2026-09-02", Symbol: "PENNY", Flagged: true, Direction: "up",
		PrevClose: 1, ATR: 0.004, EntryPrice: 1, SpreadPct: 1.0,
		ExitReason: ExitReasonClose, MaxFavourablePct: 5, CloseReturnPct: 1,
	}
	writeSweepSession(t, dir, "2026-09-02", []Outcome{cheap})

	result, err := Sweep(cfg, "", []float64{0.05, 0.10, 0.25})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range result.Rows {
		if row.CostFloored != 1 {
			t.Errorf("k=%.2f: cost_floored = %d, want 1", row.ATRMult, row.CostFloored)
		}
	}
	if sweepIsInformative(result) {
		t.Error("sweep reported as informative, but every target came from the floor")
	}
}

func TestSweepIsInformativeWhenExpectancyVaries(t *testing.T) {
	dir := t.TempDir()
	cfg := sweepConfig(t, dir)
	writeSweepSession(t, dir, "2026-09-02", sweepFixture())

	result, err := Sweep(cfg, "", []float64{0.25, 0.50, 1.00})
	if err != nil {
		t.Fatal(err)
	}
	if !sweepIsInformative(result) {
		t.Error("expectancy varies across multiples but the sweep reported flat")
	}
}

func TestSweepMultiplesFallsBackAndSanitises(t *testing.T) {
	cfg := DefaultScannerConfig()

	if got := sweepMultiples(cfg); len(got) != len(defaultSweepMultiples()) {
		t.Errorf("unset multiples gave %v, want the built-in grid", got)
	}

	cfg.SweepATRMultiples = []float64{1.0, 0.25, 0.5}
	got := sweepMultiples(cfg)
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Errorf("multiples not sorted: %v", got)
			break
		}
	}

	// A zero or negative multiple would collapse the target onto the floor.
	cfg.SweepATRMultiples = []float64{-1, 0, 0.5}
	if got := sweepMultiples(cfg); len(got) != 1 || got[0] != 0.5 {
		t.Errorf("sanitised multiples = %v, want [0.5]", got)
	}

	cfg.SweepATRMultiples = []float64{-1, 0}
	if got := sweepMultiples(cfg); len(got) != len(defaultSweepMultiples()) {
		t.Errorf("all-invalid multiples gave %v, want the built-in grid", got)
	}
}

// The sweep must size targets through resolveTarget so it cannot drift from
// how the live and simulated paths do it.
func TestSweepTargetsMatchResolveTarget(t *testing.T) {
	dir := t.TempDir()
	cfg := sweepConfig(t, dir)
	writeSweepSession(t, dir, "2026-09-02", []Outcome{sweepTrade("AAA", 5, 1)})

	result, err := Sweep(cfg, "", []float64{0.75})
	if err != nil {
		t.Fatal(err)
	}

	sized := cfg
	sized.ExitTargetATR = 0.75
	_, want, _ := sized.resolveTarget(GapCandidate{PrevClose: 100, ATR: 4}, 100)

	if math.Abs(result.Rows[0].MeanTargetPct-want) > 1e-9 {
		t.Errorf("sweep target = %v%%, resolveTarget says %v%%",
			result.Rows[0].MeanTargetPct, want)
	}
}

func TestSweepChargesTheRightCrossingsPerOutcome(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultScannerConfig()
	cfg.DataDir = dir
	cfg.ExitReference = ExitRefEntry
	cfg.MinTargetCostMult = 0 // isolate the cost, not the floor
	cfg.Costs = CostModel{AssumedSpreadPct: 0.2}

	// A 1% target on a $100 stock with a 0.2% spread. The first trade reaches
	// it, the second does not.
	fill := sweepTrade("FILL", 5.0, 1.0)
	fill.SpreadPct = 0.2
	miss := sweepTrade("MISS", 0.1, 0.0)
	miss.SpreadPct = 0.2
	writeSweepSession(t, dir, "2026-09-02", []Outcome{fill})

	filled, err := Sweep(cfg, "", []float64{0.25})
	if err != nil {
		t.Fatal(err)
	}
	// Target 1%, entry crosses once for half of a 0.2% spread: net 0.9%.
	if net := filled.Rows[0].MeanFilledNet; math.Abs(net-0.9) > 1e-9 {
		t.Errorf("filled net = %v%%, want 0.9%% (1%% target less one crossing)", net)
	}

	writeSweepSession(t, dir, "2026-09-02", []Outcome{miss})
	missed, err := Sweep(cfg, "", []float64{0.25})
	if err != nil {
		t.Fatal(err)
	}
	// Flattened at the close for no gain, having crossed twice: -0.2%.
	if net := missed.Rows[0].MeanMissedNet; math.Abs(net-(-0.2)) > 1e-9 {
		t.Errorf("missed net = %v%%, want -0.2%% (flat close less two crossings)", net)
	}
}
