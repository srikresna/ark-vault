package service

import (
	"context"
	"sort"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// BadgeService checks badge conditions and awards badges.
type BadgeService struct {
	store  BadgeStore
	trades TradeRepository
	gamSvc *GamificationService
}

// NewBadgeService creates a new BadgeService.
func NewBadgeService(store BadgeStore, trades TradeRepository, gamSvc *GamificationService) *BadgeService {
	return &BadgeService{
		store:  store,
		trades: trades,
		gamSvc: gamSvc,
	}
}

// CheckAndAwardBadges checks all badge conditions after a trade is recorded.
// Returns newly awarded badges.
func (s *BadgeService) CheckAndAwardBadges(ctx context.Context, memberID int64, trade *domain.Trade, streak *domain.StreakData) ([]domain.BadgeAward, error) {
	// 1. Get all trades for this member.
	trades, err := s.trades.GetTrades(ctx, memberID)
	if err != nil {
		return nil, err
	}

	totalTrades := len(trades)

	// 2. Sort trades by date descending for streak calculation.
	sort.Slice(trades, func(i, j int) bool {
		return trades[i].Date.After(trades[j].Date)
	})

	// Calculate win streak from recent trades.
	winStreak := 0
	for _, t := range trades {
		if t.Status == domain.StatusWin {
			winStreak++
		} else {
			break
		}
	}

	// 3. Calculate win rate (closed trades only: win + loss + BE).
	var wins, closedTrades int
	for _, t := range trades {
		switch t.Status {
		case domain.StatusWin:
			wins++
			closedTrades++
		case domain.StatusLoss, domain.StatusBE:
			closedTrades++
		}
	}
	var winRate float64
	if closedTrades > 0 {
		winRate = float64(wins) / float64(closedTrades) * 100
	}

	// 4. Calculate monthly RR (current month in WIB).
	now := time.Now().In(wib)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, wib)
	var monthRR float64
	for _, t := range trades {
		if !t.Date.Before(monthStart) {
			monthRR += t.ResultRR
		}
	}

	// 5. Build condition map.
	type badgeCheck struct {
		id   domain.BadgeID
		cond bool
	}
	checks := []badgeCheck{
		{domain.BadgeFirstTrade, totalTrades >= 1},
		{domain.Badge10Trades, totalTrades >= 10},
		{domain.Badge50Trades, totalTrades >= 50},
		{domain.Badge100Trades, totalTrades >= 100},
		{domain.BadgeWinStreak3, winStreak >= 3},
		{domain.BadgeWinStreak5, winStreak >= 5},
		{domain.BadgeStreak7, streak != nil && streak.CurrentStreak >= 7},
		{domain.BadgeStreak30, streak != nil && streak.CurrentStreak >= 30},
		{domain.BadgeFirstGreenMo, monthRR > 0},
		{domain.Badge10RMonth, monthRR >= 10},
		{domain.BadgeWinrate60, winRate >= 60 && closedTrades >= 20},
	}

	// 6. Check each condition and award if not already earned.
	var awarded []domain.BadgeAward
	for _, chk := range checks {
		if !chk.cond {
			continue
		}

		has, err := s.store.HasBadge(ctx, memberID, chk.id)
		if err != nil {
			return nil, err
		}
		if has {
			continue
		}

		award := &domain.BadgeAward{
			TelegramID: memberID,
			BadgeID:    chk.id,
			AwardedAt:  now,
		}
		if err := s.store.AwardBadge(ctx, award); err != nil {
			return nil, err
		}

		// Award XP for earning a badge.
		if _, err := s.gamSvc.AwardXP(ctx, memberID, domain.XPBadgeEarned, "badge:"+string(chk.id)); err != nil {
			return nil, err
		}

		awarded = append(awarded, *award)
	}

	return awarded, nil
}

// GetBadges returns all earned badges for a member.
func (s *BadgeService) GetBadges(ctx context.Context, memberID int64) ([]domain.BadgeAward, error) {
	return s.store.GetBadges(ctx, memberID)
}

// AwardGoalBadge awards the goal_achiever badge to a member.
// Returns the award if newly granted, or nil if already earned.
func (s *BadgeService) AwardGoalBadge(ctx context.Context, memberID int64) (*domain.BadgeAward, error) {
	has, err := s.store.HasBadge(ctx, memberID, domain.BadgeGoalAchiever)
	if err != nil {
		return nil, err
	}
	if has {
		return nil, nil
	}

	now := time.Now().In(wib)
	award := &domain.BadgeAward{
		TelegramID: memberID,
		BadgeID:    domain.BadgeGoalAchiever,
		AwardedAt:  now,
	}
	if err := s.store.AwardBadge(ctx, award); err != nil {
		return nil, err
	}
	if _, err := s.gamSvc.AwardXP(ctx, memberID, domain.XPBadgeEarned, "badge:goal_achiever"); err != nil {
		return nil, err
	}
	return award, nil
}

// AwardChallengeBadge awards the challenge_winner badge to a member.
// Returns the award if newly granted, or nil if already earned.
func (s *BadgeService) AwardChallengeBadge(ctx context.Context, memberID int64) (*domain.BadgeAward, error) {
	has, err := s.store.HasBadge(ctx, memberID, domain.BadgeChallengeWin)
	if err != nil {
		return nil, err
	}
	if has {
		return nil, nil
	}

	now := time.Now().In(wib)
	award := &domain.BadgeAward{
		TelegramID: memberID,
		BadgeID:    domain.BadgeChallengeWin,
		AwardedAt:  now,
	}
	if err := s.store.AwardBadge(ctx, award); err != nil {
		return nil, err
	}
	if _, err := s.gamSvc.AwardXP(ctx, memberID, domain.XPBadgeEarned, "badge:challenge_winner"); err != nil {
		return nil, err
	}
	return award, nil
}
