package service

import (
	"context"
	"sync"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
)

// GamificationService orchestrates XP, levelling and streak logic.
//
// Concurrency: the bot handles every Telegram update in its own goroutine, so
// profile/streak updates are genuinely concurrent. Every mutation below is a
// read-modify-write of a single Badger key (Badger offers no cross-key
// transaction through this store), which without serialisation loses XP and
// lets two trades both pass the daily cap. A single mutex is sufficient and
// correct because the process is the only writer: Badger takes an exclusive
// directory lock, so the bot cannot run as more than one replica.
//
// Lock ordering across services is challenge → goal → badge → gamification;
// this service is always the innermost, so it must never call back out.
type GamificationService struct {
	mu    sync.Mutex
	store GamificationStore
}

// NewGamificationService creates a new GamificationService.
func NewGamificationService(store GamificationStore) *GamificationService {
	return &GamificationService{
		store: store,
	}
}

// OnTradeResult holds the result of gamification processing after a trade.
type OnTradeResult struct {
	XPGained  int
	TotalXP   int
	Level     int
	Title     string
	LeveledUp bool
	OldLevel  int
	Streak    int
}

// OnTradeRecorded processes gamification effects when a trade is recorded.
func (s *GamificationService) OnTradeRecorded(ctx context.Context, memberID int64, trade *domain.Trade) (*OnTradeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.onTradeRecorded(ctx, memberID, trade)
}

// onTradeRecorded is the lock-free body of OnTradeRecorded; the caller must
// hold s.mu.
func (s *GamificationService) onTradeRecorded(ctx context.Context, memberID int64, trade *domain.Trade) (*OnTradeResult, error) {
	// 1. Get or create profile
	profile, err := s.store.GetProfile(ctx, memberID)
	if err != nil {
		return nil, err
	}
	if profile == nil {
		profile = &domain.GamificationProfile{
			TelegramID: memberID,
			TotalXP:    0,
			Level:      1,
			Title:      "Retail",
		}
	}

	// 2. Get or create streak
	streak, err := s.store.GetStreak(ctx, memberID)
	if err != nil {
		return nil, err
	}
	if streak == nil {
		streak = &domain.StreakData{
			TelegramID: memberID,
		}
	}

	// 3. Compute today in WIB
	now := time.Now().In(timeutil.WIB)
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")

	// 4. Update streak
	streakIncremented := false
	switch streak.LastLogDate {
	case today:
		// Already logged today — no streak change.
	case yesterday:
		streak.CurrentStreak++
		streakIncremented = true
	default:
		streak.CurrentStreak = 1
		streakIncremented = true
	}
	if streak.CurrentStreak > streak.LongestStreak {
		streak.LongestStreak = streak.CurrentStreak
	}
	streak.LastLogDate = today
	streak.UpdatedAt = now

	// 5. Calculate XP (subject to daily trade XP cap)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, timeutil.WIB)
	todayEvents, err := s.store.GetXPEvents(ctx, memberID, todayStart)
	if err != nil {
		return nil, err
	}
	tradeXPCountToday := 0
	for _, ev := range todayEvents {
		if ev.Reason == "trade_recorded" {
			tradeXPCountToday++
		}
	}

	totalXP := 0
	if tradeXPCountToday < domain.MaxDailyTradeXPCount {
		// Trade logged
		totalXP += domain.XPTradeLogged

		// Win bonuses
		if trade.Status == domain.StatusWin {
			if trade.ResultRR >= 2 {
				totalXP += domain.XPWinHighRR
			} else {
				totalXP += domain.XPWinBonus
			}
		}

		// Screenshot bonus
		if trade.ScreenshotURL != "" {
			totalXP += domain.XPScreenshot
		}
	}

	// Streak XP (only if streak was incremented this call)
	if streakIncremented {
		totalXP += domain.XPDailyStreak

		// Milestone bonuses fire on exact match only
		if streak.CurrentStreak == 7 {
			totalXP += domain.XPStreak7
		}
		if streak.CurrentStreak == 30 {
			totalXP += domain.XPStreak30
		}
	}

	// 6. Update profile
	oldLevel, _ := domain.LevelForXP(profile.TotalXP)
	profile.TotalXP += totalXP
	profile.Level, profile.Title = domain.LevelForXP(profile.TotalXP)
	profile.UpdatedAt = now

	// 7. Persist
	if err := s.store.SaveProfile(ctx, profile); err != nil {
		return nil, err
	}
	if err := s.store.SaveStreak(ctx, streak); err != nil {
		return nil, err
	}
	if totalXP > 0 {
		if err := s.store.AppendXPEvent(ctx, &domain.XPEvent{
			TelegramID: memberID,
			Amount:     totalXP,
			Reason:     "trade_recorded",
			Timestamp:  now,
		}); err != nil {
			return nil, err
		}
	}

	// 8. Return result
	return &OnTradeResult{
		XPGained:  totalXP,
		TotalXP:   profile.TotalXP,
		Level:     profile.Level,
		Title:     profile.Title,
		LeveledUp: profile.Level > oldLevel,
		OldLevel:  oldLevel,
		Streak:    streak.CurrentStreak,
	}, nil
}

