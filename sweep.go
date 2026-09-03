package main

import (
	"fmt"
	"log"
	"math"
	"path/filepath"
	"sort"
	"time"
)

// SweepRow scores one candidate ATR multiple across every recorded trade.
//
// Nothing is re-simulated: a trade's MaxFavourablePct already says whether any
// given target would have been reached, so the whole sweep is a replay over
// two numbers per trade.
type SweepRow struct {
	ATRMult float64 `json:"atr_mult"`

	Trades      int     `json:"trades"`
	Filled      int     `json:"filled"`
	FillRate    float64 `json:"fill_rate"`
	CostFloored int     `json:"cost_floored"`

	MeanTargetPct float64 `json:"mean_target_pct"`

	// MeanNetPct is the expectancy: what one trade is worth at this multiple,
	// net of friction, averaging fills and misses together. This is the column
	// the sweet spot maximises.
	MeanNetPct    float64 `json:"mean_net_pct"`
	MedianNetPct  float64 `json:"median_net_pct"`
	NetWinRate    float64 `json:"net_win_rate"`
	MeanFilledNet float64 `json:"mean_filled_net_pct"`
	MeanMissedNet float64 `json:"mean_missed_net_pct"`
	WorstNetPct   float64 `json:"worst_net_pct"`

	// MeanGivebackPct is how much a miss showed and then handed back, the cost
	// of aiming too high. MeanOvershootPct is how far fills ran past the
	// target, the cost of aiming too low.
	MeanGivebackPct  float64 `json:"mean_giveback_pct"`
	MeanOvershootPct float64 `json:"mean_overshoot_pct"`
}

// SweepResult is the full sweep, saved so a run can be revisited.
type SweepResult struct {
	GeneratedAt   string  `json:"generated_at"`
	Sessions      int     `json:"sessions"`
	Trades        int     `json:"trades"`
	SkippedNoATR  int     `json:"skipped_no_atr"`
	SkippedNoFill int     `json:"skipped_no_entry"`
	SkippedUnheld int     `json:"skipped_not_flagged"`
	ExitReference string  `json:"exit_reference"`
	MinCostMult   float64 `json:"min_target_cost_mult"`

	Rows []SweepRow `json:"rows"`
	// Best is the highest-expectancy row, or nil when there were no trades.
	Best *SweepRow `json:"best,omitempty"`
}

// defaultSweepMultiples spans a tight scalp to a full ATR of follow-through.
func defaultSweepMultiples() []float64 {
	return []float64{0.05, 0.10, 0.15, 0.20, 0.25, 0.30, 0.40, 0.50, 0.75, 1.00, 1.50, 2.00}
}

// loadSweepOutcomes gathers scored trades from the per-session outcome files.
//
// It deliberately reads those rather than outcomes.jsonl: the log is appended
// on every run, so re-scoring a session duplicates its rows there, which would
// silently weight that day twice.
func loadSweepOutcomes(dataDir, sessionDate string) ([]Outcome, int, error) {
	sessions, err := sessionDirs(dataDir, sessionDate)
	if err != nil {
		return nil, 0, err
	}

	var all []Outcome
	found := 0
	for _, date := range sessions {
		outcomes, err := LoadJSON[[]Outcome](PhaseFile(dataDir, date, PhaseOutcome))
		if err != nil {
			return nil, 0, err
		}
		if outcomes == nil {
			continue
		}
		found++
		all = append(all, *outcomes...)
	}
	return all, found, nil
}

