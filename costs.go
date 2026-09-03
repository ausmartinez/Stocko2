package main

// CostModel is the friction charged against a simulated round trip. Percent
// fields are percent, not fractions, matching the rest of the config.
type CostModel struct {
	// Disabled rather than Enabled so an older config.json that predates this
	// block still gets costs charged instead of silently reporting gross.
	Disabled bool `json:"disabled"`

	// PerShareFee is charged on the sell leg per share, e.g. the FINRA Trading
	// Activity Fee. Tiny in dollars but heavy against a thin target on a cheap
	// stock, since share count scales as notional/price.
	PerShareFee float64 `json:"per_share_fee"`

	// SellNotionalPct is charged on the sell proceeds, e.g. the SEC Section 31
	// fee. The SEC resets this rate periodically, so check it.
	SellNotionalPct float64 `json:"sell_notional_pct"`

	// SlippagePct is extra cost per market-order crossing, on top of the half
	// spread. Gappers in the first minutes after the bell routinely fill worse
	// than the quote implies.
	SlippagePct float64 `json:"slippage_pct"`

	// AssumedSpreadPct stands in when a candidate carries no captured quote.
	// Left at zero it charges no spread at all, which flatters the result;
	// set it once you have measured what your own candidates quote.
	AssumedSpreadPct float64 `json:"assumed_spread_pct"`
}

// DefaultCostModel uses published US equity rates. Commission is absent
// because Alpaca does not charge one on equities.
func DefaultCostModel() CostModel {
	return CostModel{
		PerShareFee:     0.000166, // FINRA TAF, per share, sells only
		SellNotionalPct: 0.00278,  // SEC Section 31, ~$27.80 per $1M of proceeds
	}
}

// spreadFor prefers the measured spread and falls back to the assumed one. A
// momentarily locked market also reads as zero here and so gets the fallback,
// which overstates its cost rather than assuming a free round trip.
func (m CostModel) spreadFor(spreadPct float64) float64 {
	if spreadPct > 0 {
		return spreadPct
	}
	return m.AssumedSpreadPct
}

// marketCrossings counts the legs of a round trip that pay the spread. A
// market buy always does, and flattening at the close does. Hitting the target
// fills a resting limit order, which does not.
func marketCrossings(exitReason string) float64 {
	if exitReason == ExitReasonClose {
		return 2
	}
	return 1
}

// RoundTripCostPct is the friction of one round trip as a percentage of the
// entry notional, so it subtracts directly from a percentage return.
func (m CostModel) RoundTripCostPct(entryPrice, exitPrice, spreadPct, crossings float64) float64 {
	if m.Disabled || entryPrice <= 0 {
		return 0
	}

	// Crossing the spread costs half of it, since the quote straddles the mid.
	cost := crossings * (m.spreadFor(spreadPct)/2 + m.SlippagePct)

	// A per-share fee only becomes a percentage once divided by the price.
	cost += m.PerShareFee / entryPrice * 100

	// Charged on the proceeds, restated against the entry notional.
	if exitPrice > 0 {
		cost += m.SellNotionalPct * exitPrice / entryPrice
	}
	return cost
}

// BreakevenPct is the gross move a round trip must make to cover its own
// friction. A target below this loses before the position is even opened.
func (m CostModel) BreakevenPct(price, spreadPct, crossings float64) float64 {
	return m.RoundTripCostPct(price, price, spreadPct, crossings)
}

// quoteSpread reports the two-sided quote and its width as a percentage of the
// mid. All zero when there is no usable quote, which the cost model reads as
// "unmeasured" rather than "free".
func quoteSpread(bid, ask float64) (float64, float64, float64) {
	if bid <= 0 || ask <= 0 || ask < bid {
		return 0, 0, 0
	}
	mid := (bid + ask) / 2
	return bid, ask, (ask - bid) / mid * 100
}