// AwardXP grants the given amount of XP to a member for the specified reason.
// It updates the profile, appends an XP event, and returns the updated profile.
func (s *GamificationService) AwardXP(ctx context.Context, memberID int64, amount int, reason string) (*domain.GamificationProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.awardXP(ctx, memberID, amount, reason)
}

// awardXP is the lock-free body of AwardXP; the caller must hold s.mu.
func (s *GamificationService) awardXP(ctx context.Context, memberID int64, amount int, reason string) (*domain.GamificationProfile, error) {
	profile, err := s.GetProfile(ctx, memberID)
	if err != nil {
		return nil, err
	}

	now := time.Now().In(timeutil.WIB)
	profile.TotalXP += amount
	profile.Level, profile.Title = domain.LevelForXP(profile.TotalXP)
	profile.UpdatedAt = now

	if err := s.store.SaveProfile(ctx, profile); err != nil {
		return nil, err
	}
	if err := s.store.AppendXPEvent(ctx, &domain.XPEvent{
		TelegramID: memberID,
		Amount:     amount,
		Reason:     reason,
		Timestamp:  now,
	}); err != nil {
		return nil, err
	}
	return profile, nil
}

// GetProfile returns the gamification profile for the member, creating a default if none exists.
func (s *GamificationService) GetProfile(ctx context.Context, memberID int64) (*domain.GamificationProfile, error) {
	profile, err := s.store.GetProfile(ctx, memberID)
	if err != nil {
		return nil, err
	}
	if profile == nil {
		profile = &domain.GamificationProfile{
			TelegramID: memberID,
			TotalXP:    0,
			Level:      1,
			Title:      "Retail",
			UpdatedAt:  time.Now().In(timeutil.WIB),
		}
	} else {
		profile.Level, profile.Title = domain.LevelForXP(profile.TotalXP)
	}
	return profile, nil
}

// GetStreak returns the streak data for the member, creating a default if none exists.
func (s *GamificationService) GetStreak(ctx context.Context, memberID int64) (*domain.StreakData, error) {
	streak, err := s.store.GetStreak(ctx, memberID)
	if err != nil {
		return nil, err
	}
	if streak == nil {
		streak = &domain.StreakData{
			TelegramID: memberID,
			UpdatedAt:  time.Now().In(timeutil.WIB),
		}
	}
	return streak, nil
}

// TradeBaseXP computes the trade-intrinsic XP for a single trade (log + win bonus + screenshot).
func TradeBaseXP(trade *domain.Trade) int {
	if trade == nil {
		return 0
	}
	xp := domain.XPTradeLogged
	if trade.Status == domain.StatusWin {
		if trade.ResultRR >= 2 {
			xp += domain.XPWinHighRR
		} else {
			xp += domain.XPWinBonus
		}
	}
	if trade.ScreenshotURL != "" {
		xp += domain.XPScreenshot
	}
	return xp
}

// AdjustXPForTradeChange reconciles a member's XP and level when a trade is edited, closed, or deleted (newTrade == nil).
func (s *GamificationService) AdjustXPForTradeChange(ctx context.Context, memberID int64, oldTrade, newTrade *domain.Trade) (*domain.GamificationProfile, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	profile, err := s.GetProfile(ctx, memberID)
	if err != nil {
		return nil, 0, err
	}

	oldXP := TradeBaseXP(oldTrade)
	newXP := TradeBaseXP(newTrade)
	delta := newXP - oldXP
	if delta == 0 {
		return profile, 0, nil
	}

	now := time.Now().In(timeutil.WIB)
	profile.TotalXP += delta
	if profile.TotalXP < 0 {
		profile.TotalXP = 0
	}
	profile.Level, profile.Title = domain.LevelForXP(profile.TotalXP)
	profile.UpdatedAt = now

	if err := s.store.SaveProfile(ctx, profile); err != nil {
		return nil, 0, err
	}
	reason := "trade_edited"
	if newTrade == nil {
		reason = "trade_deleted"
	}
	if err := s.store.AppendXPEvent(ctx, &domain.XPEvent{
		TelegramID: memberID,
		Amount:     delta,
		Reason:     reason,
		Timestamp:  now,
	}); err != nil {
		return nil, 0, err
	}
	return profile, delta, nil
}
