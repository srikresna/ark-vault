package importer

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

func TestParseCallout(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantID   int64
		wantUser string
		wantOK   bool
	}{
		{
			name:     "id and username",
			text:     "Telegram ID: 12345678 | Username: @trader",
			wantID:   12345678,
			wantUser: "trader",
			wantOK:   true,
		},
		{
			name:   "id only",
			text:   "Telegram ID: 42",
			wantID: 42,
			wantOK: true,
		},
		{
			name:     "empty username keeps the id",
			text:     "Telegram ID: 900 | Username: @",
			wantID:   900,
			wantUser: "",
			wantOK:   true,
		},
		{
			name:   "no callout marker",
			text:   "Just a note about the journal",
			wantOK: false,
		},
		{
			name:   "marker without digits",
			text:   "Telegram ID: abc",
			wantOK: false,
		},
		{
			name:   "leading text before the marker",
			text:   "📊 Telegram ID: 555 | Username: @someone",
			wantID: 555,
			wantUser: "someone",
			wantOK: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, user, ok := parseCallout(tc.text)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if id != tc.wantID {
				t.Errorf("id = %d, want %d", id, tc.wantID)
			}
			if user != tc.wantUser {
				t.Errorf("user = %q, want %q", user, tc.wantUser)
			}
		})
	}
}

func TestTradeFromProperties(t *testing.T) {
	props := func(date string) map[string]json.RawMessage {
		return map[string]json.RawMessage{
			"Symbol":     json.RawMessage(`{"title":[{"plain_text":"xauusd"}]}`),
			"Direction":  json.RawMessage(`{"select":{"name":"BUY"}}`),
			"Status":     json.RawMessage(`{"select":{"name":"WIN"}}`),
			"Asset Type": json.RawMessage(`{"select":{"name":"GOLD"}}`),
			"Result RR":  json.RawMessage(`{"number":2.5}`),
			"Time Window": json.RawMessage(`{"select":{"name":"London"}}`),
			"Confluence": json.RawMessage(`{"rich_text":[{"plain_text":"FVG "},{"plain_text":"+ OB"}]}`),
			"Notes":      json.RawMessage(`{"rich_text":[{"plain_text":"note"}]}`),
			"Date":       []byte(date),
		}
	}

	t.Run("full row", func(t *testing.T) {
		p := page{ID: "old-page", CreatedAt: "2026-03-10T08:00:00Z", Properties: props(`{"date":{"start":"2026-03-10"}}`)}
		got, err := tradeFromProperties(p)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Trade.Symbol != "XAUUSD" {
			t.Errorf("symbol = %q, want upper-cased input", got.Trade.Symbol)
		}
		if got.Trade.Direction != domain.DirBuy || got.Trade.Status != domain.StatusWin {
			t.Errorf("direction/status = %v/%v", got.Trade.Direction, got.Trade.Status)
		}
		if got.Trade.AssetType != domain.AssetGold {
			t.Errorf("asset = %v, want GOLD", got.Trade.AssetType)
		}
		if got.Trade.ResultRR != 2.5 {
			t.Errorf("rr = %v, want 2.5", got.Trade.ResultRR)
		}
		if got.Trade.TimeWindow != domain.SessionLondon {
			t.Errorf("session = %v", got.Trade.TimeWindow)
		}
		if got.Trade.Confluence != "FVG + OB" {
			t.Errorf("confluence = %q, want the segments joined", got.Trade.Confluence)
		}
		if got.Trade.Notes != "note" {
			t.Errorf("notes = %q", got.Trade.Notes)
		}
		wantDate := time.Date(2026, 3, 10, 0, 0, 0, 0, got.Trade.Date.Location())
		if !got.Trade.Date.Equal(wantDate) {
			t.Errorf("date = %v, want %v", got.Trade.Date, wantDate)
		}
	})

	t.Run("missing symbol is an error, not a skipped row", func(t *testing.T) {
		bad := map[string]json.RawMessage{"Direction": json.RawMessage(`{"select":{"name":"BUY"}}`)}
		if _, err := tradeFromProperties(page{ID: "x", CreatedAt: "2026-03-10T08:00:00Z", Properties: bad}); err == nil {
			t.Fatal("expected an error for a row without Symbol")
		}
	})

	t.Run("missing date falls back to the creation time", func(t *testing.T) {
		p := page{ID: "y", CreatedAt: "2026-03-10T08:00:00Z", Properties: props(`{"date":null}`)}
		got, err := tradeFromProperties(p)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Trade.Date.IsZero() {
			t.Fatal("date is zero")
		}
	})

	t.Run("missing status defaults to OPEN", func(t *testing.T) {
		p := page{ID: "z", CreatedAt: "2026-03-10T08:00:00Z", Properties: map[string]json.RawMessage{
			"Symbol": json.RawMessage(`{"title":[{"plain_text":"EURUSD"}]}`),
		}}
		got, err := tradeFromProperties(p)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Trade.Status != domain.StatusOpen {
			t.Errorf("status = %v, want OPEN", got.Trade.Status)
		}
	})
}

