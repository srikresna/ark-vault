package badger

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("failed to open test badger store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func TestGamificationRepo(t *testing.T) {
	store := newTestStore(t)
	repo := NewGamificationRepo(store)
	ctx := context.Background()

	// Get non-existent profile
	p, err := repo.GetProfile(ctx, 999)
	if err != nil {
		t.Fatalf("unexpected error getting missing profile: %v", err)
	}
	if p != nil {
		t.Fatalf("expected nil profile, got %+v", p)
	}

	// Save and Get profile
	now := time.Now().Truncate(time.Second)
	profile := &domain.GamificationProfile{
		TelegramID: 12345,
		TotalXP:    350,
		Level:      3,
		Title:      "Apprentice",
		UpdatedAt:  now,
	}
	if err := repo.SaveProfile(ctx, profile); err != nil {
		t.Fatalf("failed to save profile: %v", err)
	}

	gotProfile, err := repo.GetProfile(ctx, 12345)
	if err != nil {
		t.Fatalf("failed to get profile: %v", err)
	}
	if gotProfile == nil || gotProfile.TotalXP != 350 || gotProfile.Level != 3 || gotProfile.Title != "Apprentice" {
		t.Fatalf("unexpected profile fetched: %+v", gotProfile)
	}

	// Get non-existent streak
	s, err := repo.GetStreak(ctx, 999)
	if err != nil {
		t.Fatalf("unexpected error getting missing streak: %v", err)
	}
	if s != nil {
		t.Fatalf("expected nil streak, got %+v", s)
	}

	// Save and Get streak
	streak := &domain.StreakData{
		TelegramID:    12345,
		CurrentStreak: 5,
		LongestStreak: 12,
		LastLogDate:   "2026-10-11",
		UpdatedAt:     now,
	}
	if err := repo.SaveStreak(ctx, streak); err != nil {
		t.Fatalf("failed to save streak: %v", err)
	}

	gotStreak, err := repo.GetStreak(ctx, 12345)
	if err != nil {
		t.Fatalf("failed to get streak: %v", err)
	}
	if gotStreak == nil || gotStreak.CurrentStreak != 5 || gotStreak.LongestStreak != 12 || gotStreak.LastLogDate != "2026-10-11" {
		t.Fatalf("unexpected streak fetched: %+v", gotStreak)
	}

	// Append and Get XP Events
	ev1 := &domain.XPEvent{
		TelegramID: 12345,
		Amount:     50,
		Reason:     "trade logged",
		Timestamp:  now.Add(-10 * time.Minute),
	}
	ev2 := &domain.XPEvent{
		TelegramID: 12345,
		Amount:     25,
		Reason:     "streak bonus",
		Timestamp:  now,
	}
	if err := repo.AppendXPEvent(ctx, ev1); err != nil {
		t.Fatalf("failed to append ev1: %v", err)
	}
	if err := repo.AppendXPEvent(ctx, ev2); err != nil {
		t.Fatalf("failed to append ev2: %v", err)
	}

	events, err := repo.GetXPEvents(ctx, 12345, now.Add(-15*time.Minute))
	if err != nil {
		t.Fatalf("failed to get xp events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 xp events, got %d", len(events))
	}

	// Filter since time after ev1
	eventsAfter, err := repo.GetXPEvents(ctx, 12345, now.Add(-5*time.Minute))
	if err != nil {
		t.Fatalf("failed to get xp events after cutoff: %v", err)
	}
	if len(eventsAfter) != 1 {
		t.Fatalf("expected 1 xp event after cutoff, got %d", len(eventsAfter))
	}
}

