package main

import (
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// exportRow is one candidate-session observation: the signal as it was known
// at entry, plus what happened. One row per (session_date, symbol), which is
// the shape a regression wants.
type exportRow struct {
	SessionDate  string
	Symbol       string
	Direction    string
	ScanPhase    string
	DiscoveredAt string
	Source       string // "ledger" (live) or "outcome" (simulated)

	SeenPremarket bool
	Faded         bool
	IsDownGap     bool

	// Method dummies, so a regression can test each measurement separately.
	MethodCount  int
	MethodPct    bool
	MethodATR    bool
	MethodSigma  bool
	MethodsJoins string

	// Signal features.
	PrevClose            float64
	PrevVolume           uint64
	AvgVolume            uint64
	PrevVolumeRatio      float64
	PrevDayReturnPct     float64
	PrevDayRangePct      float64
	RefPrice             float64
	RefSource            string
	GapPct               float64
	AbsGapPct            float64
	GapATR               float64
	GapSigma             float64
	ATR                  float64
	OvernightStdevPct    float64
	Bid                  float64
	Ask                  float64
	SpreadPct            float64
	PremarketVolume      uint64
	PremarketVolumeRatio float64
	PremarketRangePct    float64
	ProjectedGapPct      float64
	GapDeltaPct          float64

	// Outcome.
	EntryPrice           float64
	TargetPrice          float64
	TargetPct            float64
	TargetBasis          string
	ExitPrice            float64
	ExitReason           string
	ExitMinutes          int
	ReturnPct            float64
	PnL                  float64
	MaxFavourablePct     float64
	MaxAdversePct        float64
	AdverseBeforeExitPct float64
	CloseReturnPct       float64
	GapFilled            bool
	HitTarget            bool

	// Friction and the return after paying it. Regress on the net columns.
	CostPct      float64
	NetReturnPct float64
	NetPnL       float64
}

var exportHeader = []string{
	"session_date", "symbol", "direction", "scan_phase", "discovered_at", "source",
	"seen_premarket", "faded", "is_down_gap",
	"method_count", "method_percent", "method_atr", "method_sigma", "methods",
	"prev_close", "prev_volume", "avg_volume", "prev_volume_ratio",
	"prev_day_return_pct", "prev_day_range_pct",
	"ref_price", "ref_source", "gap_pct", "abs_gap_pct", "gap_atr", "gap_sigma",
	"atr", "overnight_stdev_pct", "bid", "ask", "spread_pct",
	"premarket_volume", "premarket_volume_ratio", "premarket_range_pct",
	"projected_gap_pct", "gap_delta_pct",
	"entry_price", "target_price", "target_pct", "target_basis",
	"exit_price", "exit_reason", "exit_minutes",
	"return_pct", "pnl", "max_favourable_pct", "max_adverse_pct",
	"adverse_before_exit_pct", "close_return_pct", "gap_filled", "hit_target",
	"cost_pct", "net_return_pct", "net_pnl",
}

func (r exportRow) record() []string {
	return []string{
		r.SessionDate, r.Symbol, r.Direction, r.ScanPhase, r.DiscoveredAt, r.Source,
		b(r.SeenPremarket), b(r.Faded), b(r.IsDownGap),
		strconv.Itoa(r.MethodCount), b(r.MethodPct), b(r.MethodATR), b(r.MethodSigma), r.MethodsJoins,
		f(r.PrevClose), u(r.PrevVolume), u(r.AvgVolume), f(r.PrevVolumeRatio),
		f(r.PrevDayReturnPct), f(r.PrevDayRangePct),
		f(r.RefPrice), r.RefSource, f(r.GapPct), f(r.AbsGapPct), f(r.GapATR), f(r.GapSigma),
		f(r.ATR), f(r.OvernightStdevPct), f(r.Bid), f(r.Ask), f(r.SpreadPct),
		u(r.PremarketVolume), f(r.PremarketVolumeRatio), f(r.PremarketRangePct),
		f(r.ProjectedGapPct), f(r.GapDeltaPct),
		f(r.EntryPrice), f(r.TargetPrice), f(r.TargetPct), r.TargetBasis,
		f(r.ExitPrice), r.ExitReason, strconv.Itoa(r.ExitMinutes),
		f(r.ReturnPct), f(r.PnL), f(r.MaxFavourablePct), f(r.MaxAdversePct),
		f(r.AdverseBeforeExitPct), f(r.CloseReturnPct), b(r.GapFilled), b(r.HitTarget),
		f(r.CostPct), f(r.NetReturnPct), f(r.NetPnL),
	}
}

// Booleans are written as 1/0 rather than true/false so OLS tooling reads them
// as usable dummies without extra conversion.
func b(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func f(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
func u(v uint64) string  { return strconv.FormatUint(v, 10) }

// ExportCSV walks the data directory and writes a flat candidates table plus a
// long-format sample table. Passing a sessionDate limits it to one day.
func ExportCSV(dataDir, sessionDate, outDir string) (int, int, error) {
	sessions, err := sessionDirs(dataDir, sessionDate)
	if err != nil {
		return 0, 0, err
	}
	if len(sessions) == 0 {
		return 0, 0, fmt.Errorf("no session directories found under %s", dataDir)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return 0, 0, fmt.Errorf("creating %s: %w", outDir, err)
	}

	candidates, err := os.Create(filepath.Join(outDir, "candidates.csv"))
	if err != nil {
		return 0, 0, fmt.Errorf("creating candidates.csv: %w", err)
	}
	defer candidates.Close()

	cw := csv.NewWriter(candidates)
	defer cw.Flush()
	if err := cw.Write(exportHeader); err != nil {
		return 0, 0, err
	}

	samplesFile, err := os.Create(filepath.Join(outDir, "samples.csv"))
	if err != nil {
		return 0, 0, fmt.Errorf("creating samples.csv: %w", err)
	}
	defer samplesFile.Close()

	sw := csv.NewWriter(samplesFile)
	defer sw.Flush()
	if err := sw.Write([]string{
		"session_date", "symbol", "at", "minutes_from_open",
		"price", "high", "low", "unrealized_pct", "status",
	}); err != nil {
		return 0, 0, err
	}

	var rowCount, sampleCount int
	for _, date := range sessions {
		rows, err := buildExportRows(dataDir, date)
		if err != nil {
			return 0, 0, err
		}
		for _, r := range rows {
			if err := cw.Write(r.record()); err != nil {
				return 0, 0, err
			}
			rowCount++
		}

		n, err := writeSamples(sw, dataDir, date)
		if err != nil {
			return 0, 0, err
		}
		sampleCount += n
	}

	cw.Flush()
	sw.Flush()
	if err := cw.Error(); err != nil {
		return 0, 0, err
	}
	return rowCount, sampleCount, sw.Error()
}

// sessionDirs lists YYYY-MM-DD directories under the data directory.
func sessionDirs(dataDir, only string) ([]string, error) {
	if only != "" {
		return []string{only}, nil
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dataDir, err)
	}

	var dates []string
	for _, e := range entries {
		// Session directories are exactly a date; skip export/ and the logs.
		if e.IsDir() && len(e.Name()) == 10 && strings.Count(e.Name(), "-") == 2 {
			dates = append(dates, e.Name())
		}
	}
	sort.Strings(dates)
	return dates, nil
}

// buildExportRows joins the session's scan features to its outcome labels,
// preferring the live ledger over the simulated outcomes when both exist.
func buildExportRows(dataDir, date string) ([]exportRow, error) {
	scan, err := loadAnyScan(dataDir, date)
	if err != nil {
		return nil, err
	}
	if scan == nil {
		log.Printf("export: %s has no scan file, skipping", date)
		return nil, nil
	}

	rows := make(map[string]*exportRow, len(scan.Candidates))
	order := make([]string, 0, len(scan.Candidates))
	for _, c := range scan.Candidates {
		r := rowFromCandidate(date, scan.Phase, c)
		rows[c.Symbol] = &r
		order = append(order, c.Symbol)
	}

	ledger, err := LoadJSON[Ledger](LedgerPath(dataDir, date))
	if err != nil {
		return nil, err
	}
	if ledger != nil {
		for _, p := range ledger.Positions {
			r, ok := rows[p.Symbol]
			if !ok {
				continue
			}
			applyLedger(r, p)
		}
	}

	outcomes, err := LoadJSON[[]Outcome](PhaseFile(dataDir, date, PhaseOutcome))
	if err != nil {
		return nil, err
	}
	if outcomes != nil {
		for _, o := range *outcomes {
			r, ok := rows[o.Symbol]
			if !ok {
				continue
			}
			applyOutcome(r, o)
		}
	}

	out := make([]exportRow, 0, len(order))
	for _, sym := range order {
		out = append(out, *rows[sym])
	}
	return out, nil
}

// loadAnyScan prefers the open run, which carries lineage and faded rows.
func loadAnyScan(dataDir, date string) (*ScanResult, error) {
	for _, phase := range []string{PhaseOpen, PhasePremarket} {
		scan, err := LoadScanResult(PhaseFile(dataDir, date, phase))
		if err != nil {
			return nil, err
		}
		if scan != nil {
			return scan, nil
		}
	}
	return nil, nil
}

func rowFromCandidate(date, phase string, c GapCandidate) exportRow {
	r := exportRow{
		SessionDate:          date,
		Symbol:               c.Symbol,
		Direction:            c.Direction,
		ScanPhase:            phase,
		DiscoveredAt:         c.DiscoveredAt,
		SeenPremarket:        c.SeenPremarket,
		Faded:                c.Faded,
		IsDownGap:            c.Direction == "down",
		MethodCount:          len(c.Methods),
		MethodsJoins:         strings.Join(c.Methods, "|"),
		PrevClose:            c.PrevClose,
		PrevVolume:           c.PrevVolume,
		AvgVolume:            c.AvgVolume,
		PrevVolumeRatio:      c.PrevVolumeRatio,
		PrevDayReturnPct:     c.PrevDayReturnPct,
		PrevDayRangePct:      c.PrevDayRangePct,
		RefPrice:             c.RefPrice,
		RefSource:            c.RefSource,
		GapPct:               c.GapPct,
		AbsGapPct:            abs(c.GapPct),
		GapATR:               c.GapATR,
		GapSigma:             c.GapSigma,
		ATR:                  c.ATR,
		OvernightStdevPct:    c.OvernightStdevPct,
		Bid:                  c.Bid,
		Ask:                  c.Ask,
		SpreadPct:            c.SpreadPct,
		PremarketVolume:      c.PremarketVolume,
		PremarketVolumeRatio: c.PremarketVolumeRatio,
		PremarketRangePct:    c.PremarketRangePct,
		ProjectedGapPct:      c.ProjectedGapPct,
		GapDeltaPct:          c.GapDeltaPct,
	}
	for _, m := range c.Methods {
		switch m {
		case MethodPercent:
			r.MethodPct = true
		case MethodATR:
			r.MethodATR = true
		case MethodSigma:
			r.MethodSigma = true
		}
	}
	return r
}

func applyLedger(r *exportRow, p PaperPosition) {
	// A position that never closed is not a completed trade. Letting it win
	// over the simulated outcome would write an empty exit reason and a zero
	// return, silently masking what the symbol actually did — which is what a
	// missed flatten tick or a crashed session would otherwise produce.
	if p.Status != PositionClosed {
		return
	}

	r.Source = "ledger"
	r.EntryPrice = p.EntryPrice
	r.TargetPrice = p.TargetPrice
	r.TargetPct = p.TargetPct
	r.TargetBasis = p.TargetBasis
	r.ExitPrice = p.ExitPrice
	r.ExitReason = p.ExitReason
	r.ExitMinutes = p.ClosedMinutes
	r.ReturnPct = p.ReturnPct
	r.PnL = p.PnL
	r.MaxFavourablePct = p.MaxFavourablePct
	r.MaxAdversePct = p.MaxAdversePct
	r.AdverseBeforeExitPct = p.AdverseBeforeExitPct
	r.HitTarget = p.ExitReason == ExitReasonTarget
	r.CostPct = p.CostPct
	r.NetReturnPct = p.NetReturnPct
	r.NetPnL = p.NetPnL
}

// applyOutcome only fills gaps the ledger did not, so a live result is never
// overwritten by the simulated one.
func applyOutcome(r *exportRow, o Outcome) {
	r.CloseReturnPct = o.CloseReturnPct
	r.GapFilled = o.GapFilled
	if r.Source == "ledger" {
		return
	}
	r.Source = "outcome"
	r.EntryPrice = o.EntryPrice
	r.TargetPrice = o.ExitTargetPrice
	r.TargetPct = o.ExitTargetPct
	r.TargetBasis = o.ExitTargetBasis
	r.ExitPrice = o.ExitPrice
	r.ExitReason = o.ExitReason
	r.ExitMinutes = o.ExitMinutes
	r.ReturnPct = o.StrategyReturnPct
	r.MaxFavourablePct = o.MaxFavourablePct
	r.MaxAdversePct = o.MaxAdversePct
	r.AdverseBeforeExitPct = o.AdverseBeforeExitPct
	r.HitTarget = o.ExitReason == ExitReasonTarget
	r.CostPct = o.CostPct
	r.NetReturnPct = o.NetStrategyReturnPct
	// The simulated path has no share count, so notional P&L stays a ledger
	// concept and is left at zero here.
}

// writeSamples emits the long-format panel: one row per position per tick from
// the live tracker, falling back to the simulated 5-minute tracks.
func writeSamples(w *csv.Writer, dataDir, date string) (int, error) {
	n := 0

	live, err := LoadJSON[[]PositionSample](SessionFile(dataDir, date, "samples"))
	if err != nil {
		return 0, err
	}
	if live != nil {
		for _, s := range *live {
			if err := w.Write([]string{
				s.SessionDate, s.Symbol, s.At, strconv.Itoa(s.MinutesFromOpen),
				f(s.Price), f(s.High), f(s.Low), f(s.UnrealizedPct), s.Status,
			}); err != nil {
				return n, err
			}
			n++
		}
		return n, nil
	}

	tracks, err := LoadJSON[map[string][]IntradayBar](SessionFile(dataDir, date, "tracks"))
	if err != nil {
		return 0, err
	}
	if tracks == nil {
		return 0, nil
	}

	symbols := make([]string, 0, len(*tracks))
	for sym := range *tracks {
		symbols = append(symbols, sym)
	}
	sort.Strings(symbols)

	for _, sym := range symbols {
		for _, bar := range (*tracks)[sym] {
			if err := w.Write([]string{
				date, sym, bar.Time, strconv.Itoa(bar.MinutesFromOpen),
				f(bar.Close), f(bar.High), f(bar.Low), "", "track",
			}); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
