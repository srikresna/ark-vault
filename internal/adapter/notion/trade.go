package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// TradeCacheStore caches member trades to avoid repeated Notion database queries.
type TradeCacheStore interface {
	GetCachedTrades(ctx context.Context, memberID int64) ([]domain.Trade, bool, error)
	SaveCachedTrades(ctx context.Context, memberID int64, trades []domain.Trade) error
	InvalidateCachedTrades(ctx context.Context, memberID int64) error
}

// TradeRepo implements service.TradeRepository using Notion + optional TTL cache.
type TradeRepo struct {
	client     *Client
	memberRepo *MemberRepo
	cacheStore TradeCacheStore
}

// NewTradeRepo creates a TradeRepo.
func NewTradeRepo(client *Client, memberRepo *MemberRepo, caches ...TradeCacheStore) *TradeRepo {
	var cs TradeCacheStore
	if len(caches) > 0 {
		cs = caches[0]
	}
	return &TradeRepo{
		client:     client,
		memberRepo: memberRepo,
		cacheStore: cs,
	}
}

// SaveTrade creates a new trade entry in the member's inline database and invalidates their trade cache.
//
// The journaling gate keeps the write from racing /reset: a trade recorded while
// journals are being archived would otherwise land in a page that is about to
// disappear. It fails fast with ErrResetInProgress instead.
func (r *TradeRepo) SaveTrade(ctx context.Context, memberID int64, trade *domain.Trade) error {
	release, err := r.memberRepo.beginJournaling()
	if err != nil {
		return err
	}
	defer release()

	member, err := r.memberRepo.GetMember(ctx, memberID)
	if err != nil {
		return fmt.Errorf("get member: %w", err)
	}
	if member.NotionDBID == "" {
		return fmt.Errorf("member %d has no trade database", memberID)
	}

	props := r.buildTradeProperties(trade, false)
	payload := map[string]interface{}{
		"parent": map[string]interface{}{
			"database_id": member.NotionDBID,
		},
		"properties": props,
	}

	raw, err := r.client.CreatePage(ctx, payload)
	if err != nil {
		if IsAmbiguousWriteError(err) {
			// The trade may exist in Notion even though the response was lost.
			// Drop the cached history so the next read reflects Notion instead
			// of serving a list that cannot contain it, and tell the user to
			// check /history rather than retry (a retry would duplicate it).
			if r.cacheStore != nil {
				_ = r.cacheStore.InvalidateCachedTrades(ctx, memberID)
			}
			return fmt.Errorf("create trade page (may have been saved, check /history before retrying): %w", err)
		}
		return fmt.Errorf("create trade page: %w", err)
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		// The page was created; only its ID is unknown. That is an ambiguous
		// write: the trade exists, so the caller must not be told to retry.
		if r.cacheStore != nil {
			_ = r.cacheStore.InvalidateCachedTrades(ctx, memberID)
		}
		return AmbiguousWriteError(fmt.Errorf("parse create response: %w", err))
	}
	if resp.ID == "" {
		if r.cacheStore != nil {
			_ = r.cacheStore.InvalidateCachedTrades(ctx, memberID)
		}
		return AmbiguousWriteError(fmt.Errorf("parse create response: empty page id"))
	}
	trade.ID = resp.ID

	if r.cacheStore != nil {
		_ = r.cacheStore.InvalidateCachedTrades(ctx, memberID)
	}
	return nil
}

// GetTrades retrieves all trades for a member from cache or their Notion inline database.
func (r *TradeRepo) GetTrades(ctx context.Context, memberID int64) ([]domain.Trade, error) {
	if r.cacheStore != nil {
		// A nil cached value is not a valid history (and would otherwise be
		// returned as a silent "no trades"); fall through to Notion.
		if cached, hit, err := r.cacheStore.GetCachedTrades(ctx, memberID); err == nil && hit && cached != nil {
			return cached, nil
		}
	}

	member, err := r.memberRepo.GetMember(ctx, memberID)
	if err != nil {
		return nil, fmt.Errorf("get member: %w", err)
	}
	if member.NotionDBID == "" {
		// GetMember repairs a journal whose database was never created, so an
		// empty ID here means the repair failed. Returning an empty list would
		// silently report "no trades" to /stats, /report and the leaderboard.
		return nil, fmt.Errorf("member %d has no trade database", memberID)
	}

	payload := map[string]interface{}{
		"sorts": []map[string]interface{}{
			{"property": "Date", "direction": "descending"},
		},
	}

	items, err := r.client.QueryDatabaseAll(ctx, member.NotionDBID, payload)
	if err != nil {
		return nil, fmt.Errorf("query trades: %w", err)
	}

	trades, err := r.parseTrades(items)
	if err != nil {
		return nil, err
	}
	if r.cacheStore != nil {
		_ = r.cacheStore.SaveCachedTrades(ctx, memberID, trades)
	}
	return trades, nil
}

