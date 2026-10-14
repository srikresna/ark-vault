package timeutil

import "time"

// WIB is the canonical timezone for every user-facing date boundary in the
// application: daily streaks, the daily trade-XP cap, period filters
// (week/month), reminders, weekly challenges, monthly goals and report cards.
//
// Asia/Jakarta has been a fixed UTC+7 offset with no DST since 1964, so a fixed
// zone is used instead of time.LoadLocation. This keeps the invariant
// deterministic, requires no tzdata in the runtime image, and cannot silently
// fall back to UTC (which would shift every day boundary by seven hours).
//
// Callers MUST evaluate date boundaries in this location; domain helpers
// normalise their inputs with .In(WIB) so a caller passing UTC or a bare
// time.Now() cannot shift a day, week or month boundary.
var WIB = time.FixedZone("WIB", 7*60*60)

// StartOfWeek returns Monday 00:00 WIB of the week containing now.
//
// The location is fixed to WIB on purpose: an earlier signature accepted an
// arbitrary *time.Location, and passing time.UTC there silently produced
// UTC-aligned weeks for reports and challenges.
func StartOfWeek(now time.Time) time.Time {
	t := now.In(WIB)
	weekday := int(t.Weekday())
	if weekday == 0 {
		weekday = 7 // Sunday → 7 so Monday starts the week
	}
	monday := t.AddDate(0, 0, -(weekday - 1))
	return time.Date(monday.Year(), monday.Month(), monday.Day(), 0, 0, 0, 0, WIB)
}

// PreviousYearMonth returns the "2006-01" key of the calendar month before
// that of t (evaluated in WIB).
//
// Subtracting a month with AddDate(0, -1, 0) directly on t normalizes day
// overflow — on 31 March it yields 3 March, i.e. the CURRENT month. Anchoring
// on the 1st before subtracting keeps the result on the previous month for
// every day of the month.
func PreviousYearMonth(t time.Time) string {
	t = t.In(WIB)
	first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, WIB)
	return first.AddDate(0, -1, 0).Format("2006-01")
}
