package domain

import (
	"math"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/pkg/timeutil"
)
func TestCalculateStats(t *testing.T) {
	trades := []Trade{
		{Status: StatusWin, ResultRR: 2.0, AssetType: AssetForex},
		{Status: StatusWin, ResultRR: 1.5, AssetType: AssetForex},
		{Status: StatusLoss, ResultRR: -1.0, AssetType: AssetGold},
		{Status: StatusWin, ResultRR: 3.0, AssetType: AssetGold},
		{Status: StatusBE, ResultRR: 0, AssetType: AssetForex},
		{Status: StatusOpen, AssetType: AssetCrypto},
	}

	s := CalculateStats(trades)

	if s.TotalTrades != 6 {
		t.Errorf("total: got %d, want 6", s.TotalTrades)
	}
	if s.Wins != 3 {
		t.Errorf("wins: got %d, want 3", s.Wins)
	}
	if s.Losses != 1 {
		t.Errorf("losses: got %d, want 1", s.Losses)
	}
	if s.BreakEvens != 1 {
		t.Errorf("be: got %d, want 1", s.BreakEvens)
	}
	if s.OpenTrades != 1 {
		t.Errorf("open: got %d, want 1", s.OpenTrades)
	}

	// Win rate: 3 wins / (3+1) closed = 75%
	if s.WinRate != 75.0 {
		t.Errorf("win rate: got %.1f, want 75.0", s.WinRate)
	}

	// Total RR: 2.0+1.5-1.0+3.0 = 5.5
	if s.TotalRR != 5.5 {
		t.Errorf("total RR: got %.1f, want 5.5", s.TotalRR)
	}

	if s.BestRR != 3.0 {
		t.Errorf("best: got %.1f, want 3.0", s.BestRR)
	}
	if s.WorstRR != -1.0 {
		t.Errorf("worst: got %.1f, want -1.0", s.WorstRR)
	}
}

func TestCalculateStats_Empty(t *testing.T) {
	s := CalculateStats(nil)
	if s.TotalTrades != 0 {
		t.Errorf("expected 0 trades, got %d", s.TotalTrades)
	}
	if s.WinRate != 0 {
		t.Errorf("expected 0 win rate, got %f", s.WinRate)
	}
}

// TestCalculateStats_BestWorstIgnoreOpenAndBE pins the invariant that an OPEN
// or BE trade's 0R never surfaces as the member's best or worst result, even
// when it appears first in the slice.
func TestCalculateStats_BestWorstIgnoreOpenAndBE(t *testing.T) {
	s := CalculateStats([]Trade{
		{Status: StatusOpen, AssetType: AssetForex},
		{Status: StatusBE, AssetType: AssetForex},
		{Status: StatusLoss, ResultRR: -1.5, AssetType: AssetGold},
		{Status: StatusWin, ResultRR: 2.0, AssetType: AssetGold},
	})
	if s.BestRR != 2.0 {
		t.Errorf("BestRR = %.1f, want 2.0 (not the OPEN trade's 0R)", s.BestRR)
	}
	if s.WorstRR != -1.5 {
		t.Errorf("WorstRR = %.1f, want -1.5 (not the OPEN trade's 0R)", s.WorstRR)
	}

	// Only-open trades: best/worst stay zero rather than reporting 0R as a result.
	s2 := CalculateStats([]Trade{{Status: StatusOpen}})
	if s2.BestRR != 0 || s2.WorstRR != 0 {
		t.Errorf("BestRR/WorstRR for open-only trades = %.1f/%.1f, want 0/0", s2.BestRR, s2.WorstRR)
	}
}

func TestDetectAssetType(t *testing.T) {
	tests := []struct {
		symbol string
		want   AssetType
	}{
		{"EURUSD", AssetForex},
		{"XAUUSD", AssetGold},
		{"NAS100", AssetIndices},
		{"BTCUSD", AssetCrypto},
		{"GBPJPY", AssetForex},
		{"GOLD", AssetGold},
		{"unknown", AssetForex}, // fallback
	}

	for _, tt := range tests {
		got := DetectAssetType(tt.symbol)
		if got != tt.want {
			t.Errorf("DetectAssetType(%q) = %q, want %q", tt.symbol, got, tt.want)
		}
	}
}

