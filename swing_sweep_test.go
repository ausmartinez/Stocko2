package main

import (
	"math"
	"testing"
)

// swingSweepOutcome is a scored row shaped for replay: $100 entry, ATR 4, so a
// target multiple k puts the target at 4k percent.
func swingSweepOutcome(symbol string, opts ...func(*SwingOutcome)) SwingOutcome {
	o := SwingOutcome{
		SessionDate: "2026-09-02",
		Symbol:      symbol,
		Flagged:     true,
		Direction:   "up",
		PrevClose:   100,
		ATR:         4,
		EntryPrice:  100,
		EntryBasis:  SwingEntrySessionClose,
		ExitReason:  ExitReasonTarget,
		ReturnPct:   map[string]float64{},
	}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

func withNews(o *SwingOutcome)     { o.HasNews = true }
func withEarnings(o *SwingOutcome) { o.HasNews = true; o.LooksEarning = true }
func withDownGap(o *SwingOutcome)  { o.Direction = "down" }

// writeSwingSession stores scored rows and their forward paths where the sweep
// looks for them.
func writeSwingSession(
	t *testing.T, dataDir, date string, outcomes []SwingOutcome, tracks map[string][]SwingBar,
) {
	t.Helper()

	if err := SaveJSON(SwingFile(dataDir, date), outcomes); err != nil {
		t.Fatal(err)
	}
	if err := SaveJSON(SwingTracksFile(dataDir, date), tracks); err != nil {
		t.Fatal(err)
	}
}

func swingSweepConfig(dataDir string) ScannerConfig {
	cfg := swingTestConfig()
	cfg.DataDir = dataDir
	cfg.SwingStopATR = 1.0 // stop at 96
	return cfg
}

func TestSwingSweepReplaysAgainstTheRealPath(t *testing.T) {
	dir := t.TempDir()
	cfg := swingSweepConfig(dir)

	// A path that climbs to 110 on day 3, so a 1x ATR target (104) fills on
	// day 2 while a 2x target (108) needs day 3.
	track := swingBars(
		[3]float64{102, 99, 101},
		[3]float64{105, 100, 104},
		[3]float64{110, 103, 109},
	)
	writeSwingSession(t, dir, "2026-09-02",
		[]SwingOutcome{swingSweepOutcome("AAA")},
		map[string][]SwingBar{"AAA": track})

	cfg.SwingSweepDays = []int{5}
	cfg.SwingSweepTargetATR = []float64{1.0, 2.0, 4.0}

	r, err := SwingSweep(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Replayable != 1 {
		t.Fatalf("replayable = %d, want 1", r.Replayable)
	}

	byTarget := map[float64]SwingSweepCell{}
	for _, c := range r.Cells {
		byTarget[c.TargetATR] = c
	}

	// 1x ATR = 4% target, reached on day 2.
	if c := byTarget[1.0]; c.HitTarget != 1 || math.Abs(c.MeanNetPct-4) > 1e-9 {
		t.Errorf("1x: target hits=%d net=%v, want 1 and 4%%", c.HitTarget, c.MeanNetPct)
	}
	// 2x ATR = 8% target, reached on day 3.
	if c := byTarget[2.0]; c.HitTarget != 1 || math.Abs(c.MeanNetPct-8) > 1e-9 {
		t.Errorf("2x: target hits=%d net=%v, want 1 and 8%%", c.HitTarget, c.MeanNetPct)
	}
	// 4x ATR = 16% target: never reached, the stop never broken, and the
	// 5-day hold outlives the 3-day path. The trade has no outcome yet, so the
	// cell must be empty rather than pretending it timed out early.
	if c := byTarget[4.0]; c.Trades != 0 {
		t.Errorf("4x: trades=%d, want 0 (unfinished at this holding period)", c.Trades)
	}
}

func TestSwingSweepHoldingPeriodTruncatesThePath(t *testing.T) {
	dir := t.TempDir()
	cfg := swingSweepConfig(dir)

	// The 8% target is only reached on day 3.
	track := swingBars(
		[3]float64{102, 99, 101},
		[3]float64{103, 100, 102},
		[3]float64{110, 103, 109},
	)
	writeSwingSession(t, dir, "2026-09-02",
		[]SwingOutcome{swingSweepOutcome("AAA")},
		map[string][]SwingBar{"AAA": track})

	cfg.SwingSweepDays = []int{2, 5}
	cfg.SwingSweepTargetATR = []float64{2.0}

	r, err := SwingSweep(cfg, "")
	if err != nil {
		t.Fatal(err)
	}

	byDays := map[int]SwingSweepCell{}
	for _, c := range r.Cells {
		byDays[c.HoldDays] = c
	}

	// A 2-day hold stops out on time at day 2's close of 102, missing the run.
	if c := byDays[2]; c.TimeStop != 1 || math.Abs(c.MeanNetPct-2) > 1e-9 {
		t.Errorf("2d hold: time=%d net=%v, want 1 and 2%%", c.TimeStop, c.MeanNetPct)
	}
	// A 5-day hold is still in the position on day 3 and takes the target.
	if c := byDays[5]; c.HitTarget != 1 || math.Abs(c.MeanNetPct-8) > 1e-9 {
		t.Errorf("5d hold: target=%d net=%v, want 1 and 8%%", c.HitTarget, c.MeanNetPct)
	}
	if r.Best == nil || r.Best.HoldDays != 5 {
		t.Errorf("best hold = %+v, want the 5-day cell", r.Best)
	}
}

func TestSwingSweepSkipsRowsItCannotReplay(t *testing.T) {
	dir := t.TempDir()
	cfg := swingSweepConfig(dir)
	cfg.SwingSweepDays = []int{5}
	cfg.SwingSweepTargetATR = []float64{2.0}

	track := swingBars([3]float64{110, 100, 109})

	faded := swingSweepOutcome("FADE")
	faded.Faded = true
	unflagged := swingSweepOutcome("UNFL")
	unflagged.Flagged = false
	noATR := swingSweepOutcome("NOAT")
	noATR.ATR = 0
	noEntry := swingSweepOutcome("NOEN")
	noEntry.EntryPrice = 0
	noTrack := swingSweepOutcome("NOTR")

	writeSwingSession(t, dir, "2026-09-02",
		[]SwingOutcome{swingSweepOutcome("GOOD"), faded, unflagged, noATR, noEntry, noTrack},
		map[string][]SwingBar{
			"GOOD": track, "FADE": track, "UNFL": track, "NOAT": track, "NOEN": track,
			// NOTR deliberately has no path.
		})

	r, err := SwingSweep(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Candidates != 6 {
		t.Errorf("candidates = %d, want 6", r.Candidates)
	}
	if r.Replayable != 1 {
		t.Errorf("replayable = %d, want only GOOD", r.Replayable)
	}
}

// An unfinished trade must not be counted at a holding period the path does
// not cover, or recent sessions would read as early time stops.
func TestSwingSweepExcludesImmatureCells(t *testing.T) {
	dir := t.TempDir()
	cfg := swingSweepConfig(dir)
	cfg.SwingSweepTargetATR = []float64{2.0}
	cfg.SwingSweepDays = []int{2, 10}

	// Only two forward days exist, and neither touches the target or stop.
	track := swingBars(
		[3]float64{101, 99, 100},
		[3]float64{102, 99, 101},
	)
	writeSwingSession(t, dir, "2026-09-02",
		[]SwingOutcome{swingSweepOutcome("AAA")},
		map[string][]SwingBar{"AAA": track})

	r, err := SwingSweep(cfg, "")
	if err != nil {
		t.Fatal(err)
	}

	byDays := map[int]SwingSweepCell{}
	for _, c := range r.Cells {
		byDays[c.HoldDays] = c
	}
	// A 2-day hold completes: the time stop fires on the last available day.
	if c := byDays[2]; c.Trades != 1 || c.TimeStop != 1 {
		t.Errorf("2d cell: trades=%d time=%d, want 1 and 1", c.Trades, c.TimeStop)
	}
	// A 10-day hold cannot be judged from two days of data.
	if c := byDays[10]; c.Trades != 0 {
		t.Errorf("10d cell: trades=%d, want 0 (path too short)", c.Trades)
	}
}

func TestSwingSweepBucketsCarryTheirOwnNames(t *testing.T) {
	dir := t.TempDir()
	cfg := swingSweepConfig(dir)
	cfg.SwingSweepDays = []int{5}
	cfg.SwingSweepTargetATR = []float64{2.0}

	// A news-driven winner and a quiet loser, so the buckets must differ.
	winner := swingSweepOutcome("NEWS", withEarnings)
	loser := swingSweepOutcome("QUIET", withDownGap)

	writeSwingSession(t, dir, "2026-09-02",
		[]SwingOutcome{winner, loser},
		map[string][]SwingBar{
			"NEWS":  swingBars([3]float64{110, 100, 109}), // target
			"QUIET": swingBars([3]float64{100, 95, 96}),   // stop
		})

	r, err := SwingSweep(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	trades, _, _, err := loadSwingTrades(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	buckets := swingBucketSplits(cfg, trades, *r.Best)

	byName := map[string]SwingSweepCell{}
	for _, b := range buckets {
		if b.Bucket == "" {
			t.Error("a bucket cell was returned without its name")
		}
		byName[b.Bucket] = b
	}

	// The split the drift-versus-reversal question turns on.
	if c := byName["news"]; c.Trades != 1 || math.Abs(c.MeanNetPct-8) > 1e-9 {
		t.Errorf("news bucket = %+v, want one trade at 8%%", c)
	}
	if c := byName["no news"]; c.Trades != 1 || math.Abs(c.MeanNetPct-(-4)) > 1e-9 {
		t.Errorf("no-news bucket = %+v, want one trade at -4%%", c)
	}
	if c := byName["earnings"]; c.Trades != 1 {
		t.Errorf("earnings bucket = %+v, want one trade", c)
	}
	if c := byName["down gaps"]; c.Trades != 1 {
		t.Errorf("down-gaps bucket = %+v, want one trade", c)
	}
}

func TestSwingSweepWithNoDataIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	cfg := swingSweepConfig(dir)
	// A session directory with no swing scoring, the state before -swing runs.
	if err := SaveJSON(PhaseFile(dir, "2026-09-02", PhaseOpen), &ScanResult{}); err != nil {
		t.Fatal(err)
	}

	r, err := SwingSweep(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Replayable != 0 || r.Best != nil || len(r.Cells) != 0 {
		t.Errorf("expected an empty sweep, got %+v", r)
	}
}

func TestSwingSweepSpansSessions(t *testing.T) {
	dir := t.TempDir()
	cfg := swingSweepConfig(dir)
	cfg.SwingSweepDays = []int{5}
	cfg.SwingSweepTargetATR = []float64{2.0}

	track := swingBars([3]float64{110, 100, 109})
	for _, date := range []string{"2026-09-01", "2026-09-02"} {
		o := swingSweepOutcome("AAA")
		o.SessionDate = date
		writeSwingSession(t, dir, date, []SwingOutcome{o}, map[string][]SwingBar{"AAA": track})
	}

	all, err := SwingSweep(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if all.Sessions != 2 || all.Replayable != 2 {
		t.Errorf("sessions=%d replayable=%d, want 2 and 2", all.Sessions, all.Replayable)
	}

	one, err := SwingSweep(cfg, "2026-09-02")
	if err != nil {
		t.Fatal(err)
	}
	if one.Sessions != 1 || one.Replayable != 1 {
		t.Errorf("scoped sessions=%d replayable=%d, want 1 and 1", one.Sessions, one.Replayable)
	}
}

func TestSwingGridAxesAreSortedAndDeduplicated(t *testing.T) {
	cells := []SwingSweepCell{
		{HoldDays: 10, TargetATR: 2.0},
		{HoldDays: 2, TargetATR: 1.0},
		{HoldDays: 10, TargetATR: 1.0},
		{HoldDays: 2, TargetATR: 2.0},
	}

	days, targets := swingGridAxes(cells)
	if len(days) != 2 || days[0] != 2 || days[1] != 10 {
		t.Errorf("days = %v, want [2 10]", days)
	}
	if len(targets) != 2 || targets[0] != 1.0 || targets[1] != 2.0 {
		t.Errorf("targets = %v, want [1 2]", targets)
	}
}
