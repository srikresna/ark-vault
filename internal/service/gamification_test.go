package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

type mockGamificationStore struct {
	profiles map[int64]*domain.GamificationProfile
	streaks  map[int64]*domain.StreakData
	events   map[int64][]domain.XPEvent
}

func newMockGamificationStore() *mockGamificationStore {
	return &mockGamificationStore{
		profiles: make(map[int64]*domain.GamificationProfile),
		streaks:  make(map[int64]*domain.StreakData),
		events:   make(map[int64][]domain.XPEvent),
	}
}

func (m *mockGamificationStore) GetProfile(ctx context.Context, telegramID int64) (*domain.GamificationProfile, error) {
	if p, ok := m.profiles[telegramID]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, nil
}

func (m *mockGamificationStore) SaveProfile(ctx context.Context, profile *domain.GamificationProfile) error {
	cp := *profile
	m.profiles[profile.TelegramID] = &cp
	return nil
}

func (m *mockGamificationStore) GetStreak(ctx context.Context, telegramID int64) (*domain.StreakData, error) {
	if s, ok := m.streaks[telegramID]; ok {
		cp := *s
		return &cp, nil
	}
	return nil, nil
}

func (m *mockGamificationStore) SaveStreak(ctx context.Context, streak *domain.StreakData) error {
	cp := *streak
	m.streaks[streak.TelegramID] = &cp
	return nil
}

func (m *mockGamificationStore) AppendXPEvent(ctx context.Context, event *domain.XPEvent) error {
	m.events[event.TelegramID] = append(m.events[event.TelegramID], *event)
	return nil
}

func (m *mockGamificationStore) GetXPEvents(ctx context.Context, telegramID int64, since time.Time) ([]domain.XPEvent, error) {
	var out []domain.XPEvent
	for _, ev := range m.events[telegramID] {
		if !ev.Timestamp.Before(since) {
			out = append(out, ev)
		}
	}
	return out, nil
}

type mockGamificationTradeRepo struct {
	trades map[int64][]domain.Trade
}

func newMockGamificationTradeRepo() *mockGamificationTradeRepo {
	return &mockGamificationTradeRepo{trades: make(map[int64][]domain.Trade)}
}

func (m *mockGamificationTradeRepo) GetTrades(ctx context.Context, memberID int64) ([]domain.Trade, error) {
	return m.trades[memberID], nil
}
func (m *mockGamificationTradeRepo) SaveTrade(ctx context.Context, memberID int64, trade *domain.Trade) error {
	m.trades[memberID] = append(m.trades[memberID], *trade)
	return nil
}

func (m *mockGamificationTradeRepo) UpdateTrade(ctx context.Context, memberID int64, trade *domain.Trade) error {
	for i := range m.trades[memberID] {
		if m.trades[memberID][i].ID == trade.ID {
			m.trades[memberID][i] = *trade
			return nil
		}
	}
	return nil
}

func (m *mockGamificationTradeRepo) DeleteTrade(ctx context.Context, memberID int64, tradeID string) error {
	var kept []domain.Trade
	for _, t := range m.trades[memberID] {
		if t.ID != tradeID {
			kept = append(kept, t)
		}
	}
	m.trades[memberID] = kept
	return nil
}

type mockGamificationMemberRepo struct {
	members map[int64]*domain.Member
}

func newMockGamificationMemberRepo() *mockGamificationMemberRepo {
	return &mockGamificationMemberRepo{members: make(map[int64]*domain.Member)}
}

func (m *mockGamificationMemberRepo) EnsureMember(ctx context.Context, member *domain.Member) (*domain.Member, error) {
	m.members[member.TelegramID] = member
	return member, nil
}

func (m *mockGamificationMemberRepo) GetMember(ctx context.Context, tgID int64) (*domain.Member, error) {
	if member, ok := m.members[tgID]; ok {
		return member, nil
	}
	return nil, errors.New("member not found")
}

func (m *mockGamificationMemberRepo) ListMembers(ctx context.Context) ([]domain.Member, error) {
	var out []domain.Member
	for _, member := range m.members {
		out = append(out, *member)
	}
	return out, nil
}

