package main

import (
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
)

// PhaseSwing is the multi-day scoring pass.
const PhaseSwing = "swing"

// Where a swing position is entered. Both are readable from a daily bar, so
// the whole swing path needs no intraday or real-time data.
const (
	SwingEntrySessionClose = "session_close"
	SwingEntryNextOpen     = "next_open"
)

// How a swing position left the book. Target reuses ExitReasonTarget so the
// cost model's crossing rules stay consistent across both strategies.
const (
	SwingExitStop = "stop"
	SwingExitTime = "time_stop"
	// SwingExitOpen means the session is too recent to have run its course, so
	// the row is not yet a finished trade and must be excluded from any
	// expectancy figure.
	SwingExitOpen = "open"
)

// SwingBar is one forward daily bar, kept so a sweep can replay a different
// target or stop against the real path rather than guessing from summaries.
type SwingBar struct {
	Date   string  `json:"d"`
	Day    int     `json:"n"` // trading days after the entry
	Open   float64 `json:"o"`
	High   float64 `json:"h"`
	Low    float64 `json:"l"`
	Close  float64 `json:"c"`
	Volume uint64  `json:"v"`
}

// SwingOutcome is what one gap candidate did over the following days. It
// repeats the signal so each row stands alone as a training example.
type SwingOutcome struct {
	SessionDate string `json:"session_date"`
	Symbol      string `json:"symbol"`
	ScanPhase   string `json:"scan_phase"`
	Flagged     bool   `json:"flagged"`
	Faded       bool   `json:"faded"`

	Direction string   `json:"direction"`
	Methods   []string `json:"methods"`
	PrevClose float64  `json:"prev_close"`
	GapPct    float64  `json:"gap_pct"`
	GapATR    float64  `json:"gap_atr"`
	GapSigma  float64  `json:"gap_sigma"`
	ATR       float64  `json:"atr"`
	SpreadPct float64  `json:"spread_pct"`

	// Why the stock gapped. The documented discriminator between drift and
	// reversal, and the reason a news tag is worth more than another indicator.
	HasNews      bool   `json:"has_news"`
	NewsCount    int    `json:"news_count"`
	LooksEarning bool   `json:"looks_earnings"`
	TopHeadline  string `json:"top_headline,omitempty"`

	EntryBasis string  `json:"entry_basis"`
	EntryDate  string  `json:"entry_date"`
	EntryPrice float64 `json:"entry_price"`

	// ReturnPct is the return from the entry at each day horizon, keyed "1d",
	// "2d" and so on. A horizon the data has not reached yet is absent.
	ReturnPct map[string]float64 `json:"return_pct"`

	MaxFavourablePct float64 `json:"max_favourable_pct"`
	MaxAdversePct    float64 `json:"max_adverse_pct"`

	// Simulated bracket: take profit, stop loss, or a time stop.
	TargetPrice float64 `json:"target_price"`
	TargetPct   float64 `json:"target_pct"`
	TargetBasis string  `json:"target_basis"`
	StopPrice   float64 `json:"stop_price"`
	StopPct     float64 `json:"stop_pct"`

	ExitReason string  `json:"exit_reason"`
	ExitDay    int     `json:"exit_day"`
	ExitPrice  float64 `json:"exit_price"`

	// Ambiguous marks a day whose range covered both the target and the stop.
	// Daily bars cannot order the two, so it is resolved to the stop.
	Ambiguous bool `json:"ambiguous"`

	StrategyReturnPct    float64 `json:"strategy_return_pct"`
	CostPct              float64 `json:"cost_pct"`
	NetStrategyReturnPct float64 `json:"net_strategy_return_pct"`

	ForwardBars int `json:"forward_bars"`
}

// Mature reports whether the trade reached a definite exit. Immature rows must
// be excluded from expectancy, or recent sessions read as early time stops.
func (o SwingOutcome) Mature() bool {
	return o.ExitReason != "" && o.ExitReason != SwingExitOpen
}

// swingCrossings counts the spread crossings a swing round trip pays. A target
// fills a resting limit; a stop and a time stop are both market orders.
func swingCrossings(exitReason string) float64 {
	if exitReason == ExitReasonTarget {
		return 1
	}
	return 2
}