// TestGamificationRepo_TradeXPGrantRoundtripAndPrune covers the per-trade XP
// grant store used by cap-aware XP reconciliation, and the prune job that
// keeps the XP log from growing without bound. Grants are exempt from pruning:
// an old grant is still needed to reconcile an edit or delete of its trade.
func TestGamificationRepo_TradeXPGrantRoundtripAndPrune(t *testing.T) {
	store := newTestStore(t)
	repo := NewGamificationRepo(store)
	ctx := context.Background()

	// Missing grant returns nil, not an error.
	g, err := repo.GetTradeXPGrant(ctx, 7, "nope")
	if err != nil || g != nil {
		t.Fatalf("GetTradeXPGrant(missing) = (%v, %v), want (nil, nil)", g, err)
	}

	// Roundtrip.
	grant := &domain.TradeXPGrant{
		TelegramID: 7,
		TradeID:    "t-1",
		Base:       domain.XPTradeLogged,
		Screenshot: domain.XPScreenshot,
		GrantedAt:  time.Now(),
	}
	if err := repo.SaveTradeXPGrant(ctx, grant); err != nil {
		t.Fatalf("SaveTradeXPGrant failed: %v", err)
	}
	g, err = repo.GetTradeXPGrant(ctx, 7, "t-1")
	if err != nil || g == nil || g.Base != grant.Base || g.Screenshot != grant.Screenshot {
		t.Fatalf("grant roundtrip = (%+v, %v)", g, err)
	}
	if err := repo.DeleteTradeXPGrant(ctx, 7, "t-1"); err != nil {
		t.Fatalf("DeleteTradeXPGrant failed: %v", err)
	}
	if g, _ := repo.GetTradeXPGrant(ctx, 7, "t-1"); g != nil {
		t.Fatalf("grant should be deleted, got %+v", g)
	}

	// Prune: the stale XP event disappears, but neither grant does — grants
	// are never pruned by age because a trade can be edited or deleted long
	// after it was recorded, and reconciliation needs the exact amount granted.
	now := time.Now()
	old := &domain.XPEvent{TelegramID: 9, Amount: 10, Reason: "trade_recorded", Timestamp: now.Add(-120 * 24 * time.Hour)}
	fresh := &domain.XPEvent{TelegramID: 9, Amount: 5, Reason: "trade_recorded", Timestamp: now.Add(-time.Hour)}
	if err := repo.AppendXPEvent(ctx, old); err != nil {
		t.Fatalf("AppendXPEvent(old) failed: %v", err)
	}
	if err := repo.AppendXPEvent(ctx, fresh); err != nil {
		t.Fatalf("AppendXPEvent(fresh) failed: %v", err)
	}
	if err := repo.SaveTradeXPGrant(ctx, &domain.TradeXPGrant{TelegramID: 9, TradeID: "old", GrantedAt: old.Timestamp}); err != nil {
		t.Fatalf("SaveTradeXPGrant(old) failed: %v", err)
	}
	if err := repo.SaveTradeXPGrant(ctx, &domain.TradeXPGrant{TelegramID: 9, TradeID: "fresh", GrantedAt: now}); err != nil {
		t.Fatalf("SaveTradeXPGrant(fresh) failed: %v", err)
	}

	deleted, err := repo.PruneGamification(ctx, now.Add(-90*24*time.Hour))
	if err != nil {
		t.Fatalf("PruneGamification failed: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 pruned key (the stale event only), got %d", deleted)
	}
	events, err := repo.GetXPEvents(ctx, 9, time.Time{})
	if err != nil || len(events) != 1 || events[0].Amount != 5 {
		t.Fatalf("expected only the fresh event to survive, got %+v (err %v)", events, err)
	}
	if g, _ := repo.GetTradeXPGrant(ctx, 9, "old"); g == nil {
		t.Fatal("old grant must survive the prune: a pruned grant would make its trade unreconcilable")
	}
	if g, _ := repo.GetTradeXPGrant(ctx, 9, "fresh"); g == nil {
		t.Fatal("fresh grant should survive the prune")
	}
}

