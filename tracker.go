package main

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
)

// PhaseTrack is the live paper-trading tick, run every few minutes while the
// market is open.
const PhaseTrack = "track"

// Position lifecycle.
const (
	PositionOpen   = "open"
	PositionClosed = "closed"
)

// PaperPosition is one pretend purchase, carried across ticks in the ledger.
type PaperPosition struct {
	Symbol string `json:"symbol"`

	OpenedAt      string  `json:"opened_at"`
	OpenedMinutes int     `json:"opened_minutes_from_open"`
	EntryPrice    float64 `json:"entry_price"`
	Shares        float64 `json:"shares"`
	Notional      float64 `json:"notional"`
	TargetPrice   float64 `json:"target_price"`
	ExitReference string  `json:"exit_reference"`

	// Signal as it stood at entry, frozen so later scans cannot rewrite it.
	Direction       string   `json:"direction"`
	PrevClose       float64  `json:"prev_close"`
	GapPct          float64  `json:"gap_pct"`
	GapATR          float64  `json:"gap_atr"`
	GapSigma        float64  `json:"gap_sigma"`
	Methods         []string `json:"methods"`
	DiscoveredAt    string   `json:"discovered_at"`
	SeenPremarket   bool     `json:"seen_premarket"`
	PremarketVolume uint64   `json:"premarket_volume"`

	Status           string  `json:"status"`
	LastPrice        float64 `json:"last_price"`
	LastSampledAt    string  `json:"last_sampled_at"`
	Samples          int     `json:"samples"`
	MaxFavourablePct float64 `json:"max_favourable_pct"`
	MaxAdversePct    float64 `json:"max_adverse_pct"`

	ClosedAt             string  `json:"closed_at,omitempty"`
	ClosedMinutes        int     `json:"closed_minutes_from_open,omitempty"`
	ExitPrice            float64 `json:"exit_price,omitempty"`
	ExitReason           string  `json:"exit_reason,omitempty"`
	ReturnPct            float64 `json:"return_pct"`
	PnL                  float64 `json:"pnl"`
	AdverseBeforeExitPct float64 `json:"adverse_before_exit_pct"`
}

