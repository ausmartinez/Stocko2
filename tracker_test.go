package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
)

func testSession(t *testing.T, nowET string) TradingSession {
	t.Helper()

	loc, err := easternLocation()
	if err != nil {
		t.Fatal(err)
	}
	at := func(hhmm string) time.Time {
		ts, err := time.ParseInLocation("2006-01-02 15:04", "2026-09-02 "+hhmm, loc)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}

	return TradingSession{
		Date:           "2026-09-02",
		Open:           at("09:30"),
		Close:          at("16:00"),
		PremarketStart: at("04:00"),
		Now:            at(nowET),
		BeforeOpen:     false,
	}
}

func testScanner(t *testing.T, cfg ScannerConfig) *Scanner {
	t.Helper()

	loc, err := easternLocation()
	if err != nil {
		t.Fatal(err)
	}
	return &Scanner{cfg: cfg, loc: loc}
}

// minuteBar builds a bar at an ET wall-clock time on the test session date.
func minuteBar(t *testing.T, hhmm string, high, low, close float64) marketdata.Bar {
	t.Helper()

	loc, err := easternLocation()
	if err != nil {
		t.Fatal(err)
	}
	ts, err := time.ParseInLocation("2006-01-02 15:04", "2026-09-02 "+hhmm, loc)
	if err != nil {
		t.Fatal(err)
	}
	return marketdata.Bar{Timestamp: ts, High: high, Low: low, Close: close, Open: close}
}

func openPosition(t *testing.T, entry, target float64, lastSampled string) PaperPosition {
	t.Helper()

	loc, err := easternLocation()
	if err != nil {
		t.Fatal(err)
	}
	ts, err := time.ParseInLocation("2006-01-02 15:04", "2026-09-02 "+lastSampled, loc)
	if err != nil {
		t.Fatal(err)
	}
	return PaperPosition{
		Symbol:        "AAA",
		EntryPrice:    entry,
		TargetPrice:   target,
		Shares:        1000 / entry,
		Notional:      1000,
		Status:        PositionOpen,
		LastPrice:     entry,
		LastSampledAt: ts.Format(time.RFC3339),
	}
}

func TestAdvanceClosesOnTargetTouch(t *testing.T) {
	s := testScanner(t, DefaultScannerConfig())
	session := testSession(t, "09:45")
	p := openPosition(t, 100, 100.1, "09:35")

	// Dips first, then trades through the target.
	sample := s.advance(session, &p, []marketdata.Bar{
		minuteBar(t, "09:36", 100.0, 99.0, 99.5),
		minuteBar(t, "09:37", 100.2, 99.6, 100.15),
		minuteBar(t, "09:38", 101.0, 100.0, 100.8),
	}, false)

	if p.Status != PositionClosed {
		t.Fatalf("status = %q, want closed", p.Status)
	}
	if p.ExitReason != ExitReasonTarget {
		t.Errorf("exit_reason = %q, want target", p.ExitReason)
	}
	if math.Abs(p.ExitPrice-100.1) > 1e-9 {
		t.Errorf("exit_price = %v, want the target 100.1", p.ExitPrice)
	}
	// Filled on the 09:37 bar, which ends 8 minutes after the 09:30 open.
	if p.ClosedMinutes != 8 {
		t.Errorf("closed_minutes = %d, want 8", p.ClosedMinutes)
	}
	if math.Abs(p.ReturnPct-0.1) > 1e-9 {
		t.Errorf("return_pct = %v, want 0.1", p.ReturnPct)
	}
	// The -1% dip before the fill must be recorded.
	if math.Abs(p.AdverseBeforeExitPct-(-1.0)) > 1e-9 {
		t.Errorf("adverse_before_exit_pct = %v, want -1.0", p.AdverseBeforeExitPct)
	}
	if p.PnL <= 0 {
		t.Errorf("pnl = %v, want positive", p.PnL)
	}
	if sample.Status != PositionClosed {
		t.Errorf("sample status = %q, want closed", sample.Status)
	}
	if sample.High != 101.0 || sample.Low != 99.0 {
		t.Errorf("sample high/low = %v/%v, want 101/99", sample.High, sample.Low)
	}
}

