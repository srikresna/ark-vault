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
// read-modify-write of the member's gamification state, which without
// serialisation loses XP and lets two trades both pass the daily cap. A single
// mutex is sufficient and correct because the process is the only writer:
// Badger takes an exclusive directory lock, so the bot cannot run as more than
// one replica. Atomicity of the individual trade write sets is the store's job
// (CommitTradeRecord for recording, CommitTradeAdjustment for edits/deletes),
// not the mutex's.
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

	// Trade-intrinsic XP, split from streak XP so the exact amount granted for
	// this trade can be persisted (see TradeXPGrant) and later reconciled on
	// edit/delete without ever clawing back XP the daily cap suppressed.
	baseXP := 0
	shotXP := 0
	if tradeXPCountToday < domain.MaxDailyTradeXPCount {
		baseXP = tradeBaseXPWithoutScreenshot(trade)
		if trade.ScreenshotURL != "" {
			shotXP = domain.XPScreenshot
		}
	}

	// Streak XP (only if streak was incremented this call)
	streakXP := 0
	if streakIncremented {
		streakXP += domain.XPDailyStreak

		// Milestone bonuses fire on exact match only
		if streak.CurrentStreak == 7 {
			streakXP += domain.XPStreak7
		}
		if streak.CurrentStreak == 30 {
			streakXP += domain.XPStreak30
		}
	}
	totalXP := baseXP + shotXP + streakXP

	// 6. Update profile
	oldLevel, _ := domain.LevelForXP(profile.TotalXP)
	profile.TotalXP += totalXP
	profile.Level, profile.Title = domain.LevelForXP(profile.TotalXP)
	profile.UpdatedAt = now

	// 7. Persist. Profile, streak, grant and XP event land in one transaction:
	// a partial failure would otherwise leave the profile ahead of the grant
	// that a later edit/delete reconciles against, or hide the XP event the
	// daily cap counts.
	var grant *domain.TradeXPGrant
	if trade != nil && trade.ID != "" {
		// Record what this trade actually earned (possibly nothing under the
		// daily cap) so AdjustXPForTradeChange reconciles the real amount.
		grant = &domain.TradeXPGrant{
			TelegramID: memberID,
			TradeID:    trade.ID,
			Base:       baseXP,
			Screenshot: shotXP,
			GrantedAt:  now,
		}
	}
	var event *domain.XPEvent
	if totalXP > 0 {
		event = &domain.XPEvent{
			TelegramID: memberID,
			Amount:     totalXP,
			Reason:     "trade_recorded",
			Timestamp:  now,
		}
	}
	if err := s.store.CommitTradeRecord(ctx, profile, streak, grant, event); err != nil {
		// The trade itself may already be saved in the journal (Notion) even
		// though this commit failed, and a Badger transaction is all-or-
		// nothing, so nothing was granted: no profile XP, no XP event, no
		// grant. Record a zero grant best-effort so a later edit or delete of
		// that trade reconciles it as "earned nothing" instead of falling into
		// the unknown (pre-grant legacy) branch. If this write fails too, the
		// absent grant still cannot cause a wrong subtraction: the adjustment
		// path never invents XP for a missing grant.
		if grant != nil {
			_ = s.store.CommitTradeAdjustment(ctx, nil, nil, &domain.TradeXPGrant{
				TelegramID: memberID,
				TradeID:    grant.TradeID,
				Base:       0,
				Screenshot: 0,
				GrantedAt:  now,
			}, nil)
		}
		return nil, err
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
	xp := tradeBaseXPWithoutScreenshot(trade)
	if trade != nil && trade.ScreenshotURL != "" {
		xp += domain.XPScreenshot
	}
	return xp
}

// tradeBaseXPWithoutScreenshot is the adjustable part of a trade's XP: log +
// win bonus. The screenshot bonus is excluded because edits cannot add or
// remove an attached screenshot, so it never needs reconciling.
func tradeBaseXPWithoutScreenshot(trade *domain.Trade) int {
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
	return xp
}

