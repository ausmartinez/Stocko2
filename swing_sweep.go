package main

import (
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"time"
)

// SwingSweepCell is one (holding period, target multiple) pair scored across
// every finished trade.
type SwingSweepCell struct {
	HoldDays  int     `json:"hold_days"`
	TargetATR float64 `json:"target_atr"`
	// Bucket names a subset re-scored at the best cell; empty on grid cells.
	Bucket string `json:"bucket,omitempty"`

	Trades    int `json:"trades"`
	HitTarget int `json:"hit_target"`
	HitStop   int `json:"hit_stop"`
	TimeStop  int `json:"time_stop"`
	Ambiguous int `json:"ambiguous"`

	MeanNetPct   float64 `json:"mean_net_pct"`
	MedianNetPct float64 `json:"median_net_pct"`
	NetWinRate   float64 `json:"net_win_rate"`
	MeanHoldDays float64 `json:"mean_hold_days"`
	WorstNetPct  float64 `json:"worst_net_pct"`
}

// SwingSweepResult is the whole grid plus the best cell.
type SwingSweepResult struct {
	GeneratedAt string `json:"generated_at"`
	Sessions    int    `json:"sessions"`
	Candidates  int    `json:"candidates"`
	Replayable  int    `json:"replayable"`

	EntryBasis string  `json:"entry_basis"`
	StopATR    float64 `json:"stop_atr"`

	Cells   []SwingSweepCell `json:"cells"`
	Best    *SwingSweepCell  `json:"best,omitempty"`
	Buckets []SwingSweepCell `json:"buckets,omitempty"`
}

// swingBuckets are the subsets the drift-versus-reversal question turns on.
// Defined once so the reported name can never drift from the rows it counts.
var swingBuckets = []struct {
	name string
	keep func(SwingOutcome) bool
}{
	{"news", func(o SwingOutcome) bool { return o.HasNews }},
	{"no news", func(o SwingOutcome) bool { return !o.HasNews }},
	{"earnings", func(o SwingOutcome) bool { return o.LooksEarning }},
	{"up gaps", func(o SwingOutcome) bool { return o.Direction != "down" }},
	{"down gaps", func(o SwingOutcome) bool { return o.Direction == "down" }},
}

// swingTrade pairs a scored candidate with the forward path it needs to be
// replayed against a different target or holding period.
type swingTrade struct {
	outcome SwingOutcome
	track   []SwingBar
}

func SwingSweepPath(dataDir string) string {
	return filepath.Join(dataDir, "swing-sweep.json")
}

// loadSwingTrades gathers scored swing rows with their forward paths.
//
// Like -sweep it reads the per-session files rather than an append-only log,
// so re-running -swing cannot weight a session twice.
func loadSwingTrades(dataDir, sessionDate string) ([]swingTrade, int, int, error) {
	sessions, err := sessionDirs(dataDir, sessionDate)
	if err != nil {
		return nil, 0, 0, err
	}

	var trades []swingTrade
	found, candidates := 0, 0

	for _, date := range sessions {
		outcomes, err := LoadJSON[[]SwingOutcome](SwingFile(dataDir, date))
		if err != nil {
			return nil, 0, 0, err
		}
		if outcomes == nil {
			continue
		}
		tracks, err := LoadJSON[map[string][]SwingBar](SwingTracksFile(dataDir, date))
		if err != nil {
			return nil, 0, 0, err
		}
		found++

		for _, o := range *outcomes {
			candidates++
			// Only trades we would have taken, and only ones with a path and
			// the ATR the sizing rule needs.
			if !o.Flagged || o.Faded || o.EntryPrice <= 0 || o.ATR <= 0 {
				continue
			}
			if tracks == nil {
				continue
			}
			track := (*tracks)[o.Symbol]
			if len(track) == 0 {
				continue
			}
			trades = append(trades, swingTrade{outcome: o, track: track})
		}
	}
	return trades, found, candidates, nil
}

// SwingSweep replays every finished trade across the grid.
func SwingSweep(cfg ScannerConfig, sessionDate string) (*SwingSweepResult, error) {
	trades, sessions, candidates, err := loadSwingTrades(cfg.DataDir, sessionDate)
	if err != nil {
		return nil, err
	}

	result := &SwingSweepResult{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Sessions:    sessions,
		Candidates:  candidates,
		Replayable:  len(trades),
		EntryBasis:  cfg.SwingEntry,
		StopATR:     cfg.SwingStopATR,
	}
	if len(trades) == 0 {
		return result, nil
	}

	days := append([]int(nil), cfg.SwingSweepDays...)
	sort.Ints(days)
	targets := append([]float64(nil), cfg.SwingSweepTargetATR...)
	sort.Float64s(targets)

	for _, d := range days {
		for _, k := range targets {
			result.Cells = append(result.Cells, swingSweepAt(cfg, trades, d, k))
		}
	}

	for i := range result.Cells {
		if result.Cells[i].Trades == 0 {
			continue
		}
		if result.Best == nil || result.Cells[i].MeanNetPct > result.Best.MeanNetPct {
			result.Best = &result.Cells[i]
		}
	}
	return result, nil
}

