package domain

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/arkcode369/ark-vault/pkg/timeutil"
)

// Stats holds computed statistics for a single member.
type Stats struct {
	TotalTrades  int
	Wins         int
	Losses       int
	BreakEvens   int
	OpenTrades   int
	WinRate      float64 // percentage 0-100 over closed trades (see WinRate)
	TotalRR      float64
	AvgRR        float64
	Expectancy   float64 // average R per closed trade (Wins+Losses+BE)
	ProfitFactor float64 // gross win RR / |gross loss RR|
	BestRR       float64
	WorstRR      float64
	CurStreak    int // positive = win streak, negative = loss streak
	MaxWinStrk   int
	ByAsset      map[AssetType]AssetStats
	BySession    map[TimeWindow]AssetStats
	TopSymbols   []SymbolStats
	EquityCurve  []float64 // chronological cumulative R after each closed trade
	MaxDrawdownR float64   // largest peak-to-trough drop in cumulative R
}

// StatsFilter specifies optional period, symbol, and session filters for stats.
type StatsFilter struct {
	Period  string     // "all", "week", "month"
	Symbol  string     // optional pair/symbol filter (case-insensitive)
	Session TimeWindow // optional session filter
}

// AssetStats is a per-asset-type or per-session breakdown.
type AssetStats struct {
	Total      int // every trade in the bucket, OPEN included
	Wins       int
	Losses     int
	BreakEvens int
	TotalRR    float64
	WinRate    float64 // wins / (wins+losses+BE); 0 when nothing is closed
}

// SymbolStats is a per-symbol breakdown.
type SymbolStats struct {
	Symbol     string
	Total      int // every trade for the symbol, OPEN included
	Wins       int
	Losses     int
	BreakEvens int
	TotalRR    float64
	WinRate    float64 // wins / (wins+losses+BE); 0 when nothing is closed
}

// ResultCounts tallies trade outcomes by status.
type ResultCounts struct {
	Wins       int
	Losses     int
	BreakEvens int
	Open       int
}

// CountResults tallies the statuses of trades. Trades with an unknown status
// are ignored, matching CalculateStats.
func CountResults(trades []Trade) ResultCounts {
	var c ResultCounts
	for _, t := range trades {
		switch t.Status {
		case StatusWin:
			c.Wins++
		case StatusLoss:
			c.Losses++
		case StatusBE:
			c.BreakEvens++
		case StatusOpen:
			c.Open++
		}
	}
	return c
}

// ClosedTrades returns the number of closed trades: wins + losses + break-evens.
func ClosedTrades(wins, losses, breakEvens int) int {
	return wins + losses + breakEvens
}

// StatsFormulaVersion identifies the current win-rate/statistics definition.
//
// It is mixed into every cached artifact derived from trades (AI analytics,
// monthly report cards) so a formula change invalidates stale output instead of
// serving numbers computed under the previous definition. Bump it whenever the
// meaning of a cached statistic changes.
const StatsFormulaVersion = 2

// WinRate returns the win percentage over closed trades.
//
// Every closed trade counts in the denominator — wins, losses, and break-evens
// alike — while OPEN trades never do. This is the single definition of win rate
// used by stats, report cards, challenges, goals, badges, leaderboards,
// analytics, and community reports. It returns 0 when nothing is closed.
func WinRate(wins, losses, breakEvens int) float64 {
	if closed := ClosedTrades(wins, losses, breakEvens); closed > 0 {
		return float64(wins) / float64(closed) * 100
	}
	return 0
}

// Closed returns the number of closed trades (WIN+LOSS+BE).
func (c ResultCounts) Closed() int { return ClosedTrades(c.Wins, c.Losses, c.BreakEvens) }

// WinRate returns the counts' win percentage over their closed trades.
func (c ResultCounts) WinRate() float64 { return WinRate(c.Wins, c.Losses, c.BreakEvens) }

