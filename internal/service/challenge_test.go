package service

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
)

type mockChallengeStore struct {
	mu         sync.Mutex
	challenges map[string]*domain.WeeklyChallenge
}

func newMockChallengeStore() *mockChallengeStore {
	return &mockChallengeStore{
		challenges: make(map[string]*domain.WeeklyChallenge),
	}
}

func (m *mockChallengeStore) GetChallenge(ctx context.Context, yearWeek string) (*domain.WeeklyChallenge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.challenges[yearWeek]; ok && c != nil {
		cp := *c
		return &cp, nil
	}
	return nil, nil
}

func (m *mockChallengeStore) SaveChallenge(ctx context.Context, c *domain.WeeklyChallenge) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *c
	m.challenges[c.YearWeek] = &cp
	return nil
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
	gamSvc := NewGamificationService(gamStore)
	badgeSvc := NewBadgeService(newMockBadgeStore(), tradesRepo, gamSvc)

	svc := NewChallengeService(store, tradesRepo, membersRepo, gamSvc, badgeSvc)

	ctx := context.Background()
	now := time.Now().In(timeutil.WIB)

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
	gamSvc := NewGamificationService(gamStore)
	badgeSvc := NewBadgeService(newMockBadgeStore(), tradesRepo, gamSvc)

	svc := NewChallengeService(store, tradesRepo, membersRepo, gamSvc, badgeSvc)
	ctx := context.Background()
	now := time.Now().In(timeutil.WIB)

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

// TestChallengeService_HighestWRCountsClosedTradesOnly pins the invariant that
// the win-rate challenge denominator excludes OPEN trades (consistent with
// domain.CalculateStats) and that the min-3 gate counts closed trades.
func TestChallengeService_HighestWRCountsClosedTradesOnly(t *testing.T) {
	store := newMockChallengeStore()
	tradesRepo := newMockGamificationTradeRepo()
	membersRepo := newMockGamificationMemberRepo()
	gamStore := newMockGamificationStore()
	gamSvc := NewGamificationService(gamStore)
	badgeSvc := NewBadgeService(newMockBadgeStore(), tradesRepo, gamSvc)
	svc := NewChallengeService(store, tradesRepo, membersRepo, gamSvc, badgeSvc)

	ctx := context.Background()
	now := time.Now().In(timeutil.WIB)

	// alice: 2W + 2OPEN -> only 2 closed trades, below the min-3 gate.
	// bob: 2W + 1L + 1BE + 2OPEN -> 4 closed, WR = 50% (break-even dilutes,
	// OPEN excluded; 2/5 = 40% would be the old OPEN-inclusive mistake).
	_, _ = membersRepo.EnsureMember(ctx, &domain.Member{TelegramID: 1, Username: "alice"})
	_, _ = membersRepo.EnsureMember(ctx, &domain.Member{TelegramID: 2, Username: "bob"})
	tradesRepo.trades[1] = []domain.Trade{
		{Date: now, Status: domain.StatusWin, ResultRR: 1},
		{Date: now, Status: domain.StatusWin, ResultRR: 1},
		{Date: now, Status: domain.StatusOpen},
		{Date: now, Status: domain.StatusOpen},
	}
	tradesRepo.trades[2] = []domain.Trade{
		{Date: now, Status: domain.StatusWin, ResultRR: 1},
		{Date: now, Status: domain.StatusWin, ResultRR: 1},
		{Date: now, Status: domain.StatusLoss, ResultRR: -1},
		{Date: now, Status: domain.StatusBE},
		{Date: now, Status: domain.StatusOpen},
		{Date: now, Status: domain.StatusOpen},
	}

	ch := &domain.WeeklyChallenge{
		YearWeek:  domain.YearWeekString(now),
		Type:      domain.ChallengeHighestWR,
		StartDate: now.Add(-time.Hour),
		EndDate:   now.Add(time.Hour),
	}
	standings, err := svc.GetCurrentStandings(ctx, ch)
	if err != nil {
		t.Fatalf("GetCurrentStandings failed: %v", err)
	}
	if len(standings) != 1 || standings[0].TelegramID != 2 {
		t.Fatalf("expected only bob (4 closed trades) in standings, got %+v", standings)
	}
	if math.Abs(standings[0].Value-50.0) > 1e-9 {
		t.Fatalf("bob WR = %.2f%%, want 50.00%% (2 wins over 4 closed; BE dilutes, OPEN excluded)", standings[0].Value)
	}
}

