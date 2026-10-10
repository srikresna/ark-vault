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

	"github.com/arkcode369/ark-vault/internal/domain"
)

type mockResettableMemberRepo struct {
	resetCalled bool
	archived    int
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
	return m.archived, nil
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