// Ledger is the session's paper book, rewritten on every tick.
type Ledger struct {
	SessionDate   string  `json:"session_date"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
	ScanPhase     string  `json:"scan_phase"`
	ExitReference string  `json:"exit_reference"`
	ExitTargetPct float64 `json:"exit_target_pct"`
	Ticks         int     `json:"ticks"`

	Positions []PaperPosition `json:"positions"`
}

// PositionSample is one observation of one position, appended every tick.
type PositionSample struct {
	SessionDate     string  `json:"session_date"`
	Symbol          string  `json:"symbol"`
	At              string  `json:"at"`
	MinutesFromOpen int     `json:"minutes_from_open"`
	Price           float64 `json:"price"`
	High            float64 `json:"high"`
	Low             float64 `json:"low"`
	UnrealizedPct   float64 `json:"unrealized_pct"`
	Status          string  `json:"status"`
}

// Open counts positions still on the book.
func (l *Ledger) Open() int {
	n := 0
	for _, p := range l.Positions {
		if p.Status == PositionOpen {
			n++
		}
	}
	return n
}

// LedgerPath is the session's paper book.
func LedgerPath(dataDir, sessionDate string) string {
	return SessionFile(dataDir, sessionDate, "ledger")
}

// Track runs one paper-trading tick: open the book if this is the first tick
// after the bell, sample every open position, close the ones that hit target,
// and flatten everything near the close.
func (s *Scanner) Track(session TradingSession) (*Ledger, []PositionSample, error) {
	if session.BeforeOpen {
		return nil, nil, fmt.Errorf("session %s has not opened yet; nothing to track", session.Date)
	}

	path := LedgerPath(s.cfg.DataDir, session.Date)
	ledger, err := LoadJSON[Ledger](path)
	if err != nil {
		return nil, nil, err
	}

	if ledger == nil {
		ledger, err = s.openBook(session)
		if err != nil {
			return nil, nil, err
		}
		log.Printf("tracker: opened %d paper positions for %s", len(ledger.Positions), session.Date)
	}

	samples, err := s.tick(session, ledger)
	if err != nil {
		return nil, nil, err
	}

	ledger.Ticks++
	ledger.UpdatedAt = session.Now.Format(time.RFC3339)
	return ledger, samples, nil
}

// openBook buys every flagged candidate from the session's scan at the current
// price. Faded rows are skipped: they are not something we would have bought.
func (s *Scanner) openBook(session TradingSession) (*Ledger, error) {
	scan, err := loadScanForOutcomes(s.cfg.DataDir, session.Date)
	if err != nil {
		return nil, err
	}

	var buys []GapCandidate
	for _, c := range scan.Candidates {
		if len(c.Methods) > 0 && !c.Faded {
			buys = append(buys, c)
		}
	}
	if len(buys) == 0 {
		return nil, fmt.Errorf("scan for %s has no flagged candidates to buy", session.Date)
	}

	symbols := make([]string, 0, len(buys))
	for _, c := range buys {
		symbols = append(symbols, c.Symbol)
	}
	prices, err := s.latestPrices(symbols)
	if err != nil {
		return nil, err
	}

	minutes := int(session.Now.Sub(session.Open).Minutes())
	ledger := &Ledger{
		SessionDate:   session.Date,
		CreatedAt:     session.Now.Format(time.RFC3339),
		UpdatedAt:     session.Now.Format(time.RFC3339),
		ScanPhase:     scan.Phase,
		ExitReference: s.cfg.ExitReference,
		ExitTargetPct: s.cfg.ExitTargetPct,
	}

	for _, c := range buys {
		entry, ok := prices[c.Symbol]
		if !ok || entry <= 0 {
			log.Printf("tracker: no price for %s, not buying", c.Symbol)
			continue
		}

		reference := entry
		if s.cfg.ExitReference == ExitRefPrevClose && c.PrevClose > 0 {
			reference = c.PrevClose
		}

		ledger.Positions = append(ledger.Positions, PaperPosition{
			Symbol:          c.Symbol,
			OpenedAt:        session.Now.Format(time.RFC3339),
			OpenedMinutes:   minutes,
			EntryPrice:      entry,
			Shares:          s.cfg.PositionNotional / entry,
			Notional:        s.cfg.PositionNotional,
			TargetPrice:     reference * (1 + s.cfg.ExitTargetPct/100),
			ExitReference:   s.cfg.ExitReference,
			Direction:       c.Direction,
			PrevClose:       c.PrevClose,
			GapPct:          c.GapPct,
			GapATR:          c.GapATR,
			GapSigma:        c.GapSigma,
			Methods:         c.Methods,
			DiscoveredAt:    c.DiscoveredAt,
			SeenPremarket:   c.SeenPremarket,
			PremarketVolume: c.PremarketVolume,
			Status:          PositionOpen,
			LastPrice:       entry,
			LastSampledAt:   session.Now.Format(time.RFC3339),
		})
	}
	return ledger, nil
}

// tick samples open positions over the window since the last check.
//
// It reads 1-minute bars rather than a single spot price so that a target
// touched between two ticks is still caught. That is what a resting limit
// order would have done, and polling the last trade alone would miss it.
func (s *Scanner) tick(session TradingSession, ledger *Ledger) ([]PositionSample, error) {
	open := make([]int, 0, len(ledger.Positions))
	symbols := make([]string, 0, len(ledger.Positions))
	for i, p := range ledger.Positions {
		if p.Status == PositionOpen {
			open = append(open, i)
			symbols = append(symbols, p.Symbol)
		}
	}
	if len(open) == 0 {
		return nil, nil
	}

	// Earliest point any open position still needs covering.
	windowStart := session.Now
	for _, i := range open {
		if t, err := time.Parse(time.RFC3339, ledger.Positions[i].LastSampledAt); err == nil && t.Before(windowStart) {
			windowStart = t
		}
	}

	bars, err := s.minuteBars(symbols, windowStart, session.Now)
	if err != nil {
		return nil, err
	}

	// Flatten the book near the bell so nothing is carried overnight.
	sessionMinutes := int(session.Close.Sub(session.Open).Minutes())
	nowMinutes := int(session.Now.Sub(session.Open).Minutes())
	flatten := nowMinutes >= sessionMinutes-s.cfg.CloseAllMinutesBeforeClose

	samples := make([]PositionSample, 0, len(open))
	for _, i := range open {
		p := &ledger.Positions[i]
		samples = append(samples, s.advance(session, p, bars[p.Symbol], flatten))
	}
	return samples, nil
}

// advance walks one position through the new bars, closing it on the first
// touch of the target and otherwise updating its running path stats.
func (s *Scanner) advance(
	session TradingSession, p *PaperPosition, bars []marketdata.Bar, flatten bool,
) PositionSample {
	last, err := time.Parse(time.RFC3339, p.LastSampledAt)
	if err != nil {
		last = session.Open
	}

	sample := PositionSample{
		SessionDate:     session.Date,
		Symbol:          p.Symbol,
		At:              session.Now.Format(time.RFC3339),
		MinutesFromOpen: int(session.Now.Sub(session.Open).Minutes()),
		Price:           p.LastPrice,
		Status:          p.Status,
	}

	for _, b := range bars {
		if !b.Timestamp.After(last) || !b.Timestamp.Before(session.Close) {
			continue
		}

		if sample.High == 0 || b.High > sample.High {
			sample.High = b.High
		}
		if sample.Low == 0 || b.Low < sample.Low {
			sample.Low = b.Low
		}
		p.LastPrice = b.Close
		p.MaxFavourablePct = math.Max(p.MaxFavourablePct, pctChange(p.EntryPrice, b.High))
		p.MaxAdversePct = math.Min(p.MaxAdversePct, pctChange(p.EntryPrice, b.Low))

		if p.Status == PositionOpen && b.High >= p.TargetPrice {
			p.Status = PositionClosed
			p.ExitReason = ExitReasonTarget
			p.ExitPrice = p.TargetPrice
			p.ClosedAt = b.Timestamp.In(s.loc).Format(time.RFC3339)
			p.ClosedMinutes = int(b.Timestamp.Add(time.Minute).Sub(session.Open).Minutes())
			p.AdverseBeforeExitPct = p.MaxAdversePct
		}
	}

	p.Samples++
	p.LastSampledAt = session.Now.Format(time.RFC3339)

	if p.Status == PositionOpen && flatten {
		p.Status = PositionClosed
		p.ExitReason = ExitReasonClose
		p.ExitPrice = p.LastPrice
		p.ClosedAt = session.Now.Format(time.RFC3339)
		p.ClosedMinutes = sample.MinutesFromOpen
		p.AdverseBeforeExitPct = p.MaxAdversePct
	}

	if p.Status == PositionClosed {
		p.ReturnPct = pctChange(p.EntryPrice, p.ExitPrice)
		p.PnL = (p.ExitPrice - p.EntryPrice) * p.Shares
	}

	if sample.High == 0 {
		sample.High, sample.Low = p.LastPrice, p.LastPrice
	}
	sample.Price = p.LastPrice
	sample.UnrealizedPct = pctChange(p.EntryPrice, p.LastPrice)
	sample.Status = p.Status
	return sample
}

// latestPrices fetches the current trade price for each symbol.
func (s *Scanner) latestPrices(symbols []string) (map[string]float64, error) {
	prices := make(map[string]float64, len(symbols))
	var mu sync.Mutex

	err := s.forEachBatch(symbols, s.cfg.SnapshotBatchSize, func(batch []string) error {
		trades, err := s.data.GetLatestTrades(batch, marketdata.GetLatestTradeRequest{Feed: s.cfg.Feed})
		if err != nil {
			return fmt.Errorf("getting latest trades: %w", err)
		}
		mu.Lock()
		defer mu.Unlock()
		for symbol, t := range trades {
			prices[symbol] = t.Price
		}
		return nil
	})
	return prices, err
}

// minuteBars fetches 1-minute bars over a window for the given symbols.
func (s *Scanner) minuteBars(symbols []string, start, end time.Time) (map[string][]marketdata.Bar, error) {
	out := make(map[string][]marketdata.Bar, len(symbols))
	var mu sync.Mutex

	err := s.forEachBatch(symbols, s.cfg.HistoryBatchSize, func(batch []string) error {
		bars, err := s.data.GetMultiBars(batch, marketdata.GetBarsRequest{
			TimeFrame: marketdata.OneMin,
			Start:     start,
			End:       end,
			Feed:      s.cfg.Feed,
		})
		if err != nil {
			return fmt.Errorf("getting minute bars: %w", err)
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

// PrintLedger summarises the current paper book.
func PrintLedger(l *Ledger, samples []PositionSample) {
	closed := len(l.Positions) - l.Open()
	fmt.Printf("\nPaper book  session=%s  tick=%d  open=%d  closed=%d  target=%s +%.2f%%\n\n",
		l.SessionDate, l.Ticks, l.Open(), closed, l.ExitReference, l.ExitTargetPct)

	var realised, unrealised float64
	fmt.Printf("%-8s %-7s %9s %9s %9s %8s %8s %13s\n",
		"SYMBOL", "STATUS", "ENTRY", "TARGET", "LAST", "RET%", "MAE%", "EXIT")
	for _, p := range l.Positions {
		exit := "-"
		if p.Status == PositionClosed {
			exit = fmt.Sprintf("%s+%dm", p.ExitReason, p.ClosedMinutes)
			realised += p.PnL
		} else {
			unrealised += (p.LastPrice - p.EntryPrice) * p.Shares
		}
		ret := p.ReturnPct
		if p.Status == PositionOpen {
			ret = pctChange(p.EntryPrice, p.LastPrice)
		}
		fmt.Printf("%-8s %-7s %9.2f %9.2f %9.2f %8.3f %8.2f %13s\n",
			p.Symbol, p.Status, p.EntryPrice, p.TargetPrice, p.LastPrice, ret, p.MaxAdversePct, exit)
	}

	fmt.Printf("\nrealised P&L $%.2f   unrealised $%.2f   total $%.2f   (notional $%.0f/position)\n\n",
		realised, unrealised, realised+unrealised, l.positionNotional())
}

func (l *Ledger) positionNotional() float64 {
	if len(l.Positions) == 0 {
		return 0
	}
	return l.Positions[0].Notional
}