// TestChallengeService_BestRRIgnoresOpenTrades pins the invariant that an OPEN
// trade's 0R can never win the RR Hunter challenge for a week of losses, and
// that members with only OPEN/BE trades are not ranked at all.
func TestChallengeService_BestRRIgnoresOpenTrades(t *testing.T) {
	store := newMockChallengeStore()
	tradesRepo := newMockGamificationTradeRepo()
	membersRepo := newMockGamificationMemberRepo()
	gamStore := newMockGamificationStore()
	gamSvc := NewGamificationService(gamStore)
	badgeSvc := NewBadgeService(newMockBadgeStore(), tradesRepo, gamSvc)
	svc := NewChallengeService(store, tradesRepo, membersRepo, gamSvc, badgeSvc)

	ctx := context.Background()
	now := time.Now().In(timeutil.WIB)

	_, _ = membersRepo.EnsureMember(ctx, &domain.Member{TelegramID: 1, Username: "loser"})
	_, _ = membersRepo.EnsureMember(ctx, &domain.Member{TelegramID: 2, Username: "ghost"})
	tradesRepo.trades[1] = []domain.Trade{
		{Date: now, Status: domain.StatusLoss, ResultRR: -1.0},
		{Date: now, Status: domain.StatusLoss, ResultRR: -2.0},
		{Date: now, Status: domain.StatusOpen},
	}
	tradesRepo.trades[2] = []domain.Trade{
		{Date: now, Status: domain.StatusOpen},
		{Date: now, Status: domain.StatusBE},
	}

	ch := &domain.WeeklyChallenge{
		YearWeek:  domain.YearWeekString(now),
		Type:      domain.ChallengeBestRR,
		StartDate: now.Add(-time.Hour),
		EndDate:   now.Add(time.Hour),
	}
	standings, err := svc.GetCurrentStandings(ctx, ch)
	if err != nil {
		t.Fatalf("GetCurrentStandings failed: %v", err)
	}
	if len(standings) != 1 || standings[0].TelegramID != 1 {
		t.Fatalf("expected only member 1 (has decisive trades), got %+v", standings)
	}
	if standings[0].Value != -1.0 {
		t.Fatalf("best RR = %.1f, want -1.0 (the OPEN trade's 0R must not win)", standings[0].Value)
	}
}

// TestChallengeService_FinalizeIsIdempotentAndWIBNormalised verifies that:
//  1. GetOrCreateChallenge normalises a UTC timestamp to WIB before deriving the week;
//  2. FinalizeChallenge persists Finalized=true before awarding XP and never double-awards
//     when invoked concurrently.
func TestChallengeService_FinalizeIsIdempotentAndWIBNormalised(t *testing.T) {
	store := newMockChallengeStore()
	tradesRepo := newMockGamificationTradeRepo()
	membersRepo := newMockGamificationMemberRepo()
	gamStore := newMockGamificationStore()
	gamSvc := NewGamificationService(gamStore)
	badgeSvc := NewBadgeService(newMockBadgeStore(), tradesRepo, gamSvc)
	svc := NewChallengeService(store, tradesRepo, membersRepo, gamSvc, badgeSvc)

	ctx := context.Background()
	// Sunday 2026-10-11 20:00 UTC is Monday 2026-10-12 03:00 WIB (ISO week W42 in WIB, W41 in UTC).
	utcTime := time.Date(2026, 10, 11, 20, 0, 0, 0, time.UTC)
	ch, err := svc.GetOrCreateChallenge(ctx, utcTime)
	if err != nil {
		t.Fatalf("GetOrCreateChallenge failed: %v", err)
	}
	if ch.YearWeek != "2026-W42" {
		t.Fatalf("YearWeek for Monday 03:00 WIB (passed as UTC) = %s, want 2026-W42", ch.YearWeek)
	}

	_, _ = membersRepo.EnsureMember(ctx, &domain.Member{TelegramID: 1, Username: "winner"})
	tradesRepo.trades[1] = []domain.Trade{
		{Date: ch.StartDate.Add(time.Hour), Status: domain.StatusWin, ResultRR: 3.0},
		{Date: ch.StartDate.Add(2 * time.Hour), Status: domain.StatusWin, ResultRR: 2.0},
		{Date: ch.StartDate.Add(3 * time.Hour), Status: domain.StatusWin, ResultRR: 1.0},
	}

	var wg sync.WaitGroup
	wg.Add(4)
	for range 4 {
		go func() {
			defer wg.Done()
			_, _ = svc.FinalizeChallenge(ctx, ch.YearWeek)
		}()
	}
	wg.Wait()

	prof, err := gamSvc.GetProfile(ctx, 1)
	if err != nil {
		t.Fatalf("GetProfile failed: %v", err)
	}
	wantXP := domain.XPChallenge1st + domain.XPBadgeEarned
	if prof.TotalXP != wantXP {
		t.Fatalf("winner TotalXP after concurrent FinalizeChallenge = %d, want %d (awarded once)", prof.TotalXP, wantXP)
	}
}
