package main

import (
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
)

// fastRetries removes the backoff so tests don't sleep, restoring the real
// budget afterwards.
func fastRetries(t *testing.T, attempts int) {
	t.Helper()

	oldAttempts, oldDelay := apiRetryAttempts, apiRetryBaseDelay
	apiRetryAttempts, apiRetryBaseDelay = attempts, 0
	t.Cleanup(func() {
		apiRetryAttempts, apiRetryBaseDelay = oldAttempts, oldDelay
	})
}

func TestIsTransient(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		// The outage that prompted this: /v2/clock returning 500.
		{"500", &alpaca.APIError{StatusCode: 500}, true},
		{"502", &alpaca.APIError{StatusCode: 502}, true},
		{"503", &alpaca.APIError{StatusCode: 503}, true},
		{"429 rate limited", &alpaca.APIError{StatusCode: 429}, true},

		// Retrying these only delays an accurate report.
		{"401 bad key", &alpaca.APIError{StatusCode: 401}, false},
		{"403 forbidden", &alpaca.APIError{StatusCode: 403}, false},
		{"404 not found", &alpaca.APIError{StatusCode: 404}, false},
		{"422 bad request", &alpaca.APIError{StatusCode: 422}, false},

		// Network-level failures carry no status code and are worth a retry.
		{"network error", &net.OpError{Op: "dial", Err: errors.New("refused")}, true},
		{"plain error", errors.New("connection reset"), true},

		// Wrapped errors must still be classified correctly.
		{"wrapped 500", fmt.Errorf("getting market clock: %w",
			&alpaca.APIError{StatusCode: 500}), true},
		{"wrapped 401", fmt.Errorf("getting account: %w",
			&alpaca.APIError{StatusCode: 401}), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransient(tt.err); got != tt.want {
				t.Errorf("isTransient(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}

func TestRetryTransientSucceedsFirstTry(t *testing.T) {
	fastRetries(t, 4)

	calls := 0
	got, err := retryTransient("thing", func() (int, error) {
		calls++
		return 42, nil
	})

	if err != nil || got != 42 {
		t.Fatalf("got (%v, %v), want (42, nil)", got, err)
	}
	if calls != 1 {
		t.Errorf("called %d times, want 1", calls)
	}
}

// The case that matters: a couple of 500s then success, which is what a brief
// Alpaca wobble looks like.
func TestRetryTransientRecoversAfterFailures(t *testing.T) {
	fastRetries(t, 4)

	calls := 0
	got, err := retryTransient("market clock", func() (string, error) {
		calls++
		if calls < 3 {
			return "", &alpaca.APIError{StatusCode: 500, Message: "internal server error"}
		}
		return "ok", nil
	})

	if err != nil {
		t.Fatalf("err = %v, want nil after recovery", err)
	}
	if got != "ok" {
		t.Errorf("got %q, want %q", got, "ok")
	}
	if calls != 3 {
		t.Errorf("called %d times, want 3", calls)
	}
}

func TestRetryTransientGivesUpAndReturnsLastError(t *testing.T) {
	fastRetries(t, 4)

	calls := 0
	_, err := retryTransient("market clock", func() (int, error) {
		calls++
		return 0, &alpaca.APIError{StatusCode: 503, Message: "unavailable"}
	})

	if calls != 4 {
		t.Errorf("called %d times, want the full budget of 4", calls)
	}
	var apiErr *alpaca.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 503 {
		t.Errorf("err = %v, want the last 503", err)
	}
}

// A permanent failure must not burn the retry budget — a dead API key should
// report immediately, as it did before this change.
func TestRetryTransientDoesNotRetryPermanentFailures(t *testing.T) {
	fastRetries(t, 4)

	calls := 0
	_, err := retryTransient("account", func() (int, error) {
		calls++
		return 0, &alpaca.APIError{StatusCode: 401, Message: "unauthorized"}
	})

	if calls != 1 {
		t.Errorf("called %d times, want 1 — 401 is not transient", calls)
	}
	var apiErr *alpaca.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Errorf("err = %v, want the 401 surfaced unchanged", err)
	}
}

// The backoff has to stay well inside the 5-minute cron cadence, or a retrying
// tick could overlap the next one.
func TestRetryBudgetFitsInsideTheCronCadence(t *testing.T) {
	total := time.Duration(0)
	delay := apiRetryBaseDelay
	for i := 1; i < apiRetryAttempts; i++ {
		total += delay
		delay *= 2
	}

	if total >= 60*time.Second {
		t.Errorf("worst-case backoff is %s; too close to the 5-minute tick", total)
	}
	t.Logf("worst-case backoff: %s across %d attempts", total, apiRetryAttempts)
}
