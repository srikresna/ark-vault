package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/internal/service"
)

// auditOKMessageServer answers every Telegram API call with a valid
// sendMessage-style response and records the raw request bodies.
func auditOKMessageServer(record *[]string, mu *sync.Mutex) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if record != nil {
			mu.Lock()
			*record = append(*record, string(body))
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}
}

func auditTestHandler(t *testing.T, svc Services) (*Handler, *sync.Mutex, *[]string) {
	t.Helper()
	var (
		mu     sync.Mutex
		sent   []string
		record = &sent
	)
	srv := httptest.NewServer(auditOKMessageServer(record, &mu))
	t.Cleanup(srv.Close)

	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(sender, svc, nil, nil, nil, NewRateLimiter(100, time.Minute), logger, 0, 0)
	t.Cleanup(h.Close)
	return h, &mu, record
}

// TestFormatOpenTrades_EscapesTimeWindow pins the HTML-injection fix: the
// Session/TimeWindow value is raw member input and must pass escapeHTML like
// every other user-controlled interpolation (formatter.go convention). A
// forged <tg://user?id> mention or fake formatting in /open output renders
// inside the trusted community group.
func TestFormatOpenTrades_EscapesTimeWindow(t *testing.T) {
	trades := []domain.Trade{{
		ID:         "t1",
		Date:       time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
		Symbol:     "BTCUSD",
		Direction:  domain.DirBuy,
		Status:     domain.StatusOpen,
		TimeWindow: `<b>pwn</b><a href="tg://user?id=1">owner</a>`,
	}}
	text, _ := FormatOpenTrades(trades)
	if strings.Contains(text, "<b>pwn</b>") || strings.Contains(text, "<a ") {
		t.Errorf("TimeWindow rendered unescaped (live markup): %s", text)
	}
	if !strings.Contains(text, "&lt;b&gt;pwn&lt;/b&gt;") || !strings.Contains(text, "&lt;a href=") {
		t.Errorf("expected fully escaped TimeWindow in output: %s", text)
	}
}

// TestCmdJournal_ReplacesPendingEditSession pins flow exclusivity: starting
// the guided flow must clear a pending trade-edit session, otherwise the edit
// session (checked first by HandleUpdate) hijacks every guided answer and can
// mutate the old trade via editf:symbol.
func TestCmdJournal_ReplacesPendingEditSession(t *testing.T) {
	h, _, _ := auditTestHandler(t, Services{})

	uid := int64(42)
	h.setEditSession(uid, &TradeEditSession{TradeID: "t1", Field: EditFieldSymbol})

	h.cmdJournal(context.Background(), &Message{
		From: &User{ID: uid, Username: "trader"},
		Chat: &Chat{ID: uid, Type: "private"},
	})

	if h.getEditSession(uid) != nil {
		t.Error("/journal must clear a pending trade edit session")
	}
	if h.guided.Get(uid) == nil {
		t.Error("/journal must start a guided session")
	}
}

// TestSetEditSession_ReplacesPendingGuidedFlow pins the mirror direction:
// opening an interactive edit session must cancel a pending guided flow so
// text input is always consumed by the flow that asked for it.
func TestSetEditSession_ReplacesPendingGuidedFlow(t *testing.T) {
	h, _, _ := auditTestHandler(t, Services{})

	uid := int64(43)
	h.guided.Start(uid, uid, 0)

	h.setEditSession(uid, &TradeEditSession{TradeID: "t1"})

	if h.guided.Get(uid) != nil {
		t.Error("starting an edit session must clear a pending guided flow")
	}
	if h.getEditSession(uid) == nil {
		t.Error("edit session should be active")
	}
}

// auditErrGamStore fails every profile read, making AdjustXPForTradeChange
// return an error on its first store call.
type auditErrGamStore struct{ service.GamificationStore }

func (auditErrGamStore) GetProfile(context.Context, int64) (*domain.GamificationProfile, error) {
	return nil, errors.New("badger unavailable")
}

// auditTradeRepo is a no-op TradeRepository for service construction.
type auditTradeRepo struct{}

func (auditTradeRepo) SaveTrade(context.Context, int64, *domain.Trade) error { return nil }
func (auditTradeRepo) GetTrades(context.Context, int64) ([]domain.Trade, error) {
	return nil, nil
}
func (auditTradeRepo) UpdateTrade(context.Context, int64, *domain.Trade) error { return nil }
func (auditTradeRepo) DeleteTrade(context.Context, int64, string) error        { return nil }

