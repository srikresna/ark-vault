package telegram

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/arkcode369/ark-vault/internal/domain"
)

var (
	journalTag = regexp.MustCompile(`(?i)^#journal`)
	fieldRe    = regexp.MustCompile(`(?i)^(pair|symbol|type|direction|rr|result|session|confluence|notes?)\s*[:=]\s*(.+)$`)
	rrValueRe  = regexp.MustCompile(`(?i)^([+-]?\d+\.?\d*)\s*$`)
	rrStatusRe = regexp.MustCompile(`(?i)^(WIN|LOSS|BE)\s*(\d+\.?\d*)?\s*(rr|r)?$`)
)

// Notion property limits: a rich_text segment holds at most 2000 characters
// and titles are also capped. Truncating user input before it reaches the API
// prevents hard 400 errors on very long confluence/notes/symbol values.
const (
	maxSymbolLen    = 64
	maxTextFieldLen = 1900 // headroom below Notion's 2000-char limit
)

// truncateUTF8 cuts s to at most max runes without splitting a multi-byte
// character.
func truncateUTF8(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// ParseResult holds the output of parsing a #journal message.
type ParseResult struct {
	Trade *domain.Trade
	Err   string // user-friendly error message if parsing failed
}

// ParseJournalMessage extracts trade data from a structured text message.
func ParseJournalMessage(text string) ParseResult {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) == 0 || !journalTag.MatchString(lines[0]) {
		return ParseResult{Err: "Message harus diawali dengan #journal"}
	}

	t := &domain.Trade{}
	parsed := make(map[string]bool)
	var invalidRR string

	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		matches := fieldRe.FindStringSubmatch(line)
		if matches == nil {
			continue
		}
		key := strings.ToLower(matches[1])
		val := strings.TrimSpace(matches[2])

		switch {
		case key == "pair" || key == "symbol":
			t.Symbol = strings.ToUpper(strings.ReplaceAll(truncateUTF8(val, maxSymbolLen), "/", ""))
			parsed["symbol"] = true
		case key == "type" || key == "direction":
			dir := strings.ToUpper(val)
			switch dir {
			case "BUY", "LONG":
				t.Direction = domain.DirBuy
			case "SELL", "SHORT":
				t.Direction = domain.DirSell
			}
			parsed["direction"] = true
		case key == "rr" || key == "result":
			if !parseRR(val, t) {
				invalidRR = val
			}
			parsed["rr"] = true
		case key == "session":
			t.TimeWindow = parseSession(val)
			parsed["session"] = true
		case key == "confluence":
			t.Confluence = truncateUTF8(val, maxTextFieldLen)
			parsed["confluence"] = true
		case strings.HasPrefix(key, "note"):
			t.Notes = truncateUTF8(val, maxTextFieldLen)
		}
	}

	// Validate required fields
	var missing []string
	if !parsed["symbol"] {
		missing = append(missing, "Pair/Symbol")
	}
	if !parsed["direction"] {
		missing = append(missing, "Type/Direction")
	}

	if len(missing) > 0 {
		return ParseResult{
			Err: "Field berikut belum diisi: " + strings.Join(missing, ", ") +
				"\n\nFormat yang benar:\n#journal\nPair: XAUUSD\nType: BUY\nRR: +2\nSession: London\nConfluence: FVG + OB mitigation on 15m",
		}
	}

	if invalidRR != "" {
		// Err messages are rendered via SendHTML, so the echoed raw value
		// must be entity-escaped or markup- and ampersand-bearing input
		// breaks entity parsing and the member gets no feedback at all.
		return ParseResult{
			Err: "Format RR/Result tidak valid: \"" + escapeHTML(invalidRR) + "\"\n\n" +
				"Gunakan salah satu dari:\n+2, -1, 0 (angka) atau WIN 2RR, LOSS 1, BE",
		}
	}

	return ParseResult{Trade: t}
}

func parseRR(val string, t *domain.Trade) bool {
	trimmed := strings.TrimSpace(val)
	// Try simple numeric: +2, -1, 0
	if m := rrValueRe.FindStringSubmatch(trimmed); m != nil {
		if rr, err := strconv.ParseFloat(m[1], 64); err == nil {
			t.ResultRR = rr
			deriveStatusFromRR(t)
			return true
		}
	}

	// Try status format: WIN 2RR, LOSS 1RR, BE
	if m := rrStatusRe.FindStringSubmatch(trimmed); m != nil {
		switch strings.ToUpper(m[1]) {
		case "WIN":
			t.Status = domain.StatusWin
			if m[2] != "" {
				if rr, err := strconv.ParseFloat(m[2], 64); err == nil {
					t.ResultRR = rr
				}
			}
		case "LOSS":
			t.Status = domain.StatusLoss
			if m[2] != "" {
				if rr, err := strconv.ParseFloat(m[2], 64); err == nil {
					t.ResultRR = -rr
				}
			}
		case "BE":
			t.Status = domain.StatusBE
			t.ResultRR = 0
		}
		return true
	}
	// Present but unparseable: the caller must reject the line instead of
	// silently recording an OPEN 0R trade.
	return false
}

func deriveStatusFromRR(t *domain.Trade) {
	if t.ResultRR > 0 {
		t.Status = domain.StatusWin
	} else if t.ResultRR < 0 {
		t.Status = domain.StatusLoss
	} else {
		t.Status = domain.StatusBE
	}
}

func parseSession(val string) domain.TimeWindow {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "asia":
		return domain.SessionAsia
	case "london":
		return domain.SessionLondon
	case "ny am", "nyam", "ny_am":
		return domain.SessionNYAM
	case "ny pm", "nypm", "ny_pm":
		return domain.SessionNYPM
	default:
		// Free-text sessions are kept but bounded: Notion select options cap
		// at 100 characters, and unbounded raw values are an injection vector
		// into the CSV/HTML sinks downstream.
		return domain.TimeWindow(truncateUTF8(strings.TrimSpace(val), 100))
	}
}
