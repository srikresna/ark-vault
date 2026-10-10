package service

import (
	"context"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// GoalService manages monthly goals, progress tracking, and goal completion.
type GoalService struct {
	store    GoalStore
	trades   TradeRepository
	gamSvc   *GamificationService
	badgeSvc *BadgeService
}

// NewGoalService creates a new GoalService.
func NewGoalService(store GoalStore, trades TradeRepository, gamSvc *GamificationService, badgeSvc *BadgeService) *GoalService {
	return &GoalService{
		store:    store,
		trades:   trades,
		gamSvc:   gamSvc,
		badgeSvc: badgeSvc,
	}
}

// currentYearMonth returns the current year-month string in WIB.
func currentYearMonth() string {
	return time.Now().In(wib).Format("2006-01")
}

// monthStart returns the first moment of the current month in WIB.
func monthStart() time.Time {
	now := time.Now().In(wib)
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, wib)
}

// SetGoal creates or updates a monthly goal for the user in the current month.
func (s *GoalService) SetGoal(ctx context.Context, telegramID int64, goalType domain.GoalType, targetValue float64) (*domain.MonthlyGoal, error) {
	ym := currentYearMonth()
	now := time.Now().In(wib)

	// Check for an existing goal this month.
	existing, err := s.store.GetGoal(ctx, telegramID, ym)
	if err != nil {
		return nil, err
	}

	if existing != nil {
		// Update the existing goal (only if not yet achieved).
		if !existing.Achieved {
			existing.GoalType = goalType
			existing.TargetValue = targetValue
		}
		if err := s.store.SaveGoal(ctx, existing); err != nil {
			return nil, err
		}
		return existing, nil
	}

	goal := &domain.MonthlyGoal{
		TelegramID:  telegramID,
		YearMonth:   ym,
		GoalType:    goalType,
		TargetValue: targetValue,
		CreatedAt:   now,
	}
	if err := s.store.SaveGoal(ctx, goal); err != nil {
		return nil, err
	}
	return goal, nil
}

// GetProgress returns the current progress toward the user's monthly goal.
// Returns nil if no goal is set for the current month.
func (s *GoalService) GetProgress(ctx context.Context, telegramID int64) (*domain.GoalProgress, error) {
	ym := currentYearMonth()
	goal, err := s.store.GetGoal(ctx, telegramID, ym)
	if err != nil {
		return nil, err
	}
	if goal == nil {
		return nil, nil
	}

	currentValue, err := s.calculateCurrentValue(ctx, telegramID, goal.GoalType)
	if err != nil {
		return nil, err
	}

	var pct float64
	if goal.TargetValue > 0 {
		pct = (currentValue / goal.TargetValue) * 100
	}

	return &domain.GoalProgress{
		Goal:         goal,
		CurrentValue: currentValue,
		Percentage:   pct,
	}, nil
}

// CheckGoalCompletion checks if the user achieved their monthly goal.
// Call this after each trade is recorded. Returns true if the goal was
// newly completed on this call.
func (s *GoalService) CheckGoalCompletion(ctx context.Context, telegramID int64) (bool, error) {
	progress, err := s.GetProgress(ctx, telegramID)
	if err != nil {
		return false, err
	}
	if progress == nil {
		return false, nil
	}
	if progress.Goal.Achieved {
		return false, nil
	}
	if progress.Percentage < 100 {
		return false, nil
	}

	// Mark goal as achieved.
	now := time.Now().In(wib)
	progress.Goal.Achieved = true
	progress.Goal.AchievedAt = now
	if err := s.store.SaveGoal(ctx, progress.Goal); err != nil {
		return false, err
	}

	// Award XP for achieving the goal.
	if _, err := s.gamSvc.AwardXP(ctx, telegramID, domain.XPGoalAchieved, "goal_achieved"); err != nil {
		return false, err
	}

	// Award the goal_achiever badge.
	if _, err := s.badgeSvc.AwardGoalBadge(ctx, telegramID); err != nil {
		return false, err
	}

	return true, nil
}

// calculateCurrentValue computes the current metric value for the given goal type.
func (s *GoalService) calculateCurrentValue(ctx context.Context, telegramID int64, goalType domain.GoalType) (float64, error) {
	switch goalType {
	case domain.GoalStreakDays:
		// NOTE: streak_days uses the global current streak, which may have
		// started before the current month. This is intentional — a goal like
		// "maintain a 7-day streak this month" is satisfied as long as the
		// user's current consecutive-day streak reaches the target, regardless
		// of when it began. Acceptable for a small community.
		streak, err := s.gamSvc.GetStreak(ctx, telegramID)
		if err != nil {
			return 0, err
		}
		return float64(streak.CurrentStreak), nil

	default:
		return s.calculateFromTrades(ctx, telegramID, goalType)
	}
}

// calculateFromTrades computes trade-based metrics for the current month.
func (s *GoalService) calculateFromTrades(ctx context.Context, telegramID int64, goalType domain.GoalType) (float64, error) {
	trades, err := s.trades.GetTrades(ctx, telegramID)
	if err != nil {
		return 0, err
	}

	ms := monthStart()

	// Filter to trades in the current month.
	var monthTrades []domain.Trade
	for _, t := range trades {
		if !t.Date.Before(ms) {
			monthTrades = append(monthTrades, t)
		}
	}

	switch goalType {
	case domain.GoalTotalTrades:
		return float64(len(monthTrades)), nil

	case domain.GoalTotalRR:
		var total float64
		for _, t := range monthTrades {
			total += t.ResultRR
		}
		return total, nil

	case domain.GoalWinRate:
		if len(monthTrades) == 0 {
			return 0, nil
		}
		var wins int
		for _, t := range monthTrades {
			if t.Status == domain.StatusWin {
				wins++
			}
		}
		return float64(wins) / float64(len(monthTrades)) * 100, nil

	default:
		return 0, nil
	}
}