// SwingFile is a session's swing scoring; SwingTracksFile is the forward path.
func SwingFile(dataDir, sessionDate string) string {
	return SessionFile(dataDir, sessionDate, PhaseSwing)
}

func SwingTracksFile(dataDir, sessionDate string) string {
	return SessionFile(dataDir, sessionDate, PhaseSwing+"-tracks")
}

// earningsPattern matches the formulaic wording of an earnings headline. A
// heuristic: Alpaca carries no earnings calendar, so this stands in for one.
var earningsPattern = regexp.MustCompile(
	`(?i)\b(earnings|eps|q[1-4]|quarterly|full[- ]year|guidance|revenue)\b`)

func looksLikeEarnings(headline string) bool {
	return earningsPattern.MatchString(headline)
}

// CollectSwingOutcomes scores every candidate in a scan over the days that
// followed, and returns the forward paths alongside so a sweep can replay them.
func (s *Scanner) CollectSwingOutcomes(
	session TradingSession, scan *ScanResult,
) ([]SwingOutcome, map[string][]SwingBar, error) {
	if len(scan.Candidates) == 0 {
		return nil, nil, nil
	}

	symbols := make([]string, 0, len(scan.Candidates))
	for _, c := range scan.Candidates {
		symbols = append(symbols, c.Symbol)
	}

	daily, err := s.forwardDailyBars(symbols, session)
	if err != nil {
		return nil, nil, err
	}

	outcomes := make([]SwingOutcome, 0, len(scan.Candidates))
	tracks := make(map[string][]SwingBar, len(scan.Candidates))
	for _, c := range scan.Candidates {
		o, track := s.scoreSwing(session, scan, c, daily[c.Symbol])
		if len(track) == 0 && o.EntryPrice <= 0 {
			// No usable daily data at all for this symbol.
			continue
		}
		outcomes = append(outcomes, o)
		tracks[c.Symbol] = track
	}

	if err := s.attachNews(outcomes, session); err != nil {
		// News is an enrichment, not a prerequisite, and the endpoint may not
		// be on the account's plan. Losing it must not lose the scoring.
		log.Printf("swing: news lookup failed, continuing untagged: %v", err)
	}

	sort.Slice(outcomes, func(i, j int) bool {
		return outcomes[i].NetStrategyReturnPct > outcomes[j].NetStrategyReturnPct
	})
	return outcomes, tracks, nil
}

// forwardDailyBars pulls daily bars from the gap session forward far enough to
// cover the longest horizon, allowing for weekends and holidays.
func (s *Scanner) forwardDailyBars(
	symbols []string, session TradingSession,
) (map[string][]marketdata.Bar, error) {
	span := s.cfg.SwingMaxHoldDays
	for _, h := range s.cfg.SwingHorizonDays {
		span = max(span, h)
	}
	// Trading days to calendar days, plus slack for holiday clusters.
	end := session.Open.AddDate(0, 0, span*2+10)

	// Stay clear of the last 15 minutes, which SIP does not serve on the free
	// plan. Only completed sessions matter here, so this costs nothing.
	if cutoff := time.Now().Add(-16 * time.Minute); end.After(cutoff) {
		end = cutoff
	}
	if !end.After(session.Open) {
		return map[string][]marketdata.Bar{}, nil
	}

	out := make(map[string][]marketdata.Bar, len(symbols))
	var mu sync.Mutex

	err := s.forEachBatch(symbols, s.cfg.HistoryBatchSize, func(batch []string) error {
		bars, err := s.data.GetMultiBars(batch, marketdata.GetBarsRequest{
			TimeFrame: marketdata.OneDay,
			// A split inside the holding window would otherwise read as a
			// catastrophic loss.
			Adjustment: marketdata.AdjustmentAll,
			Start:      session.Open,
			End:        end,
			Feed:       s.cfg.Feed,
		})
		if err != nil {
			return fmt.Errorf("getting forward daily bars: %w", err)
		}

		mu.Lock()
		defer mu.Unlock()
		for symbol, series := range bars {
			out[symbol] = series
		}
		return nil
	})
	return out, err
}

