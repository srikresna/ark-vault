package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
)

// LeaderboardEntry represents one member's ranking.
type LeaderboardEntry struct {
	Username    string
	FirstName   string
	TelegramID  int64
	WinRate     float64
	TotalRR     float64
	TotalTrades int
}

// LeaderboardService computes community rankings.
type LeaderboardService struct {
	trades  TradeRepository
	members MemberRepository
}

// NewLeaderboardService creates a new LeaderboardService.
func NewLeaderboardService(tr TradeRepository, mr MemberRepository) *LeaderboardService {
	return &LeaderboardService{trades: tr, members: mr}
}

// GetLeaderboard returns top N members sorted by the given metric.
// metric: "winrate" or "rr". minTrades is the minimum trade count to qualify.
func (s *LeaderboardService) GetLeaderboard(ctx context.Context, metric string, topN, minTrades int) ([]LeaderboardEntry, error) {
	return s.GetLeaderboardByPeriod(ctx, metric, "all", topN, minTrades)
}

// GetLeaderboardByPeriod returns top N members sorted by metric ("winrate" or "rr") for a given period ("week", "month", "all").
func (s *LeaderboardService) GetLeaderboardByPeriod(ctx context.Context, metric, period string, topN, minTrades int) ([]LeaderboardEntry, error) {
	members, err := s.members.ListMembers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}

	now := time.Now().In(timeutil.WIB)
	var entries []LeaderboardEntry
	for _, m := range members {
		trades, err := s.trades.GetTrades(ctx, m.TelegramID)
		if err != nil {
			continue
		}
		filtered := domain.FilterTradesByPeriod(trades, period, now)
		stats := domain.CalculateStats(filtered)
		closedTrades := domain.ClosedTrades(stats.Wins, stats.Losses, stats.BreakEvens)
		if closedTrades < minTrades {
			continue
		}
		entries = append(entries, LeaderboardEntry{
			Username:    m.Username,
			FirstName:   m.FirstName,
			TelegramID:  m.TelegramID,
			WinRate:     stats.WinRate,
			TotalRR:     stats.TotalRR,
			TotalTrades: closedTrades,
		})
	}

	switch metric {
	case "rr":
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].TotalRR > entries[j].TotalRR
		})
	default: // winrate
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].WinRate > entries[j].WinRate
		})
	}

	if topN > 0 && len(entries) > topN {
		entries = entries[:topN]
	}
	return entries, nil
}
