package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// WeeklySummary holds aggregated data for a weekly community report.
type WeeklySummary struct {
	PeriodStart   time.Time
	PeriodEnd     time.Time
	TotalTrades   int
	TotalMembers  int
	CommunityWR   float64 // wins / (wins+losses+BE) across the week; OPEN excluded
	TotalRR       float64
	MostTraded    string // most popular symbol
	TopPerformers []LeaderboardEntry
}

// ReportService generates community summary reports.
type ReportService struct {
	trades  TradeRepository
	members MemberRepository
}

// NewReportService creates a ReportService.
func NewReportService(tr TradeRepository, mr MemberRepository) *ReportService {
	return &ReportService{trades: tr, members: mr}
}

// GenerateWeeklySummary computes a summary for the current week.
func (s *ReportService) GenerateWeeklySummary(ctx context.Context, weekStart, weekEnd time.Time) (*WeeklySummary, error) {
	members, err := s.members.ListMembers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}

	summary := &WeeklySummary{
		PeriodStart: weekStart,
		PeriodEnd:   weekEnd,
	}

	symbolCount := make(map[string]int)
	var community domain.ResultCounts
	var entries []LeaderboardEntry

	for _, m := range members {
		trades, err := s.trades.GetTrades(ctx, m.TelegramID)
		if err != nil {
			continue
		}

		// Filter trades within this week
		var weekTrades []domain.Trade
		for _, t := range trades {
			if !t.Date.Before(weekStart) && t.Date.Before(weekEnd) {
				weekTrades = append(weekTrades, t)
				symbolCount[t.Symbol]++
			}
		}

		if len(weekTrades) == 0 {
			continue
		}

		summary.TotalMembers++
		stats := domain.CalculateStats(weekTrades)
		summary.TotalTrades += stats.TotalTrades
		summary.TotalRR += stats.TotalRR
		community.Wins += stats.Wins
		community.Losses += stats.Losses
		community.BreakEvens += stats.BreakEvens

		entries = append(entries, LeaderboardEntry{
			Username:    m.Username,
			FirstName:   m.FirstName,
			TelegramID:  m.TelegramID,
			WinRate:     stats.WinRate,
			TotalRR:     stats.TotalRR,
			TotalTrades: stats.TotalTrades,
		})
	}

	summary.CommunityWR = community.WinRate()

	// Find most traded symbol
	maxCount := 0
	for sym, count := range symbolCount {
		if count > maxCount {
			maxCount = count
			summary.MostTraded = sym
		}
	}

	// Top 3 by RR
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].TotalRR > entries[j].TotalRR
	})
	topN := 3
	if len(entries) < topN {
		topN = len(entries)
	}
	summary.TopPerformers = entries[:topN]

	return summary, nil
}
