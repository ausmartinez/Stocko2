package main

import (
	"math"
	"testing"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
)

// noFeeCosts isolates the spread so a test can assert on it alone.
func noFeeCosts(spreadCrossingsSpread float64) CostModel {
	return CostModel{AssumedSpreadPct: spreadCrossingsSpread}
}

func TestQuoteSpread(t *testing.T) {
	tests := []struct {
		name          string
		bid, ask      float64
		wantSpreadPct float64
		wantUsable    bool
	}{
		// 10.00/10.02 straddles a 10.01 mid, so 0.02/10.01 = 0.1998%.
		{"normal", 10.00, 10.02, 0.19980019980019987, true},
		// A one-cent spread on a dollar stock is a full percent.
		{"penny stock tick is huge", 1.00, 1.01, 0.9950248756218906, true},
		// A locked market is a real quote that happens to cost nothing.
		{"locked quote", 10.00, 10.00, 0, true},
		{"no bid", 0, 10.02, 0, false},
		{"no ask", 10.00, 0, 0, false},
		{"crossed quote", 10.02, 10.00, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bid, ask, spreadPct := quoteSpread(tt.bid, tt.ask)
			if math.Abs(spreadPct-tt.wantSpreadPct) > 1e-9 {
				t.Errorf("spreadPct = %v, want %v", spreadPct, tt.wantSpreadPct)
			}
			// An unusable quote must zero every field, so nothing downstream
			// reads a half-populated one.
			if !tt.wantUsable && (bid != 0 || ask != 0) {
				t.Errorf("unusable quote leaked bid=%v ask=%v", bid, ask)
			}
			if tt.wantUsable && (bid != tt.bid || ask != tt.ask) {
				t.Errorf("usable quote altered: bid=%v ask=%v", bid, ask)
			}
		})
	}
}

func TestMarketCrossings(t *testing.T) {
	// A target exit fills a resting limit, so only the entry crosses.
	if got := marketCrossings(ExitReasonTarget); got != 1 {
		t.Errorf("target crossings = %v, want 1", got)
	}
	// Flattening at the close is a second market order.
	if got := marketCrossings(ExitReasonClose); got != 2 {
		t.Errorf("close crossings = %v, want 2", got)
	}
}

func TestRoundTripCostPctChargesHalfSpreadPerCrossing(t *testing.T) {
	m := noFeeCosts(0.2) // 0.2% quoted spread

	one := m.RoundTripCostPct(10, 10, 0.2, 1)
	if math.Abs(one-0.1) > 1e-9 {
		t.Errorf("one crossing = %v%%, want 0.1%%", one)
	}
	two := m.RoundTripCostPct(10, 10, 0.2, 2)
	if math.Abs(two-0.2) > 1e-9 {
		t.Errorf("two crossings = %v%%, want 0.2%%", two)
	}
}

func TestRoundTripCostPctPerShareFeeScalesInverselyWithPrice(t *testing.T) {
	m := CostModel{PerShareFee: 0.000166}

	// The same per-share fee is a hundred times heavier on a $1 stock than on
	// a $100 one, because share count is notional/price.
	cheap := m.RoundTripCostPct(1, 1, 0, 1)
	dear := m.RoundTripCostPct(100, 100, 0, 1)

	if math.Abs(cheap-0.0166) > 1e-9 {
		t.Errorf("$1 stock cost = %v%%, want 0.0166%%", cheap)
	}
	if math.Abs(dear-0.000166) > 1e-9 {
		t.Errorf("$100 stock cost = %v%%, want 0.000166%%", dear)
	}
	if cheap <= dear {
		t.Errorf("per-share fee should hurt cheap stocks more: %v vs %v", cheap, dear)
	}
}

func TestRoundTripCostPctSellFeeFollowsProceeds(t *testing.T) {
	m := CostModel{SellNotionalPct: 0.00278}

	// Charged on the sell side, so a position that doubled pays twice as much
	// relative to what went in.
	flat := m.RoundTripCostPct(10, 10, 0, 1)
	doubled := m.RoundTripCostPct(10, 20, 0, 1)

	if math.Abs(flat-0.00278) > 1e-9 {
		t.Errorf("flat exit = %v%%, want 0.00278%%", flat)
	}
	if math.Abs(doubled-2*0.00278) > 1e-9 {
		t.Errorf("doubled exit = %v%%, want %v%%", doubled, 2*0.00278)
	}
}

