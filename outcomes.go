package main

import (
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
)

// PhaseOutcome is the post-close run that scores what the candidates did.
const PhaseOutcome = "outcome"

// HorizonClose is the pseudo-horizon for "held to the closing bell".
const HorizonClose = "close"

// Exit reference points for the simulated strategy.
const (
	// ExitRefEntry sells on any profit against the fill price.
	ExitRefEntry = "entry"
	// ExitRefPrevClose sells only once the gap has filled and gone green.
	ExitRefPrevClose = "prev_close"
)

// Why a simulated position was closed.
const (
	ExitReasonTarget  = "target"
	ExitReasonClose   = "session_close"
	ExitReasonNoEntry = "no_entry"
)

// IntradayBar is one interval of a candidate's session.
//
// MinutesFromOpen is measured to the END of the bar, so the first 5-minute bar
// of the session is 5. Alpaca timestamps bars at their start.
type IntradayBar struct {
	Time            string  `json:"t"`
	MinutesFromOpen int     `json:"m"`
	Open            float64 `json:"o"`
	High            float64 `json:"h"`
	Low             float64 `json:"l"`
	Close           float64 `json:"c"`
	Volume          uint64  `json:"v"`
	VWAP            float64 `json:"vw"`
}

// Outcome is what happened to one candidate after the bell. It repeats the
// headline gap features so each row is a self-contained training example.
type Outcome struct {
	SessionDate  string `json:"session_date"`
	Symbol       string `json:"symbol"`
	ScanPhase    string `json:"scan_phase"`
	DiscoveredAt string `json:"discovered_at"`
	Faded        bool   `json:"faded"`
	Flagged      bool   `json:"flagged"`

	Direction string   `json:"direction"`
	Methods   []string `json:"methods"`
	PrevClose float64  `json:"prev_close"`
	GapPct    float64  `json:"gap_pct"`
	GapATR    float64  `json:"gap_atr"`
	GapSigma  float64  `json:"gap_sigma"`

	SessionOpenPrice float64 `json:"session_open_price"`
	EntryPrice       float64 `json:"entry_price"`
	EntryMinutes     int     `json:"entry_minutes_from_open"`
	EntryTime        string  `json:"entry_time"`

	// ReturnPct is the long return from EntryPrice at each horizon, keyed by
	// minutes-from-open as a string plus "close".
	ReturnPct map[string]float64 `json:"return_pct"`

	MaxFavourablePct float64 `json:"max_favourable_pct"`
	MaxAdversePct    float64 `json:"max_adverse_pct"`
	CloseReturnPct   float64 `json:"close_return_pct"`

	// GapFilled records whether price traded back through the previous close.
	GapFilled      bool `json:"gap_filled"`
	GapFillMinutes int  `json:"gap_fill_minutes,omitempty"`

	// Simulated "sell the moment it turns positive, else hold to the close".
	ExitReference     string  `json:"exit_reference"`
	ExitTargetPrice   float64 `json:"exit_target_price"`
	ExitReason        string  `json:"exit_reason"`
	ExitMinutes       int     `json:"exit_minutes_from_open"`
	ExitPrice         float64 `json:"exit_price"`
	StrategyReturnPct float64 `json:"strategy_return_pct"`
	// AdverseBeforeExitPct is the worst drawdown suffered before the exit
	// fired. This is what MaxAdversePct alone cannot tell you: whether the
	// small profit arrived before or after a large loss.
	AdverseBeforeExitPct float64 `json:"adverse_before_exit_pct"`

	Bars int `json:"bars"`
}

// StrategySummary aggregates the simulated strategy across a basket.
type StrategySummary struct {
	ExitReference string  `json:"exit_reference"`
	ExitTargetPct float64 `json:"exit_target_pct"`

	Count       int `json:"count"`
	HitTarget   int `json:"hit_target"`
	HeldToClose int `json:"held_to_close"`

	MeanReturnPct   float64 `json:"mean_return_pct"`
	MedianReturnPct float64 `json:"median_return_pct"`
	WinRate         float64 `json:"win_rate"`
	BestPct         float64 `json:"best_pct"`
	WorstPct        float64 `json:"worst_pct"`

	MeanExitMinutes           float64 `json:"mean_exit_minutes"`
	MeanAdverseBeforeExitPct  float64 `json:"mean_adverse_before_exit_pct"`
	WorstAdverseBeforeExitPct float64 `json:"worst_adverse_before_exit_pct"`
}

