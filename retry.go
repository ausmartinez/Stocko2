package main

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
)

// Retry budget for the trading API. Kept well inside the 5-minute cron cadence
// so a retrying tick can never overlap the next one: 1s + 2s + 4s at worst.
//
// Vars rather than consts so tests can drop the delay to zero.
var (
	apiRetryAttempts  = 4
	apiRetryBaseDelay = time.Second
)

// isTransient reports whether an error is worth retrying. Alpaca 5xx means the
// server is unwell and 429 means slow down; every other 4xx is a permanent
// problem (bad key, bad request) where retrying only delays the report.
func isTransient(err error) bool {
	var apiErr *alpaca.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= http.StatusInternalServerError ||
			apiErr.StatusCode == http.StatusTooManyRequests
	}
	// Not an API error at all, so almost always a network or timeout failure.
	return true
}

// retryTransient runs fn, retrying transient failures with exponential backoff.
//
// The clock and calendar endpoints back every session lookup, so a single 5xx
// there otherwise kills a whole cron tick — including the 15:55 tick that is
// the only thing that flattens the book.
func retryTransient[T any](what string, fn func() (T, error)) (T, error) {
	var (
		result T
		err    error
	)

	delay := apiRetryBaseDelay
	for attempt := 1; attempt <= apiRetryAttempts; attempt++ {
		result, err = fn()
		if err == nil {
			if attempt > 1 {
				log.Printf("retry: %s succeeded on attempt %d", what, attempt)
			}
			return result, nil
		}
		if !isTransient(err) || attempt == apiRetryAttempts {
			break
		}

		log.Printf("retry: %s failed (attempt %d/%d), waiting %s: %v",
			what, attempt, apiRetryAttempts, delay, err)
		time.Sleep(delay)
		delay *= 2
	}
	return result, err
}
