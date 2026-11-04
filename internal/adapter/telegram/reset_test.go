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
	"sync/atomic"
	"testing"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// mockResettableMemberRepo is the member repository used by the reset tests. It
// no longer performs the reset itself: storage is one transactional store, so
// the handler calls the Resetter directly.
//
// The call counter is atomic because the dispatcher runs different users
// concurrently, and a plain int races under -race even though the value is only
// read after the bot stops.
type mockResettableMemberRepo struct {
	ensureCalls atomic.Int64
}

func (m *mockResettableMemberRepo) EnsureMember(_ context.Context, member *domain.Member) (*domain.Member, error) {
	m.ensureCalls.Add(1)
	return member, nil
}

func (m *mockResettableMemberRepo) GetMember(context.Context, int64) (*domain.Member, error) {
	return nil, nil
}

func (m *mockResettableMemberRepo) ListMembers(context.Context) ([]domain.Member, error) {
	return nil, nil
}

func TestFormatHelpForUser_OwnerVisibility(t *testing.T) {
	publicHelp := FormatHelpForUser(false)
	if strings.Contains(publicHelp, "/reset") {
		t.Errorf("public help should NOT contain /reset, got:\n%s", publicHelp)
	}

	ownerHelp := FormatHelpForUser(true)
	if !strings.Contains(ownerHelp, "/reset") {
		t.Errorf("owner help SHOULD contain /reset, got:\n%s", ownerHelp)
	}
}

func TestHandler_ResetCommand_OwnerOnly(t *testing.T) {
	var (
		mu       sync.Mutex
		messages []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		if text, ok := payload["text"].(string); ok {
			mu.Lock()
			messages = append(messages, text)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()

	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	limiter := NewRateLimiter(100, 60_000_000_000)

	ownerID := int64(99999)
	resetter := &recordingResetter{removed: 7}
	h := NewHandler(sender, Services{}, nil, nil, &mockResettableMemberRepo{},
		limiter, logger, 0, ownerID)
	h.SetResetter(resetter)

	ctx := context.Background()

	// A non-owner is silently ignored: no reset, no reply.
	h.HandleUpdate(ctx, Update{
		Message: &Message{
			From: &User{ID: 12345, Username: "intruder"},
			Chat: &Chat{ID: 12345, Type: "private"},
			Text: "/reset confirm",
		},
	})
	if resetter.calls != 0 {
		t.Fatalf("non-owner triggered a reset (%d calls)", resetter.calls)
	}
	mu.Lock()
	if len(messages) != 0 {
		t.Fatalf("non-owner /reset should be silently ignored, got messages: %v", messages)
	}
	mu.Unlock()

	// The owner gets the real effect reported back.
	h.HandleUpdate(ctx, Update{
		Message: &Message{
			From: &User{ID: ownerID, Username: "owner"},
			Chat: &Chat{ID: ownerID, Type: "private"},
			Text: "/reset confirm",
		},
	})
	if resetter.calls != 1 {
		t.Fatalf("owner reset calls = %d, want 1", resetter.calls)
	}
	mu.Lock()
	got := strings.Join(messages, "\n")
	mu.Unlock()
	if !strings.Contains(got, "Reset selesai") {
		t.Errorf("expected a success message, got %q", got)
	}
	if !strings.Contains(got, "7") {
		t.Errorf("expected the removed trade count in the message, got %q", got)
	}
}

// TestExecuteReset_FailureDoesNotClaimSuccess pins that a failed reset is
// reported as a failure and never as "selesai".
func TestExecuteReset_FailureDoesNotClaimSuccess(t *testing.T) {
	var (
		mu       sync.Mutex
		messages []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		if text, ok := payload["text"].(string); ok {
			mu.Lock()
			messages = append(messages, text)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()

	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	limiter := NewRateLimiter(100, 60_000_000_000)

	ownerID := int64(99999)
	resetter := &recordingResetter{err: errors.New("postgres unavailable")}
	h := NewHandler(sender, Services{}, nil, nil, &mockResettableMemberRepo{},
		limiter, logger, 0, ownerID)
	h.SetResetter(resetter)

	h.HandleUpdate(context.Background(), Update{
		Message: &Message{
			From: &User{ID: ownerID, Username: "owner"},
			Chat: &Chat{ID: ownerID, Type: "private"},
			Text: "/reset confirm",
		},
	})

	mu.Lock()
	got := strings.Join(messages, "\n")
	mu.Unlock()
	if strings.Contains(got, "Reset selesai") {
		t.Errorf("a failed reset must not claim success: %q", got)
	}
	if !strings.Contains(got, "gagal") {
		t.Errorf("expected an explicit failure message, got %q", got)
	}
}

// TestExecuteReset_UnavailableResetterIsReported pins that a missing resetter
// produces a message instead of a silent no-op.
func TestExecuteReset_UnavailableResetterIsReported(t *testing.T) {
	var (
		mu       sync.Mutex
		messages []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		if text, ok := payload["text"].(string); ok {
			mu.Lock()
			messages = append(messages, text)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()

	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	limiter := NewRateLimiter(100, 60_000_000_000)

	ownerID := int64(99999)
	h := NewHandler(sender, Services{}, nil, nil, &mockResettableMemberRepo{},
		limiter, logger, 0, ownerID)

	h.HandleUpdate(context.Background(), Update{
		Message: &Message{
			From: &User{ID: ownerID, Username: "owner"},
			Chat: &Chat{ID: ownerID, Type: "private"},
			Text: "/reset confirm",
		},
	})

	mu.Lock()
	got := strings.Join(messages, "\n")
	mu.Unlock()
	if !strings.Contains(got, "belum tersedia") {
		t.Errorf("expected an unavailable-reset message, got %q", got)
	}
}

func TestSender_RegisterCommandScopes_OwnerOnlyReset(t *testing.T) {
	var payloads []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		payloads = append(payloads, payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer srv.Close()

	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"

	if err := sender.RegisterCommandScopes(context.Background(), 99999); err != nil {
		t.Fatalf("RegisterCommandScopes failed: %v", err)
	}

	if len(payloads) != 2 {
		t.Fatalf("expected 2 setMyCommands calls (default + owner chat), got %d", len(payloads))
	}

	defaultCmds, _ := json.Marshal(payloads[0]["commands"])
	if strings.Contains(string(defaultCmds), `"reset"`) {
		t.Errorf("default scope commands should NOT contain reset: %s", string(defaultCmds))
	}

	ownerCmds, _ := json.Marshal(payloads[1]["commands"])
	if !strings.Contains(string(ownerCmds), `"reset"`) {
		t.Errorf("owner chat scope commands SHOULD contain reset: %s", string(ownerCmds))
	}
}

// recordingResetter stands in for the Postgres resetter.
type recordingResetter struct {
	calls   int
	removed int
	err     error
}

func (r *recordingResetter) ResetAll(context.Context) (int, error) {
	r.calls++
	return r.removed, r.err
}
