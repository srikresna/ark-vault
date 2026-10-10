package service

import (
	"context"
	"fmt"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)
// WeeklySummary holds aggregated data for a weekly community report.
type WeeklySummary struct {
	PeriodStart  time.Time
	PeriodEnd    time.Time
	TotalTrades  int
	TotalMembers int
	CommunityWR  float64
	TotalRR      float64
	MostTraded   string // most popular symbol
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
	var allWins, allClosed int
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
		allWins += stats.Wins
		allClosed += stats.Wins + stats.Losses

		entries = append(entries, LeaderboardEntry{
			Username:    m.Username,
			FirstName:   m.FirstName,
			TelegramID:  m.TelegramID,
			WinRate:     stats.WinRate,
			TotalRR:     stats.TotalRR,
			TotalTrades: stats.TotalTrades,
		})
	}

	if allClosed > 0 {
		summary.CommunityWR = float64(allWins) / float64(allClosed) * 100
	}

	// Find most traded symbol
	maxCount := 0
	for sym, count := range symbolCount {
		if count > maxCount {
			maxCount = count
			summary.MostTraded = sym
		}
	}

	// Top 3 by RR
	topN := 3
	if len(entries) < topN {
		topN = len(entries)
	}
	// Simple sort by RR
	for i := 0; i < topN; i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].TotalRR > entries[i].TotalRR {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
	summary.TopPerformers = entries[:topN]

	return summary, nil
}
