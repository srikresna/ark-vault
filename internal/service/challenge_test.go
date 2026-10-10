package service

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

type mockChallengeStore struct {
	challenges map[string]*domain.WeeklyChallenge
	entries    map[string]map[int64]*domain.ChallengeEntry
}

func newMockChallengeStore() *mockChallengeStore {
	return &mockChallengeStore{
		challenges: make(map[string]*domain.WeeklyChallenge),
		entries:    make(map[string]map[int64]*domain.ChallengeEntry),
	}
}

func (m *mockChallengeStore) GetChallenge(ctx context.Context, yearWeek string) (*domain.WeeklyChallenge, error) {
	if c, ok := m.challenges[yearWeek]; ok {
		return c, nil
	}
	return nil, nil
}

func (m *mockChallengeStore) SaveChallenge(ctx context.Context, c *domain.WeeklyChallenge) error {
	m.challenges[c.YearWeek] = c
	return nil
}

func (m *mockChallengeStore) GetChallengeEntry(ctx context.Context, yearWeek string, telegramID int64) (*domain.ChallengeEntry, error) {
	if byUser, ok := m.entries[yearWeek]; ok {
		return byUser[telegramID], nil
	}
	return nil, nil
}

func (m *mockChallengeStore) SaveChallengeEntry(ctx context.Context, entry *domain.ChallengeEntry) error {
	if m.entries[entry.YearWeek] == nil {
		m.entries[entry.YearWeek] = make(map[int64]*domain.ChallengeEntry)
	}
	m.entries[entry.YearWeek][entry.TelegramID] = entry
	return nil
}

func (m *mockChallengeStore) GetChallengeEntries(ctx context.Context, yearWeek string) ([]domain.ChallengeEntry, error) {
	var out []domain.ChallengeEntry
	for _, e := range m.entries[yearWeek] {
		out = append(out, *e)
	}
	return out, nil
}

type mockBadgeStore struct {
	badges map[int64]map[domain.BadgeID]domain.BadgeAward
}

func newMockBadgeStore() *mockBadgeStore {
	return &mockBadgeStore{badges: make(map[int64]map[domain.BadgeID]domain.BadgeAward)}
}

func (m *mockBadgeStore) GetBadges(ctx context.Context, telegramID int64) ([]domain.BadgeAward, error) {
	var out []domain.BadgeAward
	for _, b := range m.badges[telegramID] {
		out = append(out, b)
	}
	return out, nil
}

func (m *mockBadgeStore) HasBadge(ctx context.Context, telegramID int64, badgeID domain.BadgeID) (bool, error) {
	if byID, ok := m.badges[telegramID]; ok {
		_, exists := byID[badgeID]
		return exists, nil
	}
	return false, nil
}

func (m *mockBadgeStore) AwardBadge(ctx context.Context, award *domain.BadgeAward) error {
	if m.badges[award.TelegramID] == nil {
		m.badges[award.TelegramID] = make(map[domain.BadgeID]domain.BadgeAward)
	}
	m.badges[award.TelegramID][award.BadgeID] = *award
	return nil
}

func TestChallengeService_GetOrCreateChallenge_NewAndExisting(t *testing.T) {
	store := newMockChallengeStore()
	tradesRepo := newMockGamificationTradeRepo()
	membersRepo := newMockGamificationMemberRepo()
	gamStore := newMockGamificationStore()
	gamSvc := NewGamificationService(gamStore, tradesRepo, membersRepo)
	badgeSvc := NewBadgeService(newMockBadgeStore(), tradesRepo, gamSvc)

	svc := NewChallengeService(store, tradesRepo, membersRepo, gamSvc, badgeSvc)

	ctx := context.Background()
	now := time.Now().In(wib)

	challenge, err := svc.GetOrCreateChallenge(ctx, now)
	if err != nil {
		t.Fatalf("GetOrCreateChallenge failed: %v", err)
	}
	if challenge == nil {
		t.Fatal("Expected challenge to be created, got nil")
	}
	expectedYearWeek := domain.YearWeekString(now)
	if challenge.YearWeek != expectedYearWeek {
		t.Errorf("Expected YearWeek = %s, got %s", expectedYearWeek, challenge.YearWeek)
	}

	// Second call should return existing challenge
	again, err := svc.GetOrCreateChallenge(ctx, now)
	if err != nil {
		t.Fatalf("second GetOrCreateChallenge failed: %v", err)
	}
	if again.Title != challenge.Title {
		t.Errorf("Expected existing challenge title %q, got %q", challenge.Title, again.Title)
	}
}

func TestChallengeService_Standings_NegativeOnlyBestRR(t *testing.T) {
	store := newMockChallengeStore()
	tradesRepo := newMockGamificationTradeRepo()
	membersRepo := newMockGamificationMemberRepo()
	gamStore := newMockGamificationStore()
	gamSvc := NewGamificationService(gamStore, tradesRepo, membersRepo)
	badgeSvc := NewBadgeService(newMockBadgeStore(), tradesRepo, gamSvc)

	svc := NewChallengeService(store, tradesRepo, membersRepo, gamSvc, badgeSvc)
	ctx := context.Background()
	now := time.Now().In(wib)

	_, _ = membersRepo.EnsureMember(ctx, &domain.Member{TelegramID: 1, Username: "alice"})
	tradesRepo.trades[1] = []domain.Trade{
		{Date: now, Status: domain.StatusLoss, ResultRR: -2.0},
		{Date: now, Status: domain.StatusLoss, ResultRR: -0.5},
	}

	ch := &domain.WeeklyChallenge{
		YearWeek:  domain.YearWeekString(now),
		Type:      domain.ChallengeBestRR,
		StartDate: now.Add(-1 * time.Hour),
		EndDate:   now.Add(1 * time.Hour),
	}
	standings, err := svc.GetCurrentStandings(ctx, ch)
	if err != nil {
		t.Fatalf("GetCurrentStandings failed: %v", err)
	}
	if len(standings) != 1 || math.Abs(standings[0].Value-(-0.5)) > 1e-9 {
		t.Fatalf("Expected best RR -0.5 for negative-only trades, got %+v", standings)
	}
}
