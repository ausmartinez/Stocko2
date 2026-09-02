package main

import (
	"math"

	"github.com/alpacahq/alpaca-trade-api-go/v3/marketdata"
)

// trueRange is the classic Wilder true range: the day's own range, extended to
// cover any gap away from the previous close.
func trueRange(bar marketdata.Bar, prevClose float64) float64 {
	hl := bar.High - bar.Low
	hc := math.Abs(bar.High - prevClose)
	lc := math.Abs(bar.Low - prevClose)
	return math.Max(hl, math.Max(hc, lc))
}

// AverageTrueRange returns the mean of the last `period` true ranges and how
// many were available. Simple average, not Wilder's smoothing. Bars must be
// ascending and must exclude the session being scanned.
func AverageTrueRange(bars []marketdata.Bar, period int) (atr float64, samples int) {
	if len(bars) < 2 || period < 1 {
		return 0, 0
	}

	ranges := make([]float64, 0, len(bars)-1)
	for i := 1; i < len(bars); i++ {
		prevClose := bars[i-1].Close
		if prevClose <= 0 {
			continue
		}
		ranges = append(ranges, trueRange(bars[i], prevClose))
	}
	if len(ranges) == 0 {
		return 0, 0
	}

	if len(ranges) > period {
		ranges = ranges[len(ranges)-period:]
	}

	var sum float64
	for _, r := range ranges {
		sum += r
	}
	return sum / float64(len(ranges)), len(ranges)
}

// OvernightStats returns the mean and sample stdev of close-to-open returns as
// fractions (0.01 == 1%). Bars must be ascending and must exclude the session
// being scanned.
func OvernightStats(bars []marketdata.Bar) (mean, stdev float64, samples int) {
	if len(bars) < 3 {
		return 0, 0, 0
	}

	returns := make([]float64, 0, len(bars)-1)
	for i := 1; i < len(bars); i++ {
		prevClose := bars[i-1].Close
		if prevClose <= 0 || bars[i].Open <= 0 {
			continue
		}
		returns = append(returns, (bars[i].Open-prevClose)/prevClose)
	}
	// A sample standard deviation needs at least two observations.
	if len(returns) < 2 {
		return 0, 0, len(returns)
	}

	var sum float64
	for _, r := range returns {
		sum += r
	}
	mean = sum / float64(len(returns))

	var sumSq float64
	for _, r := range returns {
		d := r - mean
		sumSq += d * d
	}
	stdev = math.Sqrt(sumSq / float64(len(returns)-1))

	return mean, stdev, len(returns)
}

// chunk splits a slice into batches of at most size elements.
func chunk(items []string, size int) [][]string {
	if size < 1 {
		size = 1
	}
	batches := make([][]string, 0, (len(items)+size-1)/size)
	for start := 0; start < len(items); start += size {
		end := start + size
		if end > len(items) {
			end = len(items)
		}
		batches = append(batches, items[start:end])
	}
	return batches
}