func TestAdvanceStaysOpenBelowTarget(t *testing.T) {
	s := testScanner(t, DefaultScannerConfig())
	session := testSession(t, "09:45")
	p := openPosition(t, 100, 105, "09:35")

	s.advance(session, &p, []marketdata.Bar{
		minuteBar(t, "09:36", 101.0, 98.0, 99.0),
	}, false)

	if p.Status != PositionOpen {
		t.Fatalf("status = %q, want open", p.Status)
	}
	if p.ExitReason != "" {
		t.Errorf("exit_reason = %q, want empty while open", p.ExitReason)
	}
	if math.Abs(p.MaxFavourablePct-1.0) > 1e-9 {
		t.Errorf("MFE = %v, want 1.0", p.MaxFavourablePct)
	}
	if math.Abs(p.MaxAdversePct-(-2.0)) > 1e-9 {
		t.Errorf("MAE = %v, want -2.0", p.MaxAdversePct)
	}
	if p.Samples != 1 {
		t.Errorf("samples = %d, want 1", p.Samples)
	}
}

func TestAdvanceFlattensNearClose(t *testing.T) {
	s := testScanner(t, DefaultScannerConfig())
	session := testSession(t, "15:56")
	p := openPosition(t, 100, 105, "15:50")

	s.advance(session, &p, []marketdata.Bar{
		minuteBar(t, "15:51", 98.0, 97.0, 97.5),
	}, true)

	if p.Status != PositionClosed {
		t.Fatalf("status = %q, want closed", p.Status)
	}
	if p.ExitReason != ExitReasonClose {
		t.Errorf("exit_reason = %q, want session_close", p.ExitReason)
	}
	if math.Abs(p.ExitPrice-97.5) > 1e-9 {
		t.Errorf("exit_price = %v, want the last price 97.5", p.ExitPrice)
	}
	if math.Abs(p.ReturnPct-(-2.5)) > 1e-9 {
		t.Errorf("return_pct = %v, want -2.5", p.ReturnPct)
	}
	if p.PnL >= 0 {
		t.Errorf("pnl = %v, want negative", p.PnL)
	}
}

// Bars at or after the closing bell must not be counted.
func TestAdvanceIgnoresBarsPastTheClose(t *testing.T) {
	s := testScanner(t, DefaultScannerConfig())
	session := testSession(t, "16:05")
	p := openPosition(t, 100, 100.1, "15:58")

	s.advance(session, &p, []marketdata.Bar{
		minuteBar(t, "16:01", 105.0, 104.0, 104.5),
	}, false)

	if p.Status != PositionOpen {
		t.Errorf("status = %q; an after-hours bar closed the position", p.Status)
	}
	if p.MaxFavourablePct != 0 {
		t.Errorf("MFE = %v; after-hours data leaked in", p.MaxFavourablePct)
	}
}

// Already-closed positions must not be reopened or re-priced by later ticks.
func TestAdvanceLeavesClosedPositionsAlone(t *testing.T) {
	s := testScanner(t, DefaultScannerConfig())
	session := testSession(t, "10:00")

	p := openPosition(t, 100, 100.1, "09:35")
	p.Status = PositionClosed
	p.ExitReason = ExitReasonTarget
	p.ExitPrice = 100.1
	p.ClosedMinutes = 8

	s.advance(session, &p, []marketdata.Bar{
		minuteBar(t, "09:50", 120.0, 90.0, 110.0),
	}, true)

	if p.ExitReason != ExitReasonTarget || p.ClosedMinutes != 8 {
		t.Errorf("closed position was rewritten: reason=%q minutes=%d", p.ExitReason, p.ClosedMinutes)
	}
	if math.Abs(p.ExitPrice-100.1) > 1e-9 {
		t.Errorf("exit_price = %v, want 100.1 unchanged", p.ExitPrice)
	}
}

func TestLedgerOpenCount(t *testing.T) {
	l := &Ledger{Positions: []PaperPosition{
		{Status: PositionOpen}, {Status: PositionClosed}, {Status: PositionOpen},
	}}
	if got := l.Open(); got != 2 {
		t.Errorf("Open() = %d, want 2", got)
	}
}

func TestLedgerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := LedgerPath(dir, "2026-09-02")

	want := &Ledger{
		SessionDate:   "2026-09-02",
		ExitReference: ExitRefEntry,
		ExitTargetPct: 0.1,
		Ticks:         3,
		Positions: []PaperPosition{
			{Symbol: "AAA", Status: PositionOpen, EntryPrice: 10, TargetPrice: 10.01},
			{Symbol: "BBB", Status: PositionClosed, ExitReason: ExitReasonTarget, ReturnPct: 0.1},
		},
	}
	if err := SaveJSON(path, want); err != nil {
		t.Fatal(err)
	}

	got, err := LoadJSON[Ledger](path)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got.Positions) != 2 {
		t.Fatalf("got %+v, want 2 positions", got)
	}
	if got.Ticks != 3 || got.Positions[1].ExitReason != ExitReasonTarget {
		t.Errorf("ledger did not survive the round trip: %+v", got)
	}

	// A missing ledger is the first-tick case, not an error.
	missing, err := LoadJSON[Ledger](LedgerPath(dir, "2026-01-01"))
	if err != nil {
		t.Fatalf("err = %v, want nil for a missing ledger", err)
	}
	if missing != nil {
		t.Errorf("got %+v, want nil", missing)
	}
}

// The CSV header and each record must stay the same width, or every column
// silently shifts.
// An unflattened position must not override the simulated outcome, or a missed
// 15:55 tick silently zeroes the exit and return for every open row.
func TestApplyLedgerIgnoresPositionsThatNeverClosed(t *testing.T) {
	simulated := Outcome{
		Symbol:            "AAA",
		EntryPrice:        100,
		ExitPrice:         103,
		ExitReason:        ExitReasonClose,
		StrategyReturnPct: 3.0,
		CloseReturnPct:    3.0,
	}

	t.Run("open position is skipped", func(t *testing.T) {
		r := exportRow{Symbol: "AAA"}
		applyLedger(&r, PaperPosition{Symbol: "AAA", Status: PositionOpen, EntryPrice: 100})
		applyOutcome(&r, simulated)

		if r.Source != "outcome" {
			t.Errorf("source = %q, want the simulated outcome to win", r.Source)
		}
		if math.Abs(r.ReturnPct-3.0) > 1e-9 {
			t.Errorf("return = %v, want the simulated 3.0", r.ReturnPct)
		}
		if r.ExitReason != ExitReasonClose {
			t.Errorf("exit_reason = %q, want it filled from the outcome", r.ExitReason)
		}
	})

	t.Run("closed position still wins", func(t *testing.T) {
		r := exportRow{Symbol: "AAA"}
		applyLedger(&r, PaperPosition{
			Symbol: "AAA", Status: PositionClosed, EntryPrice: 100,
			ExitPrice: 101, ExitReason: ExitReasonTarget, ReturnPct: 1.0, PnL: 10,
		})
		applyOutcome(&r, simulated)

		if r.Source != "ledger" {
			t.Errorf("source = %q, want the live ledger to win", r.Source)
		}
		if math.Abs(r.ReturnPct-1.0) > 1e-9 {
			t.Errorf("return = %v, want the live 1.0", r.ReturnPct)
		}
		if !r.HitTarget {
			t.Error("hit_target should be set from the ledger")
		}
	})
}

func TestExportHeaderMatchesRecord(t *testing.T) {
	got := len(exportRow{}.record())
	if got != len(exportHeader) {
		t.Fatalf("record has %d fields but header has %d", got, len(exportHeader))
	}
}

