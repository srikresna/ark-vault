package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
		MaxDrawdownR: 1.5,
		EquityCurve:  []float64{2.0, 0.5, 3.0, 5.0},
		BySession: map[domain.TimeWindow]domain.AssetStats{
			domain.SessionLondon: {Total: 3, Wins: 3, TotalRR: 6.0, WinRate: 100.0},
		},
		TopSymbols: []domain.SymbolStats{
			{Symbol: "XAUUSD", Total: 3, Wins: 3, TotalRR: 6.0, WinRate: 100.0},
		},
	}
	out := FormatStatsWithFilter("kresna", domain.StatsFilter{Period: "month", Symbol: "XAUUSD", Session: domain.SessionLondon}, s)
	for _, want := range []string{"Expectancy", "Profit Factor", "Max Drawdown", "R-Curve", "London", "XAUUSD"} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatStatsWithFilter missing %q:\n%s", want, out)
		}
	}

	warn := FormatRevengeWarning(3)
	if !strings.Contains(warn, "Cooldown Warning") {
		t.Errorf("FormatRevengeWarning(3) should contain Cooldown Warning, got %q", warn)
	}
}

func TestHandler_EditSessionSweepAndClose(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(NewSender("tok"), Services{}, nil, nil, nil, NewRateLimiter(10, time.Minute), logger, 0, 0)
	defer h.Close()

	// Insert an already-expired session manually
	h.editMu.Lock()
	h.editSessions[100] = &TradeEditSession{
		TradeID:   "expired",
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	h.editMu.Unlock()

	// Setting a new session should sweep the expired one
	h.setEditSession(200, &TradeEditSession{TradeID: "active"})

	h.editMu.RLock()
	_, expiredStillPresent := h.editSessions[100]
	_, activePresent := h.editSessions[200]
	h.editMu.RUnlock()

	if expiredStillPresent {
		t.Errorf("expected expired edit session for user 100 to be swept")
	}
	if !activePresent {
		t.Errorf("expected active edit session for user 200 to be present")
	}
}

// TestEscapeHTML pins the Telegram HTML parse-mode contract: user-controlled
// text must have &, < and > escaped or sendMessage is rejected outright.
func TestEscapeHTML(t *testing.T) {
	got := escapeHTML(`S&D demand <15m "OB" & FVG`)
	want := `S&amp;D demand &lt;15m "OB" &amp; FVG`
	if got != want {
		t.Errorf("escapeHTML = %q, want %q", got, want)
	}
	if escapeHTML(FormatTradeConfirmation(&domain.Trade{Symbol: "<b>", Status: domain.StatusOpen, AssetType: domain.AssetForex})) == "" {
		t.Error("escaped confirmation should still render")
	}
	if !strings.Contains(FormatTradeConfirmation(&domain.Trade{Symbol: "XAU<USD", AssetType: domain.AssetGold, Status: domain.StatusWin, ResultRR: 2}), "XAU&lt;USD") {
		t.Error("confirmation must escape the symbol")
	}
}

// TestHandler_GuidedFlowDoesNotSwallowCommands pins the regression where an
// active guided session captured slash commands as free-text input (turning
// "/help" into the trade symbol), leaving no way to escape the flow.
func TestHandler_GuidedFlowDoesNotSwallowCommands(t *testing.T) {
	var sent []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		if text, ok := payload["text"].(string); ok {
			mu.Lock()
			sent = append(sent, text)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":11}}`))
	}))
	defer srv.Close()

	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(sender, Services{}, nil, nil, nil, NewRateLimiter(100, time.Minute), logger, 0, 0)
	defer h.Close()

	ctx := context.Background()
	uid := int64(77)
	upd := func(text string) Update {
		return Update{Message: &Message{
			From: &User{ID: uid, Username: "trader"},
			Chat: &Chat{ID: uid, Type: "private"},
			Text: text,
		}}
	}

	session := h.guided.Start(uid, uid, 0)
	session.Trade = domain.Trade{AssetType: domain.AssetForex}

	// A slash command mid-flow routes normally instead of becoming the symbol.
	h.HandleUpdate(ctx, upd("/help"))
	mu.Lock()
	last := sent[len(sent)-1]
	mu.Unlock()
	if !strings.Contains(last, "ARK Vault — Help") {
		t.Fatalf("/help during guided flow was swallowed, last message: %q", last)
	}
	if h.guided.Get(uid) == nil {
		t.Fatal("guided session should survive unrelated commands")
	}

	// /cancel aborts the flow.
	h.HandleUpdate(ctx, upd("/cancel"))
	if h.guided.Get(uid) != nil {
		t.Fatal("/cancel should remove the guided session")
	}
	mu.Lock()
	last = sent[len(sent)-1]
	mu.Unlock()
	if !strings.Contains(last, "dibatalkan") {
		t.Fatalf("/cancel should confirm cancellation, got %q", last)
	}
}

// TestHandler_EditSessionDoesNotSwallowJournal pins the regression where a
// pending edit prompt consumed the next #journal message and stored its whole
// text as the new symbol.
func TestHandler_EditSessionDoesNotSwallowJournal(t *testing.T) {
	var sent []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		if text, ok := payload["text"].(string); ok {
			mu.Lock()
			sent = append(sent, text)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":12}}`))
	}))
	defer srv.Close()

	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"
	tradeRepo := newMockFeatureTradeRepo()
	memberRepo := &mockResettableMemberRepo{}
	journalSvc := service.NewJournalService(tradeRepo, memberRepo, nil)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(sender, Services{Journal: journalSvc}, nil, tradeRepo, memberRepo, NewRateLimiter(100, time.Minute), logger, 0, 0)
	defer h.Close()

	ctx := context.Background()
	uid := int64(88)
	h.setEditSession(uid, &TradeEditSession{
		TradeID:  "t-1",
		OldTrade: domain.Trade{ID: "t-1", Symbol: "XAUUSD"},
		NewTrade: domain.Trade{ID: "t-1", Symbol: "XAUUSD"},
		Field:    EditFieldSymbol,
		ChatID:   uid,
	})

	h.HandleUpdate(ctx, Update{Message: &Message{
		From: &User{ID: uid, Username: "trader"},
		Chat: &Chat{ID: uid, Type: "private"},
		Text: "#journal\nPair: EURUSD\nType: BUY\nRR: +2",
	}})

	// The journal entry must have been recorded...
	saved := tradeRepo.trades[uid]
	if len(saved) != 1 || saved[0].Symbol != "EURUSD" || saved[0].Status != domain.StatusWin {
		t.Fatalf("#journal was not recorded independently of the edit session, got %+v", saved)
	}
	// ...and the pending edit session must still be waiting for its answer.
	if h.getEditSession(uid) == nil {
		t.Fatal("edit session should not be consumed by a #journal message")
	}
	if tradeRepo.updated != nil {
		t.Fatalf("edit session must not apply an update from a #journal message, got %+v", tradeRepo.updated)
	}
}
