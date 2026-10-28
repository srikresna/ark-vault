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

// recordingSender captures outgoing Telegram calls.
type recordingSender struct {
	mu    sync.Mutex
	texts []string
}

func newRecordingHandler(t *testing.T, svc Services, ownerID int64) (*Handler, *recordingSender) {
	t.Helper()

	rec := &recordingSender{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		if text, ok := payload["text"].(string); ok {
			rec.mu.Lock()
			rec.texts = append(rec.texts, text)
			rec.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	t.Cleanup(srv.Close)

	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	limiter := NewRateLimiter(1000, time.Minute)

	h := NewHandler(sender, svc, nil, nil, nil, limiter, logger, 0, ownerID)
	t.Cleanup(h.Close)
	return h, rec
}

func (r *recordingSender) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.texts...)
}

func (r *recordingSender) contains(substr string) bool {
	for _, text := range r.all() {
		if strings.Contains(text, substr) {
			return true
		}
	}
	return false
}

func privateMessage(userID int64, text string) Update {
	return Update{Message: &Message{
		From: &User{ID: userID, Username: "trader"},
		Chat: &Chat{ID: userID, Type: "private"},
		Text: text,
	}}
}

// newReportCardService builds a real ReportCardService over stub stores.
func newReportCardService(called *bool) *service.ReportCardService {
	return service.NewReportCardService(
		&stubReportCardStore{},
		&stubTradeRepo{},
		&stubGamStore{},
		&stubBadgeStore{},
		&stubAI{called: called},
	)
}

// TestHandler_ReportCardIsReachable pins the regression where /reportcard was
// unreachable because the "/report" prefix case was evaluated first and matched
// it, so users asking for their monthly report card received the weekly
// community summary instead.
func TestHandler_ReportCardIsReachable(t *testing.T) {
	aiCalled := false
	h, rec := newRecordingHandler(t, Services{ReportCard: newReportCardService(&aiCalled)}, 0)

	h.HandleUpdate(context.Background(), privateMessage(11, "/reportcard"))

	if !rec.contains("Monthly Report Card") {
		t.Fatalf("/reportcard did not render a report card, got %v", rec.all())
	}
}

// TestHandler_ReportStillReachable is the counterpart guard: fixing the
// shadowing must not break /report itself.
func TestHandler_ReportStillReachable(t *testing.T) {
	tradeRepo := newMockFeatureTradeRepo()
	memberRepo := &mockResettableMemberRepo{}
	reportSvc := service.NewReportService(tradeRepo, memberRepo)

	h, rec := newRecordingHandler(t, Services{Report: reportSvc}, 0)

	h.HandleUpdate(context.Background(), privateMessage(12, "/report"))

	if !rec.contains("Weekly Report") {
		t.Fatalf("/report did not render a weekly summary, got %v", rec.all())
	}
}

// TestHandler_CommandWithBotSuffix checks group-chat style commands such as
// /stats@ArkVaultBot still route, which the old prefix matching only handled by
// accident for commands that happened to be prefixes of the suffixed text.
func TestHandler_CommandWithBotSuffix(t *testing.T) {
	aiCalled := false
	h, rec := newRecordingHandler(t, Services{ReportCard: newReportCardService(&aiCalled)}, 0)

	h.HandleUpdate(context.Background(), privateMessage(13, "/reportcard@ArkVaultBot"))

	if !rec.contains("Monthly Report Card") {
		t.Fatalf("/reportcard@ArkVaultBot did not route, got %v", rec.all())
	}
}

// TestHandler_UnknownCommandIsIgnored makes sure an unroutable command is not
// silently swallowed by a prefix match against a real command.
func TestHandler_UnknownCommandIsIgnored(t *testing.T) {
	aiCalled := false
	h, rec := newRecordingHandler(t, Services{ReportCard: newReportCardService(&aiCalled)}, 0)

	h.HandleUpdate(context.Background(), privateMessage(14, "/reportx"))

	if aiCalled {
		t.Error("unknown command /reportx must not invoke any service")
	}
	if len(rec.all()) != 0 {
		t.Errorf("unknown command should not produce output, got %v", rec.all())
	}
}