// AdjustXPForTradeChange reconciles a member's XP and level when a trade is edited, closed, or deleted (newTrade == nil).
//
// The delta is computed against the XP that was actually granted for the trade
// (see TradeXPGrant), not its nominal base: a trade recorded after the daily
// cap was reached earned nothing, so editing or deleting it must not grant or
// subtract anything. Deleting such a trade still removes its grant. The
// profile, XP event and grant mutation are committed atomically.
//
// A missing grant is treated as "contribution unknown" and reconciles nothing.
// That covers trades recorded before grant tracking existed and trades whose
// record commit failed (OnTradeRecorded compensates those with a zero grant
// best-effort). Assuming the nominal base instead would subtract XP the member
// never received. The trade-off is deliberate: a pre-grant trade keeps its XP
// when deleted, and editing one does not grant the newly earned bonus, because
// no record proves what it contributed.
func (s *GamificationService) AdjustXPForTradeChange(ctx context.Context, memberID int64, oldTrade, newTrade *domain.Trade) (*domain.GamificationProfile, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	profile, err := s.GetProfile(ctx, memberID)
	if err != nil {
		return nil, 0, err
	}

	// A grant is the authoritative record of what the old trade contributed.
	// Its absence means the contribution is unknown: either the trade predates
	// grant tracking, or its record commit failed and the best-effort zero
	// grant could not be written. Assuming the nominal base in that case would
	// subtract XP the member may never have received, so reconcile nothing.
	var grant *domain.TradeXPGrant
	if oldTrade != nil && oldTrade.ID != "" {
		grant, err = s.store.GetTradeXPGrant(ctx, memberID, oldTrade.ID)
		if err != nil {
			return nil, 0, err
		}
	}
	if grant == nil {
		return profile, 0, nil
	}

	// A deletion removes the old trade's grant even when the trade earned no
	// XP; an edit keeps (or replaces) it.
	deleting := newTrade == nil
	var deleteGrant *domain.TradeXPGrant
	if (newTrade == nil || newTrade.ID == "") && oldTrade != nil && oldTrade.ID != "" {
		deleteGrant = &domain.TradeXPGrant{TelegramID: memberID, TradeID: oldTrade.ID}
	}

	if grant.Base == 0 && grant.Screenshot == 0 {
		// Earned nothing (daily cap or a failed record compensated with a zero
		// grant): nothing was granted to reconcile. Editing must not
		// retroactively grant the new base, but deleting must still drop the
		// grant.
		if deleteGrant != nil {
			if err := s.store.CommitTradeAdjustment(ctx, nil, nil, nil, deleteGrant); err != nil {
				return nil, 0, err
			}
		}
		return profile, 0, nil
	}

	// The screenshot bonus is sticky — it stays granted regardless of the edit.
	delta := tradeBaseXPWithoutScreenshot(newTrade) - grant.Base
	if delta == 0 && deleteGrant == nil {
		return profile, 0, nil
	}

	now := time.Now().In(timeutil.WIB)
	var event *domain.XPEvent
	if delta != 0 {
		profile.TotalXP += delta
		if profile.TotalXP < 0 {
			profile.TotalXP = 0
		}
		profile.Level, profile.Title = domain.LevelForXP(profile.TotalXP)
		profile.UpdatedAt = now

		reason := "trade_edited"
		if deleting {
			reason = "trade_deleted"
		}
		event = &domain.XPEvent{
			TelegramID: memberID,
			Amount:     delta,
			Reason:     reason,
			Timestamp:  now,
		}
	}

	// Keep the grant in sync so a second adjustment reconciles against the
	// updated state; a deletion removes it entirely.
	var nextGrant *domain.TradeXPGrant
	if !deleting && newTrade != nil && newTrade.ID != "" {
		nextGrant = &domain.TradeXPGrant{
			TelegramID: memberID,
			TradeID:    newTrade.ID,
			Base:       tradeBaseXPWithoutScreenshot(newTrade),
			Screenshot: grant.Screenshot,
			GrantedAt:  now,
		}
	}

	var committed *domain.GamificationProfile
	if delta != 0 {
		committed = profile
	}
	if err := s.store.CommitTradeAdjustment(ctx, committed, event, nextGrant, deleteGrant); err != nil {
		return nil, 0, err
	}
	return profile, delta, nil
}
