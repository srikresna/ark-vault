package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/arkcode369/ark-vault/internal/domain"
)

type mockResettableMemberRepo struct {
	resetCalled bool
	resetCalls  int
	archived    int
	resetErr    error
}

func (m *mockResettableMemberRepo) EnsureMember(ctx context.Context, member *domain.Member) (*domain.Member, error) {
	return member, nil
}
func (m *mockResettableMemberRepo) GetMember(ctx context.Context, telegramID int64) (*domain.Member, error) {
	return nil, nil
}
func (m *mockResettableMemberRepo) ListMembers(ctx context.Context) ([]domain.Member, error) {
	return nil, nil
}
func (m *mockResettableMemberRepo) ResetAll(ctx context.Context) (int, error) {
	m.resetCalled = true
	m.resetCalls++
	return m.archived, m.resetErr
}

type mockResetStore struct {
	resetCalled bool
}

func (m *mockResetStore) Reset() error {
	m.resetCalled = true
	return nil
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
	var sentMessages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		if text, ok := payload["text"].(string); ok {
			sentMessages = append(sentMessages, text)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()

	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"

	memberRepo := &mockResettableMemberRepo{archived: 4}
	resetStore := &mockResetStore{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	limiter := NewRateLimiter(100, 60_000_000_000)

	ownerID := int64(99999)
	h := NewHandler(sender, Services{}, nil, nil, memberRepo, limiter, logger, 0, ownerID)
	h.SetResetter(resetStore)

	ctx := context.Background()

	// 1. Non-owner invokes /reset confirm -> silently ignored, nothing reset, no message sent
	h.HandleUpdate(ctx, Update{
		Message: &Message{
			From: &User{ID: 12345, Username: "intruder"},
			Chat: &Chat{ID: 12345, Type: "private"},
			Text: "/reset confirm",
		},
	})
	if memberRepo.resetCalled || resetStore.resetCalled {
		t.Fatalf("non-owner should NOT be able to trigger reset")
	}
	if len(sentMessages) != 0 {
		t.Fatalf("non-owner /reset should be silently ignored, got messages: %v", sentMessages)
	}

	// 2. Owner invokes /reset confirm -> resets both BadgerDB and Notion
	h.HandleUpdate(ctx, Update{
		Message: &Message{
			From: &User{ID: ownerID, Username: "owner"},
			Chat: &Chat{ID: ownerID, Type: "private"},
			Text: "/reset confirm",
		},
	})
	if !memberRepo.resetCalled {
		t.Errorf("expected Notion MemberRepo.ResetAll to be called for owner")
	}
	if !resetStore.resetCalled {
		t.Errorf("expected BadgerDB Reset to be called for owner")
	}
	if len(sentMessages) == 0 {
		t.Errorf("expected confirmation message sent to owner")
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

// TestHandler_ResetCommand_NotionFailureAbortsBeforeBadger pins the reset
// ordering: when Notion archiving fails, BadgerDB must not be reset — its
// gamification state, member mappings and AI caches are the baseline the owner
// retries from — and the owner must never be told the reset succeeded. Once the
// Notion failure clears, a retry must run both resets, proving the handler
// latches nothing and no state was destroyed.
func TestHandler_ResetCommand_NotionFailureAbortsBeforeBadger(t *testing.T) {
	var (
		mu   sync.Mutex
		sent []string
	)
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
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()

	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"

	memberRepo := &mockResettableMemberRepo{
		archived: 2,
		resetErr: errors.New("notion API error (status 400)"),
	}
	resetStore := &mockResetStore{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	limiter := NewRateLimiter(100, 60_000_000_000)

	ownerID := int64(99999)
	h := NewHandler(sender, Services{}, nil, nil, memberRepo, limiter, logger, 0, ownerID)
	h.SetResetter(resetStore)

	ctx := context.Background()
	resetMsg := Update{
		Message: &Message{
			From: &User{ID: ownerID, Username: "owner"},
			Chat: &Chat{ID: ownerID, Type: "private"},
			Text: "/reset confirm",
		},
	}

	h.HandleUpdate(ctx, resetMsg)

	mu.Lock()
	firstRun := append([]string(nil), sent...)
	sent = nil
	mu.Unlock()

	if !memberRepo.resetCalled {
		t.Fatal("expected the Notion reset to be attempted")
	}
	if resetStore.resetCalled {
		t.Error("BadgerDB was reset even though the Notion archive failed")
	}
	if len(firstRun) == 0 {
		t.Fatal("expected a failure message to the owner, got none")
	}
	for _, msg := range firstRun {
		if strings.Contains(msg, "Reset selesai") {
			t.Errorf("owner was told the reset succeeded after a Notion failure: %q", msg)
		}
	}
	if !strings.Contains(strings.Join(firstRun, "\n"), "Reset dibatalkan") {
		t.Errorf("expected an explicit reset-cancelled message, got %v", firstRun)
	}

	// Retry after the Notion failure clears: nothing was wiped, so both resets
	// must run and the owner must finally see the success message.
	memberRepo.resetErr = nil
	h.HandleUpdate(ctx, resetMsg)

	mu.Lock()
	secondRun := append([]string(nil), sent...)
	mu.Unlock()

	if memberRepo.resetCalls != 2 {
		t.Errorf("ResetAll calls = %d, want 2 (retry must re-attempt Notion)", memberRepo.resetCalls)
	}
	if !resetStore.resetCalled {
		t.Error("retry did not reset BadgerDB")
	}
	if !strings.Contains(strings.Join(secondRun, "\n"), "Reset selesai") {
		t.Errorf("expected success message on retry, got %v", secondRun)
	}
}

// mockLeasedMemberRepo implements ResetFinalizer, so the handler can wipe
// BadgerDB while the member repository still holds its journaling lease.
type mockLeasedMemberRepo struct {
	mockResettableMemberRepo
	finalizeRan bool
	// resetInsideFinalize records whether the Badger wipe happened while the
	// lease was held.
	resetInsideFinalize bool
	store               *mockResetStore
}

func (m *mockLeasedMemberRepo) ResetAllWith(_ context.Context, finalize func() error) (int, error) {
	m.resetCalled = true
	m.resetCalls++
	if m.resetErr != nil {
		return m.archived, m.resetErr
	}
	if finalize != nil {
		m.finalizeRan = true
		if err := finalize(); err != nil {
			return m.archived, err
		}
		if m.store != nil {
			m.resetInsideFinalize = m.store.resetCalled
		}
	}
	return m.archived, nil
}

// TestExecuteReset_WipesBadgerInsideResetLease pins that the BadgerDB wipe is
// handed to ResetAllWith instead of running after it, so no journal write can be
// accepted between archiving and the wipe.
func TestExecuteReset_WipesBadgerInsideResetLease(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()

	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"

	resetStore := &mockResetStore{}
	memberRepo := &mockLeasedMemberRepo{
		mockResettableMemberRepo: mockResettableMemberRepo{archived: 3},
		store:                    resetStore,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	limiter := NewRateLimiter(100, 60_000_000_000)
	ownerID := int64(99999)
	h := NewHandler(sender, Services{}, nil, nil, memberRepo, limiter, logger, 0, ownerID)
	h.SetResetter(resetStore)

	h.HandleUpdate(context.Background(), Update{
		Message: &Message{
			From: &User{ID: ownerID, Username: "owner"},
			Chat: &Chat{ID: ownerID, Type: "private"},
			Text: "/reset confirm",
		},
	})

	if !memberRepo.finalizeRan {
		t.Fatal("handler did not pass the Badger wipe to ResetAllWith")
	}
	if !memberRepo.resetInsideFinalize {
		t.Error("BadgerDB was not wiped while the reset lease was held")
	}
	mu.Lock()
	got := strings.Join(sent, "\n")
	mu.Unlock()
	if !strings.Contains(got, "Reset selesai") {
		t.Errorf("expected success message, got %q", got)
	}
}

// TestExecuteReset_ReportsBadgerFailureFromLease pins that a wipe failure inside
// the lease is reported as a BadgerDB failure, not as a Notion archive failure.
func TestExecuteReset_ReportsBadgerFailureFromLease(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()

	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"

	resetStore := &failingResetStore{err: errors.New("badger write failed")}
	memberRepo := &mockLeasedMemberRepo{
		mockResettableMemberRepo: mockResettableMemberRepo{archived: 1},
		store:                    nil,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	limiter := NewRateLimiter(100, 60_000_000_000)
	ownerID := int64(99999)
	h := NewHandler(sender, Services{}, nil, nil, memberRepo, limiter, logger, 0, ownerID)
	h.SetResetter(resetStore)

	h.HandleUpdate(context.Background(), Update{
		Message: &Message{
			From: &User{ID: ownerID, Username: "owner"},
			Chat: &Chat{ID: ownerID, Type: "private"},
			Text: "/reset confirm",
		},
	})

	mu.Lock()
	got := strings.Join(sent, "\n")
	mu.Unlock()
	if !strings.Contains(got, "Gagal mereset BadgerDB") {
		t.Errorf("expected a BadgerDB failure message, got %q", got)
	}
	if strings.Contains(got, "Reset dibatalkan") {
		t.Errorf("BadgerDB failure was reported as a Notion archive failure: %q", got)
	}
}

type failingResetStore struct{ err error }

func (s *failingResetStore) Reset() error { return s.err }

// mayHaveBeenSavedErr models the adapter's ambiguous-write marker without
// importing the notion package.
type mayHaveBeenSavedErr struct{}

func (mayHaveBeenSavedErr) Error() string          { return "notion API error (status 500)" }
func (mayHaveBeenSavedErr) MayHaveBeenSaved() bool { return true }

// TestTradeSaveFailureMessage_WarnsToCheckHistory pins the user-facing wording:
// an ambiguous save must not tell the user to simply retry, which would
// duplicate the trade.
func TestTradeSaveFailureMessage_WarnsToCheckHistory(t *testing.T) {
	ambiguous := fmt.Errorf("save trade: %w", mayHaveBeenSavedErr{})
	if got := tradeSaveFailureMessage(ambiguous); !strings.Contains(got, "/history") {
		t.Errorf("ambiguous save message = %q, want it to point at /history", got)
	} else if strings.Contains(got, "coba lagi") {
		t.Errorf("ambiguous save message must not invite a retry: %q", got)
	}

	plain := errors.New("validation: symbol required")
	if got := tradeSaveFailureMessage(plain); !strings.Contains(got, "coba lagi") {
		t.Errorf("plain failure message = %q, want the retry hint", got)
	}
}
