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
			t.Symbol = strings.ToUpper(strings.ReplaceAll(val, "/", ""))
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
			parseRR(val, t)
			parsed["rr"] = true
		case key == "session":
			t.TimeWindow = parseSession(val)
			parsed["session"] = true
		case key == "confluence":
			t.Confluence = val
			parsed["confluence"] = true
		case strings.HasPrefix(key, "note"):
			t.Notes = val
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

	return ParseResult{Trade: t}
}

func parseRR(val string, t *domain.Trade) {
	// Try simple numeric: +2, -1, 0
	if m := rrValueRe.FindStringSubmatch(strings.TrimSpace(val)); m != nil {
		if rr, err := strconv.ParseFloat(m[1], 64); err == nil {
			t.ResultRR = rr
			deriveStatusFromRR(t)
			return
		}
	}

	// Try status format: WIN 2RR, LOSS 1RR, BE
	if m := rrStatusRe.FindStringSubmatch(strings.TrimSpace(val)); m != nil {
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
	}
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
		return domain.TimeWindow(val)
	}
}
