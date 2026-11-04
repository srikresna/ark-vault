package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// signTelegram builds a valid Login Widget payload for a token, mirroring what
// Telegram sends so the verifier is tested against the real algorithm rather
// than a hand-written signature.
func signTelegram(t *testing.T, token string, p telegramAuthPayload) telegramAuthPayload {
	t.Helper()
	pairs := map[string]string{
		"auth_date":  fmt.Sprintf("%d", p.AuthDate),
		"first_name": p.FirstName,
		"id":         fmt.Sprintf("%d", p.ID),
	}
	if p.LastName != "" {
		pairs["last_name"] = p.LastName
	}
	if p.Username != "" {
		pairs["username"] = p.Username
	}
	if p.PhotoURL != "" {
		pairs["photo_url"] = p.PhotoURL
	}
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(k + "=" + pairs[k])
	}
	secret := sha256.Sum256([]byte(token))
	mac := hmac.New(sha256.New, secret[:])
	mac.Write([]byte(sb.String()))
	p.Hash = hex.EncodeToString(mac.Sum(nil))
	return p
}

func TestVerifyTelegramAuth(t *testing.T) {
	const token = "123456:AAH-test-token"
	now := time.Now()

	valid := signTelegram(t, token, telegramAuthPayload{
		ID: 42, FirstName: "Trader", Username: "trader", AuthDate: now.Unix(),
	})

	tests := []struct {
		name    string
		payload telegramAuthPayload
		wantErr bool
	}{
		{name: "valid", payload: valid},
		{name: "tampered id", payload: func() telegramAuthPayload {
			p := valid
			p.ID = 43
			return p
		}(), wantErr: true},
		{name: "tampered username", payload: func() telegramAuthPayload {
			p := valid
			p.Username = "attacker"
			return p
		}(), wantErr: true},
		{name: "missing hash", payload: func() telegramAuthPayload {
			p := valid
			p.Hash = ""
			return p
		}(), wantErr: true},
		{name: "expired", payload: signTelegram(t, token, telegramAuthPayload{
			ID: 42, FirstName: "Trader", AuthDate: now.Add(-25 * time.Hour).Unix(),
		}), wantErr: true},
		{name: "wrong token", payload: signTelegram(t, "999:other", telegramAuthPayload{
			ID: 42, FirstName: "Trader", AuthDate: now.Unix(),
		}), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyTelegramAuth(token, tc.payload, now, 24*time.Hour)
			if tc.wantErr && err == nil {
				t.Fatal("expected the payload to be rejected")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected the payload to be accepted, got %v", err)
			}
		})
	}
}

func TestSanitizeRedirect(t *testing.T) {
	tests := map[string]string{
		"/trades":           "/trades",
		"":                  "",
		"//evil.example":    "",
		"https://evil.test": "",
		"/\\evil":           "",
		"trades":            "",
		"/trades?x=1":       "/trades?x=1",
	}
	for in, want := range tests {
		if got := sanitizeRedirect(in); got != want {
			t.Errorf("sanitizeRedirect(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanActOn(t *testing.T) {
	member := &domain.Member{ID: "m1", Role: domain.RoleMember}
	admin := &domain.Member{ID: "a1", Role: domain.RoleAdmin}
	owner := &domain.Member{ID: "o1", Role: domain.RoleOwner}
	other := &domain.Member{ID: "m2", Role: domain.RoleMember}

	if !canActOn(member, member) {
		t.Error("a member must access their own data")
	}
	if canActOn(member, other) {
		t.Error("a member must not access another member's data")
	}
	if !canActOn(admin, other) {
		t.Error("an admin must access any member's data")
	}
	if !canActOn(owner, other) {
		t.Error("an owner must access any member's data")
	}
	if canActOn(nil, member) || canActOn(member, nil) {
		t.Error("a nil actor or owner must never be allowed")
	}
}

func TestWithAuthRejectsAnonymous(t *testing.T) {
	s := &Server{
		cfg:  Config{SessionCookieName: "ark_session"},
		deps: Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
	}
	handler := s.withAuth(func(http.ResponseWriter, *http.Request, *domain.Member) {
		t.Fatal("handler must not run without a session")
	})
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"symbol":"XAUUSD","bogus":1}`))
	var out createTradeRequest
	if err := decodeJSON(req, &out); err == nil {
		t.Fatal("expected an unknown field to be rejected")
	}
}

func TestTradeFromRequestValidates(t *testing.T) {
	tests := []struct {
		name    string
		req     createTradeRequest
		wantErr bool
	}{
		{name: "valid", req: createTradeRequest{Symbol: "xauusd", Direction: "buy"}},
		{name: "missing symbol", req: createTradeRequest{Direction: "BUY"}, wantErr: true},
		{name: "bad direction", req: createTradeRequest{Symbol: "XAUUSD", Direction: "HOLD"}, wantErr: true},
		{name: "bad status", req: createTradeRequest{Symbol: "XAUUSD", Direction: "BUY", Status: "MAYBE"}, wantErr: true},
		{name: "bad date", req: createTradeRequest{Symbol: "XAUUSD", Direction: "BUY", Date: "10/03/2026"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			trade, err := tradeFromRequest(tc.req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", trade)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if trade.Symbol != "XAUUSD" {
				t.Errorf("symbol = %q, want the upper-cased input", trade.Symbol)
			}
		})
	}
}
