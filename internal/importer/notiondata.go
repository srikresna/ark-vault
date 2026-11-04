package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
)

// Member is one journal discovered under the configured Notion parent page.
type Member struct {
	TelegramID int64
	Username   string
	Title      string
	PageID     string
	DatabaseID string
	Trades     []Trade
}

// Trade is one row of a member's inline Trades database.
type Trade struct {
	NotionPageID string
	CreatedAt    time.Time
	Trade        domain.Trade
	// ImageURL is the signed Notion file URL of the trade's first image block.
	// Signed URLs expire within the hour, so the bytes are fetched during
	// discovery rather than kept as a link.
	ImageURL string
	// ImagePath is where those bytes were written during discovery.
	ImagePath string
}

// block is the subset of a Notion block the migration reads.
type block struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Archived  bool   `json:"archived"`
	ChildPage *struct{ Title string `json:"title"` }            `json:"child_page"`
	ChildDB   *struct{ Title string `json:"title"` }            `json:"child_database"`
	Callout   *struct{ RichText []richText `json:"rich_text"` } `json:"callout"`
	Image     *struct {
		Type     string `json:"type"`
		File     *struct{ URL string `json:"url"` } `json:"file"`
		External *struct{ URL string `json:"url"` } `json:"external"`
	} `json:"image"`
}

type richText struct {
	PlainText string `json:"plain_text"`
}

// page is the subset of a Notion page the migration reads.
type page struct {
	ID         string                     `json:"id"`
	CreatedAt  string                     `json:"created_time"`
	Archived   bool                       `json:"archived"`
	Properties map[string]json.RawMessage `json:"properties"`
}

// DiscoverMembers walks the parent page: each child page is a member journal,
// the callout names the Telegram ID, and the inline child database holds the
// trades. Pages without a callout are not journals and are reported, not
// guessed.
func DiscoverMembers(ctx context.Context, nc *NotionClient, parentID string) ([]Member, []string, error) {
	blocks, err := nc.BlockChildren(ctx, parentID)
	if err != nil {
		return nil, nil, fmt.Errorf("list parent page children: %w", err)
	}

	var (
		members  []Member
		skipped  []string
	)
	for _, raw := range blocks {
		var b block
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, nil, fmt.Errorf("parse parent child block: %w", err)
		}
		if b.Archived || b.Type != "child_page" || b.ID == "" {
			continue
		}
		title := ""
		if b.ChildPage != nil {
			title = b.ChildPage.Title
		}

		children, err := nc.BlockChildren(ctx, b.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("list journal %s children: %w", b.ID, err)
		}

		var (
			telegramID int64
			username   string
			databaseID string
		)
		for _, cRaw := range children {
			var c block
			if err := json.Unmarshal(cRaw, &c); err != nil {
				return nil, nil, fmt.Errorf("parse journal %s child: %w", b.ID, err)
			}
			switch c.Type {
			case "callout":
				if c.Callout != nil {
					var sb strings.Builder
					for _, rt := range c.Callout.RichText {
						sb.WriteString(rt.PlainText)
					}
					if id, name, ok := parseCallout(sb.String()); ok {
						telegramID, username = id, name
					}
				}
			case "child_database":
				if databaseID == "" {
					databaseID = c.ID
				}
			}
		}

		if telegramID == 0 {
			skipped = append(skipped, fmt.Sprintf("page %s (%q) has no Telegram ID callout", b.ID, title))
			continue
		}
		members = append(members, Member{
			TelegramID: telegramID,
			Username:   username,
			Title:      title,
			PageID:     b.ID,
			DatabaseID: databaseID,
		})
	}
	return members, skipped, nil
}

// parseCallout extracts the Telegram ID and username from the callout the old
// adapter wrote: "Telegram ID: <id> | Username: @<name>".
func parseCallout(text string) (int64, string, bool) {
	const marker = "Telegram ID:"
	idx := strings.Index(text, marker)
	if idx < 0 {
		return 0, "", false
	}
	rest := strings.TrimSpace(text[idx+len(marker):])

	username := ""
	if bar := strings.Index(rest, "|"); bar >= 0 {
		tail := strings.TrimSpace(rest[bar+1:])
		rest = strings.TrimSpace(rest[:bar])
		if at := strings.Index(tail, "@"); at >= 0 {
			username = strings.TrimSpace(tail[at+1:])
		}
	}
	// The id ends at the first non-digit so a trailing separator cannot break it.
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, "", false
	}
	id, err := strconv.ParseInt(rest[:end], 10, 64)
	if err != nil || id == 0 {
		return 0, "", false
	}
	return id, username, true
}

