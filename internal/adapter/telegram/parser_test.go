package telegram

import (
	"strings"
	"testing"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// TestParseJournalMessage_TruncatesLongFields pins the guard against Notion
// 400 errors: rich_text segments hold at most 2000 characters, so oversized
// confluence/notes/symbol values are truncated before they reach the API.
func TestParseJournalMessage_TruncatesLongFields(t *testing.T) {
	long := strings.Repeat("A", 5000)
	input := "#journal\nPair: " + long + "\nType: BUY\nRR: +1\nConfluence: " + long + "\nNotes: " + long

	result := ParseJournalMessage(input)
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if got := len([]rune(result.Trade.Symbol)); got > maxSymbolLen {
		t.Errorf("symbol length = %d, want <= %d", got, maxSymbolLen)
	}
	if got := len([]rune(result.Trade.Confluence)); got != maxTextFieldLen {
		t.Errorf("confluence length = %d, want exactly %d", got, maxTextFieldLen)
	}
	if got := len([]rune(result.Trade.Notes)); got != maxTextFieldLen {
		t.Errorf("notes length = %d, want exactly %d", got, maxTextFieldLen)
	}
}

// TestTruncateUTF8 does not split multi-byte characters.
func TestTruncateUTF8(t *testing.T) {
	if got := truncateUTF8("héllo wörld", 5); got != "héllo" {
		t.Errorf("truncateUTF8 = %q, want %q", got, "héllo")
	}
	if got := truncateUTF8("short", 10); got != "short" {
		t.Errorf("truncateUTF8 should keep short strings, got %q", got)
	}
}

func TestParseJournalMessage_Valid(t *testing.T) {
	input := `#journal
Pair: XAUUSD
Type: BUY
RR: +2
Session: London
Confluence: FVG + OB mitigation on 15m`

	result := ParseJournalMessage(input)
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}

	tr := result.Trade
	if tr.Symbol != "XAUUSD" {
		t.Errorf("symbol: got %q, want XAUUSD", tr.Symbol)
	}
	if tr.Direction != domain.DirBuy {
		t.Errorf("direction: got %q, want BUY", tr.Direction)
	}
	if tr.ResultRR != 2 {
		t.Errorf("rr: got %f, want 2", tr.ResultRR)
	}
	if tr.Status != domain.StatusWin {
		t.Errorf("status: got %q, want WIN", tr.Status)
	}
	if tr.TimeWindow != domain.SessionLondon {
		t.Errorf("session: got %q, want London", tr.TimeWindow)
	}
	if tr.Confluence != "FVG + OB mitigation on 15m" {
		t.Errorf("confluence: got %q", tr.Confluence)
	}
}

func TestParseJournalMessage_LossRR(t *testing.T) {
	input := `#journal
Pair: EURUSD
Type: SELL
RR: -1
Session: NY AM`

	result := ParseJournalMessage(input)
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}

	tr := result.Trade
	if tr.Symbol != "EURUSD" {
		t.Errorf("symbol: got %q", tr.Symbol)
	}
	if tr.Direction != domain.DirSell {
		t.Errorf("direction: got %q", tr.Direction)
	}
	if tr.Status != domain.StatusLoss {
		t.Errorf("status: got %q", tr.Status)
	}
	if tr.ResultRR != -1 {
		t.Errorf("rr: got %f, want -1", tr.ResultRR)
	}
	if tr.TimeWindow != domain.SessionNYAM {
		t.Errorf("session: got %q, want NY AM", tr.TimeWindow)
	}
}

func TestParseJournalMessage_OpenTrade(t *testing.T) {
	input := `#journal
Pair: BTCUSD
Type: BUY`

	result := ParseJournalMessage(input)
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if result.Trade.Status != "" {
		t.Errorf("status should be empty for open trade, got %q", result.Trade.Status)
	}
}

func TestParseJournalMessage_MissingFields(t *testing.T) {
	input := `#journal
RR: +2`

	result := ParseJournalMessage(input)
	if result.Err == "" {
		t.Fatal("expected error for missing fields")
	}
	if result.Trade != nil {
		t.Fatal("should not return trade on error")
	}
}

func TestParseJournalMessage_NoTag(t *testing.T) {
	input := `Pair: EURUSD
Type: BUY
RR: +2`

	result := ParseJournalMessage(input)
	if result.Err == "" {
		t.Fatal("expected error for missing #journal tag")
	}
}

func TestParseJournalMessage_CaseInsensitive(t *testing.T) {
	input := `#JOURNAL
pair: eurusd
type: buy`

	result := ParseJournalMessage(input)
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if result.Trade.Symbol != "EURUSD" {
		t.Errorf("symbol: got %q", result.Trade.Symbol)
	}
	if result.Trade.Direction != domain.DirBuy {
		t.Errorf("direction: got %q", result.Trade.Direction)
	}
}

func TestParseJournalMessage_BreakEven(t *testing.T) {
	input := `#journal
Pair: GBPJPY
Type: BUY
RR: 0`

	result := ParseJournalMessage(input)
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if result.Trade.Status != domain.StatusBE {
		t.Errorf("status: got %q, want BE", result.Trade.Status)
	}
	if result.Trade.ResultRR != 0 {
		t.Errorf("rr: got %f, want 0", result.Trade.ResultRR)
	}
}

func TestParseJournalMessage_WithSlash(t *testing.T) {
	input := `#journal
Pair: EUR/USD
Type: BUY`

	result := ParseJournalMessage(input)
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if result.Trade.Symbol != "EURUSD" {
		t.Errorf("symbol: got %q, want EURUSD (slash stripped)", result.Trade.Symbol)
	}
}

func TestParseJournalMessage_StatusFormat(t *testing.T) {
	input := `#journal
Pair: XAUUSD
Type: SELL
RR: WIN 2RR`

	result := ParseJournalMessage(input)
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if result.Trade.Status != domain.StatusWin {
		t.Errorf("status: got %q, want WIN", result.Trade.Status)
	}
	if result.Trade.ResultRR != 2 {
		t.Errorf("rr: got %f, want 2", result.Trade.ResultRR)
	}
}

func TestParseJournalMessage_LossStatusFormat(t *testing.T) {
	input := `#journal
Pair: EURUSD
Type: BUY
RR: LOSS 1RR`

	result := ParseJournalMessage(input)
	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if result.Trade.Status != domain.StatusLoss {
		t.Errorf("status: got %q, want LOSS", result.Trade.Status)
	}
	if result.Trade.ResultRR != -1 {
		t.Errorf("rr: got %f, want -1", result.Trade.ResultRR)
	}
}