// CalculateStats computes statistics from a slice of trades.
func CalculateStats(trades []Trade) Stats {
	s := Stats{
		ByAsset:   make(map[AssetType]AssetStats),
		BySession: make(map[TimeWindow]AssetStats),
	}
	symbolMap := make(map[string]*SymbolStats)
	var rrSum, grossWin, grossLoss float64
	var rrCount int
	var curStreak, maxWin int

	bestInit := false
	for _, t := range trades {
		s.TotalTrades++

		// Per-asset accumulator
		as := s.ByAsset[t.AssetType]
		as.Total++

		switch t.Status {
		case StatusWin:
			s.Wins++
			as.Wins++
			as.TotalRR += t.ResultRR
			s.TotalRR += t.ResultRR
			grossWin += t.ResultRR
			if curStreak > 0 {
				curStreak++
			} else {
				curStreak = 1
			}
			if curStreak > maxWin {
				maxWin = curStreak
			}
		case StatusLoss:
			s.Losses++
			as.Losses++
			as.TotalRR += t.ResultRR // negative value
			s.TotalRR += t.ResultRR
			grossLoss += math.Abs(t.ResultRR)
			if curStreak < 0 {
				curStreak--
			} else {
				curStreak = -1
			}
		case StatusBE:
			s.BreakEvens++
			as.BreakEvens++
			curStreak = 0
		case StatusOpen:
			s.OpenTrades++
		}

		if t.ResultRR != 0 {
			rrSum += t.ResultRR
			rrCount++
		}

		// Best/worst only over decisive results: an OPEN or BE trade's 0R must
		// never surface as the member's best or worst result.
		if t.Status == StatusWin || t.Status == StatusLoss {
			if !bestInit {
				s.BestRR = t.ResultRR
				s.WorstRR = t.ResultRR
				bestInit = true
			} else {
				if t.ResultRR > s.BestRR {
					s.BestRR = t.ResultRR
				}
				if t.ResultRR < s.WorstRR {
					s.WorstRR = t.ResultRR
				}
			}
		}

		// Update per-asset win rate over closed trades (OPEN excluded).
		as.WinRate = WinRate(as.Wins, as.Losses, as.BreakEvens)
		s.ByAsset[t.AssetType] = as

		// Per-session accumulator
		if t.TimeWindow != "" {
			ss := s.BySession[t.TimeWindow]
			ss.Total++
			switch t.Status {
			case StatusWin:
				ss.Wins++
				ss.TotalRR += t.ResultRR
			case StatusLoss:
				ss.Losses++
				ss.TotalRR += t.ResultRR
			case StatusBE:
				ss.BreakEvens++
			}
			ss.WinRate = WinRate(ss.Wins, ss.Losses, ss.BreakEvens)
			s.BySession[t.TimeWindow] = ss
		}

		// Per-symbol accumulator
		if t.Symbol != "" {
			sym, ok := symbolMap[t.Symbol]
			if !ok {
				sym = &SymbolStats{Symbol: t.Symbol}
				symbolMap[t.Symbol] = sym
			}
			sym.Total++
			switch t.Status {
			case StatusWin:
				sym.Wins++
				sym.TotalRR += t.ResultRR
			case StatusLoss:
				sym.Losses++
				sym.TotalRR += t.ResultRR
			case StatusBE:
				sym.BreakEvens++
			}
			sym.WinRate = WinRate(sym.Wins, sym.Losses, sym.BreakEvens)
		}
	}

	s.WinRate = WinRate(s.Wins, s.Losses, s.BreakEvens)
	if rrCount > 0 {
		s.AvgRR = rrSum / float64(rrCount)
	}
	if totalClosedWithBE := ClosedTrades(s.Wins, s.Losses, s.BreakEvens); totalClosedWithBE > 0 {
		s.Expectancy = s.TotalRR / float64(totalClosedWithBE)
	}
	if grossLoss > 0 {
		s.ProfitFactor = grossWin / grossLoss
	}

	for _, sym := range symbolMap {
		s.TopSymbols = append(s.TopSymbols, *sym)
	}
	sort.Slice(s.TopSymbols, func(i, j int) bool {
		if s.TopSymbols[i].Total != s.TopSymbols[j].Total {
			return s.TopSymbols[i].Total > s.TopSymbols[j].Total
		}
		return s.TopSymbols[i].TotalRR > s.TopSymbols[j].TotalRR
	})
	if len(s.TopSymbols) > 3 {
		s.TopSymbols = s.TopSymbols[:3]
	}

	s.CurStreak = curStreak
	s.MaxWinStrk = maxWin

	// Compute chronological EquityCurve and MaxDrawdownR over closed trades.
	var closedTrades []Trade
	for _, t := range trades {
		if t.Status == StatusWin || t.Status == StatusLoss || t.Status == StatusBE {
			closedTrades = append(closedTrades, t)
		}
	}
	sort.SliceStable(closedTrades, func(i, j int) bool {
		return closedTrades[i].Date.Before(closedTrades[j].Date)
	})
	var cumR, peakR, maxDD float64
	for _, t := range closedTrades {
		cumR += t.ResultRR
		s.EquityCurve = append(s.EquityCurve, cumR)
		if cumR > peakR {
			peakR = cumR
		}
		if dd := peakR - cumR; dd > maxDD {
			maxDD = dd
		}
	}
	s.MaxDrawdownR = maxDD

	return s
}