// TestDeleteTradeAndNotify_WarnsWhenXPAdjustmentFails pins the silent-swallow
// fix: when the Notion trade is deleted but XP reconciliation fails, the
// member must see a warning (and the failure must be logged) instead of the
// stale TradeXPGrant silently poisoning every later XP reconciliation.
func TestDeleteTradeAndNotify_WarnsWhenXPAdjustmentFails(t *testing.T) {
	h, mu, sent := auditTestHandler(t, Services{
		Journal:      service.NewJournalService(&auditTradeRepo{}, &mockResettableMemberRepo{}, nil),
		Gamification: service.NewGamificationService(auditErrGamStore{}),
	})

	uid := int64(9)
	h.deleteTradeAndNotify(context.Background(), uid,
		&domain.Trade{ID: "t1", Symbol: "BTCUSD", Direction: domain.DirBuy, Status: domain.StatusOpen},
		uid, 0)

	mu.Lock()
	defer mu.Unlock()
	if len(*sent) == 0 {
		t.Fatal("expected a confirmation message to be sent")
	}
	last := (*sent)[len(*sent)-1]
	if !strings.Contains(last, "XP gagal disesuaikan") {
		t.Errorf("failed XP adjustment must surface a warning, last payload: %s", last)
	}
}

// --- Membership gate: verdict cache + fail-closed fallback ---

type auditRoundTripper func(*http.Request) (*http.Response, error)

func (f auditRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func auditAPIResponse(body []byte) auditRoundTripper {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	}
}

func auditFailingTransport() auditRoundTripper {
	return func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
}

func auditMembershipHandler(t *testing.T, rt http.RoundTripper) *Handler {
	h, _, _ := auditTestHandler(t, Services{})
	h.communityGroupID = -100123
	h.bot = &Bot{
		token:  "TOK",
		client: &http.Client{Transport: rt},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return h
}

// TestIsAuthorized_FailsClosedWhenNeverVerified pins the fail-open fix: with
// the membership API erroring and no prior verdict, an unknown user must be
// denied — an API outage must not hand full access to every non-member.
func TestIsAuthorized_FailsClosedWhenNeverVerified(t *testing.T) {
	h := auditMembershipHandler(t, auditFailingTransport())

	if h.isAuthorized(context.Background(), 7, -100123, 0, "group") {
		t.Error("API error with no prior verdict must fail closed")
	}
}

// TestIsAuthorized_FreshVerdictSkipsAPI pins the verdict cache: a fresh
// membership check short-circuits later calls (this also bounds getChatMember
// traffic to one call per user per TTL).
func TestIsAuthorized_FreshVerdictSkipsAPI(t *testing.T) {
	var calls atomic.Int32
	rt := auditRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return auditAPIResponse([]byte(`{"ok":true,"result":{"status":"member"}}`))(nil)
	})
	h := auditMembershipHandler(t, rt)

	for range 2 {
		if !h.isAuthorized(context.Background(), 7, -100123, 0, "group") {
			t.Fatal("member should be authorized")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("fresh verdict must skip the API call, got %d calls", got)
	}
}

// TestIsAuthorized_CachesNonMemberVerdict pins that a definitive non-member
// verdict is cached too, so a spam burst from a non-member cannot flood
// getChatMember.
func TestIsAuthorized_CachesNonMemberVerdict(t *testing.T) {
	var calls atomic.Int32
	rt := auditRoundTripper(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return auditAPIResponse([]byte(`{"ok":true,"result":{"status":"left"}}`))(nil)
	})
	h := auditMembershipHandler(t, rt)

	for range 2 {
		if h.isAuthorized(context.Background(), 7, -100123, 0, "group") {
			t.Fatal("non-member must be denied")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("non-member verdict must be cached, got %d calls", got)
	}
}