func TestCalculateStats_SessionSymbolsExpectancyProfitFactor(t *testing.T) {
	trades := []Trade{
		{Symbol: "XAUUSD", Status: StatusWin, ResultRR: 2.0, AssetType: AssetGold, TimeWindow: SessionLondon},
		{Symbol: "XAUUSD", Status: StatusWin, ResultRR: 3.0, AssetType: AssetGold, TimeWindow: SessionLondon},
		{Symbol: "EURUSD", Status: StatusLoss, ResultRR: -1.0, AssetType: AssetForex, TimeWindow: SessionAsia},
		{Symbol: "XAUUSD", Status: StatusBE, ResultRR: 0, AssetType: AssetGold, TimeWindow: SessionNYAM},
	}

	s := CalculateStats(trades)

	// GrossWin = 5.0, GrossLoss = 1.0 -> ProfitFactor = 5.0
	if math.Abs(s.ProfitFactor-5.0) > 1e-6 {
		t.Errorf("ProfitFactor = %.2f, want 5.00", s.ProfitFactor)
	}

	// Expectancy over 4 closed trades (2W + 1L + 1BE = 4): 4.0 / 4 = 1.0R
	if math.Abs(s.Expectancy-1.0) > 1e-6 {
		t.Errorf("Expectancy = %.2f, want 1.00", s.Expectancy)
	}

	london, ok := s.BySession[SessionLondon]
	if !ok || london.Total != 2 || math.Abs(london.TotalRR-5.0) > 1e-6 || math.Abs(london.WinRate-100.0) > 1e-6 {
		t.Errorf("BySession[London] = %+v, want 2 trades, +5.0R, 100%% WR", london)
	}

	if len(s.TopSymbols) == 0 || s.TopSymbols[0].Symbol != "XAUUSD" || s.TopSymbols[0].Total != 3 {
		t.Errorf("TopSymbols = %+v, want XAUUSD first with 3 trades", s.TopSymbols)
	}
}

func TestFilterTradesByPeriodAndConsecutiveLossesToday(t *testing.T) {
	wib := time.FixedZone("WIB", 7*3600)
	now := time.Date(2026, 10, 10, 15, 0, 0, 0, wib) // Saturday

	trades := []Trade{
		{ID: "1", Date: now, Status: StatusLoss, ResultRR: -1},
		{ID: "2", Date: now.Add(-1 * time.Hour), Status: StatusLoss, ResultRR: -1},
		{ID: "3", Date: now.Add(-2 * time.Hour), Status: StatusLoss, ResultRR: -1},
		{ID: "4", Date: now.AddDate(0, 0, -3), Status: StatusWin, ResultRR: 2},   // Earlier this week/month
		{ID: "5", Date: now.AddDate(0, -1, 0), Status: StatusWin, ResultRR: 3},   // Last month
	}

	if got := ConsecutiveLossesToday(trades, now); got != 3 {
		t.Errorf("ConsecutiveLossesToday = %d, want 3", got)
	}

	weekTrades := FilterTradesByPeriod(trades, "week", now)
	if len(weekTrades) != 4 {
		t.Errorf("FilterTradesByPeriod(week) count = %d, want 4", len(weekTrades))
	}

	monthTrades := FilterTradesByPeriod(trades, "month", now)
	if len(monthTrades) != 4 {
		t.Errorf("FilterTradesByPeriod(month) count = %d, want 4", len(monthTrades))
	}

	allTrades := FilterTradesByPeriod(trades, "all", now)
	if len(allTrades) != 5 {
		t.Errorf("FilterTradesByPeriod(all) count = %d, want 5", len(allTrades))
	}
}

