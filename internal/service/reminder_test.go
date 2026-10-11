package service

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
)

type mockReminderStore struct {
	prefs map[int64]*domain.ReminderPreference
}

func newMockReminderStore() *mockReminderStore {
	return &mockReminderStore{prefs: make(map[int64]*domain.ReminderPreference)}
}

func (m *mockReminderStore) GetReminderPref(_ context.Context, telegramID int64) (*domain.ReminderPreference, error) {
	if p, ok := m.prefs[telegramID]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, nil
}

func (m *mockReminderStore) SaveReminderPref(_ context.Context, pref *domain.ReminderPreference) error {
	cp := *pref
	m.prefs[pref.TelegramID] = &cp
	return nil
}

func (m *mockReminderStore) ListEnabledReminders(_ context.Context) ([]domain.ReminderPreference, error) {
	var out []domain.ReminderPreference
	for _, p := range m.prefs {
		if p.Enabled {
			out = append(out, *p)
		}
	}
	return out, nil
}

func TestReminderService_GetDueReminders_CommunityVsPersonal(t *testing.T) {
	remStore := newMockReminderStore()
	gamStore := newMockGamificationStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewReminderService(remStore, gamStore, logger)

	ctx := context.Background()
	now := time.Now().In(timeutil.WIB)
	hour := now.Hour()
	today := now.Format("2006-01-02")

	// 1. Personal user who ALREADY logged today -> should NOT receive DM reminder
	userLogged := int64(111)
	gamStore.streaks[userLogged] = &domain.StreakData{
		TelegramID:    userLogged,
		CurrentStreak: 5,
		LastLogDate:   today,
	}
	if err := svc.SetReminder(ctx, userLogged, userLogged, 0, true, hour); err != nil {
		t.Fatalf("SetReminder userLogged: %v", err)
	}

	// 2. Personal user who HAS NOT logged today -> SHOULD receive DM reminder
	userNotLogged := int64(222)
	if err := svc.SetReminder(ctx, userNotLogged, userNotLogged, 0, true, hour); err != nil {
		t.Fatalf("SetReminder userNotLogged: %v", err)
	}

	// 3. Community group reminder (negative chatID, thread 69) -> SHOULD always fire at configured hour
	// even if someone has a streak entry or no personal streak exists
	communityChatID := int64(-1001234567890)
	gamStore.streaks[communityChatID] = &domain.StreakData{
		TelegramID:  communityChatID,
		LastLogDate: today,
	}
	if err := svc.SetReminder(ctx, communityChatID, communityChatID, 69, true, hour); err != nil {
		t.Fatalf("SetReminder community: %v", err)
	}

	due, err := svc.GetDueReminders(ctx)
	if err != nil {
		t.Fatalf("GetDueReminders failed: %v", err)
	}

	dueMap := make(map[int64]domain.ReminderPreference)
	for _, p := range due {
		dueMap[p.TelegramID] = p
	}

	if _, ok := dueMap[userLogged]; ok {
		t.Errorf("userLogged already logged today, should not be due")
	}
	if p, ok := dueMap[userNotLogged]; !ok || p.IsCommunity {
		t.Errorf("userNotLogged should be due as personal reminder, got %+v", p)
	}
	if p, ok := dueMap[communityChatID]; !ok || !p.IsCommunity || p.ThreadID != 69 {
		t.Errorf("community reminder should be due with IsCommunity=true and ThreadID=69, got %+v", p)
	}
}