func TestRoundTripCostPctUsesMeasuredSpreadOverAssumption(t *testing.T) {
	m := CostModel{AssumedSpreadPct: 1.0}

	// A measured quote wins.
	measured := m.RoundTripCostPct(10, 10, 0.2, 1)
	if math.Abs(measured-0.1) > 1e-9 {
		t.Errorf("measured spread = %v%%, want 0.1%%", measured)
	}
	// Without one, the assumption stands in rather than charging nothing.
	assumed := m.RoundTripCostPct(10, 10, 0, 1)
	if math.Abs(assumed-0.5) > 1e-9 {
		t.Errorf("assumed spread = %v%%, want 0.5%%", assumed)
	}
}

func TestRoundTripCostPctDisabledIsFree(t *testing.T) {
	m := DefaultCostModel()
	m.Disabled = true

	if got := m.RoundTripCostPct(10, 10, 5.0, 2); got != 0 {
		t.Errorf("disabled cost = %v, want 0", got)
	}
}

func TestRoundTripCostPctIgnoresUnpricedEntry(t *testing.T) {
	m := DefaultCostModel()

	// A zero entry price would make the per-share term explode.
	if got := m.RoundTripCostPct(0, 10, 0.2, 1); got != 0 {
		t.Errorf("unpriced entry cost = %v, want 0", got)
	}
}

// TestBreakevenBeatsTheDefaultTarget is the headline check: the shipped 0.1%
// target does not clear its own friction across most of the shipped price
// range, so a target hit is a realised loss.
func TestBreakevenBeatsTheDefaultTarget(t *testing.T) {
	cfg := DefaultScannerConfig()
	m := cfg.Costs

	// One cent is the minimum tick, so these are best-case spreads.
	tests := []struct {
		price          float64
		tickSpreadPct  float64
		wantUnderwater bool
	}{
		{1, 1.0, true},     // MinPrice default: breakeven ~0.52%
		{5, 0.2, true},     // breakeven ~0.11%
		{10, 0.1, false},   // breakeven ~0.055%, the first price that clears
		{100, 0.01, false}, // breakeven ~0.008%
	}

	for _, tt := range tests {
		// Entry crosses, target exit rests: the most favourable assumption.
		breakeven := m.BreakevenPct(tt.price, tt.tickSpreadPct, 1)
		underwater := breakeven >= cfg.ExitTargetPct

		if underwater != tt.wantUnderwater {
			t.Errorf("price $%.0f: breakeven %.4f%% vs target %.2f%%, underwater=%t want %t",
				tt.price, breakeven, cfg.ExitTargetPct, underwater, tt.wantUnderwater)
		}
	}
}

// TestSimulateExitWithoutFloorLosesInsideTheSpread pins the behaviour the cost
// floor exists to prevent: a flat 0.1% target on a 0.2% spread fills at a loss.
func TestSimulateExitWithoutFloorLosesInsideTheSpread(t *testing.T) {
	cfg := DefaultScannerConfig()
	cfg.ExitTargetMode = ExitTargetModePct
	cfg.ExitTargetPct = 0.1
	cfg.MinTargetCostMult = 0 // floor off, so the raw target stands
	s := testScanner(t, cfg)

	// A $10 stock quoting two cents: 0.2% spread against a 0.1% target.
	c := GapCandidate{Symbol: "AAA", PrevClose: 10, SpreadPct: 0.2}
	o := Outcome{EntryPrice: 10, ReturnPct: map[string]float64{}}
	s.simulateExit(&o, c, trackFrom(10, 10.02, 10.05), 0)

	if o.ExitReason != ExitReasonTarget {
		t.Fatalf("exit reason = %q, want %q", o.ExitReason, ExitReasonTarget)
	}
	if o.StrategyReturnPct <= 0 {
		t.Fatalf("gross return = %v, want positive", o.StrategyReturnPct)
	}
	if o.SpreadPct != 0.2 {
		t.Errorf("spread not carried across: %v", o.SpreadPct)
	}
	// Entry crosses a 0.2% spread, so 0.1% of cost before any fee.
	if o.CostPct <= 0.1 {
		t.Errorf("cost = %v%%, want more than the 0.1%% half-spread", o.CostPct)
	}
	// A winning trade that loses money.
	if o.NetStrategyReturnPct >= 0 {
		t.Errorf("net return = %v%%, want negative once friction is paid", o.NetStrategyReturnPct)
	}
	if math.Abs(o.NetStrategyReturnPct-(o.StrategyReturnPct-o.CostPct)) > 1e-9 {
		t.Errorf("net %v != gross %v - cost %v",
			o.NetStrategyReturnPct, o.StrategyReturnPct, o.CostPct)
	}
}