// TestWIBBoundariesAreLocationIndependent pins the invariant that every
// day/week/month boundary is derived in WIB no matter which location the caller
// passes in. A caller handing over a UTC instant must not shift the boundary.
func TestWIBBoundariesAreLocationIndependent(t *testing.T) {
	// 2026-10-10 23:30 UTC == 2026-10-11 06:30 WIB (Sunday in WIB, Saturday in UTC).
	utcNow := time.Date(2026, 10, 10, 23, 30, 0, 0, time.UTC)
	wibNow := utcNow.In(timeutil.WIB)

	if got, want := timeutil.StartOfWeek(utcNow), timeutil.StartOfWeek(wibNow); !got.Equal(want) {
		t.Errorf("StartOfWeek differs by location: utc=%s wib=%s", got, want)
	}
	// The week starts Monday 2026-10-05 00:00 WIB, not Sunday 2026-10-04.
	wantMonday := time.Date(2026, 10, 5, 0, 0, 0, 0, timeutil.WIB)
	if got := timeutil.StartOfWeek(utcNow); !got.Equal(wantMonday) {
		t.Errorf("StartOfWeek = %s, want %s", got, wantMonday)
	}

	// A trade recorded at 06:30 WIB on Sunday belongs to that same WIB day.
	trade := Trade{ID: "1", Date: wibNow, Status: StatusLoss, ResultRR: -1}
	if got := ConsecutiveLossesToday([]Trade{trade}, utcNow); got != 1 {
		t.Errorf("ConsecutiveLossesToday(utc caller) = %d, want 1", got)
	}

	// Week filter must include the Sunday trade for both callers.
	if got := len(FilterTradesByPeriod([]Trade{trade}, "week", utcNow)); got != 1 {
		t.Errorf("FilterTradesByPeriod(week, utc) = %d trades, want 1", got)
	}

	// YearWeekString must agree regardless of the caller's location.
	if got, want := YearWeekString(utcNow), YearWeekString(wibNow); got != want {
		t.Errorf("YearWeekString differs by location: utc=%s wib=%s", got, want)
	}
	if got, want := YearWeekString(utcNow), "2026-W41"; got != want {
		t.Errorf("YearWeekString = %s, want %s", got, want)
	}
}

func TestCalculateStats_EquityCurveAndMaxDrawdown(t *testing.T) {
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, timeutil.WIB)
	// Chronological sequence: +3R -> -2R (drawdown 2R from peak +3R) -> +4R (new peak +5R)
	trades := []Trade{
		{ID: "3", Date: base.Add(4 * time.Hour), Symbol: "XAUUSD", TimeWindow: SessionLondon, Status: StatusWin, ResultRR: 4.0},
		{ID: "1", Date: base, Symbol: "XAUUSD", TimeWindow: SessionAsia, Status: StatusWin, ResultRR: 3.0},
		{ID: "2", Date: base.Add(2 * time.Hour), Symbol: "EURUSD", TimeWindow: SessionLondon, Status: StatusLoss, ResultRR: -2.0},
	}

	s := CalculateStats(trades)
	wantCurve := []float64{3.0, 1.0, 5.0}
	if len(s.EquityCurve) != len(wantCurve) {
		t.Fatalf("EquityCurve len = %d, want %d (%v)", len(s.EquityCurve), len(wantCurve), s.EquityCurve)
	}
	for i := range wantCurve {
		if math.Abs(s.EquityCurve[i]-wantCurve[i]) > 1e-6 {
			t.Errorf("EquityCurve[%d] = %.2f, want %.2f", i, s.EquityCurve[i], wantCurve[i])
		}
	}
	if math.Abs(s.MaxDrawdownR-2.0) > 1e-6 {
		t.Errorf("MaxDrawdownR = %.2f, want 2.00", s.MaxDrawdownR)
	}

	filtered := FilterTrades(trades, StatsFilter{Period: "all", Symbol: "xau/usd", Session: SessionLondon}, base.Add(5*time.Hour))
	if len(filtered) != 1 || filtered[0].ID != "3" {
		t.Fatalf("FilterTrades(XAUUSD, London) = %+v, want [ID=3]", filtered)
	}
}