// swingSweepAt scores one grid cell by re-running the bracket simulation with
// the cell's holding period and target, against the real recorded path.
func swingSweepAt(cfg ScannerConfig, trades []swingTrade, holdDays int, targetATR float64) SwingSweepCell {
	sized := cfg
	sized.SwingMaxHoldDays = holdDays
	sized.SwingTargetATR = targetATR

	cell := SwingSweepCell{HoldDays: holdDays, TargetATR: targetATR}

	nets := make([]float64, 0, len(trades))
	var totalNet, totalDays, wins float64

	for _, t := range trades {
		// A trade can only be replayed at this holding period if the path
		// actually covers it, or it exited before reaching it.
		replay := SwingOutcome{EntryPrice: t.outcome.EntryPrice}
		c := GapCandidate{
			Symbol:    t.outcome.Symbol,
			Direction: t.outcome.Direction,
			PrevClose: t.outcome.PrevClose,
			ATR:       t.outcome.ATR,
			SpreadPct: t.outcome.SpreadPct,
		}
		sized.simulateSwingBracket(&replay, c, t.track)
		if !replay.Mature() {
			continue
		}

		cell.Trades++
		switch replay.ExitReason {
		case ExitReasonTarget:
			cell.HitTarget++
		case SwingExitStop:
			cell.HitStop++
		case SwingExitTime:
			cell.TimeStop++
		}
		if replay.Ambiguous {
			cell.Ambiguous++
		}

		nets = append(nets, replay.NetStrategyReturnPct)
		totalNet += replay.NetStrategyReturnPct
		totalDays += float64(replay.ExitDay)
		if replay.NetStrategyReturnPct > 0 {
			wins++
		}
	}
	if cell.Trades == 0 {
		return cell
	}

	n := float64(cell.Trades)
	cell.MeanNetPct = totalNet / n
	cell.MeanHoldDays = totalDays / n
	cell.NetWinRate = wins / n * 100

	sort.Float64s(nets)
	cell.MedianNetPct = median(nets)
	cell.WorstNetPct = nets[0]
	return cell
}

// swingBucketSplits re-scores the best cell across each bucket.
func swingBucketSplits(cfg ScannerConfig, trades []swingTrade, best SwingSweepCell) []SwingSweepCell {
	out := make([]SwingSweepCell, 0, len(swingBuckets))
	for _, b := range swingBuckets {
		var subset []swingTrade
		for _, t := range trades {
			if b.keep(t.outcome) {
				subset = append(subset, t)
			}
		}
		if len(subset) == 0 {
			continue
		}
		cell := swingSweepAt(cfg, subset, best.HoldDays, best.TargetATR)
		if cell.Trades == 0 {
			continue
		}
		cell.Bucket = b.name
		out = append(out, cell)
	}
	return out
}