// scoreSwing turns one candidate plus its forward daily bars into an outcome.
func (s *Scanner) scoreSwing(
	session TradingSession, scan *ScanResult, c GapCandidate, bars []marketdata.Bar,
) (SwingOutcome, []SwingBar) {
	o := SwingOutcome{
		SessionDate: session.Date,
		Symbol:      c.Symbol,
		ScanPhase:   scan.Phase,
		Flagged:     len(c.Methods) > 0,
		Faded:       c.Faded,
		Direction:   c.Direction,
		Methods:     c.Methods,
		PrevClose:   c.PrevClose,
		GapPct:      c.GapPct,
		GapATR:      c.GapATR,
		GapSigma:    c.GapSigma,
		ATR:         c.ATR,
		SpreadPct:   c.SpreadPct,
		EntryBasis:  s.cfg.SwingEntry,
		ReturnPct:   map[string]float64{},
	}
	if o.Methods == nil {
		o.Methods = []string{}
	}

	// Locate the gap session itself, which anchors day 0.
	gapIdx := -1
	for i, b := range bars {
		if sessionDateOf(b.Timestamp, s.loc) == session.Date {
			gapIdx = i
			break
		}
	}
	if gapIdx < 0 {
		return o, nil
	}

	// The entry, and the first bar whose range the position is exposed to.
	//
	// Both bases land on gapIdx+1 for opposite reasons, so don't "simplify"
	// this: buying the next OPEN means that day's whole range is still ahead
	// of the position, while buying the gap day's CLOSE means that day's range
	// already happened and cannot be counted.
	firstForward := gapIdx + 1
	switch s.cfg.SwingEntry {
	case SwingEntryNextOpen:
		if gapIdx+1 >= len(bars) {
			// The next session has not happened yet.
			return o, nil
		}
		o.EntryPrice = bars[gapIdx+1].Open
		o.EntryDate = sessionDateOf(bars[gapIdx+1].Timestamp, s.loc)
	default:
		o.EntryPrice = bars[gapIdx].Close
		o.EntryDate = session.Date
	}
	if o.EntryPrice <= 0 {
		return o, nil
	}

	track := make([]SwingBar, 0, len(bars)-firstForward)
	for i := firstForward; i < len(bars); i++ {
		b := bars[i]
		track = append(track, SwingBar{
			Date:   sessionDateOf(b.Timestamp, s.loc),
			Day:    i - firstForward + 1,
			Open:   b.Open,
			High:   b.High,
			Low:    b.Low,
			Close:  b.Close,
			Volume: b.Volume,
		})
	}
	o.ForwardBars = len(track)

	for _, h := range s.cfg.SwingHorizonDays {
		if h <= len(track) {
			o.ReturnPct[fmt.Sprintf("%dd", h)] = pctChange(o.EntryPrice, track[h-1].Close)
		}
	}

	// Excursions over the holding window only, and strictly after the entry.
	window := track
	if len(window) > s.cfg.SwingMaxHoldDays {
		window = window[:s.cfg.SwingMaxHoldDays]
	}
	high, low := o.EntryPrice, o.EntryPrice
	for _, b := range window {
		high = math.Max(high, b.High)
		low = math.Min(low, b.Low)
	}
	o.MaxFavourablePct = pctChange(o.EntryPrice, high)
	o.MaxAdversePct = pctChange(o.EntryPrice, low)

	s.cfg.simulateSwingBracket(&o, c, track)
	return o, track
}

// swingBracket sizes the target and stop for one candidate.
func (cfg ScannerConfig) swingBracket(c GapCandidate, entryPrice float64) (
	targetPrice, targetPct float64, basis string, stopPrice, stopPct float64,
) {
	// Reuse the shared sizer with the swing multiple so the cost floor and the
	// ATR arithmetic stay in one place.
	sized := cfg
	sized.ExitTargetMode = ExitTargetModeATR
	sized.ExitTargetATR = cfg.SwingTargetATR
	targetPrice, targetPct, basis = sized.resolveTarget(c, entryPrice)

	if cfg.SwingStopATR > 0 && c.ATR > 0 {
		stopPrice = entryPrice - cfg.SwingStopATR*c.ATR
		if stopPrice < 0 {
			stopPrice = 0
		}
		stopPct = pctChange(entryPrice, stopPrice)
	}
	return targetPrice, targetPct, basis, stopPrice, stopPct
}