// Sweep replays every recorded trade against each candidate ATR multiple.
func Sweep(cfg ScannerConfig, sessionDate string, multiples []float64) (*SweepResult, error) {
	outcomes, sessions, err := loadSweepOutcomes(cfg.DataDir, sessionDate)
	if err != nil {
		return nil, err
	}

	result := &SweepResult{
		GeneratedAt:   time.Now().Format(time.RFC3339),
		Sessions:      sessions,
		ExitReference: cfg.ExitReference,
		MinCostMult:   cfg.MinTargetCostMult,
	}

	// Only trades that would actually have been taken, and that have the ATR
	// the sizing rule needs.
	trades := make([]Outcome, 0, len(outcomes))
	for _, o := range outcomes {
		switch {
		case !o.Flagged || o.Faded:
			result.SkippedUnheld++
		case o.EntryPrice <= 0 || o.ExitReason == "" || o.ExitReason == ExitReasonNoEntry:
			result.SkippedNoFill++
		case o.ATR <= 0:
			// Without ATR the target falls back to a flat percent, so the
			// multiple under test would have no effect on this row.
			result.SkippedNoATR++
		default:
			trades = append(trades, o)
		}
	}
	result.Trades = len(trades)
	if len(trades) == 0 {
		return result, nil
	}

	for _, k := range multiples {
		result.Rows = append(result.Rows, sweepAt(cfg, trades, k))
	}

	for i := range result.Rows {
		if result.Best == nil || result.Rows[i].MeanNetPct > result.Best.MeanNetPct {
			result.Best = &result.Rows[i]
		}
	}
	return result, nil
}

// sweepAt scores one multiple. Targets come from resolveTarget so the sweep
// cannot drift from how production sizes them, cost floor included.
func sweepAt(cfg ScannerConfig, trades []Outcome, k float64) SweepRow {
	sized := cfg
	sized.ExitTargetMode = ExitTargetModeATR
	sized.ExitTargetATR = k

	row := SweepRow{ATRMult: k, Trades: len(trades)}

	nets := make([]float64, 0, len(trades))
	var totalTarget, totalNet, netWins float64
	var filledNet, missedNet, giveback, overshoot float64

	for _, o := range trades {
		c := GapCandidate{
			Symbol:    o.Symbol,
			Direction: o.Direction,
			PrevClose: o.PrevClose,
			ATR:       o.ATR,
			SpreadPct: o.SpreadPct,
		}
		targetPrice, targetPct, basis := sized.resolveTarget(c, o.EntryPrice)
		if basis == TargetBasisCostFloor {
			row.CostFloored++
		}
		totalTarget += targetPct

		var net float64
		if o.MaxFavourablePct >= targetPct {
			// The target was reached, so a resting limit would have filled.
			row.Filled++
			cost := cfg.Costs.RoundTripCostPct(
				o.EntryPrice, targetPrice, o.SpreadPct, marketCrossings(ExitReasonTarget))
			net = targetPct - cost
			filledNet += net
			overshoot += o.MaxFavourablePct - targetPct
		} else {
			// Never reached, so the position was flattened at the close and
			// handed back whatever it had shown.
			exitPrice := o.EntryPrice * (1 + o.CloseReturnPct/100)
			cost := cfg.Costs.RoundTripCostPct(
				o.EntryPrice, exitPrice, o.SpreadPct, marketCrossings(ExitReasonClose))
			net = o.CloseReturnPct - cost
			missedNet += net
			giveback += o.MaxFavourablePct - o.CloseReturnPct
		}

		nets = append(nets, net)
		totalNet += net
		if net > 0 {
			netWins++
		}
	}

	n := float64(len(trades))
	row.FillRate = float64(row.Filled) / n * 100
	row.MeanTargetPct = totalTarget / n
	row.MeanNetPct = totalNet / n
	row.NetWinRate = netWins / n * 100

	if row.Filled > 0 {
		row.MeanFilledNet = filledNet / float64(row.Filled)
		row.MeanOvershootPct = overshoot / float64(row.Filled)
	}
	if missed := len(trades) - row.Filled; missed > 0 {
		row.MeanMissedNet = missedNet / float64(missed)
		row.MeanGivebackPct = giveback / float64(missed)
	}

	sorted := append([]float64(nil), nets...)
	sort.Float64s(sorted)
	row.MedianNetPct = median(sorted)
	row.WorstNetPct = sorted[0]
	return row
}

// SweepPath is where a sweep is saved.
func SweepPath(dataDir string) string {
	return filepath.Join(dataDir, "sweep.json")
}