// UpdateTrade updates an existing trade page in Notion and invalidates the member's trade cache.
func (r *TradeRepo) UpdateTrade(ctx context.Context, memberID int64, trade *domain.Trade) error {
	if trade == nil || trade.ID == "" {
		return fmt.Errorf("trade ID is required for update")
	}
	release, err := r.memberRepo.beginJournaling()
	if err != nil {
		return err
	}
	defer release()

	trade.AutoDetectAsset()
	// forUpdate=true emits clear-values for optional properties so an edit can
	// actually empty Time Window/Confluence/Notes instead of leaving the old
	// value behind in Notion.
	props := r.buildTradeProperties(trade, true)
	payload := map[string]interface{}{
		"properties": props,
	}
	if _, err := r.client.UpdatePage(ctx, trade.ID, payload); err != nil {
		return fmt.Errorf("update trade page: %w", err)
	}
	if r.cacheStore != nil {
		_ = r.cacheStore.InvalidateCachedTrades(ctx, memberID)
	}
	return nil
}

// DeleteTrade archives a trade page in Notion and invalidates the member's trade cache.
func (r *TradeRepo) DeleteTrade(ctx context.Context, memberID int64, tradeID string) error {
	if tradeID == "" {
		return fmt.Errorf("trade ID is required for delete")
	}
	release, err := r.memberRepo.beginJournaling()
	if err != nil {
		return err
	}
	defer release()

	payload := map[string]interface{}{
		"archived": true,
	}
	if _, err := r.client.UpdatePage(ctx, tradeID, payload); err != nil {
		return fmt.Errorf("archive trade page: %w", err)
	}
	if r.cacheStore != nil {
		_ = r.cacheStore.InvalidateCachedTrades(ctx, memberID)
	}
	return nil
}

// buildTradeProperties converts a Trade to Notion page properties.
//
// When forUpdate is true, optional properties are emitted even when empty
// (select:null, rich_text:[]) so an update clears them in Notion; a create
// omits them instead, leaving the Notion defaults in place.
func (r *TradeRepo) buildTradeProperties(t *domain.Trade, forUpdate bool) map[string]interface{} {
	props := map[string]interface{}{
		"Symbol": map[string]interface{}{
			"title": []map[string]interface{}{
				{"text": map[string]interface{}{"content": t.Symbol}},
			},
		},
		"Direction": map[string]interface{}{
			"select": map[string]interface{}{"name": string(t.Direction)},
		},
		"Asset Type": map[string]interface{}{
			"select": map[string]interface{}{"name": t.AssetType.String()},
		},
		"Status": map[string]interface{}{
			"select": map[string]interface{}{"name": string(t.Status)},
		},
	}

	if !t.Date.IsZero() {
		props["Date"] = map[string]interface{}{
			"date": map[string]interface{}{
				"start": t.Date.Format("2006-01-02"),
			},
		}
	}
	if t.ResultRR != 0 || t.Status == domain.StatusBE || t.Status == domain.StatusOpen {
		props["Result RR"] = map[string]interface{}{"number": t.ResultRR}
	}

	switch {
	case t.TimeWindow != "":
		props["Time Window"] = map[string]interface{}{
			"select": map[string]interface{}{"name": string(t.TimeWindow)},
		}
	case forUpdate:
		props["Time Window"] = map[string]interface{}{"select": nil}
	}

	emptyText := []map[string]interface{}{}
	switch {
	case t.Confluence != "":
		props["Confluence"] = map[string]interface{}{
			"rich_text": []map[string]interface{}{
				{"text": map[string]interface{}{"content": t.Confluence}},
			},
		}
	case forUpdate:
		props["Confluence"] = map[string]interface{}{"rich_text": emptyText}
	}

	switch {
	case t.Notes != "":
		props["Notes"] = map[string]interface{}{
			"rich_text": []map[string]interface{}{
				{"text": map[string]interface{}{"content": t.Notes}},
			},
		}
	case forUpdate:
		props["Notes"] = map[string]interface{}{"rich_text": emptyText}
	}
	return props
}