// simulateSwingBracket walks the forward days and closes the position on the
// target, the stop, or the time stop, whichever comes first.
func (cfg ScannerConfig) simulateSwingBracket(o *SwingOutcome, c GapCandidate, track []SwingBar) {
	o.TargetPrice, o.TargetPct, o.TargetBasis, o.StopPrice, o.StopPct =
		cfg.swingBracket(c, o.EntryPrice)

	o.ExitReason = SwingExitOpen
	for _, b := range track {
		hitTarget := o.TargetPrice > 0 && b.High >= o.TargetPrice
		hitStop := o.StopPrice > 0 && b.Low <= o.StopPrice

		switch {
		case hitTarget && hitStop:
			// A daily bar cannot say which came first. Resolving to the target
			// is the optimistic assumption that makes a backtest look
			// tradeable when it is not, so take the stop and flag the row.
			o.Ambiguous = true
			o.ExitReason, o.ExitPrice, o.ExitDay = SwingExitStop, o.StopPrice, b.Day
		case hitTarget:
			o.ExitReason, o.ExitPrice, o.ExitDay = ExitReasonTarget, o.TargetPrice, b.Day
		case hitStop:
			o.ExitReason, o.ExitPrice, o.ExitDay = SwingExitStop, o.StopPrice, b.Day
		case b.Day >= cfg.SwingMaxHoldDays:
			o.ExitReason, o.ExitPrice, o.ExitDay = SwingExitTime, b.Close, b.Day
		default:
			continue
		}
		break
	}

	if o.ExitReason == SwingExitOpen {
		// Not enough days have elapsed for this trade to have finished.
		o.CostPct, o.StrategyReturnPct, o.NetStrategyReturnPct = 0, 0, 0
		return
	}

	o.StrategyReturnPct = pctChange(o.EntryPrice, o.ExitPrice)
	o.CostPct = cfg.Costs.RoundTripCostPct(
		o.EntryPrice, o.ExitPrice, c.SpreadPct, swingCrossings(o.ExitReason))
	o.NetStrategyReturnPct = o.StrategyReturnPct - o.CostPct
}

// attachNews tags each candidate with the stories published between the prior
// close and the open, which is the window that explains an overnight gap.
func (s *Scanner) attachNews(outcomes []SwingOutcome, session TradingSession) error {
	if len(outcomes) == 0 || s.cfg.NewsLookbackHours <= 0 {
		return nil
	}

	index := make(map[string]int, len(outcomes))
	symbols := make([]string, 0, len(outcomes))
	for i, o := range outcomes {
		index[o.Symbol] = i
		symbols = append(symbols, o.Symbol)
	}

	start := session.Open.Add(-time.Duration(s.cfg.NewsLookbackHours) * time.Hour)
	var mu sync.Mutex

	return s.forEachBatch(symbols, s.cfg.HistoryBatchSize, func(batch []string) error {
		articles, err := s.data.GetNews(marketdata.GetNewsRequest{
			Symbols: batch,
			Start:   start,
			End:     session.Open,
			// The default caps at 50 articles, which a batch of symbols over a
			// full day would silently exceed.
			NoTotalLimit: true,
		})
		if err != nil {
			return fmt.Errorf("getting news: %w", err)
		}

		mu.Lock()
		defer mu.Unlock()
		for _, a := range articles {
			for _, symbol := range a.Symbols {
				i, found := index[symbol]
				if !found {
					continue
				}
				o := &outcomes[i]
				o.HasNews = true
				o.NewsCount++
				if o.TopHeadline == "" {
					o.TopHeadline = a.Headline
				}
				if looksLikeEarnings(a.Headline) {
					o.LooksEarning = true
					// An earnings headline outranks whatever landed first.
					o.TopHeadline = a.Headline
				}
			}
		}
		return nil
	})
}

