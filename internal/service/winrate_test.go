package service

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
)

// TestReportCardService_WinRateCountsClosedTradesOnly pins the report card to
// the shared domain win rate: wins / (wins+losses+BE), with OPEN trades
// excluded. The per-asset breakdown must follow the same rule instead of
// dividing by every trade recorded for the symbol.
func TestReportCardService_WinRateCountsClosedTradesOnly(t *testing.T) {
	rcStore := &mockReportCardStore{}
	tradesRepo := newMockGamificationTradeRepo()
	svc := NewReportCardService(rcStore, tradesRepo, newMockGamificationStore(), newMockBadgeStore(), &mockAIAnalyzer{})

	ctx := context.Background()
	uid := int64(77)
	dt := time.Date(2026, 3, 15, 10, 0, 0, 0, timeutil.WIB)
	tradesRepo.trades[uid] = []domain.Trade{
		{ID: "1", Date: dt, Symbol: "XAUUSD", Status: domain.StatusWin, ResultRR: 2},
		{ID: "2", Date: dt, Symbol: "XAUUSD", Status: domain.StatusWin, ResultRR: 1},
		{ID: "3", Date: dt, Symbol: "XAUUSD", Status: domain.StatusLoss, ResultRR: -1},
		{ID: "4", Date: dt, Symbol: "XAUUSD", Status: domain.StatusBE},
		{ID: "5", Date: dt, Symbol: "XAUUSD", Status: domain.StatusOpen},
		{ID: "6", Date: dt, Symbol: "EURUSD", Status: domain.StatusWin, ResultRR: 1},
		{ID: "7", Date: dt, Symbol: "EURUSD", Status: domain.StatusOpen},
	}

	rc, err := svc.GenerateMonthlyReport(ctx, uid, "2026-03")
	if err != nil {
		t.Fatalf("GenerateMonthlyReport failed: %v", err)
	}

	// 3 wins / (3W + 1L + 1BE) = 60%; the two OPEN trades never count.
	if math.Abs(rc.WinRate-60.0) > 1e-9 {
		t.Errorf("report WinRate = %.2f, want 60.00", rc.WinRate)
	}

	xau := rc.AssetBreakdown["XAUUSD"]
	if xau.Total != 5 || xau.Wins != 2 || xau.Losses != 1 || xau.BreakEvens != 1 {
		t.Errorf("XAUUSD breakdown = %+v, want 5 trades: 2W/1L/1BE/1OPEN", xau)
	}
	// 2 wins / (2W + 1L + 1BE) = 50%, not 2/5 = 40% over all trades.
	if math.Abs(xau.WinRate-50.0) > 1e-9 {
		t.Errorf("XAUUSD WinRate = %.2f, want 50.00", xau.WinRate)
	}

	eur := rc.AssetBreakdown["EURUSD"]
	if eur.Total != 2 || eur.BreakEvens != 0 || math.Abs(eur.WinRate-100.0) > 1e-9 {
		t.Errorf("EURUSD breakdown = %+v, want 2 trades: 1W/1OPEN and 100%% WR", eur)
	}
}

// TestBadgeService_Winrate60CountsBreakEvens pins that the winrate_60 badge
// uses the shared domain denominator: break-evens dilute the rate, so 13W/6L/3BE
// (59.1% over 22 closed) must not qualify, while 13W/6L/1BE (65%) does even
// with extra OPEN trades in the history.
func TestBadgeService_Winrate60CountsBreakEvens(t *testing.T) {
	tradesRepo := newMockGamificationTradeRepo()
	badgeStore := newMockBadgeStore()
	gamSvc := NewGamificationService(newMockGamificationStore())
	svc := NewBadgeService(badgeStore, tradesRepo, gamSvc)

	ctx := context.Background()
	now := time.Now().In(timeutil.WIB)

	closed := func(wins, losses, breakEvens, open int) []domain.Trade {
		var out []domain.Trade
		add := func(status domain.TradeStatus, n int) {
			for range n {
				out = append(out, domain.Trade{Date: now, Symbol: "XAUUSD", Status: status})
			}
		}
		add(domain.StatusWin, wins)
		add(domain.StatusLoss, losses)
		add(domain.StatusBE, breakEvens)
		add(domain.StatusOpen, open)
		return out
	}

	// Diluted: 13/22 = 59.1% once break-evens count.
	tradesRepo.trades[1] = closed(13, 6, 3, 0)
	if _, err := svc.CheckAndAwardBadges(ctx, 1, &domain.Trade{}, nil); err != nil {
		t.Fatalf("CheckAndAwardBadges(1) failed: %v", err)
	}
	if has, _ := badgeStore.HasBadge(ctx, 1, domain.BadgeWinrate60); has {
		t.Error("BadgeWinrate60 awarded at 13W/6L/3BE (59.1%), want not awarded")
	}

	// Qualifying: 13/20 = 65%, unaffected by the OPEN trades.
	tradesRepo.trades[2] = closed(13, 6, 1, 4)
	if _, err := svc.CheckAndAwardBadges(ctx, 2, &domain.Trade{}, nil); err != nil {
		t.Fatalf("CheckAndAwardBadges(2) failed: %v", err)
	}
	if has, _ := badgeStore.HasBadge(ctx, 2, domain.BadgeWinrate60); !has {
		t.Error("BadgeWinrate60 not awarded at 13W/6L/1BE (65%), want awarded")
	}
}