// TestIsAuthorized_StaleVerdictSurvivesOutage pins the outage fallback: a
// verified member stays authorized on stale-but-recent verdicts while the
// membership API is down, instead of being locked out or re-failing-open.
func TestIsAuthorized_StaleVerdictSurvivesOutage(t *testing.T) {
	h := auditMembershipHandler(t, auditAPIResponse([]byte(`{"ok":true,"result":{"status":"member"}}`)))

	uid := int64(7)
	if !h.isAuthorized(context.Background(), uid, -100123, 0, "group") {
		t.Fatal("member should be authorized on first check")
	}

	// Age the verdict past the fresh TTL but within the stale ceiling.
	h.memberVerdictMu.Lock()
	h.memberVerdicts[uid] = membershipVerdict{isMember: true, checkedAt: time.Now().Add(-2 * time.Hour)}
	h.memberVerdictMu.Unlock()

	h.bot.client.Transport = auditFailingTransport()
	if !h.isAuthorized(context.Background(), uid, -100123, 0, "group") {
		t.Error("stale member verdict must keep the member authorized during an API outage")
	}
}

// --- Token redaction in logged transport errors ---

// TestRedactTokenErr pins the log-redaction fix: Bot API calls embed the token
// in the URL path, so a transport failure yields a *url.Error whose message
// contains the raw token. Every logged error must be token-free.
func TestRedactTokenErr(t *testing.T) {
	token := "12345:SECRET"

	err := &url.Error{Op: "Post", URL: telegramAPI + token + "/sendMessage", Err: errors.New("connection refused")}
	sanitized := redactTokenErr(err, token)
	if strings.Contains(sanitized.Error(), token) {
		t.Errorf("token leaked in error: %s", sanitized)
	}
	if !strings.Contains(sanitized.Error(), "REDACTED") {
		t.Errorf("expected REDACTED marker: %s", sanitized)
	}

	// Wrapped *url.Error must be redacted too (errors.As unwrapping).
	wrapped := fmt.Errorf("getUpdates failed: %w", err)
	if strings.Contains(redactTokenErr(wrapped, token).Error(), token) {
		t.Errorf("token leaked through wrapper: %s", wrapped)
	}

	// Non-URL errors pass through untouched.
	plain := errors.New("plain failure")
	if got := redactTokenErr(plain, token); got != plain {
		t.Errorf("non-url errors must pass through unchanged, got %v", got)
	}
	if redactTokenErr(nil, token) != nil {
		t.Error("nil must stay nil")
	}
}

// TestSender_SendDocument_RedactsTokenOnTransportError pins redaction on the
// multipart upload path: SendDocument has its own client.Do whose *url.Error
// embeds the token-bearing URL.
func TestSender_SendDocument_RedactsTokenOnTransportError(t *testing.T) {
	token := "12345:SECRET"
	s := NewSender(token)
	s.client = &http.Client{Transport: auditFailingTransport()}

	err := s.SendDocument(context.Background(), 1, "trades.csv", []byte("a,b"), "")
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("token leaked in SendDocument error: %s", err)
	}
}

// TestFormatTradeConfirmation_EscapesTimeWindow pins the second HTML sink for
// free-text sessions: the #journal confirmation renders TimeWindow in HTML
// parse mode, so raw markup would inject or break entity parsing.
func TestFormatTradeConfirmation_EscapesTimeWindow(t *testing.T) {
	trade := &domain.Trade{
		Symbol:     "BTCUSD",
		Direction:  domain.DirBuy,
		Status:     domain.StatusOpen,
		TimeWindow: `<a href="tg://user?id=1">x</a>`,
	}
	text := FormatTradeConfirmation(trade)
	if strings.Contains(text, "<a ") {
		t.Errorf("TimeWindow rendered as live markup in confirmation: %s", text)
	}
	if !strings.Contains(text, "&lt;a href=") {
		t.Errorf("expected escaped TimeWindow in confirmation: %s", text)
	}
}

// TestFormatEditTradeMenu_EscapesTimeWindow pins the third HTML sink: the
// trade-edit menu renders TimeWindow in HTML parse mode.
func TestFormatEditTradeMenu_EscapesTimeWindow(t *testing.T) {
	trade := &domain.Trade{
		ID:         "t1",
		Symbol:     "BTCUSD",
		Direction:  domain.DirBuy,
		Status:     domain.StatusOpen,
		TimeWindow: `<a href="tg://user?id=1">x</a>`,
	}
	text, _ := FormatEditTradeMenu(trade)
	if strings.Contains(text, "<a ") {
		t.Errorf("TimeWindow rendered as live markup in edit menu: %s", text)
	}
	if !strings.Contains(text, "&lt;a href=") {
		t.Errorf("expected escaped TimeWindow in edit menu: %s", text)
	}
}