// SwingSummary aggregates finished swing trades.
type SwingSummary struct {
	Trades    int `json:"trades"`
	Immature  int `json:"immature"`
	HitTarget int `json:"hit_target"`
	HitStop   int `json:"hit_stop"`
	TimeStop  int `json:"time_stop"`
	Ambiguous int `json:"ambiguous"`

	MeanNetPct   float64 `json:"mean_net_pct"`
	MedianNetPct float64 `json:"median_net_pct"`
	NetWinRate   float64 `json:"net_win_rate"`
	MeanCostPct  float64 `json:"mean_cost_pct"`
	MeanHoldDays float64 `json:"mean_hold_days"`
	WorstNetPct  float64 `json:"worst_net_pct"`
	BestNetPct   float64 `json:"best_net_pct"`
}

// SummariseSwing aggregates the finished trades in a set of outcomes.
func SummariseSwing(outcomes []SwingOutcome) SwingSummary {
	var sum SwingSummary
	nets := make([]float64, 0, len(outcomes))
	var totalNet, totalCost, totalDays, wins float64

	for _, o := range outcomes {
		if !o.Mature() {
			sum.Immature++
			continue
		}
		sum.Trades++
		switch o.ExitReason {
		case ExitReasonTarget:
			sum.HitTarget++
		case SwingExitStop:
			sum.HitStop++
		case SwingExitTime:
			sum.TimeStop++
		}
		if o.Ambiguous {
			sum.Ambiguous++
		}
		nets = append(nets, o.NetStrategyReturnPct)
		totalNet += o.NetStrategyReturnPct
		totalCost += o.CostPct
		totalDays += float64(o.ExitDay)
		if o.NetStrategyReturnPct > 0 {
			wins++
		}
	}
	if sum.Trades == 0 {
		return sum
	}

	n := float64(sum.Trades)
	sum.MeanNetPct = totalNet / n
	sum.MeanCostPct = totalCost / n
	sum.MeanHoldDays = totalDays / n
	sum.NetWinRate = wins / n * 100

	sort.Float64s(nets)
	sum.MedianNetPct = median(nets)
	sum.WorstNetPct = nets[0]
	sum.BestNetPct = nets[len(nets)-1]
	return sum
}

// PrintSwing writes the per-candidate table and the news split to stdout.
func PrintSwing(sessionDate string, outcomes []SwingOutcome, cfg ScannerConfig) {
	fmt.Printf("\nSwing outcomes  session=%s  entry=%s  target=%.2f x ATR  stop=%.2f x ATR  max hold=%dd\n",
		sessionDate, cfg.SwingEntry, cfg.SwingTargetATR, cfg.SwingStopATR, cfg.SwingMaxHoldDays)

	if len(outcomes) == 0 {
		fmt.Println("no daily data for this session's candidates yet")
		return
	}

	fmt.Printf("\n%-8s %-5s %8s %9s %6s %-9s %8s %8s %6s %10s %8s\n",
		"SYMBOL", "DIR", "GAP%", "ENTRY", "NEWS", "EXIT", "TGT%", "STOP%", "DAY", "NET%", "MFE%")
	for _, o := range outcomes {
		news := "-"
		if o.LooksEarning {
			news = "earnings"
		} else if o.HasNews {
			news = fmt.Sprintf("%d", o.NewsCount)
		}
		exit := o.ExitReason
		if o.Ambiguous {
			exit += "?"
		}
		fmt.Printf("%-8s %-5s %7.2f%% %9.2f %6s %-9s %8.2f %8.2f %6d %10.3f %8.2f\n",
			o.Symbol, o.Direction, o.GapPct, o.EntryPrice, news, exit,
			o.TargetPct, o.StopPct, o.ExitDay, o.NetStrategyReturnPct, o.MaxFavourablePct)
	}

	printSwingSplits(outcomes)
}

