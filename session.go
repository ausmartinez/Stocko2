package main

import (
	"fmt"
	"time"
	// Embed the tz database so the scanner does not depend on the host having
	// zoneinfo installed. Every session boundary below is an ET wall-clock time.
	_ "time/tzdata"

	"github.com/alpacahq/alpaca-trade-api-go/v3/alpaca"
)

const easternTZName = "America/New_York"

// TradingSession is the session a scan measures against, resolved from Alpaca's
// calendar so half days and holidays are handled.
type TradingSession struct {
	Date           string // YYYY-MM-DD, ET
	Open           time.Time
	Close          time.Time
	PremarketStart time.Time
	Now            time.Time // Alpaca's clock, not the local machine's
	// BeforeOpen means the gap can only be projected from pre-market prints.
	BeforeOpen bool
}

// Mode describes how the gap for this session is being measured.
func (s TradingSession) Mode() string {
	if s.BeforeOpen {
		return "projected_premarket"
	}
	return "confirmed_open"
}

func easternLocation() (*time.Location, error) {
	loc, err := time.LoadLocation(easternTZName)
	if err != nil {
		return nil, fmt.Errorf("loading %s timezone: %w", easternTZName, err)
	}
	return loc, nil
}

// parseETTime combines a YYYY-MM-DD calendar date with an HH:MM wall clock in
// Eastern time.
func parseETTime(loc *time.Location, date, hhmm string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02 15:04", date+" "+hhmm, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing %q %q: %w", date, hhmm, err)
	}
	return t, nil
}

// ResolveSession picks the next session whose close is still ahead of us, so
// running after the bell targets the following trading day.
func ResolveSession(client *alpaca.Client, premarketStartET string) (TradingSession, error) {
	loc, err := easternLocation()
	if err != nil {
		return TradingSession{}, err
	}

	clock, err := retryTransient("market clock", client.GetClock)
	if err != nil {
		return TradingSession{}, fmt.Errorf("getting market clock: %w", err)
	}
	now := clock.Timestamp.In(loc)

	// A window either side of today covers weekends and long holiday breaks.
	req := alpaca.GetCalendarRequest{
		Start: now.AddDate(0, 0, -7),
		End:   now.AddDate(0, 0, 7),
	}
	days, err := retryTransient("market calendar", func() ([]alpaca.CalendarDay, error) {
		return client.GetCalendar(req)
	})
	if err != nil {
		return TradingSession{}, fmt.Errorf("getting market calendar: %w", err)
	}
	if len(days) == 0 {
		return TradingSession{}, fmt.Errorf("market calendar returned no days around %s", now.Format("2006-01-02"))
	}

	for _, day := range days {
		open, err := parseETTime(loc, day.Date, day.Open)
		if err != nil {
			return TradingSession{}, err
		}
		closeAt, err := parseETTime(loc, day.Date, day.Close)
		if err != nil {
			return TradingSession{}, err
		}
		if !now.Before(closeAt) {
			// Session already finished; look at the next one.
			continue
		}

		premarket, err := parseETTime(loc, day.Date, premarketStartET)
		if err != nil {
			return TradingSession{}, err
		}

		return TradingSession{
			Date:           day.Date,
			Open:           open,
			Close:          closeAt,
			PremarketStart: premarket,
			Now:            now,
			BeforeOpen:     now.Before(open),
		}, nil
	}

	return TradingSession{}, fmt.Errorf("no upcoming trading session found after %s", now.Format(time.RFC3339))
}

// buildSession assembles a session from one calendar day.
func buildSession(loc *time.Location, day alpaca.CalendarDay, now time.Time, premarketStartET string) (TradingSession, error) {
	open, err := parseETTime(loc, day.Date, day.Open)
	if err != nil {
		return TradingSession{}, err
	}
	closeAt, err := parseETTime(loc, day.Date, day.Close)
	if err != nil {
		return TradingSession{}, err
	}
	premarket, err := parseETTime(loc, day.Date, premarketStartET)
	if err != nil {
		return TradingSession{}, err
	}

	return TradingSession{
		Date:           day.Date,
		Open:           open,
		Close:          closeAt,
		PremarketStart: premarket,
		Now:            now,
		BeforeOpen:     now.Before(open),
	}, nil
}

// calendarAround fetches the trading calendar in a window either side of a day.
func calendarAround(client *alpaca.Client, day time.Time) ([]alpaca.CalendarDay, time.Time, error) {
	loc, err := easternLocation()
	if err != nil {
		return nil, time.Time{}, err
	}

	clock, err := retryTransient("market clock", client.GetClock)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("getting market clock: %w", err)
	}
	now := clock.Timestamp.In(loc)

	req := alpaca.GetCalendarRequest{
		Start: day.AddDate(0, 0, -14),
		End:   day.AddDate(0, 0, 7),
	}
	days, err := retryTransient("market calendar", func() ([]alpaca.CalendarDay, error) {
		return client.GetCalendar(req)
	})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("getting market calendar: %w", err)
	}
	if len(days) == 0 {
		return nil, time.Time{}, fmt.Errorf("market calendar returned no days around %s", day.Format("2006-01-02"))
	}
	return days, now, nil
}

// ResolveSessionByDate returns the session for an explicit YYYY-MM-DD.
func ResolveSessionByDate(client *alpaca.Client, date, premarketStartET string) (TradingSession, error) {
	loc, err := easternLocation()
	if err != nil {
		return TradingSession{}, err
	}
	target, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return TradingSession{}, fmt.Errorf("parsing date %q: %w", date, err)
	}

	days, now, err := calendarAround(client, target)
	if err != nil {
		return TradingSession{}, err
	}
	for _, day := range days {
		if day.Date == date {
			return buildSession(loc, day, now, premarketStartET)
		}
	}
	return TradingSession{}, fmt.Errorf("%s is not a trading day", date)
}

// ResolveLastCompletedSession returns the most recent session that has closed,
// which is the only kind that has a full intraday record to score.
func ResolveLastCompletedSession(client *alpaca.Client, premarketStartET string) (TradingSession, error) {
	loc, err := easternLocation()
	if err != nil {
		return TradingSession{}, err
	}

	days, now, err := calendarAround(client, time.Now().In(loc))
	if err != nil {
		return TradingSession{}, err
	}

	for i := len(days) - 1; i >= 0; i-- {
		closeAt, err := parseETTime(loc, days[i].Date, days[i].Close)
		if err != nil {
			return TradingSession{}, err
		}
		if now.Before(closeAt) {
			continue
		}
		return buildSession(loc, days[i], now, premarketStartET)
	}
	return TradingSession{}, fmt.Errorf("no completed session found before %s", now.Format(time.RFC3339))
}

// sessionDateOf renders a timestamp as an ET calendar date, which is how bars
// and sessions are keyed.
func sessionDateOf(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("2006-01-02")
}