func TestLevelForXP_HardcoreThresholds(t *testing.T) {
	tests := []struct {
		xp        int
		wantLevel int
		wantTitle string
	}{
		{0, 1, "Retail"},
		{299, 1, "Retail"},
		{300, 2, "Chartist"},
		{999, 2, "Chartist"},
		{1000, 3, "Analyst"},
		{2500, 4, "Strategist"},
		{5000, 5, "Systematic"},
		{9000, 6, "Algorithmic"},
		{15000, 7, "Quantitative"},
		{23000, 8, "Portfolio Manager"},
		{35000, 9, "Fund Manager"},
		{50000, 10, "Market Maker"},
		{75000, 10, "Market Maker"},
	}

	for _, tt := range tests {
		level, title := domain.LevelForXP(tt.xp)
		if level != tt.wantLevel || title != tt.wantTitle {
			t.Errorf("LevelForXP(%d) = (%d, %q), want (%d, %q)", tt.xp, level, title, tt.wantLevel, tt.wantTitle)
		}
	}
}

func TestGamificationService_DailyTradeXPCap(t *testing.T) {
	store := newMockGamificationStore()
	tradesRepo := newMockGamificationTradeRepo()
	membersRepo := newMockGamificationMemberRepo()
	svc := NewGamificationService(store, tradesRepo, membersRepo)

	ctx := context.Background()
	memberID := int64(12345)

	trade := &domain.Trade{
		ID:       "trade-1",
		Date:     time.Now().In(wib),
		Symbol:   "XAUUSD",
		Status:   domain.StatusWin,
		ResultRR: 2.5,
	}

	// Trades 1, 2, 3 on the same day should award trade XP
	for i := 1; i <= 3; i++ {
		res, err := svc.OnTradeRecorded(ctx, memberID, trade)
		if err != nil {
			t.Fatalf("trade %d OnTradeRecorded failed: %v", i, err)
		}
		if res.XPGained <= 0 {
			t.Errorf("trade %d expected XPGained > 0, got %d", i, res.XPGained)
		}
	}

	xpAfter3 := store.profiles[memberID].TotalXP

	// 4th trade on the same day should hit the daily trade XP cap (0 XP gained)
	res4, err := svc.OnTradeRecorded(ctx, memberID, trade)
	if err != nil {
		t.Fatalf("trade 4 OnTradeRecorded failed: %v", err)
	}
	if res4.XPGained != 0 {
		t.Errorf("trade 4 on same day should grant 0 XP due to daily cap, got %d", res4.XPGained)
	}
	if res4.TotalXP != xpAfter3 {
		t.Errorf("TotalXP after capped 4th trade = %d, want %d", res4.TotalXP, xpAfter3)
	}
}

func TestGamificationService_AdjustXPForTradeChange(t *testing.T) {
	store := newMockGamificationStore()
	tradesRepo := newMockGamificationTradeRepo()
	membersRepo := newMockGamificationMemberRepo()
	svc := NewGamificationService(store, tradesRepo, membersRepo)

	ctx := context.Background()
	memberID := int64(777)

	openTrade := &domain.Trade{ID: "t1", Symbol: "XAUUSD", Status: domain.StatusOpen}
	res, err := svc.OnTradeRecorded(ctx, memberID, openTrade)
	if err != nil {
		t.Fatalf("OnTradeRecorded failed: %v", err)
	}
	// Open trade gives 10 XP (trade) + 5 XP (streak) = 15 XP
	if res.TotalXP != 15 {
		t.Fatalf("expected 15 XP after open trade, got %d", res.TotalXP)
	}

	// Close trade as WIN +2.5R -> delta is +10 XP (XPWinHighRR)
	closedWin := &domain.Trade{ID: "t1", Symbol: "XAUUSD", Status: domain.StatusWin, ResultRR: 2.5}
	prof, delta, err := svc.AdjustXPForTradeChange(ctx, memberID, openTrade, closedWin)
	if err != nil {
		t.Fatalf("AdjustXPForTradeChange close win failed: %v", err)
	}
	if delta != 10 || prof.TotalXP != 25 {
		t.Errorf("expected delta=+10 and TotalXP=25 after closing WIN 2.5R, got delta=%d TotalXP=%d", delta, prof.TotalXP)
	}

	// Delete the trade (newTrade == nil) -> delta is -20 XP (10 base + 10 win high RR)
	prof, delta, err = svc.AdjustXPForTradeChange(ctx, memberID, closedWin, nil)
	if err != nil {
		t.Fatalf("AdjustXPForTradeChange delete failed: %v", err)
	}
	if delta != -20 || prof.TotalXP != 5 {
		t.Errorf("expected delta=-20 and TotalXP=5 after deleting trade, got delta=%d TotalXP=%d", delta, prof.TotalXP)
	}
}
