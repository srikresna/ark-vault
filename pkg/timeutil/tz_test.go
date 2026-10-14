package timeutil

import (
	"testing"
	"time"
)

// TestPreviousYearMonth pins month-end arithmetic: AddDate(0,-1,0) normalizes
// day overflow (31 Mar → 3 Mar), which made "/reportcard" on the 29th–31st
// resolve to the CURRENT month instead of the previous one. Anchoring on the
// 1st before subtracting must always yield the previous calendar month.
func TestPreviousYearMonth(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		want string
	}{
		{"mid month", time.Date(2026, 10, 14, 9, 0, 0, 0, WIB), "2026-09"},
		{"march 31 over 30-day month", time.Date(2026, 3, 31, 23, 0, 0, 0, WIB), "2026-02"},
		{"may 31 over 30-day month", time.Date(2026, 5, 31, 0, 30, 0, 0, WIB), "2026-04"},
		{"october 31 over 30-day month", time.Date(2026, 10, 31, 12, 0, 0, 0, WIB), "2026-09"},
		{"march 29 over non-leap february", time.Date(2027, 3, 29, 12, 0, 0, 0, WIB), "2027-02"},
		{"march 29 over leap february", time.Date(2028, 3, 29, 12, 0, 0, 0, WIB), "2028-02"},
		{"january 31 wraps year", time.Date(2026, 1, 31, 12, 0, 0, 0, WIB), "2025-12"},
		{"first of month", time.Date(2026, 11, 1, 0, 0, 0, 0, WIB), "2026-10"},
		{"non-WIB input normalized", time.Date(2026, 3, 31, 10, 0, 0, 0, time.UTC), "2026-02"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PreviousYearMonth(tt.now); got != tt.want {
				t.Errorf("PreviousYearMonth(%s) = %q, want %q", tt.now, got, tt.want)
			}
		})
	}
}
