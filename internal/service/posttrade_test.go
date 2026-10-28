package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
)

// postTradeErrGamStore fails profile reads, so OnTradeRecorded fails on its
// first store call.
type postTradeErrGamStore struct{ GamificationStore }

func (postTradeErrGamStore) GetProfile(context.Context, int64) (*domain.GamificationProfile, error) {
	return nil, errors.New("badger unavailable")
}

// TestPostTradeService_ApplyTradeRewardsCountsScreenshot pins the screenshot
// timing contract: a screenshot reference attached to the trade before the
// reward call earns its XP bonus, and the snapshot carries the level and
// streak data the confirmation message renders.
func TestPostTradeService_ApplyTradeRewardsCountsScreenshot(t *testing.T) {
	svc := NewPostTradeService(NewGamificationService(newMockGamificationStore()), nil, nil)

	trade := &domain.Trade{
		ID:            "t-shot",
		Date:          time.Now().In(timeutil.WIB),
		Symbol:        "XAUUSD",
		Status:        domain.StatusWin,
		ResultRR:      2,
		ScreenshotURL: "https://notion.example/file/1",
	}

	res, err := svc.ApplyTradeRewards(context.Background(), 77, trade)
	if err != nil {
		t.Fatalf("ApplyTradeRewards failed: %v", err)
	}
	if res == nil {
		t.Fatal("expected a reward snapshot while gamification is enabled")
	}
	want := domain.XPTradeLogged + domain.XPWinHighRR + domain.XPScreenshot + domain.XPDailyStreak
	if res.XPGained != want {
		t.Errorf("XPGained = %d, want %d (log + win + screenshot + streak)", res.XPGained, want)
	}
	if res.TotalXP != want {
		t.Errorf("TotalXP = %d, want %d", res.TotalXP, want)
	}
	if res.Streak != 1 {
		t.Errorf("Streak = %d, want 1", res.Streak)
	}
	if res.Level == 0 || res.Title == "" {
		t.Errorf("snapshot must carry level and title, got level=%d title=%q", res.Level, res.Title)
	}
}

// TestPostTradeService_ApplyTradeRewardsSurfacesError pins the failure
// behaviour the adapter depends on: a store failure is returned as an error
// with no snapshot, so the caller degrades to the plain confirmation instead
// of rendering zeroed XP.
func TestPostTradeService_ApplyTradeRewardsSurfacesError(t *testing.T) {
	svc := NewPostTradeService(NewGamificationService(postTradeErrGamStore{}), nil, nil)

	res, err := svc.ApplyTradeRewards(context.Background(), 1, &domain.Trade{ID: "t1", Date: time.Now()})
	if err == nil {
		t.Fatal("expected the store failure to surface")
	}
	if res != nil {
		t.Errorf("failed rewards must not return a partial snapshot: %+v", res)
	}
}

// TestPostTradeService_NilSafeWhenDisabled pins that the adapter can hold the
// service without a nil check and without gamification wired: no rewards are
// applied and nothing is evaluated, so the plain trade confirmation is sent.
func TestPostTradeService_NilSafeWhenDisabled(t *testing.T) {
	ctx := context.Background()
	trade := &domain.Trade{ID: "t1", Date: time.Now()}

	services := map[string]*PostTradeService{
		"nil service":     nil,
		"no gamification": NewPostTradeService(nil, nil, nil),
	}
	for name, svc := range services {
		res, err := svc.ApplyTradeRewards(ctx, 1, trade)
		if err != nil || res != nil {
			t.Errorf("%s: ApplyTradeRewards = (%+v, %v), want (nil, nil)", name, res, err)
		}
		got := svc.EvaluateRewards(ctx, 1, trade)
		if len(got.Badges) != 0 || got.GoalAchieved {
			t.Errorf("%s: EvaluateRewards = %+v, want zero value", name, got)
		}
	}
}