// parseTrades extracts trades from Notion database query results.
//
// A malformed row fails the whole read. Silently skipping it would return (and
// cache) a truncated history, so /stats, /report and the leaderboard would
// under-count trades with no error anywhere.
func (r *TradeRepo) parseTrades(items []json.RawMessage) ([]domain.Trade, error) {
	trades := make([]domain.Trade, 0, len(items))
	for i, item := range items {
		t, err := r.parseSingleTrade(item)
		if err != nil {
			return nil, fmt.Errorf("parse trade %d: %w", i, err)
		}
		trades = append(trades, *t)
	}
	return trades, nil
}

type notionTextSegment struct {
	PlainText string `json:"plain_text"`
}

func joinNotionPlainText(segments []notionTextSegment) string {
	if len(segments) == 0 {
		return ""
	}
	if len(segments) == 1 {
		return segments[0].PlainText
	}
	var sb strings.Builder
	for _, seg := range segments {
		sb.WriteString(seg.PlainText)
	}
	return sb.String()
}

// parseSingleTrade extracts a Trade from a Notion page JSON.
func (r *TradeRepo) parseSingleTrade(raw json.RawMessage) (*domain.Trade, error) {
	var page struct {
		ID          string `json:"id"`
		CreatedTime string `json:"created_time"`
		Properties  struct {
			Symbol struct {
				Title []notionTextSegment `json:"title"`
			} `json:"Symbol"`
			Date struct {
				Date *struct {
					Start string `json:"start"`
				} `json:"date"`
			} `json:"Date"`
			AssetType struct {
				Select *struct {
					Name string `json:"name"`
				} `json:"select"`
			} `json:"Asset Type"`
			Direction struct {
				Select *struct {
					Name string `json:"name"`
				} `json:"select"`
			} `json:"Direction"`
			ResultRR struct {
				Number *float64 `json:"number"`
			} `json:"Result RR"`
			Status struct {
				Select *struct {
					Name string `json:"name"`
				} `json:"select"`
			} `json:"Status"`
			TimeWindow struct {
				Select *struct {
					Name string `json:"name"`
				} `json:"select"`
			} `json:"Time Window"`
			Confluence struct {
				RichText []notionTextSegment `json:"rich_text"`
			} `json:"Confluence"`
			Notes struct {
				RichText []notionTextSegment `json:"rich_text"`
			} `json:"Notes"`
		} `json:"properties"`
	}

	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, err
	}

	t := &domain.Trade{
		ID:         page.ID,
		Symbol:     joinNotionPlainText(page.Properties.Symbol.Title),
		Confluence: joinNotionPlainText(page.Properties.Confluence.RichText),
		Notes:      joinNotionPlainText(page.Properties.Notes.RichText),
	}

	// Page creation time; best-effort RFC3339 parse used as the tie-breaker
	// for same-day trades when picking the "latest" entry.
	if page.CreatedTime != "" {
		if ct, err := time.Parse(time.RFC3339, page.CreatedTime); err == nil {
			t.CreatedAt = ct
		}
	}

	if page.Properties.Date.Date != nil {
		// best-effort parse
		if parsed, err := parseDate(page.Properties.Date.Date.Start); err == nil {
			t.Date = parsed
		}
	}
	if page.Properties.AssetType.Select != nil {
		// Notion stores display labels ("Indices", "Gold", ...); normalize to
		// the canonical domain constants so stats grouping and IsValid work.
		if at, ok := domain.ParseAssetType(page.Properties.AssetType.Select.Name); ok {
			t.AssetType = at
		} else {
			t.AssetType = domain.AssetType(strings.ToUpper(strings.TrimSpace(page.Properties.AssetType.Select.Name)))
		}
	}
	if page.Properties.Direction.Select != nil {
		t.Direction = domain.Direction(page.Properties.Direction.Select.Name)
	}
	if page.Properties.ResultRR.Number != nil {
		t.ResultRR = *page.Properties.ResultRR.Number
	}
	if page.Properties.Status.Select != nil {
		t.Status = domain.TradeStatus(page.Properties.Status.Select.Name)
	}
	if page.Properties.TimeWindow.Select != nil {
		t.TimeWindow = domain.TimeWindow(page.Properties.TimeWindow.Select.Name)
	}

	return t, nil
}
