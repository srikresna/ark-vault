package telegram

import (
	"context"
	"encoding/json"
	"errors"
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

// postTradeTestHandler builds a handler whose sender records every message
// text, and returns an accessor for what was sent so far.
func postTradeTestHandler(t *testing.T, svc Services) (*Handler, func() []string) {
	t.Helper()

	var (
		mu    sync.Mutex
		texts []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		texts = append(texts, payload.Text)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	t.Cleanup(srv.Close)

	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(sender, svc, nil, nil, nil, NewRateLimiter(100, time.Minute), logger, 0, 0)
	t.Cleanup(h.Close)

	return h, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), texts...)
	}
}

// postTradeGoalStore serves an unfinished 100% win-rate goal, so a member with
// one winning trade completes it.
type postTradeGoalStore struct{}

func (postTradeGoalStore) GetGoal(_ context.Context, telegramID int64, yearMonth string) (*domain.MonthlyGoal, error) {
	return &domain.MonthlyGoal{
		TelegramID:  telegramID,
		YearMonth:   yearMonth,
		GoalType:    domain.GoalWinRate,
		TargetValue: 100,
	}, nil
}
func (postTradeGoalStore) SaveGoal(context.Context, *domain.MonthlyGoal) error { return nil }
func (postTradeGoalStore) ListActiveGoals(context.Context, string) ([]domain.MonthlyGoal, error) {
	return nil, nil
}

// postTradeErrGamStore fails every profile read, so reward application fails
// on its first store call.
type postTradeErrGamStore struct{ service.GamificationStore }

func (postTradeErrGamStore) GetProfile(context.Context, int64) (*domain.GamificationProfile, error) {
	return nil, errors.New("badger unavailable")
}

// TestNotifyTradeRecorded_PlainConfirmationWithoutRewards pins that the
// handler holds the post-trade service as an optional dependency: without it
// the member still gets the trade confirmation, and nothing panics.
func TestNotifyTradeRecorded_PlainConfirmationWithoutRewards(t *testing.T) {
	h, sent := postTradeTestHandler(t, Services{})

	trade := &domain.Trade{ID: "t1", Symbol: "XAUUSD", Direction: domain.DirBuy, Status: domain.StatusOpen}
	h.notifyTradeRecorded(context.Background(), 42, trade, 42, 0)

	texts := sent()
	if len(texts) != 1 {
		t.Fatalf("expected exactly one confirmation message, got %d: %v", len(texts), texts)
	}
	if want := FormatTradeConfirmation(trade); texts[0] != want {
		t.Errorf("confirmation = %q, want the plain trade confirmation %q", texts[0], want)
	}
}

// TestNotifyTradeRecorded_FallsBackWhenRewardsFail pins the error behaviour:
// a failed reward application must not swallow the confirmation, and it must
// not emit a zeroed XP confirmation or reward notifications.
func TestNotifyTradeRecorded_FallsBackWhenRewardsFail(t *testing.T) {
	gamSvc := service.NewGamificationService(postTradeErrGamStore{})
	h, sent := postTradeTestHandler(t, Services{
		PostTrade: service.NewPostTradeService(gamSvc, nil, nil),
	})

	trade := &domain.Trade{ID: "t1", Symbol: "XAUUSD", Direction: domain.DirBuy, Status: domain.StatusOpen}
	h.notifyTradeRecorded(context.Background(), 42, trade, 42, 0)

	texts := sent()
	if len(texts) != 1 {
		t.Fatalf("expected one fallback confirmation, got %d: %v", len(texts), texts)
	}
	if want := FormatTradeConfirmation(trade); texts[0] != want {
		t.Errorf("fallback confirmation = %q, want %q", texts[0], want)
	}
}

// TestNotifyTradeRecorded_SendsConfirmationThenRewards pins the message order
// and payload: the XP confirmation goes out first (so a slow badge/goal
// history read cannot delay it), followed by the badge unlock and the monthly
// goal notification.
func TestNotifyTradeRecorded_SendsConfirmationThenRewards(t *testing.T) {
	gamSvc := service.NewGamificationService(stubGamStore{})
	badgeSvc := service.NewBadgeService(stubBadgeStore{}, stubTradeRepo{}, gamSvc)
	goalSvc := service.NewGoalService(postTradeGoalStore{}, stubTradeRepo{}, gamSvc, badgeSvc)
	h, sent := postTradeTestHandler(t, Services{
		PostTrade: service.NewPostTradeService(gamSvc, badgeSvc, goalSvc),
	})

	trade := &domain.Trade{
		ID: "t1", Symbol: "XAUUSD", Direction: domain.DirBuy,
		Status: domain.StatusWin, ResultRR: 2,
		Date: time.Now(),
	}
	h.notifyTradeRecorded(context.Background(), 42, trade, 42, 0)

	texts := sent()
	if len(texts) != 3 {
		t.Fatalf("expected confirmation + badge + goal messages, got %d: %v", len(texts), texts)
	}
	if !strings.Contains(texts[0], "⚡ +") || !strings.Contains(texts[0], "XAUUSD") {
		t.Errorf("first message must be the XP confirmation, got %q", texts[0])
	}
	if !strings.Contains(texts[1], "Badge Unlocked!") {
		t.Errorf("second message must be the badge unlock, got %q", texts[1])
	}
	if !strings.Contains(texts[2], "Monthly Goal Achieved!") || !strings.Contains(texts[2], "+75 XP!") {
		t.Errorf("third message must be the goal notification, got %q", texts[2])
	}
}

// TestNotifyTradeRecorded_SkipsRewardsWhenOnlyGamificationIsWired pins that a
// reward-free configuration still sends exactly the XP confirmation.
func TestNotifyTradeRecorded_SkipsRewardsWhenOnlyGamificationIsWired(t *testing.T) {
	gamSvc := service.NewGamificationService(stubGamStore{})
	h, sent := postTradeTestHandler(t, Services{
		PostTrade: service.NewPostTradeService(gamSvc, nil, nil),
	})

	trade := &domain.Trade{ID: "t1", Symbol: "EURUSD", Direction: domain.DirSell, Status: domain.StatusOpen}
	h.notifyTradeRecorded(context.Background(), 42, trade, 42, 0)

	texts := sent()
	if len(texts) != 1 {
		t.Fatalf("expected only the XP confirmation, got %d: %v", len(texts), texts)
	}
	if !strings.Contains(texts[0], "⚡ +") {
		t.Errorf("expected the XP confirmation, got %q", texts[0])
	}
}