// TestLeaderboardService_WinRateCountsBreakEvens pins the leaderboard's
// consumer-facing win rate to the shared denominator while keeping the
// minimum-closed-trades gate.
func TestLeaderboardService_WinRateCountsBreakEvens(t *testing.T) {
	tradesRepo := newMockGamificationTradeRepo()
	membersRepo := newMockGamificationMemberRepo()
	ctx := context.Background()
	now := time.Now().In(timeutil.WIB)

	_, _ = membersRepo.EnsureMember(ctx, &domain.Member{TelegramID: 1, Username: "trader"})
	tradesRepo.trades[1] = []domain.Trade{
		{Date: now, Status: domain.StatusWin, ResultRR: 1},
		{Date: now, Status: domain.StatusWin, ResultRR: 1},
		{Date: now, Status: domain.StatusLoss, ResultRR: -1},
		{Date: now, Status: domain.StatusBE},
		{Date: now, Status: domain.StatusOpen},
		{Date: now, Status: domain.StatusOpen},
	}

	svc := NewLeaderboardService(tradesRepo, membersRepo)
	entries, err := svc.GetLeaderboardByPeriod(ctx, "winrate", "all", 10, 3)
	if err != nil {
		t.Fatalf("GetLeaderboardByPeriod failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 qualifying member, got %+v", entries)
	}
	if entries[0].TotalTrades != 4 {
		t.Errorf("TotalTrades = %d, want 4 closed trades", entries[0].TotalTrades)
	}
	if math.Abs(entries[0].WinRate-50.0) > 1e-9 {
		t.Errorf("WinRate = %.2f, want 50.00 (2W over 2W+1L+1BE)", entries[0].WinRate)
	}
}

// TestReportService_CommunityWinRateCountsBreakEvens pins the weekly community
// win rate to the shared denominator.
func TestReportService_CommunityWinRateCountsBreakEvens(t *testing.T) {
	tradesRepo := newMockGamificationTradeRepo()
	membersRepo := newMockGamificationMemberRepo()
	ctx := context.Background()
	now := time.Now().In(timeutil.WIB)

	_, _ = membersRepo.EnsureMember(ctx, &domain.Member{TelegramID: 1, Username: "a"})
	_, _ = membersRepo.EnsureMember(ctx, &domain.Member{TelegramID: 2, Username: "b"})
	tradesRepo.trades[1] = []domain.Trade{
		{Date: now, Status: domain.StatusWin, ResultRR: 1},
		{Date: now, Status: domain.StatusWin, ResultRR: 1},
		{Date: now, Status: domain.StatusBE},
		{Date: now, Status: domain.StatusOpen},
	}
	tradesRepo.trades[2] = []domain.Trade{
		{Date: now, Status: domain.StatusLoss, ResultRR: -1},
	}

	svc := NewReportService(tradesRepo, membersRepo)
	summary, err := svc.GenerateWeeklySummary(ctx, now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("GenerateWeeklySummary failed: %v", err)
	}
	// 2 wins / (2W + 1L + 1BE) = 50%; OPEN excluded, break-even counted.
	if math.Abs(summary.CommunityWR-50.0) > 1e-9 {
		t.Errorf("CommunityWR = %.2f, want 50.00", summary.CommunityWR)
	}
}

// TestAnalyticsService_WinRateCountsBreakEvens pins the AI analytics summary's
// win rate to the shared denominator.
func TestAnalyticsService_WinRateCountsBreakEvens(t *testing.T) {
	cache := &mockAnalyticsCacheStore{}
	tradesRepo := newMockGamificationTradeRepo()
	ctx := context.Background()
	uid := int64(99)
	now := time.Now().In(timeutil.WIB)

	tradesRepo.trades[uid] = []domain.Trade{
		{Date: now, Status: domain.StatusWin, ResultRR: 1},
		{Date: now, Status: domain.StatusWin, ResultRR: 1},
		{Date: now, Status: domain.StatusBE},
		{Date: now, Status: domain.StatusOpen},
	}

	svc := NewAnalyticsService(cache, &mockAIAnalyzer{}, tradesRepo)
	a, err := svc.GetAnalytics(ctx, uid)
	if err != nil {
		t.Fatalf("GetAnalytics failed: %v", err)
	}
	// 2 wins / (2W + 1BE) = 66.67%; the OPEN trade never counts.
	if math.Abs(a.WinRate-200.0/3.0) > 1e-9 {
		t.Errorf("analytics WinRate = %.2f, want 66.67", a.WinRate)
	}
}