// TestGamificationRepo_CommitTradeRecord pins that recording a trade writes
// the profile, streak, grant and XP event through one Badger transaction, and
// that a commit with nil grant/event leaves the stored grant and XP log alone.
func TestGamificationRepo_CommitTradeRecord(t *testing.T) {
	store := newTestStore(t)
	repo := NewGamificationRepo(store)
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	profile := &domain.GamificationProfile{
		TelegramID: 42,
		TotalXP:    18,
		Level:      1,
		Title:      "Retail",
		UpdatedAt:  now,
	}
	streak := &domain.StreakData{
		TelegramID:    42,
		CurrentStreak: 3,
		LongestStreak: 5,
		LastLogDate:   "2026-10-28",
		UpdatedAt:     now,
	}
	grant := &domain.TradeXPGrant{
		TelegramID: 42,
		TradeID:    "t-atomic",
		Base:       domain.XPTradeLogged,
		Screenshot: domain.XPScreenshot,
		GrantedAt:  now,
	}
	event := &domain.XPEvent{
		TelegramID: 42,
		Amount:     18,
		Reason:     "trade_recorded",
		Timestamp:  now,
	}

	if err := repo.CommitTradeRecord(ctx, profile, streak, grant, event); err != nil {
		t.Fatalf("CommitTradeRecord failed: %v", err)
	}

	gotProfile, err := repo.GetProfile(ctx, 42)
	if err != nil || gotProfile == nil || gotProfile.TotalXP != 18 {
		t.Fatalf("profile after commit = (%+v, %v), want TotalXP 18", gotProfile, err)
	}
	gotStreak, err := repo.GetStreak(ctx, 42)
	if err != nil || gotStreak == nil || gotStreak.CurrentStreak != 3 {
		t.Fatalf("streak after commit = (%+v, %v), want CurrentStreak 3", gotStreak, err)
	}
	gotGrant, err := repo.GetTradeXPGrant(ctx, 42, "t-atomic")
	if err != nil || gotGrant == nil || gotGrant.Base != domain.XPTradeLogged || gotGrant.Screenshot != domain.XPScreenshot {
		t.Fatalf("grant after commit = (%+v, %v)", gotGrant, err)
	}
	events, err := repo.GetXPEvents(ctx, 42, time.Time{})
	if err != nil || len(events) != 1 || events[0].Amount != 18 {
		t.Fatalf("xp events after commit = (%+v, %v), want one 18 XP event", events, err)
	}

	// Profile-only commits (nil streak/grant/event) must not disturb the
	// already-stored grant or XP log.
	profile.TotalXP = 25
	if err := repo.CommitTradeRecord(ctx, profile, nil, nil, nil); err != nil {
		t.Fatalf("CommitTradeRecord(profile only) failed: %v", err)
	}
	gotProfile, err = repo.GetProfile(ctx, 42)
	if err != nil || gotProfile == nil || gotProfile.TotalXP != 25 {
		t.Fatalf("profile after profile-only commit = (%+v, %v), want TotalXP 25", gotProfile, err)
	}
	if g, _ := repo.GetTradeXPGrant(ctx, 42, "t-atomic"); g == nil || g.Base != domain.XPTradeLogged {
		t.Fatalf("grant should be untouched by a profile-only commit, got %+v", g)
	}
	events, err = repo.GetXPEvents(ctx, 42, time.Time{})
	if err != nil || len(events) != 1 {
		t.Fatalf("xp log should be untouched by a profile-only commit, got %+v (err %v)", events, err)
	}
}