// PortfolioStats summarises one horizon across a basket of candidates.
type PortfolioStats struct {
	Count     int     `json:"count"`
	MeanPct   float64 `json:"mean_pct"`
	MedianPct float64 `json:"median_pct"`
	StdevPct  float64 `json:"stdev_pct"`
	WinRate   float64 `json:"win_rate"`
	BestPct   float64 `json:"best_pct"`
	WorstPct  float64 `json:"worst_pct"`
}

// PortfolioResult answers "if I had bought all of them equally, what happened".
// Computed over flagged candidates only, since a faded row is not one you would
// have bought.
type PortfolioResult struct {
	SessionDate  string   `json:"session_date"`
	ScanPhase    string   `json:"scan_phase"`
	EntryMinutes int      `json:"entry_minutes_from_open"`
	Horizons     []string `json:"horizons"`
	Flagged      int      `json:"flagged"`
	Faded        int      `json:"faded"`

	All  map[string]PortfolioStats `json:"all"`
	Up   map[string]PortfolioStats `json:"up"`
	Down map[string]PortfolioStats `json:"down"`

	// Simulated strategy, split so "only buy down gappers" is testable.
	StrategyAll  StrategySummary `json:"strategy_all"`
	StrategyUp   StrategySummary `json:"strategy_up"`
	StrategyDown StrategySummary `json:"strategy_down"`
}

// CollectOutcomes pulls the intraday track for every row in a scan and scores
// it. Faded rows are included deliberately: they are the negative examples.
func (s *Scanner) CollectOutcomes(
	session TradingSession, scan *ScanResult,
) ([]Outcome, map[string][]IntradayBar, error) {
	if len(scan.Candidates) == 0 {
		return nil, nil, nil
	}

	symbols := make([]string, 0, len(scan.Candidates))
	for _, c := range scan.Candidates {
		symbols = append(symbols, c.Symbol)
	}

	tracks, err := s.intradayTracks(symbols, session)
	if err != nil {
		return nil, nil, err
	}

	outcomes := make([]Outcome, 0, len(scan.Candidates))
	for _, c := range scan.Candidates {
		track := tracks[c.Symbol]
		if len(track) == 0 {
			log.Printf("outcomes: %s has no intraday bars for %s, skipping", c.Symbol, session.Date)
			continue
		}
		outcomes = append(outcomes, s.scoreCandidate(session, scan, c, track))
	}

	sort.Slice(outcomes, func(i, j int) bool {
		return outcomes[i].CloseReturnPct > outcomes[j].CloseReturnPct
	})
	return outcomes, tracks, nil
}

// intradayTracks fetches regular-hours bars at the configured interval.
func (s *Scanner) intradayTracks(symbols []string, session TradingSession) (map[string][]IntradayBar, error) {
	interval := s.cfg.OutcomeIntervalMinutes
	tracks := make(map[string][]IntradayBar, len(symbols))
	var mu sync.Mutex

	err := s.forEachBatch(symbols, s.cfg.HistoryBatchSize, func(batch []string) error {
		bars, err := s.data.GetMultiBars(batch, marketdata.GetBarsRequest{
			TimeFrame:  marketdata.NewTimeFrame(interval, marketdata.Min),
			Adjustment: marketdata.AdjustmentAll,
			Start:      session.Open,
			End:        session.Close,
			Feed:       s.cfg.Feed,
		})
		if err != nil {
			return fmt.Errorf("getting %d-minute bars: %w", interval, err)
		}

		local := make(map[string][]IntradayBar, len(bars))
		for symbol, series := range bars {
			track := make([]IntradayBar, 0, len(series))
			for _, b := range series {
				// Regular hours only; End is inclusive so drop the closing bar.
				if b.Timestamp.Before(session.Open) || !b.Timestamp.Before(session.Close) {
					continue
				}
				end := b.Timestamp.Add(time.Duration(interval) * time.Minute)
				track = append(track, IntradayBar{
					Time:            b.Timestamp.In(s.loc).Format(time.RFC3339),
					MinutesFromOpen: int(end.Sub(session.Open).Minutes()),
					Open:            b.Open,
					High:            b.High,
					Low:             b.Low,
					Close:           b.Close,
					Volume:          b.Volume,
					VWAP:            b.VWAP,
				})
			}
			if len(track) > 0 {
				local[symbol] = track
			}
		}

		mu.Lock()
		for symbol, track := range local {
			tracks[symbol] = track
		}
		mu.Unlock()
		return nil
	})

	return tracks, err
}