// TestSimulateExitFloorKeepsATargetHitProfitable is the same setup with the
// shipped defaults, where the floor lifts the target clear of its own cost.
func TestSimulateExitFloorKeepsATargetHitProfitable(t *testing.T) {
	cfg := DefaultScannerConfig()
	s := testScanner(t, cfg)

	c := GapCandidate{Symbol: "AAA", PrevClose: 10, SpreadPct: 0.2}
	o := Outcome{EntryPrice: 10, ReturnPct: map[string]float64{}}
	s.simulateExit(&o, c, trackFrom(10, 10.02, 10.05), 0)

	if o.ExitReason != ExitReasonTarget {
		t.Fatalf("exit reason = %q, want %q", o.ExitReason, ExitReasonTarget)
	}
	if o.ExitTargetBasis != TargetBasisCostFloor {
		t.Errorf("target basis = %q, want %q", o.ExitTargetBasis, TargetBasisCostFloor)
	}
	// MinTargetCostMult is 2.0, so the target sits at twice the breakeven the
	// floor was priced against.
	want := cfg.MinTargetCostMult * cfg.Costs.BreakevenPct(10, c.SpreadPct, 1)
	if math.Abs(o.ExitTargetPct-want) > 1e-9 {
		t.Errorf("target = %v%%, want %v%% (%.1fx breakeven)",
			o.ExitTargetPct, want, cfg.MinTargetCostMult)
	}
	// Realised cost differs from that estimate only in the sell-side fee, so
	// the target still clears it with the multiple close to intact.
	if ratio := o.ExitTargetPct / o.CostPct; ratio < 1.99 || ratio > 2.01 {
		t.Errorf("target/cost = %v, want ~%.1f", ratio, cfg.MinTargetCostMult)
	}
	if o.NetStrategyReturnPct <= 0 {
		t.Errorf("net return = %v%%, want positive once the floor applies", o.NetStrategyReturnPct)
	}
}

func TestSimulateExitChargesBothLegsWhenFlattening(t *testing.T) {
	cfg := DefaultScannerConfig()
	cfg.ExitTargetPct = 5.0 // unreachable, so the position holds to the close
	s := testScanner(t, cfg)

	c := GapCandidate{Symbol: "AAA", PrevClose: 10, SpreadPct: 0.2}
	track := trackFrom(10, 10.01, 10.01)

	o := Outcome{EntryPrice: 10, ReturnPct: map[string]float64{}}
	s.simulateExit(&o, c, track, 0)

	if o.ExitReason != ExitReasonClose {
		t.Fatalf("exit reason = %q, want %q", o.ExitReason, ExitReasonClose)
	}
	// Two market orders means the full spread, not half.
	if o.CostPct <= 0.2 {
		t.Errorf("cost = %v%%, want more than the 0.2%% full spread", o.CostPct)
	}
}

func TestAdvanceChargesCostsOnClose(t *testing.T) {
	s := testScanner(t, DefaultScannerConfig())
	session := testSession(t, "09:45")

	p := openPosition(t, 100, 100.1, "09:35")
	p.SpreadPct = 0.2
	bars := []marketdata.Bar{minuteBar(t, "09:40", 100.2, 99.9, 100.15)}

	s.advance(session, &p, bars, false)

	if p.Status != PositionClosed {
		t.Fatalf("status = %q, want closed", p.Status)
	}
	if p.CostPct <= 0 {
		t.Fatalf("cost = %v, want positive", p.CostPct)
	}
	if p.NetReturnPct >= p.ReturnPct {
		t.Errorf("net %v should be below gross %v", p.NetReturnPct, p.ReturnPct)
	}
	if p.NetPnL >= p.PnL {
		t.Errorf("net P&L %v should be below gross %v", p.NetPnL, p.PnL)
	}
	// A 0.1% target on a 0.2% spread cannot pay for itself.
	if p.NetPnL >= 0 {
		t.Errorf("net P&L = %v, want a loss on a target hit inside the spread", p.NetPnL)
	}
}

func TestApplyScannerDefaultsFillsCostRates(t *testing.T) {
	// An older config.json predating the costs block must still charge fees
	// rather than silently reporting gross returns as net.
	cfg := ScannerConfig{}
	applyScannerDefaults(&cfg)

	if cfg.Costs.Disabled {
		t.Error("costs default to disabled, which would hide friction")
	}
	if cfg.Costs.PerShareFee <= 0 {
		t.Errorf("PerShareFee = %v, want the default rate", cfg.Costs.PerShareFee)
	}
	if cfg.Costs.SellNotionalPct <= 0 {
		t.Errorf("SellNotionalPct = %v, want the default rate", cfg.Costs.SellNotionalPct)
	}
}