// TestGamificationRepo_CommitTradeAdjustment pins that editing a trade writes
// its profile, XP event and replacement grant in one transaction, and that a
// zero-XP-delta deletion still removes the trade's grant.
func TestGamificationRepo_CommitTradeAdjustment(t *testing.T) {
	store := newTestStore(t)
	repo := NewGamificationRepo(store)
	ctx := context.Background()

	now := time.Now().Truncate(time.Second)
	if err := repo.SaveProfile(ctx, &domain.GamificationProfile{
		TelegramID: 77, TotalXP: 100, Level: 2, Title: "Chartist", UpdatedAt: now,
	}); err != nil {
		t.Fatalf("SaveProfile failed: %v", err)
	}
	if err := repo.SaveTradeXPGrant(ctx, &domain.TradeXPGrant{
		TelegramID: 77, TradeID: "t-adj", Base: 20, Screenshot: 3, GrantedAt: now,
	}); err != nil {
		t.Fatalf("SaveTradeXPGrant failed: %v", err)
	}

	// Edit: profile, event and replacement grant commit together.
	profile := &domain.GamificationProfile{TelegramID: 77, TotalXP: 90, Level: 1, Title: "Retail", UpdatedAt: now}
	event := &domain.XPEvent{TelegramID: 77, Amount: -10, Reason: "trade_edited", Timestamp: now}
	grant := &domain.TradeXPGrant{TelegramID: 77, TradeID: "t-adj", Base: 10, Screenshot: 3, GrantedAt: now}
	if err := repo.CommitTradeAdjustment(ctx, profile, event, grant, nil); err != nil {
		t.Fatalf("CommitTradeAdjustment(edit) failed: %v", err)
	}

	gotProfile, err := repo.GetProfile(ctx, 77)
	if err != nil || gotProfile == nil || gotProfile.TotalXP != 90 || gotProfile.Level != 1 {
		t.Fatalf("profile after edit = (%+v, %v), want TotalXP 90 Level 1", gotProfile, err)
	}
	gotGrant, err := repo.GetTradeXPGrant(ctx, 77, "t-adj")
	if err != nil || gotGrant == nil || gotGrant.Base != 10 || gotGrant.Screenshot != 3 {
		t.Fatalf("grant after edit = (%+v, %v), want Base 10 Screenshot 3", gotGrant, err)
	}
	events, err := repo.GetXPEvents(ctx, 77, time.Time{})
	if err != nil || len(events) != 1 || events[0].Amount != -10 || events[0].Reason != "trade_edited" {
		t.Fatalf("xp events after edit = (%+v, %v), want one -10 trade_edited event", events, err)
	}

	// A zero grant is a real record ("earned nothing"), not an absent one: the
	// reconciliation contract depends on telling the two apart, so it must
	// survive a read as a present grant with zero amounts.
	if err := repo.CommitTradeAdjustment(ctx, nil, nil, &domain.TradeXPGrant{
		TelegramID: 77, TradeID: "t-zero", Base: 0, Screenshot: 0, GrantedAt: now,
	}, nil); err != nil {
		t.Fatalf("CommitTradeAdjustment(zero grant) failed: %v", err)
	}
	zero, err := repo.GetTradeXPGrant(ctx, 77, "t-zero")
	if err != nil || zero == nil || zero.Base != 0 || zero.Screenshot != 0 {
		t.Fatalf("zero grant = (%+v, %v), want a present zero grant", zero, err)
	}

	// Zero-delta delete: only the grant mutation is written, and the grant
	// still disappears even though no profile or event accompanies it.
	if err := repo.CommitTradeAdjustment(ctx, nil, nil, nil, &domain.TradeXPGrant{TelegramID: 77, TradeID: "t-adj"}); err != nil {
		t.Fatalf("CommitTradeAdjustment(zero-delta delete) failed: %v", err)
	}
	if g, _ := repo.GetTradeXPGrant(ctx, 77, "t-adj"); g != nil {
		t.Fatalf("grant must be deleted by a zero-delta adjustment, got %+v", g)
	}
	gotProfile, err = repo.GetProfile(ctx, 77)
	if err != nil || gotProfile == nil || gotProfile.TotalXP != 90 {
		t.Fatalf("profile after zero-delta delete = (%+v, %v), want TotalXP 90", gotProfile, err)
	}
	events, err = repo.GetXPEvents(ctx, 77, time.Time{})
	if err != nil || len(events) != 1 {
		t.Fatalf("zero-delta delete must not add an XP event, got %+v (err %v)", events, err)
	}

	// Deleting a grant that does not exist, or committing nothing at all, is
	// a no-op rather than an error.
	if err := repo.CommitTradeAdjustment(ctx, nil, nil, nil, &domain.TradeXPGrant{TelegramID: 77, TradeID: "missing"}); err != nil {
		t.Fatalf("deleting an absent grant should be a no-op: %v", err)
	}
	if err := repo.CommitTradeAdjustment(ctx, nil, nil, nil, nil); err != nil {
		t.Fatalf("empty adjustment should be a no-op: %v", err)
	}
}