// TestCommandCatalog_HelpAndMenuStayInSync pins the invariant that routing, the
// Telegram menu and /help are all derived from one table: every routable
// command with help text must appear in the listing, and every menu entry must
// be routable.
func TestCommandCatalog_HelpAndMenuStayInSync(t *testing.T) {
	public, ownerOnly := CommandMenus()

	menuNames := make(map[string]bool, len(public)+len(ownerOnly))
	for _, cmd := range append(append([]BotCommand{}, public...), ownerOnly...) {
		menuNames[cmd.Command] = true
		if _, ok := commandIndex()[cmd.Command]; !ok {
			t.Errorf("menu entry %q is not routable", cmd.Command)
		}
	}

	for _, cmd := range commandCatalog() {
		if cmd.OwnerOnly || cmd.Desc == "" {
			continue
		}
		if !menuNames[cmd.Name] {
			t.Errorf("command %q has a description but is missing from the menu", cmd.Name)
		}
	}

	// /reset must stay owner-only in both places.
	for _, cmd := range public {
		if cmd.Command == "reset" {
			t.Error("/reset must not be in the public menu")
		}
	}
	if !containsCommand(ownerOnly, "reset") {
		t.Error("/reset missing from the owner menu")
	}

	// The owner-only command must not leak into the public help text.
	if strings.Contains(FormatHelpForUser(false), "/reset") {
		t.Error("public help leaks the owner-only /reset command")
	}
	if !strings.Contains(FormatHelpForUser(true), "/reset") {
		t.Error("owner help is missing /reset")
	}

	// Every documented public command must be listed in the help text.
	help := FormatHelpForUser(false)
	for _, cmd := range commandCatalog() {
		if cmd.OwnerOnly || len(cmd.Help) == 0 {
			continue
		}
		if !strings.Contains(help, "/"+cmd.Name) {
			t.Errorf("command /%s is routable but missing from /help", cmd.Name)
		}
	}
}

func containsCommand(cmds []BotCommand, name string) bool {
	for _, cmd := range cmds {
		if cmd.Command == name {
			return true
		}
	}
	return false
}

func TestParseCommandName(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"/stats", "stats", true},
		{"/stats week", "stats", true},
		{"/STATS", "stats", true},
		{"/stats@ArkBot week", "stats", true},
		{"/stats@ArkBot", "stats", true},
		{"/reportcard", "reportcard", true},
		{"/", "", false},
		{"#journal", "", false},
		{"hello", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := parseCommandName(tt.in)
		if ok != tt.wantOK || got != tt.want {
			t.Errorf("parseCommandName(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

// --- stub stores for the report card service ---

type stubTradeRepo struct{}

func (stubTradeRepo) SaveTrade(context.Context, int64, *domain.Trade) error { return nil }
func (stubTradeRepo) GetTrades(context.Context, int64) ([]domain.Trade, error) {
	return []domain.Trade{
		{ID: "t1", Symbol: "XAUUSD", Status: domain.StatusWin, ResultRR: 2, Date: time.Now()},
	}, nil
}
func (stubTradeRepo) UpdateTrade(context.Context, int64, *domain.Trade) error { return nil }
func (stubTradeRepo) DeleteTrade(context.Context, int64, string) error        { return nil }

type stubGamStore struct{}

func (stubGamStore) GetProfile(context.Context, int64) (*domain.GamificationProfile, error) {
	return &domain.GamificationProfile{Level: 3, Title: "Retail", TotalXP: 500}, nil
}
func (stubGamStore) SaveProfile(context.Context, *domain.GamificationProfile) error { return nil }
func (stubGamStore) GetStreak(context.Context, int64) (*domain.StreakData, error) {
	return &domain.StreakData{CurrentStreak: 4}, nil
}
func (stubGamStore) SaveStreak(context.Context, *domain.StreakData) error { return nil }
func (stubGamStore) AppendXPEvent(context.Context, *domain.XPEvent) error { return nil }
func (stubGamStore) GetXPEvents(context.Context, int64, time.Time) ([]domain.XPEvent, error) {
	return nil, nil
}
func (stubGamStore) GetTradeXPGrant(context.Context, int64, string) (*domain.TradeXPGrant, error) {
	return nil, nil
}
func (stubGamStore) SaveTradeXPGrant(context.Context, *domain.TradeXPGrant) error { return nil }
func (stubGamStore) DeleteTradeXPGrant(context.Context, int64, string) error      { return nil }
func (stubGamStore) CommitTradeRecord(context.Context, *domain.GamificationProfile, *domain.StreakData, *domain.TradeXPGrant, *domain.XPEvent) error {
	return nil
}
func (stubGamStore) CommitTradeAdjustment(context.Context, *domain.GamificationProfile, *domain.XPEvent, *domain.TradeXPGrant, *domain.TradeXPGrant) error {
	return nil
}

type stubBadgeStore struct{}

func (stubBadgeStore) GetBadges(context.Context, int64) ([]domain.BadgeAward, error) {
	return nil, nil
}
func (stubBadgeStore) HasBadge(context.Context, int64, domain.BadgeID) (bool, error) {
	return false, nil
}
func (stubBadgeStore) AwardBadge(context.Context, *domain.BadgeAward) error { return nil }

type stubReportCardStore struct{}

func (stubReportCardStore) GetReportCard(context.Context, int64, string) (*domain.MonthlyReportCard, error) {
	return nil, nil
}
func (stubReportCardStore) SaveReportCard(context.Context, *domain.MonthlyReportCard) error {
	return nil
}

type stubAI struct{ called *bool }

func (s *stubAI) AnalyzeTrades(context.Context, string) (string, error) {
	if s.called != nil {
		*s.called = true
	}
	return "", nil
}