func TestBuildExportRowsJoinsScanAndLedger(t *testing.T) {
	dir := t.TempDir()
	date := "2026-09-02"

	scan := &ScanResult{
		SessionDate: date,
		Phase:       PhaseOpen,
		Candidates: []GapCandidate{
			{
				Symbol: "WIN", Direction: "down", DiscoveredAt: PhasePremarket, SeenPremarket: true,
				PrevClose: 100, GapPct: -5, GapATR: 1.2, GapSigma: -2.1, AvgVolume: 500000,
				PrevVolumeRatio: 1.5, Methods: []string{MethodPercent, MethodATR},
			},
			{
				Symbol: "FADE", Direction: "up", DiscoveredAt: PhasePremarket, SeenPremarket: true,
				PrevClose: 50, GapPct: 0.4, Faded: true, Methods: []string{},
			},
		},
	}
	if err := SaveScanResult(PhaseFile(dir, date, PhaseOpen), scan); err != nil {
		t.Fatal(err)
	}

	ledger := &Ledger{SessionDate: date, Positions: []PaperPosition{{
		Symbol: "WIN", Status: PositionClosed, EntryPrice: 95, TargetPrice: 95.095,
		ExitPrice: 95.095, ExitReason: ExitReasonTarget, ClosedMinutes: 20,
		ReturnPct: 0.1, PnL: 1.0, MaxAdversePct: -2.5, AdverseBeforeExitPct: -2.5,
	}}}
	if err := SaveJSON(LedgerPath(dir, date), ledger); err != nil {
		t.Fatal(err)
	}

	rows, err := buildExportRows(dir, date)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (faded row retained as a negative)", len(rows))
	}

	win := rows[0]
	if win.Symbol != "WIN" {
		t.Fatalf("first row is %q, want WIN", win.Symbol)
	}
	if win.Source != "ledger" {
		t.Errorf("source = %q, want ledger", win.Source)
	}
	if !win.IsDownGap {
		t.Error("is_down_gap = false for a down gapper")
	}
	if win.MethodCount != 2 || !win.MethodPct || !win.MethodATR || win.MethodSigma {
		t.Errorf("method dummies wrong: count=%d pct=%t atr=%t sigma=%t",
			win.MethodCount, win.MethodPct, win.MethodATR, win.MethodSigma)
	}
	if win.AbsGapPct != 5 {
		t.Errorf("abs_gap_pct = %v, want 5", win.AbsGapPct)
	}
	if !win.HitTarget || win.ExitMinutes != 20 {
		t.Errorf("outcome not joined: hit=%t minutes=%d", win.HitTarget, win.ExitMinutes)
	}

	fade := rows[1]
	if !fade.Faded || fade.MethodCount != 0 {
		t.Errorf("faded row wrong: faded=%t methods=%d", fade.Faded, fade.MethodCount)
	}
	if fade.Source != "" {
		t.Errorf("faded row source = %q, want empty (never bought)", fade.Source)
	}
}

