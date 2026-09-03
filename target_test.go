package main

import (
	"math"
	"testing"
)

// atrTargetConfig sizes targets off ATR with costs and the floor switched off,
// so a test sees the ATR arithmetic alone.
func atrTargetConfig(atrMult float64) ScannerConfig {
	return ScannerConfig{
		ExitReference:  ExitRefEntry,
		ExitTargetMode: ExitTargetModeATR,
		ExitTargetATR:  atrMult,
		ExitTargetPct:  0.1,
	}
}

func TestResolveTargetScalesWithATR(t *testing.T) {
	cfg := atrTargetConfig(0.25)

	// Two stocks a hundred times apart in price, each with an ATR worth 4% of
	// its own price. A quarter of that is 1% for both, which is the point:
	// one setting travels across the whole price range.
	for _, tt := range []struct {
		name       string
		price, atr float64
	}{
		{"cheap", 2, 0.08},
		{"dear", 200, 8.0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := GapCandidate{Symbol: "AAA", PrevClose: tt.price, ATR: tt.atr}
			price, pct, basis := cfg.resolveTarget(c, tt.price)

			if basis != TargetBasisATR {
				t.Fatalf("basis = %q, want %q", basis, TargetBasisATR)
			}
			if math.Abs(pct-1.0) > 1e-9 {
				t.Errorf("target = %v%%, want 1.0%%", pct)
			}
			// 0.25 x ATR in dollars, on top of the entry.
			if want := tt.price + 0.25*tt.atr; math.Abs(price-want) > 1e-9 {
				t.Errorf("target price = %v, want %v", price, want)
			}
		})
	}
}

func TestResolveTargetTracksVolatilityAtOnePrice(t *testing.T) {
	cfg := atrTargetConfig(0.25)

	// Same price, different ATR: the quiet name gets the tighter target.
	_, quiet, _ := cfg.resolveTarget(GapCandidate{PrevClose: 100, ATR: 1.0}, 100)
	_, wild, _ := cfg.resolveTarget(GapCandidate{PrevClose: 100, ATR: 10.0}, 100)

	if math.Abs(quiet-0.25) > 1e-9 {
		t.Errorf("quiet target = %v%%, want 0.25%%", quiet)
	}
	if math.Abs(wild-2.5) > 1e-9 {
		t.Errorf("wild target = %v%%, want 2.5%%", wild)
	}
	if wild <= quiet {
		t.Errorf("a wider ATR should demand a wider target: %v vs %v", wild, quiet)
	}
}

func TestResolveTargetFallsBackToPctWithoutATR(t *testing.T) {
	cfg := atrTargetConfig(0.25)

	// A symbol with too little history to measure ATR still needs a target.
	c := GapCandidate{Symbol: "AAA", PrevClose: 50, ATR: 0}
	price, pct, basis := cfg.resolveTarget(c, 50)

	if basis != TargetBasisPct {
		t.Fatalf("basis = %q, want %q", basis, TargetBasisPct)
	}
	if math.Abs(pct-cfg.ExitTargetPct) > 1e-9 {
		t.Errorf("target = %v%%, want the %v%% fallback", pct, cfg.ExitTargetPct)
	}
	if want := 50 * (1 + cfg.ExitTargetPct/100); math.Abs(price-want) > 1e-9 {
		t.Errorf("target price = %v, want %v", price, want)
	}
}

func TestResolveTargetPctModeIgnoresATR(t *testing.T) {
	cfg := atrTargetConfig(0.25)
	cfg.ExitTargetMode = ExitTargetModePct

	c := GapCandidate{Symbol: "AAA", PrevClose: 100, ATR: 10}
	_, pct, basis := cfg.resolveTarget(c, 100)

	if basis != TargetBasisPct {
		t.Errorf("basis = %q, want %q", basis, TargetBasisPct)
	}
	if math.Abs(pct-0.1) > 1e-9 {
		t.Errorf("target = %v%%, want the flat 0.1%%", pct)
	}
}

func TestResolveTargetPrevCloseReference(t *testing.T) {
	cfg := atrTargetConfig(0.25)
	cfg.ExitReference = ExitRefPrevClose

	// A down gapper: entry 90, previous close 100, ATR 4. The target is built
	// off the previous close, so it lands at 101 rather than 91.
	c := GapCandidate{Symbol: "AAA", Direction: "down", PrevClose: 100, ATR: 4}
	price, pct, _ := cfg.resolveTarget(c, 90)

	if math.Abs(price-101) > 1e-9 {
		t.Errorf("target price = %v, want 101", price)
	}
	// The reported percentage is the move from the entry, which is what a
	// return is measured against.
	if want := pctChange(90, 101); math.Abs(pct-want) > 1e-9 {
		t.Errorf("target = %v%%, want %v%% measured from the entry", pct, want)
	}
}