// FilterTradesByPeriod filters trades by "week", "month", or "all" relative to now.
//
// now is normalised to WIB before any boundary is derived, so the period is
// always the user's calendar week/month regardless of the caller's location.
func FilterTradesByPeriod(trades []Trade, period string, now time.Time) []Trade {
	now = now.In(timeutil.WIB)
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "week", "weekly":
		start := timeutil.StartOfWeek(now)
		var out []Trade
		for _, t := range trades {
			if !t.Date.Before(start) {
				out = append(out, t)
			}
		}
		return out
	case "month", "monthly":
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, timeutil.WIB)
		var out []Trade
		for _, t := range trades {
			if !t.Date.Before(start) {
				out = append(out, t)
			}
		}
		return out
	default:
		return trades
	}
}

// FilterTrades applies period, symbol, and session filters to trades.
func FilterTrades(trades []Trade, f StatsFilter, now time.Time) []Trade {
	periodFiltered := FilterTradesByPeriod(trades, f.Period, now)
	if f.Symbol == "" && f.Session == "" {
		return periodFiltered
	}
	wantSym := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(f.Symbol), "/", ""))
	wantSess := strings.ToLower(strings.TrimSpace(string(f.Session)))
	var out []Trade
	for _, t := range periodFiltered {
		if wantSym != "" && !strings.EqualFold(t.Symbol, wantSym) {
			continue
		}
		if wantSess != "" && !strings.EqualFold(string(t.TimeWindow), wantSess) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// ConsecutiveLossesToday counts consecutive LOSS trades recorded on the same WIB day as now (from most recent).
func ConsecutiveLossesToday(trades []Trade, now time.Time) int {
	now = now.In(timeutil.WIB)
	dayStr := now.Format("2006-01-02")
	loc := timeutil.WIB

	var todayTrades []Trade
	for _, t := range trades {
		if t.Date.In(loc).Format("2006-01-02") == dayStr {
			todayTrades = append(todayTrades, t)
		}
	}
	sort.Slice(todayTrades, func(i, j int) bool {
		return todayTrades[i].Date.After(todayTrades[j].Date)
	})

	losses := 0
	for _, t := range todayTrades {
		if t.Status == StatusLoss {
			losses++
		} else if t.Status == StatusWin || t.Status == StatusBE {
			break
		}
	}
	return losses
}
