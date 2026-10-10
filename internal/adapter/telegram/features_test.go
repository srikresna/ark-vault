package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/internal/service"
)

type mockFeatureTradeRepo struct {
	trades  map[int64][]domain.Trade
	updated *domain.Trade
	deleted string
}

func newMockFeatureTradeRepo() *mockFeatureTradeRepo {
	return &mockFeatureTradeRepo{trades: make(map[int64][]domain.Trade)}
}

func (m *mockFeatureTradeRepo) SaveTrade(_ context.Context, memberID int64, trade *domain.Trade) error {
	if trade.ID == "" {
		trade.ID = "new-id"
	}
	m.trades[memberID] = append([]domain.Trade{*trade}, m.trades[memberID]...)
	return nil
}

func (m *mockFeatureTradeRepo) GetTrades(_ context.Context, memberID int64) ([]domain.Trade, error) {
	return m.trades[memberID], nil
}

func (m *mockFeatureTradeRepo) UpdateTrade(_ context.Context, memberID int64, trade *domain.Trade) error {
	cp := *trade
	m.updated = &cp
	for i := range m.trades[memberID] {
		if m.trades[memberID][i].ID == trade.ID {
			m.trades[memberID][i] = cp
		}
	}
	return nil
}

func (m *mockFeatureTradeRepo) DeleteTrade(_ context.Context, memberID int64, tradeID string) error {
	m.deleted = tradeID
	var kept []domain.Trade
	for _, t := range m.trades[memberID] {
		if t.ID != tradeID {
			kept = append(kept, t)
		}
	}
	m.trades[memberID] = kept
	return nil
}

func TestHandler_UndoAndHistoryDeleteAndOpenClose(t *testing.T) {
	var sentTexts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		if text, ok := payload["text"].(string); ok {
			sentTexts = append(sentTexts, text)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":10}}`))
	}))
	defer srv.Close()

	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"

	tradeRepo := newMockFeatureTradeRepo()
	memberRepo := &mockResettableMemberRepo{}
	journalSvc := service.NewJournalService(tradeRepo, memberRepo, nil)
	leaderboardSvc := service.NewLeaderboardService(tradeRepo, memberRepo)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	limiter := NewRateLimiter(100, time.Minute)

	h := NewHandler(sender, Services{
		Journal:     journalSvc,
		Leaderboard: leaderboardSvc,
	}, nil, tradeRepo, memberRepo, limiter, logger, 0, 999)

	ctx := context.Background()
	userID := int64(42)
	now := time.Now().UTC()

	tradeRepo.trades[userID] = []domain.Trade{
		{ID: "t-open", MemberID: userID, Date: now, Symbol: "XAUUSD", AssetType: domain.AssetGold, Direction: domain.DirBuy, Status: domain.StatusOpen},
		{ID: "t-old", MemberID: userID, Date: now.Add(-time.Hour), Symbol: "EURUSD", AssetType: domain.AssetForex, Direction: domain.DirSell, Status: domain.StatusWin, ResultRR: 2.0},
	}

	// 1. /open lists the open trade
	h.HandleUpdate(ctx, Update{
		Message: &Message{
			From: &User{ID: userID, Username: "trader"},
			Chat: &Chat{ID: userID, Type: "private"},
			Text: "/open",
		},
	})
	if len(sentTexts) == 0 || !strings.Contains(sentTexts[len(sentTexts)-1], "XAUUSD") {
		t.Fatalf("expected /open to list XAUUSD open trade, got %v", sentTexts)
	}

	// 2. Callback open:win:t-open followed by RR text input "2.5" closes the trade as WIN +2.5R
	h.HandleUpdate(ctx, Update{
		CallbackQuery: &CallbackQuery{
			ID:      "cb-1",
			From:    &User{ID: userID, Username: "trader"},
			Message: &Message{MessageID: 10, Chat: &Chat{ID: userID, Type: "private"}},
			Data:    "open:win:t-open",
		},
	})
	h.HandleUpdate(ctx, Update{
		Message: &Message{
			From: &User{ID: userID, Username: "trader"},
			Chat: &Chat{ID: userID, Type: "private"},
			Text: "2.5",
		},
	})
	if tradeRepo.updated == nil || tradeRepo.updated.ID != "t-open" || tradeRepo.updated.Status != domain.StatusWin || tradeRepo.updated.ResultRR != 2.5 {
		t.Fatalf("expected t-open updated to WIN +2.5R, got %+v", tradeRepo.updated)
	}

	// 3. /undo deletes the most recent trade ("t-open")
	h.HandleUpdate(ctx, Update{
		Message: &Message{
			From: &User{ID: userID, Username: "trader"},
			Chat: &Chat{ID: userID, Type: "private"},
			Text: "/undo",
		},
	})
	if tradeRepo.deleted != "t-open" {
		t.Fatalf("expected /undo to delete t-open, got %q", tradeRepo.deleted)
	}

	// 4. Callback hist:del:t-old deletes specific trade from history
	h.HandleUpdate(ctx, Update{
		CallbackQuery: &CallbackQuery{
			ID:      "cb-2",
			From:    &User{ID: userID, Username: "trader"},
			Message: &Message{MessageID: 10, Chat: &Chat{ID: userID, Type: "private"}},
			Data:    "hist:del:t-old",
		},
	})
	if tradeRepo.deleted != "t-old" {
		t.Fatalf("expected hist:del:t-old to delete t-old, got %q", tradeRepo.deleted)
	}
}

func TestFormatStatsWithPeriodAndRevengeWarning(t *testing.T) {
	s := &domain.Stats{
		TotalTrades:  4,
		Wins:         3,
		Losses:       1,
		WinRate:      75.0,
		TotalRR:      5.0,
		AvgRR:        1.25,
		Expectancy:   1.25,
		ProfitFactor: 6.0,
		BySession: map[domain.TimeWindow]domain.AssetStats{
			domain.SessionLondon: {Total: 3, Wins: 3, TotalRR: 6.0, WinRate: 100.0},
		},
		TopSymbols: []domain.SymbolStats{
			{Symbol: "XAUUSD", Total: 3, Wins: 3, TotalRR: 6.0, WinRate: 100.0},
		},
	}
	out := FormatStatsWithPeriod("kresna", "month", s)
	if !strings.Contains(out, "Expectancy") || !strings.Contains(out, "Profit Factor") || !strings.Contains(out, "London") || !strings.Contains(out, "XAUUSD") {
		t.Errorf("FormatStatsWithPeriod missing expected sections:\n%s", out)
	}

	warn := FormatRevengeWarning(3)
	if !strings.Contains(warn, "Cooldown Warning") {
		t.Errorf("FormatRevengeWarning(3) should contain Cooldown Warning, got %q", warn)
	}
}