// printSwingSplits reports the buckets the drift-versus-reversal question turns
// on: whether the gap had a story behind it, and which way it gapped.
func printSwingSplits(outcomes []SwingOutcome) {
	var flagged []SwingOutcome
	for _, o := range outcomes {
		if o.Flagged && !o.Faded {
			flagged = append(flagged, o)
		}
	}

	buckets := []struct {
		name string
		keep func(SwingOutcome) bool
	}{
		{"all", func(SwingOutcome) bool { return true }},
		{"news", func(o SwingOutcome) bool { return o.HasNews }},
		{"no news", func(o SwingOutcome) bool { return !o.HasNews }},
		{"earnings", func(o SwingOutcome) bool { return o.LooksEarning }},
		{"up gaps", func(o SwingOutcome) bool { return o.Direction != "down" }},
		{"down gaps", func(o SwingOutcome) bool { return o.Direction == "down" }},
	}

	fmt.Printf("\nFinished trades among flagged candidates:\n")
	fmt.Printf("%-10s %6s %7s %7s %7s %9s %9s %8s %7s\n",
		"BUCKET", "N", "TARGET", "STOP", "TIME", "NET%", "MEDIAN%", "WIN%", "HOLD")
	for _, b := range buckets {
		var subset []SwingOutcome
		for _, o := range flagged {
			if b.keep(o) {
				subset = append(subset, o)
			}
		}
		sum := SummariseSwing(subset)
		if sum.Trades == 0 {
			continue
		}
		fmt.Printf("%-10s %6d %7d %7d %7d %9.3f %9.3f %8.1f %7.1f\n",
			b.name, sum.Trades, sum.HitTarget, sum.HitStop, sum.TimeStop,
			sum.MeanNetPct, sum.MedianNetPct, sum.NetWinRate, sum.MeanHoldDays)
	}

	all := SummariseSwing(flagged)
	if all.Immature > 0 {
		fmt.Printf("\n%d trades are still open (not enough days elapsed) and are excluded above\n",
			all.Immature)
	}
	if all.Ambiguous > 0 {
		fmt.Printf("%d/%d exits hit the target and the stop on the same day; daily bars cannot "+
			"order them, so each was resolved to the stop\n", all.Ambiguous, all.Trades)
	}
	fmt.Println()
}

// runSwing scores every session that has a scan, or one session with -date.
//
// It is idempotent: each run overwrites that session's files, so re-running as
// more days elapse fills in the horizons. Nothing is appended to a .jsonl,
// which is what keeps repeated runs from double-counting a session.
func runSwing(client *alpaca.Client, cfg ScannerConfig, date string) error {
	sessions, err := sessionDirs(cfg.DataDir, date)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		return fmt.Errorf("no session directories found under %s; run the scanner first", cfg.DataDir)
	}

	scanner, err := NewScanner(client, cfg)
	if err != nil {
		return err
	}
	defer scanner.Close()

	scored, skipped := 0, 0
	var everything []SwingOutcome

	for _, sessionDate := range sessions {
		scan, err := loadAnyScan(cfg.DataDir, sessionDate)
		if err != nil {
			return err
		}
		if scan == nil {
			skipped++
			continue
		}

		session, err := ResolveSessionByDate(client, sessionDate, cfg.PremarketStartET)
		if err != nil {
			log.Printf("swing: %s: %v", sessionDate, err)
			skipped++
			continue
		}

		outcomes, tracks, err := scanner.CollectSwingOutcomes(session, scan)
		if err != nil {
			return err
		}
		if len(outcomes) == 0 {
			log.Printf("swing: %s has no forward daily data yet", sessionDate)
			skipped++
			continue
		}

		if err := SaveJSON(SwingFile(cfg.DataDir, sessionDate), outcomes); err != nil {
			return err
		}
		if err := SaveJSON(SwingTracksFile(cfg.DataDir, sessionDate), tracks); err != nil {
			return err
		}
		scored++
		everything = append(everything, outcomes...)

		if date != "" {
			PrintSwing(sessionDate, outcomes, cfg)
		}
	}

	log.Printf("swing: scored %d sessions, skipped %d, %d candidate-sessions total",
		scored, skipped, len(everything))

	if date == "" && len(everything) > 0 {
		fmt.Printf("\nSwing scoring across %d sessions  entry=%s  target=%.2f x ATR  stop=%.2f x ATR  max hold=%dd\n",
			scored, cfg.SwingEntry, cfg.SwingTargetATR, cfg.SwingStopATR, cfg.SwingMaxHoldDays)
		printSwingSplits(everything)
		fmt.Println("note: run -swing-sweep to vary the holding period and target together")
	}
	return nil
}