func TestStateDecodeRoutesKeys(t *testing.T) {
	st := &State{Skipped: map[string]int{}}

	cases := map[string]string{
		"gam:profile:42":        `{"telegram_id":42,"total_xp":10,"level":1,"title":"Retail"}`,
		"gam:streak:42":         `{"telegram_id":42,"current_streak":2,"longest_streak":3,"last_log_date":"2026-03-10"}`,
		"gam:xp_log:42:1:9":     `{"telegram_id":42,"amount":10,"reason":"trade_recorded"}`,
		"gam:trade_xp:42:t1":    `{"telegram_id":42,"trade_id":"t1","base":10,"screenshot":8}`,
		"gam:badge:42:first_trade": `{"telegram_id":42,"badge_id":"first_trade"}`,
		"gam:goal:42:2026-03":   `{"telegram_id":42,"year_month":"2026-03","goal_type":"total_trades","target_value":10}`,
		"gam:reminder_pref:42":  `{"telegram_id":42,"enabled":true,"hour":20}`,
		"gam:challenge:2026-W12": `{"year_week":"2026-W12","type":"most_trades","title":"x","start_date":"2026-03-16T00:00:00Z","end_date":"2026-03-22T00:00:00Z"}`,
	}
	for key, raw := range cases {
		if err := st.decode(key, []byte(raw)); err != nil {
			t.Fatalf("decode %s: %v", key, err)
		}
	}

	if len(st.Profiles) != 1 || len(st.Streaks) != 1 || len(st.XPEvents) != 1 ||
		len(st.Grants) != 1 || len(st.Badges) != 1 || len(st.Goals) != 1 ||
		len(st.Reminders) != 1 || len(st.Challenges) != 1 {
		t.Fatalf("decoded counts = profiles %d streaks %d events %d grants %d badges %d goals %d reminders %d challenges %d",
			len(st.Profiles), len(st.Streaks), len(st.XPEvents), len(st.Grants),
			len(st.Badges), len(st.Goals), len(st.Reminders), len(st.Challenges))
	}

	// Transport and cache keys are counted as skipped, never written.
	for _, key := range []string{
		"gam:idem:bot:1:upd:5",
		"gam:member:42",
		"gam:trades_cache:42",
		"gam:analytics_cache:42",
		"gam:report:42:2026-03",
		"gam:future_kind:42",
	} {
		if err := st.decode(key, json.RawMessage(`{}`)); err != nil {
			t.Fatalf("decode %s: %v", key, err)
		}
	}
	if st.Skipped["ledger"] != 1 || st.Skipped["member_cache"] != 1 ||
		st.Skipped["trade_cache"] != 1 || st.Skipped["analytics_cache"] != 1 ||
		st.Skipped["report_card"] != 1 || st.Skipped["unknown:future_kind"] != 1 {
		t.Fatalf("skip counters = %+v", st.Skipped)
	}
}

func TestParseMemberID(t *testing.T) {
	if id, err := ParseMemberID("gam:profile:12345"); err != nil || id != 12345 {
		t.Fatalf("id = %d, err = %v", id, err)
	}
	if _, err := ParseMemberID("gam:profile"); err == nil {
		t.Fatal("expected an error for a key without an id segment")
	}
}

func TestExtensionForContentType(t *testing.T) {
	tests := map[string]string{
		"image/png":                ".png",
		"image/jpeg":               ".jpg",
		"image/webp":               ".webp",
		"image/gif; charset=utf-8": ".gif",
		"application/octet-stream": ".jpg",
		"":                         ".jpg",
	}
	for ct, want := range tests {
		if got := extensionForContentType(ct); got != want {
			t.Errorf("extensionForContentType(%q) = %q, want %q", ct, got, want)
		}
	}
}

func TestAllowedImageExt(t *testing.T) {
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp", ".gif"} {
		if !allowedImageExt(ext) {
			t.Errorf("%q should be accepted", ext)
		}
	}
	for _, ext := range []string{".svg", ".html", ".exe", ""} {
		if allowedImageExt(ext) {
			t.Errorf("%q should be rejected", ext)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	if got := sanitizeFilename("a/b\\c"); got != "a_b_c" {
		t.Errorf("got %q, want a_b_c", got)
	}
	if got := sanitizeFilename(""); got != "trade" {
		t.Errorf("empty name = %q, want trade", got)
	}
}
