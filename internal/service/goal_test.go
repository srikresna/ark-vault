package service

import (
	"context"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
)

type mockGoalStore struct {
	goals map[string]*domain.MonthlyGoal
}

func newMockGoalStore() *mockGoalStore {
	return &mockGoalStore{goals: make(map[string]*domain.MonthlyGoal)}
}

func (m *mockGoalStore) GetGoal(_ context.Context, telegramID int64, yearMonth string) (*domain.MonthlyGoal, error) {
	if g, ok := m.goals[yearMonth]; ok && g.TelegramID == telegramID {
		cp := *g
		return &cp, nil
	}
	return nil, nil
}

func (m *mockGoalStore) SaveGoal(_ context.Context, goal *domain.MonthlyGoal) error {
	cp := *goal
	m.goals[goal.YearMonth] = &cp
	return nil
}

func (m *mockGoalStore) ListActiveGoals(_ context.Context, yearMonth string) ([]domain.MonthlyGoal, error) {
	var out []domain.MonthlyGoal
	if g, ok := m.goals[yearMonth]; ok && !g.Achieved {
		out = append(out, *g)
	}
	return out, nil
}

// TestGoalService_WinRateGoalExcludesOpenTrades pins the invariant that the
// win-rate goal denominator counts closed trades only, consistent with
// /stats: 2 wins + 2 OPEN trades is 100%, not 50%.
func TestGoalService_WinRateGoalExcludesOpenTrades(t *testing.T) {
	store := newMockGoalStore()
	tradesRepo := newMockGamificationTradeRepo()
	gamSvc := NewGamificationService(newMockGamificationStore())
	badgeSvc := NewBadgeService(newMockBadgeStore(), tradesRepo, gamSvc)
	svc := NewGoalService(store, tradesRepo, gamSvc, badgeSvc)

	ctx := context.Background()
	uid := int64(5)

	ym := time.Now().In(timeutil.WIB).Format("2006-01")
	if err := store.SaveGoal(ctx, &domain.MonthlyGoal{
		TelegramID:  uid,
		YearMonth:   ym,
		GoalType:    domain.GoalWinRate,
		TargetValue: 80,
	}); err != nil {
		t.Fatalf("SaveGoal failed: %v", err)
	}

	tradesRepo.trades[uid] = []domain.Trade{
		{Date: time.Now().In(timeutil.WIB), Status: domain.StatusWin, ResultRR: 1},
		{Date: time.Now().In(timeutil.WIB), Status: domain.StatusWin, ResultRR: 1},
		{Date: time.Now().In(timeutil.WIB), Status: domain.StatusOpen},
		{Date: time.Now().In(timeutil.WIB), Status: domain.StatusOpen},
	}

	progress, err := svc.GetProgress(ctx, uid)
	if err != nil {
		t.Fatalf("GetProgress failed: %v", err)
	}
	if progress == nil {
		t.Fatal("expected progress for the current month's goal")
	}
	if progress.CurrentValue != 100 {
		t.Fatalf("win-rate goal current value = %.1f%%, want 100%% over 2 closed trades", progress.CurrentValue)
	}

	// 2W/2L closed (50%) against an 80% target: progress 62.5%, not achieved.
	tradesRepo.trades[uid] = append(tradesRepo.trades[uid],
		domain.Trade{Date: time.Now().In(timeutil.WIB), Status: domain.StatusLoss, ResultRR: -1},
		domain.Trade{Date: time.Now().In(timeutil.WIB), Status: domain.StatusLoss, ResultRR: -1},
	)
	progress, err = svc.GetProgress(ctx, uid)
	if err != nil {
		t.Fatalf("GetProgress failed: %v", err)
	}
	if progress.CurrentValue != 50 {
		t.Fatalf("win-rate goal current value = %.1f%%, want 50%% over 4 closed trades", progress.CurrentValue)
	}
}
