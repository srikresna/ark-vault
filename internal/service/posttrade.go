package service

import (
	"context"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// PostTradeService owns the rewards that follow a recorded or updated trade:
// XP/level/streak, badge unlocks, and monthly goal completion.
//
// ApplyTradeRewards and EvaluateRewards are separate calls so an adapter can
// send its confirmation between them: evaluation reads the member's trade
// history. The service holds no lock and calls inward only (see
// GamificationService for lock ordering). A nil *PostTradeService applies no
// rewards.
type PostTradeService struct {
	gamSvc   *GamificationService
	badgeSvc *BadgeService
	goalSvc  *GoalService
}

// NewPostTradeService creates a PostTradeService; badgeSvc and goalSvc may be
// nil when those features are disabled.
func NewPostTradeService(gamSvc *GamificationService, badgeSvc *BadgeService, goalSvc *GoalService) *PostTradeService {
	return &PostTradeService{
		gamSvc:   gamSvc,
		badgeSvc: badgeSvc,
		goalSvc:  goalSvc,
	}
}

// PostTradeRewards carries the notifications produced by EvaluateRewards.
type PostTradeRewards struct {
	// Badges lists the badges newly awarded by this trade, in award order.
	Badges []domain.BadgeAward
	// GoalAchieved reports whether the monthly goal completed on this trade.
	GoalAchieved bool
}

// ApplyTradeRewards grants the trade's XP, level and streak rewards. A nil
// result with a nil error means gamification is disabled; on error the caller
// must still send the plain confirmation.
func (s *PostTradeService) ApplyTradeRewards(ctx context.Context, memberID int64, trade *domain.Trade) (*OnTradeResult, error) {
	if s == nil || s.gamSvc == nil {
		return nil, nil
	}
	return s.gamSvc.OnTradeRecorded(ctx, memberID, trade)
}

// EvaluateRewards awards newly unlocked badges and reports monthly goal
// completion. Errors are ignored; a failed streak read only skips streak badges.
func (s *PostTradeService) EvaluateRewards(ctx context.Context, memberID int64, trade *domain.Trade) PostTradeRewards {
	var out PostTradeRewards
	if s == nil || s.gamSvc == nil {
		return out
	}
	if s.badgeSvc != nil {
		streak, _ := s.gamSvc.GetStreak(ctx, memberID)
		out.Badges, _ = s.badgeSvc.CheckAndAwardBadges(ctx, memberID, trade, streak)
	}
	if s.goalSvc != nil {
		out.GoalAchieved, _ = s.goalSvc.CheckGoalCompletion(ctx, memberID)
	}
	return out
}