// TestPostTradeService_EvaluateRewardsSkipsBadgesWithoutGamification pins the
// previous handler behaviour: badge and goal evaluation never runs when
// gamification is disabled, even if those services are wired.
func TestPostTradeService_EvaluateRewardsSkipsBadgesWithoutGamification(t *testing.T) {
	ctx := context.Background()
	trades := newMockGamificationTradeRepo()
	badgeStore := newMockBadgeStore()
	badgeSvc := NewBadgeService(badgeStore, trades, nil)
	goalStore := newMockGoalStore()
	goalSvc := NewGoalService(goalStore, trades, nil, badgeSvc)

	uid := int64(33)
	trades.trades[uid] = []domain.Trade{{
		ID: "t1", MemberID: uid, Date: time.Now().In(timeutil.WIB),
		Symbol: "XAUUSD", Status: domain.StatusWin, ResultRR: 2,
	}}
	svc := NewPostTradeService(nil, badgeSvc, goalSvc)

	got := svc.EvaluateRewards(ctx, uid, &trades.trades[uid][0])
	if len(got.Badges) != 0 || got.GoalAchieved {
		t.Errorf("EvaluateRewards = %+v, want zero value without gamification", got)
	}
	if badges, _ := badgeStore.GetBadges(ctx, uid); len(badges) != 0 {
		t.Errorf("no badge may be awarded without gamification, got %+v", badges)
	}
}

// TestPostTradeService_EvaluateRewardsReportsBadgesAndGoal pins the
// notification data the adapter renders: newly unlocked badges and monthly
// goal completion for a trade whose XP was already applied.
func TestPostTradeService_EvaluateRewardsReportsBadgesAndGoal(t *testing.T) {
	ctx := context.Background()
	uid := int64(21)
	now := time.Now().In(timeutil.WIB)

	gamStore := newMockGamificationStore()
	gamSvc := NewGamificationService(gamStore)
	trades := newMockGamificationTradeRepo()
	badgeSvc := NewBadgeService(newMockBadgeStore(), trades, gamSvc)
	goalStore := newMockGoalStore()
	goalSvc := NewGoalService(goalStore, trades, gamSvc, badgeSvc)
	svc := NewPostTradeService(gamSvc, badgeSvc, goalSvc)

	trade := &domain.Trade{
		ID: "t1", MemberID: uid, Date: now,
		Symbol: "XAUUSD", Status: domain.StatusWin, ResultRR: 2,
	}
	trades.trades[uid] = []domain.Trade{*trade}

	if err := goalStore.SaveGoal(ctx, &domain.MonthlyGoal{
		TelegramID:  uid,
		YearMonth:   now.Format("2006-01"),
		GoalType:    domain.GoalWinRate,
		TargetValue: 100,
	}); err != nil {
		t.Fatalf("SaveGoal failed: %v", err)
	}

	if _, err := svc.ApplyTradeRewards(ctx, uid, trade); err != nil {
		t.Fatalf("ApplyTradeRewards failed: %v", err)
	}
	before, err := gamSvc.GetProfile(ctx, uid)
	if err != nil {
		t.Fatalf("GetProfile failed: %v", err)
	}

	rewards := svc.EvaluateRewards(ctx, uid, trade)
	if !rewards.GoalAchieved {
		t.Error("a goal at 100% of target must be reported as achieved")
	}
	if len(rewards.Badges) == 0 {
		t.Fatal("expected the first-trade badge to be reported")
	}
	var sawFirstTrade bool
	for _, b := range rewards.Badges {
		if b.BadgeID == domain.BadgeFirstTrade {
			sawFirstTrade = true
		}
	}
	if !sawFirstTrade {
		t.Errorf("expected first_trade among the awarded badges, got %+v", rewards.Badges)
	}

	after, err := gamSvc.GetProfile(ctx, uid)
	if err != nil {
		t.Fatalf("GetProfile failed: %v", err)
	}
	// The goal itself grants XPGoalAchieved; the first-trade badge grants
	// XPBadgeEarned. Both are applied by the evaluation phase.
	if delta := after.TotalXP - before.TotalXP; delta < domain.XPGoalAchieved {
		t.Errorf("evaluation granted %d XP, want at least the %d goal XP", delta, domain.XPGoalAchieved)
	}

	// A second evaluation must not re-report or re-award anything.
	again := svc.EvaluateRewards(ctx, uid, trade)
	if again.GoalAchieved || len(again.Badges) != 0 {
		t.Errorf("second evaluation = %+v, want no repeat notifications", again)
	}
}