// PrintSwingSweep renders the grid as a matrix of net expectancy.
func PrintSwingSweep(r *SwingSweepResult) {
	fmt.Printf("\nSwing target sweep  sessions=%d  candidates=%d  replayable=%d  entry=%s  stop=%.2f x ATR\n",
		r.Sessions, r.Candidates, r.Replayable, r.EntryBasis, r.StopATR)

	if r.Replayable == 0 {
		fmt.Println("\nno replayable swing trades; run -swing on some scanned sessions first")
		return
	}

	days, targets := swingGridAxes(r.Cells)
	byKey := make(map[string]SwingSweepCell, len(r.Cells))
	for _, c := range r.Cells {
		byKey[swingCellKey(c.HoldDays, c.TargetATR)] = c
	}

	// Mean net % per trade: holding period down, target multiple across.
	fmt.Printf("\nmean net %% per trade\n%8s", "HOLD\\TGT")
	for _, k := range targets {
		fmt.Printf(" %9.2f", k)
	}
	fmt.Println()
	for _, d := range days {
		fmt.Printf("%7dd", d)
		for _, k := range targets {
			c := byKey[swingCellKey(d, k)]
			if c.Trades == 0 {
				fmt.Printf(" %9s", "-")
				continue
			}
			mark := " "
			if r.Best != nil && c.HoldDays == r.Best.HoldDays && c.TargetATR == r.Best.TargetATR {
				mark = "*"
			}
			fmt.Printf(" %8.3f%s", c.MeanNetPct, mark)
		}
		fmt.Println()
	}

	// Fill rate matters as much as the mean: a high expectancy on three trades
	// is not a finding.
	fmt.Printf("\ntarget hit rate %%\n%8s", "HOLD\\TGT")
	for _, k := range targets {
		fmt.Printf(" %9.2f", k)
	}
	fmt.Println()
	for _, d := range days {
		fmt.Printf("%7dd", d)
		for _, k := range targets {
			c := byKey[swingCellKey(d, k)]
			if c.Trades == 0 {
				fmt.Printf(" %9s", "-")
				continue
			}
			fmt.Printf(" %9.1f", float64(c.HitTarget)/float64(c.Trades)*100)
		}
		fmt.Println()
	}

	if r.Best != nil {
		b := *r.Best
		fmt.Printf("\nbest cell: hold %dd, target %.2f x ATR -> %.3f%% net per trade\n",
			b.HoldDays, b.TargetATR, b.MeanNetPct)
		fmt.Printf("  n=%d  target=%d  stop=%d  time=%d  win=%.1f%%  median=%.3f%%  worst=%.2f%%  mean hold=%.1fd\n",
			b.Trades, b.HitTarget, b.HitStop, b.TimeStop, b.NetWinRate,
			b.MedianNetPct, b.WorstNetPct, b.MeanHoldDays)
		if b.Ambiguous > 0 {
			fmt.Printf("  %d/%d exits were same-day target-and-stop, resolved to the stop\n",
				b.Ambiguous, b.Trades)
		}
	}

	if len(r.Buckets) > 0 {
		fmt.Printf("\nAt the best cell, split by what caused the gap and which way it went:\n")
		fmt.Printf("%-10s %6s %7s %7s %7s %9s %9s %8s\n",
			"BUCKET", "N", "TARGET", "STOP", "TIME", "NET%", "MEDIAN%", "WIN%")
		for _, c := range r.Buckets {
			fmt.Printf("%-10s %6d %7d %7d %7d %9.3f %9.3f %8.1f\n",
				c.Bucket, c.Trades, c.HitTarget, c.HitStop, c.TimeStop,
				c.MeanNetPct, c.MedianNetPct, c.NetWinRate)
		}
		fmt.Printf("\nThe documented split is that news gaps drift and quiet gaps revert. If\n" +
			"news and no-news read the same here, either the tag or the sample is too thin.\n")
	}

	if r.Sessions < 20 || r.Replayable < 200 {
		fmt.Printf("\nWARNING: %d sessions / %d trades is far too small to pick a cell from. "+
			"Overlapping multi-day holds correlate, so the effective sample is smaller still.\n",
			r.Sessions, r.Replayable)
	}
	fmt.Println()
}

// swingGridAxes recovers the sorted, de-duplicated axes of the grid.
func swingGridAxes(cells []SwingSweepCell) ([]int, []float64) {
	seenDays := map[int]bool{}
	seenTargets := map[float64]bool{}
	var days []int
	var targets []float64

	for _, c := range cells {
		if !seenDays[c.HoldDays] {
			seenDays[c.HoldDays] = true
			days = append(days, c.HoldDays)
		}
		if !seenTargets[c.TargetATR] {
			seenTargets[c.TargetATR] = true
			targets = append(targets, c.TargetATR)
		}
	}
	sort.Ints(days)
	sort.Float64s(targets)
	return days, targets
}

func swingCellKey(days int, target float64) string {
	return fmt.Sprintf("%d/%.4f", days, target)
}

// runSwingSweep replays the saved swing paths across the grid.
func runSwingSweep(cfg ScannerConfig, date string) error {
	result, err := SwingSweep(cfg, date)
	if err != nil {
		return err
	}

	if result.Best != nil {
		trades, _, _, err := loadSwingTrades(cfg.DataDir, date)
		if err != nil {
			return err
		}
		result.Buckets = swingBucketSplits(cfg, trades, *result.Best)
	}

	if err := SaveJSON(SwingSweepPath(cfg.DataDir), result); err != nil {
		return err
	}

	log.Printf("swing-sweep: replayed %d trades from %d sessions across %d cells",
		result.Replayable, result.Sessions, len(result.Cells))
	PrintSwingSweep(result)
	return nil
}