// PrintSweep writes the sweep table to stdout.
func PrintSweep(r *SweepResult) {
	fmt.Printf("\nTarget sweep  sessions=%d  trades=%d  exit_reference=%s  floor=%.1fx cost\n",
		r.Sessions, r.Trades, r.ExitReference, r.MinCostMult)
	if r.SkippedUnheld+r.SkippedNoFill+r.SkippedNoATR > 0 {
		fmt.Printf("skipped: %d not flagged/faded, %d without an entry, %d without ATR\n",
			r.SkippedUnheld, r.SkippedNoFill, r.SkippedNoATR)
	}
	fmt.Println()

	if r.Trades == 0 {
		fmt.Println("no scored trades found; run -outcomes on some sessions first")
		return
	}

	fmt.Printf("%6s %7s %9s %9s %10s %10s %10s %10s %8s\n",
		"xATR", "FILL%", "TGT%", "NET%", "FILL NET%", "MISS NET%", "GIVEBACK%",
		"OVERSHOOT%", "FLOORED")
	for i := range r.Rows {
		row := r.Rows[i]
		marker := " "
		if r.Best != nil && row.ATRMult == r.Best.ATRMult {
			marker = "*"
		}
		fmt.Printf("%6.2f %7.1f %9.3f %9.4f %10.3f %10.3f %10.3f %10.3f %8d%s\n",
			row.ATRMult, row.FillRate, row.MeanTargetPct, row.MeanNetPct,
			row.MeanFilledNet, row.MeanMissedNet, row.MeanGivebackPct,
			row.MeanOvershootPct, row.CostFloored, marker)
	}

	if r.Best != nil {
		fmt.Printf("\nbest expectancy: %.2f x ATR -> %.4f%% net per trade "+
			"(mean target %.3f%%, %.1f%% fill rate, %.1f%% net win rate)\n",
			r.Best.ATRMult, r.Best.MeanNetPct, r.Best.MeanTargetPct,
			r.Best.FillRate, r.Best.NetWinRate)
	}

	// A sweep is only as trustworthy as the sample and the bar resolution it
	// was measured on, so say so rather than let the table imply precision.
	fmt.Println()
	if r.Sessions < 10 || r.Trades < 100 {
		fmt.Printf("WARNING: %d sessions / %d trades is too small to optimise on; "+
			"gap behaviour is regime-dependent\n", r.Sessions, r.Trades)
	}
	fmt.Println("note: fills are decided by MaxFavourablePct, measured on " +
		"outcome-interval bar highs, so a target touched inside a bar counts as filled")
	fmt.Println()
}

// sweepMultiples falls back to the built-in grid when none are configured.
func sweepMultiples(cfg ScannerConfig) []float64 {
	if len(cfg.SweepATRMultiples) == 0 {
		return defaultSweepMultiples()
	}
	out := append([]float64(nil), cfg.SweepATRMultiples...)
	sort.Float64s(out)
	// A non-positive multiple would collapse the target onto the cost floor.
	kept := out[:0]
	for _, k := range out {
		if k > 0 {
			kept = append(kept, k)
		}
	}
	if len(kept) == 0 {
		return defaultSweepMultiples()
	}
	return kept
}

// sweepIsInformative reports whether the sweep found any variation at all. A
// flat expectancy across every multiple means the cost floor swallowed the
// range, which is a finding rather than a result.
func sweepIsInformative(r *SweepResult) bool {
	if len(r.Rows) < 2 {
		return false
	}
	lo, hi := r.Rows[0].MeanNetPct, r.Rows[0].MeanNetPct
	for _, row := range r.Rows {
		lo = math.Min(lo, row.MeanNetPct)
		hi = math.Max(hi, row.MeanNetPct)
	}
	return hi-lo > 1e-9
}

// runSweep replays the collected outcomes against a range of ATR multiples.
func runSweep(cfg ScannerConfig, date string) error {
	result, err := Sweep(cfg, date, sweepMultiples(cfg))
	if err != nil {
		return err
	}
	if err := SaveJSON(SweepPath(cfg.DataDir), result); err != nil {
		return err
	}

	log.Printf("sweep: replayed %d trades from %d sessions across %d multiples",
		result.Trades, result.Sessions, len(result.Rows))
	if result.Trades > 0 && !sweepIsInformative(result) {
		log.Printf("sweep: expectancy is flat across every multiple, " +
			"so the cost floor is setting every target")
	}
	PrintSweep(result)
	return nil
}
