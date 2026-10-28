package service

import (
	"context"
	"sync"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
)

// GoalService manages monthly goals, progress tracking, and goal completion.
//
// Concurrency: see GamificationService for the lock-ordering rationale
// (challenge → goal → badge → gamification).
type GoalService struct {
	mu       sync.Mutex
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
	return time.Now().In(timeutil.WIB).Format("2006-01")
}

// monthStart returns the first moment of the current month in WIB.
func monthStart() time.Time {
	now := time.Now().In(timeutil.WIB)
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, timeutil.WIB)
}

// SetGoal creates or updates a monthly goal for the user in the current month.
func (s *GoalService) SetGoal(ctx context.Context, telegramID int64, goalType domain.GoalType, targetValue float64) (*domain.MonthlyGoal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ym := currentYearMonth()
	now := time.Now().In(timeutil.WIB)

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
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.getProgress(ctx, telegramID)
}

// getProgress is the lock-free body of GetProgress; the caller must hold s.mu.
func (s *GoalService) getProgress(ctx context.Context, telegramID int64) (*domain.GoalProgress, error) {
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
//
// The "already achieved" check, the goal write, the XP award and the badge
// award are performed under the service lock: previously two concurrent trades
// could both observe an unachieved goal and award the goal XP twice.
func (s *GoalService) CheckGoalCompletion(ctx context.Context, telegramID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	progress, err := s.getProgress(ctx, telegramID)
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
	now := time.Now().In(timeutil.WIB)
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
		// Win rate over closed trades only (WIN+LOSS+BE), using the same
		// domain definition as /stats: OPEN trades must not drag the
		// denominator, and break-evens dilute the rate.
		counts := domain.CountResults(monthTrades)
		if counts.Closed() == 0 {
			return 0, nil
		}
		return counts.WinRate(), nil

	default:
		return 0, nil
	}
}