func TestExportCSVWritesBothFiles(t *testing.T) {
	dir := t.TempDir()
	date := "2026-09-02"

	scan := &ScanResult{SessionDate: date, Phase: PhaseOpen, Candidates: []GapCandidate{
		{Symbol: "AAA", Direction: "up", GapPct: 5, Methods: []string{MethodPercent}},
	}}
	if err := SaveScanResult(PhaseFile(dir, date, PhaseOpen), scan); err != nil {
		t.Fatal(err)
	}
	if err := SaveJSON(SessionFile(dir, date, "samples"), []PositionSample{
		{SessionDate: date, Symbol: "AAA", At: "2026-09-02T09:40:00-04:00", MinutesFromOpen: 10, Price: 101},
		{SessionDate: date, Symbol: "AAA", At: "2026-09-02T09:45:00-04:00", MinutesFromOpen: 15, Price: 102},
	}); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "export")
	rows, samples, _, err := ExportCSV(dir, "", out, []int{1, 5})
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("candidate rows = %d, want 1", rows)
	}
	if samples != 2 {
		t.Errorf("sample rows = %d, want 2", samples)
	}

	data, err := os.ReadFile(filepath.Join(out, "candidates.csv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(string(data))
	if len(lines) != 2 {
		t.Fatalf("candidates.csv has %d lines, want header + 1 row", len(lines))
	}
	if countFields(lines[0]) != countFields(lines[1]) {
		t.Errorf("header has %d fields but the row has %d",
			countFields(lines[0]), countFields(lines[1]))
	}
}

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func countFields(line string) int {
	return len(strings.Split(line, ","))
}

// swingHeader and swingRecord are parallel slices; a mismatch silently shifts
// every column in the modelling table.
func TestSwingHeaderMatchesRecord(t *testing.T) {
	for _, horizons := range [][]int{nil, {1}, {1, 2, 3, 5, 10, 20}} {
		h := len(swingHeader(horizons))
		r := len(swingRecord(SwingOutcome{ReturnPct: map[string]float64{}}, GapCandidate{}, horizons))
		if h != r {
			t.Errorf("horizons=%v: header has %d columns, record has %d", horizons, h, r)
		}
	}
}

// An unreached horizon must be blank, not zero — a missing label read as a flat
// return would bias every model fitted on it.
func TestSwingRecordLeavesUnreachedHorizonsBlank(t *testing.T) {
	horizons := []int{1, 5, 20}
	o := SwingOutcome{ReturnPct: map[string]float64{"1d": 2.5, "5d": -1.0}}

	rec := swingRecord(o, GapCandidate{}, horizons)
	got := rec[len(rec)-3:]

	if got[0] != "2.5" || got[1] != "-1" {
		t.Errorf("reached horizons = %v, want 2.5 and -1", got[:2])
	}
	if got[2] != "" {
		t.Errorf("unreached 20d horizon = %q, want empty", got[2])
	}
}

// atr_pct is the derived column the analysis turns on, so it must be present
// and correct rather than left to the caller.
func TestSwingRecordDerivesATRPct(t *testing.T) {
	cols := swingHeader(nil)
	idx := -1
	for i, c := range cols {
		if c == "atr_pct" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("swing.csv has no atr_pct column")
	}

	rec := swingRecord(SwingOutcome{
		ATR: 4, EntryPrice: 50, ReturnPct: map[string]float64{},
	}, GapCandidate{}, nil)
	if rec[idx] != "8" {
		t.Errorf("atr_pct = %q, want 8 (4/50)", rec[idx])
	}

	// A zero entry price must not divide by zero.
	rec = swingRecord(SwingOutcome{ATR: 4, ReturnPct: map[string]float64{}}, GapCandidate{}, nil)
	if rec[idx] != "0" {
		t.Errorf("atr_pct with no entry = %q, want 0", rec[idx])
	}
}

// The join is the whole point: swing.json drops most of the scan's features.
func TestWriteSwingCSVJoinsScanFeatures(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "export")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := SaveScanResult(PhaseFile(dir, "2026-09-02", PhaseOpen), &ScanResult{
		SessionDate: "2026-09-02", Phase: PhaseOpen,
		Candidates: []GapCandidate{{
			Symbol: "AAA", Methods: []string{MethodPercent},
			SeenPremarket: true, DiscoveredAt: PhasePremarket,
			GapDeltaPct: -1.75, PrevVolumeRatio: 3.2, Bid: 9.99, Ask: 10.01,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveJSON(SwingFile(dir, "2026-09-02"), []SwingOutcome{{
		SessionDate: "2026-09-02", Symbol: "AAA", Flagged: true,
		ATR: 1, EntryPrice: 20, ExitReason: ExitReasonTarget,
		ReturnPct: map[string]float64{"1d": 3.0},
	}}); err != nil {
		t.Fatal(err)
	}

	n, err := writeSwingCSV(dir, "", out, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("wrote %d rows, want 1", n)
	}

	raw, err := os.ReadFile(filepath.Join(out, "swing.csv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(string(raw))
	head, row := strings.Split(lines[0], ","), strings.Split(lines[1], ",")

	for col, want := range map[string]string{
		"gap_delta_pct":     "-1.75", // scan-only, absent from swing.json
		"prev_volume_ratio": "3.2",   // scan-only
		"seen_premarket":    "1",     // scan-only
		"bid":               "9.99",  // scan-only
		"atr_pct":           "5",     // derived: 1/20
		"ret_1d":            "3",     // from the horizon map
	} {
		idx := -1
		for i, h := range head {
			if h == col {
				idx = i
			}
		}
		if idx < 0 {
			t.Errorf("no %s column", col)
			continue
		}
		if row[idx] != want {
			t.Errorf("%s = %q, want %q", col, row[idx], want)
		}
	}
}
