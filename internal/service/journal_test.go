package service

import (
	"context"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
)

// TestJournalService_RecordTradeStampsWIBDay covers the regression where a trade
// recorded between 00:00 and 07:00 WIB was stamped with the UTC calendar day and
// therefore filed under yesterday in Notion while streaks and the daily XP cap
// counted it as today.
func TestJournalService_RecordTradeStampsWIBDay(t *testing.T) {
	ctx := context.Background()
	trades := newMockGamificationTradeRepo()
	members := newMockGamificationMemberRepo()
	svc := NewJournalService(trades, members, nil)

	trade := &domain.Trade{Symbol: "XAUUSD", Direction: domain.DirBuy, Status: domain.StatusWin, ResultRR: 2}
	if err := svc.RecordTrade(ctx, 42, "trader", "Trader", trade); err != nil {
		t.Fatalf("RecordTrade failed: %v", err)
	}

	saved := trades.trades[42]
	if len(saved) != 1 {
		t.Fatalf("expected 1 persisted trade, got %d", len(saved))
	}
	got := saved[0].Date.In(timeutil.WIB)

	// The stamped date must be the WIB calendar day, i.e. it must never be
	// earlier than the current WIB day regardless of the wall clock.
	wantDay := time.Now().In(timeutil.WIB).Format("2006-01-02")
	if gotDay := got.Format("2006-01-02"); gotDay != wantDay {
		t.Errorf("trade date day = %s, want WIB day %s (stored %s)", gotDay, wantDay, got)
	}

	// Guard the specific failure mode: a UTC stamp near midnight is in the
	// previous WIB day, so the stored location must be WIB.
	if _, offset := saved[0].Date.Zone(); offset != 7*60*60 {
		t.Errorf("trade date offset = %d seconds, want 25200 (WIB)", offset)
	}
}

// TestJournalService_StatsIncludeTradeRecordedToday is the user-visible
// consequence: a trade recorded today must appear in today's/week's stats.
func TestJournalService_StatsIncludeTradeRecordedToday(t *testing.T) {
	ctx := context.Background()
	trades := newMockGamificationTradeRepo()
	members := newMockGamificationMemberRepo()
	svc := NewJournalService(trades, members, nil)

	trade := &domain.Trade{Symbol: "EURUSD", Direction: domain.DirSell, Status: domain.StatusLoss, ResultRR: -1}
	if err := svc.RecordTrade(ctx, 7, "trader", "Trader", trade); err != nil {
		t.Fatalf("RecordTrade failed: %v", err)
	}

	stats, err := svc.GetMemberStatsByPeriod(ctx, 7, "week")
	if err != nil {
		t.Fatalf("GetMemberStatsByPeriod failed: %v", err)
	}
	if stats == nil || stats.TotalTrades != 1 {
		t.Fatalf("weekly stats = %+v, want 1 trade", stats)
	}
}