// scoreCandidate turns one intraday track into a labelled outcome.
func (s *Scanner) scoreCandidate(
	session TradingSession, scan *ScanResult, c GapCandidate, track []IntradayBar,
) Outcome {
	o := Outcome{
		SessionDate:      session.Date,
		Symbol:           c.Symbol,
		ScanPhase:        scan.Phase,
		DiscoveredAt:     c.DiscoveredAt,
		Faded:            c.Faded,
		Flagged:          len(c.Methods) > 0,
		Direction:        c.Direction,
		Methods:          c.Methods,
		PrevClose:        c.PrevClose,
		GapPct:           c.GapPct,
		GapATR:           c.GapATR,
		GapSigma:         c.GapSigma,
		SessionOpenPrice: track[0].Open,
		EntryMinutes:     s.cfg.EntryMinutesFromOpen,
		ReturnPct:        map[string]float64{},
		Bars:             len(track),
	}
	if o.Methods == nil {
		o.Methods = []string{}
	}

	entryIdx, ok := barIndexAt(track, s.cfg.EntryMinutesFromOpen)
	if !ok {
		// Not enough of the session recorded to establish an entry.
		return o
	}
	o.EntryPrice = track[entryIdx].Close
	o.EntryTime = track[entryIdx].Time
	if o.EntryPrice <= 0 {
		return o
	}

	for _, h := range s.cfg.OutcomeHorizonsMinutes {
		if i, ok := barIndexAt(track, h); ok {
			o.ReturnPct[fmt.Sprintf("%dm", h)] = pctChange(o.EntryPrice, track[i].Close)
		}
	}
	last := track[len(track)-1]
	o.CloseReturnPct = pctChange(o.EntryPrice, last.Close)
	o.ReturnPct[HorizonClose] = o.CloseReturnPct

	// Excursions are measured from the entry bar onward, since anything before
	// it is not something a buyer at that entry would have experienced.
	high, low := track[entryIdx].High, track[entryIdx].Low
	for _, b := range track[entryIdx:] {
		high = math.Max(high, b.High)
		low = math.Min(low, b.Low)
	}
	o.MaxFavourablePct = pctChange(o.EntryPrice, high)
	o.MaxAdversePct = pctChange(o.EntryPrice, low)

	o.GapFilled, o.GapFillMinutes = gapFill(track, c)
	s.simulateExit(&o, c, track, entryIdx)
	return o
}

// simulateExit walks the track from the bar after entry and closes the position
// the first time price clears the target, otherwise at the closing bell.
//
// A touch of the target counts as a fill, which is the usual limit-order
// assumption. Within the bar that triggers, the low is charged against the
// position before the exit, since bar data cannot say which came first.
func (s *Scanner) simulateExit(o *Outcome, c GapCandidate, track []IntradayBar, entryIdx int) {
	reference := o.EntryPrice
	if s.cfg.ExitReference == ExitRefPrevClose && c.PrevClose > 0 {
		reference = c.PrevClose
	}
	o.ExitReference = s.cfg.ExitReference
	o.ExitTargetPrice = reference * (1 + s.cfg.ExitTargetPct/100)

	worst := 0.0
	for _, b := range track[entryIdx+1:] {
		worst = math.Min(worst, pctChange(o.EntryPrice, b.Low))
		if b.High >= o.ExitTargetPrice {
			o.ExitReason = ExitReasonTarget
			o.ExitMinutes = b.MinutesFromOpen
			o.ExitPrice = o.ExitTargetPrice
			o.StrategyReturnPct = pctChange(o.EntryPrice, o.ExitPrice)
			o.AdverseBeforeExitPct = worst
			return
		}
	}

	last := track[len(track)-1]
	o.ExitReason = ExitReasonClose
	o.ExitMinutes = last.MinutesFromOpen
	o.ExitPrice = last.Close
	o.StrategyReturnPct = pctChange(o.EntryPrice, last.Close)
	o.AdverseBeforeExitPct = worst
}