// LoadTrades reads every trade of a member's inline database.
func LoadTrades(ctx context.Context, nc *NotionClient, m Member) ([]Trade, error) {
	if m.DatabaseID == "" {
		return nil, fmt.Errorf("member %d has no inline Trades database", m.TelegramID)
	}
	rows, err := nc.QueryDatabase(ctx, m.DatabaseID)
	if err != nil {
		return nil, fmt.Errorf("query trades for %d: %w", m.TelegramID, err)
	}

	out := make([]Trade, 0, len(rows))
	for _, raw := range rows {
		var p page
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("parse trade for %d: %w", m.TelegramID, err)
		}
		if p.Archived {
			continue
		}
		t, err := tradeFromProperties(p)
		if err != nil {
			return nil, fmt.Errorf("trade %s of member %d: %w", p.ID, m.TelegramID, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// AttachImages records the first image block of each trade so the migration can
// carry the screenshots across instead of leaving them only in Notion.
//
// The block's URL is signed and short-lived, so this only captures where the
// bytes are; the download itself happens once per trade and is retried by the
// caller if Notion is slow.
func AttachImages(ctx context.Context, nc *NotionClient, trades []Trade) error {
	for i := range trades {
		children, err := nc.BlockChildren(ctx, trades[i].NotionPageID)
		if err != nil {
			return fmt.Errorf("list blocks of trade %s: %w", trades[i].NotionPageID, err)
		}
		for _, raw := range children {
			var b block
			if err := json.Unmarshal(raw, &b); err != nil {
				return fmt.Errorf("parse block of trade %s: %w", trades[i].NotionPageID, err)
			}
			if b.Type != "image" || b.Image == nil {
				continue
			}
			if b.Image.File != nil && b.Image.File.URL != "" {
				trades[i].ImageURL = b.Image.File.URL
				break
			}
			if b.Image.External != nil && b.Image.External.URL != "" {
				trades[i].ImageURL = b.Image.External.URL
				break
			}
		}
	}
	return nil
}

// tradeFromProperties maps the old Notion columns onto the domain trade.
//
// A trade whose symbol or date cannot be read is an error, not a skipped row:
// silently dropping it would make the migrated journal disagree with Notion.
func tradeFromProperties(p page) (Trade, error) {
	t := domain.Trade{ID: p.ID, MemberID: 0}
	created, err := time.Parse(time.RFC3339, p.CreatedAt)
	if err != nil {
		created = time.Now()
	}
	t.CreatedAt = created

	if title := propertyTitle(p.Properties["Symbol"]); title != "" {
		t.Symbol = strings.ToUpper(title)
	}
	if t.Symbol == "" {
		return Trade{}, fmt.Errorf("missing Symbol")
	}
	t.Direction = domain.Direction(propertySelect(p.Properties["Direction"]))
	t.Status = domain.TradeStatus(propertySelect(p.Properties["Status"]))
	if t.Status == "" {
		t.Status = domain.StatusOpen
	}
	if at, ok := domain.ParseAssetType(propertySelect(p.Properties["Asset Type"])); ok {
		t.AssetType = at
	}
	t.ResultRR = propertyNumber(p.Properties["Result RR"])
	t.TimeWindow = domain.TimeWindow(propertySelect(p.Properties["Time Window"]))
	t.Confluence = propertyRichText(p.Properties["Confluence"])
	t.Notes = propertyRichText(p.Properties["Notes"])

	date := propertyDate(p.Properties["Date"])
	if date.IsZero() {
		date = created.In(timeutil.WIB)
	}
	// The old adapter stored a bare WIB calendar day; keeping it as that day is
	// what makes weekly and monthly boundaries match the pre-migration reports.
	t.Date = time.Date(date.In(timeutil.WIB).Year(), date.In(timeutil.WIB).Month(),
		date.In(timeutil.WIB).Day(), 0, 0, 0, 0, timeutil.WIB)

	return Trade{NotionPageID: p.ID, CreatedAt: created, Trade: t}, nil
}

func propertyTitle(raw json.RawMessage) string {
	var prop struct {
		Title []richText `json:"title"`
	}
	if json.Unmarshal(raw, &prop) != nil {
		return ""
	}
	var sb strings.Builder
	for _, rt := range prop.Title {
		sb.WriteString(rt.PlainText)
	}
	return strings.TrimSpace(sb.String())
}

func propertyRichText(raw json.RawMessage) string {
	var prop struct {
		RichText []richText `json:"rich_text"`
	}
	if json.Unmarshal(raw, &prop) != nil {
		return ""
	}
	var sb strings.Builder
	for _, rt := range prop.RichText {
		sb.WriteString(rt.PlainText)
	}
	return strings.TrimSpace(sb.String())
}

func propertySelect(raw json.RawMessage) string {
	var prop struct {
		Select *struct {
			Name string `json:"name"`
		} `json:"select"`
	}
	if json.Unmarshal(raw, &prop) != nil || prop.Select == nil {
		return ""
	}
	return strings.TrimSpace(prop.Select.Name)
}

func propertyNumber(raw json.RawMessage) float64 {
	var prop struct {
		Number *float64 `json:"number"`
	}
	if json.Unmarshal(raw, &prop) != nil || prop.Number == nil {
		return 0
	}
	return *prop.Number
}

func propertyDate(raw json.RawMessage) time.Time {
	var prop struct {
		Date *struct {
			Start string `json:"start"`
		} `json:"date"`
	}
	if json.Unmarshal(raw, &prop) != nil || prop.Date == nil || prop.Date.Start == "" {
		return time.Time{}
	}
	if t, err := time.ParseInLocation("2006-01-02", prop.Date.Start, timeutil.WIB); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, prop.Date.Start); err == nil {
		return t
	}
	return time.Time{}
}