// TestGamificationRepo_CommitTradeAdjustmentFailureWritesNothing pins the
// adapter side of atomicity: when the write transaction cannot commit, none of
// the profile, XP event or grant changes are applied.
func TestGamificationRepo_CommitTradeAdjustmentFailureWritesNothing(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("failed to open test badger store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repo := NewGamificationRepo(store)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	if err := repo.SaveProfile(ctx, &domain.GamificationProfile{
		TelegramID: 88, TotalXP: 100, Level: 2, Title: "Chartist", UpdatedAt: now,
	}); err != nil {
		t.Fatalf("SaveProfile failed: %v", err)
	}
	if err := repo.SaveTradeXPGrant(ctx, &domain.TradeXPGrant{
		TelegramID: 88, TradeID: "t-fail", Base: 20, GrantedAt: now,
	}); err != nil {
		t.Fatalf("SaveTradeXPGrant failed: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("closing the store failed: %v", err)
	}

	err = repo.CommitTradeAdjustment(ctx,
		&domain.GamificationProfile{TelegramID: 88, TotalXP: 80, Level: 1, Title: "Retail", UpdatedAt: now},
		&domain.XPEvent{TelegramID: 88, Amount: -20, Reason: "trade_deleted", Timestamp: now},
		nil,
		&domain.TradeXPGrant{TelegramID: 88, TradeID: "t-fail"},
	)
	if err == nil {
		t.Fatal("expected a commit on a closed store to fail")
	}

	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("failed to reopen test badger store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	repo = NewGamificationRepo(reopened)

	gotProfile, err := repo.GetProfile(ctx, 88)
	if err != nil || gotProfile == nil || gotProfile.TotalXP != 100 || gotProfile.Level != 2 {
		t.Fatalf("profile after failed commit = (%+v, %v), want TotalXP 100 Level 2", gotProfile, err)
	}
	if g, _ := repo.GetTradeXPGrant(ctx, 88, "t-fail"); g == nil || g.Base != 20 {
		t.Fatalf("grant after failed commit = %+v, want Base 20", g)
	}
	if events, _ := repo.GetXPEvents(ctx, 88, time.Time{}); len(events) != 0 {
		t.Fatalf("xp events after failed commit = %+v, want none", events)
	}
}

func TestBadgeRepo(t *testing.T) {
	store := newTestStore(t)
	repo := NewBadgeRepo(store)
	ctx := context.Background()

	// Initial checks
	has, err := repo.HasBadge(ctx, 100, domain.BadgeFirstTrade)
	if err != nil {
		t.Fatalf("unexpected error on HasBadge: %v", err)
	}
	if has {
		t.Fatal("expected HasBadge to be false initially")
	}

	badges, err := repo.GetBadges(ctx, 100)
	if err != nil {
		t.Fatalf("unexpected error on GetBadges: %v", err)
	}
	if len(badges) != 0 {
		t.Fatalf("expected 0 badges, got %d", len(badges))
	}

	// Award badge
	award := &domain.BadgeAward{
		TelegramID: 100,
		BadgeID:    domain.BadgeFirstTrade,
		AwardedAt:  time.Now().Truncate(time.Second),
	}
	if err := repo.AwardBadge(ctx, award); err != nil {
		t.Fatalf("failed to award badge: %v", err)
	}

	has, err = repo.HasBadge(ctx, 100, domain.BadgeFirstTrade)
	if err != nil {
		t.Fatalf("unexpected error on HasBadge: %v", err)
	}
	if !has {
		t.Fatal("expected HasBadge to be true after awarding")
	}

	badges, err = repo.GetBadges(ctx, 100)
	if err != nil {
		t.Fatalf("unexpected error on GetBadges: %v", err)
	}
	if len(badges) != 1 || badges[0].BadgeID != domain.BadgeFirstTrade {
		t.Fatalf("unexpected badges list: %+v", badges)
	}

	// ClaimBadge is atomic: first claim succeeds, repeat claim returns false.
	claimed1, err := repo.ClaimBadge(ctx, &domain.BadgeAward{
		TelegramID: 100,
		BadgeID:    domain.Badge10Trades,
		AwardedAt:  time.Now().Truncate(time.Second),
	})
	if err != nil || !claimed1 {
		t.Fatalf("expected first ClaimBadge to succeed, got claimed=%v err=%v", claimed1, err)
	}
	claimed2, err := repo.ClaimBadge(ctx, &domain.BadgeAward{
		TelegramID: 100,
		BadgeID:    domain.Badge10Trades,
		AwardedAt:  time.Now().Truncate(time.Second),
	})
	if err != nil || claimed2 {
		t.Fatalf("expected second ClaimBadge to return false, got claimed=%v err=%v", claimed2, err)
	}
}

func TestChallengeRepo(t *testing.T) {
	store := newTestStore(t)
	repo := NewChallengeRepo(store)
	ctx := context.Background()

	// Non-existent challenge
	ch, err := repo.GetChallenge(ctx, "2026-W41")
	if err != nil {
		t.Fatalf("unexpected error on GetChallenge: %v", err)
	}
	if ch != nil {
		t.Fatalf("expected nil challenge, got %+v", ch)
	}

	// Save and Get challenge
	now := time.Now().Truncate(time.Second)
	challenge := &domain.WeeklyChallenge{
		YearWeek:    "2026-W41",
		Type:        domain.ChallengeMostTrades,
		Title:       "Trade Warrior",
		Description: "Most trades logged this week",
		StartDate:   now,
		EndDate:     now.Add(7 * 24 * time.Hour),
		Finalized:   false,
	}
	if err := repo.SaveChallenge(ctx, challenge); err != nil {
		t.Fatalf("failed to save challenge: %v", err)
	}

	gotChallenge, err := repo.GetChallenge(ctx, "2026-W41")
	if err != nil {
		t.Fatalf("failed to get challenge: %v", err)
	}
	if gotChallenge == nil || gotChallenge.YearWeek != "2026-W41" || gotChallenge.Title != "Trade Warrior" {
		t.Fatalf("unexpected challenge fetched: %+v", gotChallenge)
	}
}

func TestGoalRepo(t *testing.T) {
	store := newTestStore(t)
	repo := NewGoalRepo(store)
	ctx := context.Background()

	// Non-existent goal
	g, err := repo.GetGoal(ctx, 100, "2026-10")
	if err != nil {
		t.Fatalf("unexpected error getting missing goal: %v", err)
	}
	if g != nil {
		t.Fatalf("expected nil goal, got %+v", g)
	}

	now := time.Now().Truncate(time.Second)
	goalActive := &domain.MonthlyGoal{
		TelegramID:  100,
		YearMonth:   "2026-10",
		GoalType:    domain.GoalTotalTrades,
		TargetValue: 30,
		Achieved:    false,
		CreatedAt:   now,
	}
	goalAchieved := &domain.MonthlyGoal{
		TelegramID:  200,
		YearMonth:   "2026-10",
		GoalType:    domain.GoalTotalRR,
		TargetValue: 10,
		Achieved:    true,
		CreatedAt:   now,
	}
	goalOtherMonth := &domain.MonthlyGoal{
		TelegramID:  300,
		YearMonth:   "2026-11",
		GoalType:    domain.GoalWinRate,
		TargetValue: 60,
		Achieved:    false,
		CreatedAt:   now,
	}

	if err := repo.SaveGoal(ctx, goalActive); err != nil {
		t.Fatalf("failed to save goalActive: %v", err)
	}
	if err := repo.SaveGoal(ctx, goalAchieved); err != nil {
		t.Fatalf("failed to save goalAchieved: %v", err)
	}
	if err := repo.SaveGoal(ctx, goalOtherMonth); err != nil {
		t.Fatalf("failed to save goalOtherMonth: %v", err)
	}

	gotGoal, err := repo.GetGoal(ctx, 100, "2026-10")
	if err != nil {
		t.Fatalf("failed to get goal: %v", err)
	}
	if gotGoal == nil || gotGoal.TargetValue != 30 || gotGoal.Achieved {
		t.Fatalf("unexpected goal fetched: %+v", gotGoal)
	}

	activeGoals, err := repo.ListActiveGoals(ctx, "2026-10")
	if err != nil {
		t.Fatalf("failed to list active goals: %v", err)
	}
	if len(activeGoals) != 1 || activeGoals[0].TelegramID != 100 {
		t.Fatalf("expected exactly 1 active goal for 2026-10 (id 100), got %+v", activeGoals)
	}
}

func TestReminderRepo(t *testing.T) {
	store := newTestStore(t)
	repo := NewReminderRepo(store)
	ctx := context.Background()

	// Non-existent pref
	pref, err := repo.GetReminderPref(ctx, 100)
	if err != nil {
		t.Fatalf("unexpected error getting reminder pref: %v", err)
	}
	if pref != nil {
		t.Fatalf("expected nil reminder pref, got %+v", pref)
	}

	prefEnabled := &domain.ReminderPreference{
		TelegramID:  100,
		Enabled:     true,
		Hour:        20,
		ChatID:      12345,
		ThreadID:    1,
		IsCommunity: false,
	}
	prefDisabled := &domain.ReminderPreference{
		TelegramID:  200,
		Enabled:     false,
		Hour:        18,
		ChatID:      67890,
		ThreadID:    0,
		IsCommunity: true,
	}

	if err := repo.SaveReminderPref(ctx, prefEnabled); err != nil {
		t.Fatalf("failed to save prefEnabled: %v", err)
	}
	if err := repo.SaveReminderPref(ctx, prefDisabled); err != nil {
		t.Fatalf("failed to save prefDisabled: %v", err)
	}

	gotPref, err := repo.GetReminderPref(ctx, 100)
	if err != nil {
		t.Fatalf("failed to get pref: %v", err)
	}
	if gotPref == nil || !gotPref.Enabled || gotPref.Hour != 20 || gotPref.ChatID != 12345 {
		t.Fatalf("unexpected pref fetched: %+v", gotPref)
	}

	enabledList, err := repo.ListEnabledReminders(ctx)
	if err != nil {
		t.Fatalf("failed to list enabled reminders: %v", err)
	}
	if len(enabledList) != 1 || enabledList[0].TelegramID != 100 {
		t.Fatalf("expected exactly 1 enabled reminder (id 100), got %+v", enabledList)
	}
}

func TestMemberCacheRepo(t *testing.T) {
	store := newTestStore(t)
	repo := NewMemberCacheRepo(store)
	ctx := context.Background()

	// Non-existent member
	m, err := repo.GetCachedMember(ctx, 100)
	if err != nil {
		t.Fatalf("unexpected error getting member: %v", err)
	}
	if m != nil {
		t.Fatalf("expected nil member, got %+v", m)
	}

	now := time.Now().Truncate(time.Second)
	member1 := &domain.Member{
		TelegramID:   100,
		Username:     "alice",
		FirstName:    "Alice",
		JoinDate:     now,
		NotionPageID: "page-alice",
		NotionDBID:   "db-alice",
	}
	member2 := &domain.Member{
		TelegramID:   200,
		Username:     "bob",
		FirstName:    "Bob",
		JoinDate:     now,
		NotionPageID: "page-bob",
		NotionDBID:   "db-bob",
	}

	if err := repo.SaveCachedMember(ctx, member1); err != nil {
		t.Fatalf("failed to save member1: %v", err)
	}
	if err := repo.SaveCachedMember(ctx, member2); err != nil {
		t.Fatalf("failed to save member2: %v", err)
	}

	gotMember, err := repo.GetCachedMember(ctx, 100)
	if err != nil {
		t.Fatalf("failed to get member: %v", err)
	}
	if gotMember == nil || gotMember.Username != "alice" || gotMember.NotionPageID != "page-alice" {
		t.Fatalf("unexpected member fetched: %+v", gotMember)
	}

	allMembers, err := repo.ListCachedMembers(ctx)
	if err != nil {
		t.Fatalf("failed to list members: %v", err)
	}
	if len(allMembers) != 2 {
		t.Fatalf("expected 2 cached members, got %d", len(allMembers))
	}
}

func TestTradeCacheRepo(t *testing.T) {
	store := newTestStore(t)
	repo := NewTradeCacheRepo(store)
	ctx := context.Background()

	// Non-existent trade cache
	trades, found, err := repo.GetCachedTrades(ctx, 100)
	if err != nil {
		t.Fatalf("unexpected error getting cached trades: %v", err)
	}
	if found || trades != nil {
		t.Fatalf("expected not found, got found=%v trades=%+v", found, trades)
	}

	now := time.Now().Truncate(time.Second)
	sampleTrades := []domain.Trade{
		{
			ID:         "trade-1",
			MemberID:   100,
			Date:       now,
			AssetType:  domain.AssetForex,
			Symbol:     "EURUSD",
			Direction:  domain.DirBuy,
			ResultRR:   2.5,
			Status:     domain.StatusWin,
			TimeWindow: domain.SessionLondon,
		},
		{
			ID:         "trade-2",
			MemberID:   100,
			Date:       now,
			AssetType:  domain.AssetCrypto,
			Symbol:     "BTCUSDT",
			Direction:  domain.DirSell,
			ResultRR:   -1.0,
			Status:     domain.StatusLoss,
			TimeWindow: domain.SessionNYAM,
		},
	}

	if err := repo.SaveCachedTrades(ctx, 100, sampleTrades); err != nil {
		t.Fatalf("failed to save cached trades: %v", err)
	}

	gotTrades, found, err := repo.GetCachedTrades(ctx, 100)
	if err != nil {
		t.Fatalf("failed to get cached trades: %v", err)
	}
	if !found {
		t.Fatal("expected trades to be found in cache")
	}
	if len(gotTrades) != 2 || gotTrades[0].Symbol != "EURUSD" || gotTrades[1].Symbol != "BTCUSDT" {
		t.Fatalf("unexpected cached trades: %+v", gotTrades)
	}

	// Invalidate cache
	if err := repo.InvalidateCachedTrades(ctx, 100); err != nil {
		t.Fatalf("failed to invalidate cached trades: %v", err)
	}

	afterTrades, foundAfter, err := repo.GetCachedTrades(ctx, 100)
	if err != nil {
		t.Fatalf("unexpected error getting invalidated trades: %v", err)
	}
	if foundAfter || afterTrades != nil {
		t.Fatalf("expected cache to be invalidated, got found=%v trades=%+v", foundAfter, afterTrades)
	}
}

func TestStore_Reset(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	gamRepo := NewGamificationRepo(store)
	memberRepo := NewMemberCacheRepo(store)
	goalRepo := NewGoalRepo(store)

	now := time.Now().Truncate(time.Second)
	if err := gamRepo.SaveProfile(ctx, &domain.GamificationProfile{TelegramID: 555, TotalXP: 100}); err != nil {
		t.Fatalf("failed to save profile: %v", err)
	}
	if err := memberRepo.SaveCachedMember(ctx, &domain.Member{TelegramID: 555, Username: "reset_user"}); err != nil {
		t.Fatalf("failed to save member: %v", err)
	}
	if err := goalRepo.SaveGoal(ctx, &domain.MonthlyGoal{TelegramID: 555, YearMonth: "2026-10", TargetValue: 5, CreatedAt: now}); err != nil {
		t.Fatalf("failed to save goal: %v", err)
	}

	// Verify entries exist before reset
	p, err := gamRepo.GetProfile(ctx, 555)
	if err != nil || p == nil {
		t.Fatalf("expected profile to exist before reset: %v", err)
	}

	// Perform Reset
	if err := store.Reset(); err != nil {
		t.Fatalf("store.Reset failed: %v", err)
	}

	// Verify entries are wiped
	pAfter, err := gamRepo.GetProfile(ctx, 555)
	if err != nil {
		t.Fatalf("unexpected error getting profile after reset: %v", err)
	}
	if pAfter != nil {
		t.Fatalf("expected nil profile after reset, got %+v", pAfter)
	}

	mAfter, err := memberRepo.GetCachedMember(ctx, 555)
	if err != nil {
		t.Fatalf("unexpected error getting member after reset: %v", err)
	}
	if mAfter != nil {
		t.Fatalf("expected nil member after reset, got %+v", mAfter)
	}

	activeGoals, err := goalRepo.ListActiveGoals(ctx, "2026-10")
	if err != nil {
		t.Fatalf("unexpected error listing goals after reset: %v", err)
	}
	if len(activeGoals) != 0 {
		t.Fatalf("expected 0 goals after reset, got %d", len(activeGoals))
	}
}

func TestStore_BackupToDirAndRotation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	gamRepo := NewGamificationRepo(store)
	if err := gamRepo.SaveProfile(ctx, &domain.GamificationProfile{TelegramID: 999, TotalXP: 1234}); err != nil {
		t.Fatalf("SaveProfile failed: %v", err)
	}

	tmpDir, err := os.MkdirTemp("", "badger-backup-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	path1, err := store.BackupToDir(tmpDir, 2)
	if err != nil {
		t.Fatalf("first BackupToDir failed: %v", err)
	}
	if fi, err := os.Stat(path1); err != nil || fi.Size() == 0 {
		t.Fatalf("backup file %s missing or empty (err=%v)", path1, err)
	}

	// Pre-create older dummy backup files to verify rotation prunes them
	old1 := filepath.Join(tmpDir, "badger-20260101-000000.bak.gz")
	old2 := filepath.Join(tmpDir, "badger-20260102-000000.bak.gz")
	_ = os.WriteFile(old1, []byte("old1"), 0644)
	_ = os.WriteFile(old2, []byte("old2"), 0644)

	// Run backup keeping only 2
	_, err = store.BackupToDir(tmpDir, 2)
	if err != nil {
		t.Fatalf("second BackupToDir failed: %v", err)
	}

	matches, _ := filepath.Glob(filepath.Join(tmpDir, "badger-*.bak.gz"))
	if len(matches) != 2 {
		t.Fatalf("expected 2 backups retained after pruning, got %d (%v)", len(matches), matches)
	}
	if _, err := os.Stat(old1); !os.IsNotExist(err) {
		t.Errorf("expected oldest backup %s to be pruned", old1)
	}
}