// TestResolveTargetFloorMeasuresFromEntry guards the subtle case: with
// prev_close as the reference a target can sit below the entry, and the floor
// has to lift it clear of cost regardless of what set it.
func TestResolveTargetFloorMeasuresFromEntry(t *testing.T) {
	cfg := atrTargetConfig(0.25)
	cfg.ExitReference = ExitRefPrevClose
	cfg.MinTargetCostMult = 2.0
	cfg.Costs = CostModel{AssumedSpreadPct: 0.2}

	// An up gapper whose previous close is below the entry, so the raw target
	// is underwater before costs.
	c := GapCandidate{Symbol: "AAA", Direction: "up", PrevClose: 90, ATR: 1, SpreadPct: 0.2}
	price, pct, basis := cfg.resolveTarget(c, 100)

	if basis != TargetBasisCostFloor {
		t.Fatalf("basis = %q, want %q", basis, TargetBasisCostFloor)
	}
	if price <= 100 {
		t.Errorf("target price = %v, want above the 100 entry", price)
	}
	// Entry crosses a 0.2% spread, so breakeven is 0.1% and the floor is 0.2%.
	if math.Abs(pct-0.2) > 1e-9 {
		t.Errorf("target = %v%%, want the 0.2%% floor", pct)
	}
}

func TestResolveTargetFloorOnlyRaisesNeverLowers(t *testing.T) {
	cfg := atrTargetConfig(0.25)
	cfg.MinTargetCostMult = 2.0
	cfg.Costs = CostModel{AssumedSpreadPct: 0.02}

	// A 2.5% ATR target dwarfs a 0.04% floor, so ATR must win.
	c := GapCandidate{Symbol: "AAA", PrevClose: 100, ATR: 10, SpreadPct: 0.02}
	_, pct, basis := cfg.resolveTarget(c, 100)

	if basis != TargetBasisATR {
		t.Errorf("basis = %q, want %q; the floor should not have applied", basis, TargetBasisATR)
	}
	if math.Abs(pct-2.5) > 1e-9 {
		t.Errorf("target = %v%%, want 2.5%%", pct)
	}
}

func TestResolveTargetRejectsUnpricedEntry(t *testing.T) {
	cfg := atrTargetConfig(0.25)

	if price, pct, _ := cfg.resolveTarget(GapCandidate{ATR: 1}, 0); price != 0 || pct != 0 {
		t.Errorf("unpriced entry gave price=%v pct=%v, want zeroes", price, pct)
	}
}

// TestATRTargetClearsCostAcrossThePriceRange is the counterpart to
// TestBreakevenBeatsTheDefaultTarget: the flat 0.1% target lost money below
// $10, and ATR sizing has to fix that at every price.
func TestATRTargetClearsCostAcrossThePriceRange(t *testing.T) {
	cfg := DefaultScannerConfig()

	for _, price := range []float64{1, 2, 5, 10, 20, 50, 100, 500} {
		// A 4%-of-price ATR and the tightest possible one-cent spread.
		c := GapCandidate{
			Symbol:    "AAA",
			PrevClose: price,
			ATR:       price * 0.04,
			SpreadPct: 0.01 / price * 100,
		}
		_, pct, basis := cfg.resolveTarget(c, price)
		breakeven := cfg.Costs.BreakevenPct(price, c.SpreadPct, 1)

		if pct <= breakeven {
			t.Errorf("price $%.0f: target %.4f%% does not clear breakeven %.4f%% (basis %s)",
				price, pct, breakeven, basis)
		}
	}
}

func TestApplyScannerDefaultsFillsTargetSizing(t *testing.T) {
	cfg := ScannerConfig{}
	applyScannerDefaults(&cfg)

	if cfg.ExitTargetMode != ExitTargetModeATR {
		t.Errorf("ExitTargetMode = %q, want %q", cfg.ExitTargetMode, ExitTargetModeATR)
	}
	if cfg.ExitTargetATR <= 0 {
		t.Errorf("ExitTargetATR = %v, want the default multiple", cfg.ExitTargetATR)
	}
	if cfg.MinTargetCostMult <= 0 {
		t.Errorf("MinTargetCostMult = %v, want the default floor", cfg.MinTargetCostMult)
	}
}