// summariseStrategy aggregates simulated positions.
func summariseStrategy(outcomes []Outcome, cfg ScannerConfig) StrategySummary {
	sum := StrategySummary{
		ExitReference: cfg.ExitReference,
		ExitTargetPct: cfg.ExitTargetPct,
	}

	returns := make([]float64, 0, len(outcomes))
	var totalMinutes, totalAdverse float64
	for _, o := range outcomes {
		if o.ExitReason == "" || o.ExitReason == ExitReasonNoEntry {
			continue
		}
		sum.Count++
		switch o.ExitReason {
		case ExitReasonTarget:
			sum.HitTarget++
		case ExitReasonClose:
			sum.HeldToClose++
		}
		returns = append(returns, o.StrategyReturnPct)
		totalMinutes += float64(o.ExitMinutes)
		totalAdverse += o.AdverseBeforeExitPct
		sum.WorstAdverseBeforeExitPct = math.Min(sum.WorstAdverseBeforeExitPct, o.AdverseBeforeExitPct)
	}
	if sum.Count == 0 {
		return sum
	}

	sorted := append([]float64(nil), returns...)
	sort.Float64s(sorted)

	var total, wins float64
	for _, r := range returns {
		total += r
		if r > 0 {
			wins++
		}
	}
	sum.MeanReturnPct = total / float64(sum.Count)
	sum.MedianReturnPct = sorted[len(sorted)/2]
	if len(sorted)%2 == 0 {
		sum.MedianReturnPct = (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
	}
	sum.WinRate = wins / float64(sum.Count) * 100
	sum.BestPct = sorted[len(sorted)-1]
	sum.WorstPct = sorted[0]
	sum.MeanExitMinutes = totalMinutes / float64(sum.Count)
	sum.MeanAdverseBeforeExitPct = totalAdverse / float64(sum.Count)
	return sum
}

// barIndexAt returns the first bar ending at or after minutes from the open.
func barIndexAt(track []IntradayBar, minutes int) (int, bool) {
	for i, b := range track {
		if b.MinutesFromOpen >= minutes {
			return i, true
		}
	}
	return 0, false
}

func pctChange(from, to float64) float64 {
	if from == 0 {
		return 0
	}
	return (to - from) / from * 100
}

// gapFill reports whether price traded back through the previous close, which
// is the classic test of whether a gap held.
func gapFill(track []IntradayBar, c GapCandidate) (bool, int) {
	if c.PrevClose <= 0 {
		return false, 0
	}
	for _, b := range track {
		if c.Direction == "up" && b.Low <= c.PrevClose {
			return true, b.MinutesFromOpen
		}
		if c.Direction == "down" && b.High >= c.PrevClose {
			return true, b.MinutesFromOpen
		}
	}
	return false, 0
}

// BuildPortfolio aggregates outcomes into the equal-weight result of buying
// every flagged candidate long.
func BuildPortfolio(session TradingSession, scan *ScanResult, outcomes []Outcome, cfg ScannerConfig) PortfolioResult {
	// A horizon at or before the entry is a return from entry to entry, so it
	// is always zero and would read as a real result in the summary.
	horizons := make([]string, 0, len(cfg.OutcomeHorizonsMinutes)+1)
	for _, h := range cfg.OutcomeHorizonsMinutes {
		if h > cfg.EntryMinutesFromOpen {
			horizons = append(horizons, fmt.Sprintf("%dm", h))
		}
	}
	horizons = append(horizons, HorizonClose)

	p := PortfolioResult{
		SessionDate:  session.Date,
		ScanPhase:    scan.Phase,
		EntryMinutes: cfg.EntryMinutesFromOpen,
		Horizons:     horizons,
		All:          map[string]PortfolioStats{},
		Up:           map[string]PortfolioStats{},
		Down:         map[string]PortfolioStats{},
	}

	var all, up, down []Outcome
	for _, o := range outcomes {
		if o.Faded || !o.Flagged {
			p.Faded++
			continue
		}
		p.Flagged++
		all = append(all, o)
		if o.Direction == "down" {
			down = append(down, o)
		} else {
			up = append(up, o)
		}
	}

	for _, h := range horizons {
		p.All[h] = statsFor(all, h)
		p.Up[h] = statsFor(up, h)
		p.Down[h] = statsFor(down, h)
	}

	p.StrategyAll = summariseStrategy(all, cfg)
	p.StrategyUp = summariseStrategy(up, cfg)
	p.StrategyDown = summariseStrategy(down, cfg)
	return p
}

func statsFor(outcomes []Outcome, horizon string) PortfolioStats {
	returns := make([]float64, 0, len(outcomes))
	for _, o := range outcomes {
		if r, ok := o.ReturnPct[horizon]; ok {
			returns = append(returns, r)
		}
	}
	if len(returns) == 0 {
		return PortfolioStats{}
	}

	sorted := append([]float64(nil), returns...)
	sort.Float64s(sorted)

	var sum, wins float64
	for _, r := range returns {
		sum += r
		if r > 0 {
			wins++
		}
	}
	mean := sum / float64(len(returns))

	stdev := 0.0
	if len(returns) > 1 {
		var sumSq float64
		for _, r := range returns {
			d := r - mean
			sumSq += d * d
		}
		stdev = math.Sqrt(sumSq / float64(len(returns)-1))
	}

	median := sorted[len(sorted)/2]
	if len(sorted)%2 == 0 {
		median = (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
	}

	return PortfolioStats{
		Count:     len(returns),
		MeanPct:   mean,
		MedianPct: median,
		StdevPct:  stdev,
		WinRate:   wins / float64(len(returns)) * 100,
		BestPct:   sorted[len(sorted)-1],
		WorstPct:  sorted[0],
	}
}

// PrintOutcomes writes the per-candidate and portfolio tables to stdout.
func PrintOutcomes(p PortfolioResult, outcomes []Outcome) {
	fmt.Printf("\nOutcomes  session=%s  scan_phase=%s  entry=+%dmin from open\n",
		p.SessionDate, p.ScanPhase, p.EntryMinutes)
	fmt.Printf("flagged=%d  faded/excluded=%d\n\n", p.Flagged, p.Faded)

	if len(outcomes) == 0 {
		fmt.Println("no intraday data collected")
		return
	}

	fmt.Printf("%-8s %-5s %8s %9s %8s %8s %8s %13s %8s %9s\n",
		"SYMBOL", "DIR", "GAP%", "ENTRY", "CLOSE%", "MFE%", "MAE%", "EXIT", "P&L%", "DD BEFORE")
	for _, o := range outcomes {
		exit := o.ExitReason
		if o.ExitReason == ExitReasonTarget {
			exit = fmt.Sprintf("target+%dm", o.ExitMinutes)
		}
		fmt.Printf("%-8s %-5s %7.2f%% %9.2f %8.2f %8.2f %8.2f %13s %8.3f %9.2f\n",
			o.Symbol, o.Direction, o.GapPct, o.EntryPrice, o.CloseReturnPct,
			o.MaxFavourablePct, o.MaxAdversePct, exit, o.StrategyReturnPct, o.AdverseBeforeExitPct)
	}

	printStrategy(p)

	fmt.Printf("\nHold-to-horizon return from buying every flagged candidate long:\n")
	fmt.Printf("%-8s %6s %9s %9s %9s %9s %9s\n",
		"HORIZON", "N", "MEAN%", "MEDIAN%", "WIN%", "BEST%", "WORST%")
	for _, h := range p.Horizons {
		st := p.All[h]
		if st.Count == 0 {
			continue
		}
		fmt.Printf("%-8s %6d %9.3f %9.3f %9.1f %9.2f %9.2f\n",
			h, st.Count, st.MeanPct, st.MedianPct, st.WinRate, st.BestPct, st.WorstPct)
	}

	fmt.Printf("\nSplit by gap direction (mean %% / win %%):\n")
	fmt.Printf("%-8s %18s %18s\n", "HORIZON", "UP GAPS", "DOWN GAPS")
	for _, h := range p.Horizons {
		u, d := p.Up[h], p.Down[h]
		if u.Count == 0 && d.Count == 0 {
			// Horizon the session never reached.
			continue
		}
		fmt.Printf("%-8s %8.3f (%3.0f%%) n=%-3d %8.3f (%3.0f%%) n=%-3d\n",
			h, u.MeanPct, u.WinRate, u.Count, d.MeanPct, d.WinRate, d.Count)
	}
	fmt.Println()
}

// printStrategy reports the simulated "sell on any profit, else hold to the
// close" rule, split by gap direction.
func printStrategy(p PortfolioResult) {
	s := p.StrategyAll
	fmt.Printf("\nSimulated strategy: sell at %s +%.2f%%, else hold to the close\n",
		s.ExitReference, s.ExitTargetPct)
	fmt.Printf("%-10s %5s %7s %7s %8s %8s %9s %9s %9s\n",
		"BASKET", "N", "TARGET", "CLOSE", "MEAN%", "WIN%", "BEST%", "WORST%", "WORST DD")

	for _, row := range []struct {
		name string
		sum  StrategySummary
	}{
		{"all", p.StrategyAll},
		{"up gaps", p.StrategyUp},
		{"down gaps", p.StrategyDown},
	} {
		if row.sum.Count == 0 {
			continue
		}
		fmt.Printf("%-10s %5d %7d %7d %8.3f %8.1f %9.2f %9.2f %9.2f\n",
			row.name, row.sum.Count, row.sum.HitTarget, row.sum.HeldToClose,
			row.sum.MeanReturnPct, row.sum.WinRate, row.sum.BestPct,
			row.sum.WorstPct, row.sum.WorstAdverseBeforeExitPct)
	}
	fmt.Printf("\nmean time in trade: %.0f min   mean drawdown before exit: %.2f%%\n\n",
		s.MeanExitMinutes, s.MeanAdverseBeforeExitPct)
}
