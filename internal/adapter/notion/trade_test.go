package notion

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

type mockTradeCacheStore struct {
	trades map[int64][]domain.Trade
}

func newMockTradeCacheStore() *mockTradeCacheStore {
	return &mockTradeCacheStore{trades: make(map[int64][]domain.Trade)}
}

func (m *mockTradeCacheStore) GetCachedTrades(_ context.Context, memberID int64) ([]domain.Trade, bool, error) {
	if tr, ok := m.trades[memberID]; ok {
		cp := make([]domain.Trade, len(tr))
		copy(cp, tr)
		return cp, true, nil
	}
	return nil, false, nil
}

func (m *mockTradeCacheStore) SaveCachedTrades(_ context.Context, memberID int64, trades []domain.Trade) error {
	cp := make([]domain.Trade, len(trades))
	copy(cp, trades)
	m.trades[memberID] = cp
	return nil
}

func (m *mockTradeCacheStore) InvalidateCachedTrades(_ context.Context, memberID int64) error {
	delete(m.trades, memberID)
	return nil
}

func TestTradeRepo_CachesTradesAndInvalidatesOnSave(t *testing.T) {
	var queryCalls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/databases/db-100/query":
			n := atomic.AddInt32(&queryCalls, 1)
			if n == 1 {
				_, _ = w.Write([]byte(`{"results":[{"id":"t-1","properties":{"Symbol":{"title":[{"plain_text":"XAUUSD"}]},"Direction":{"select":{"name":"BUY"}},"Asset Type":{"select":{"name":"Gold"}},"Status":{"select":{"name":"WIN"}},"Result RR":{"number":2.0},"Date":{"date":{"start":"2026-10-10"}}}}]}`))
			} else {
				_, _ = w.Write([]byte(`{"results":[{"id":"t-2","properties":{"Symbol":{"title":[{"plain_text":"EURUSD"}]},"Direction":{"select":{"name":"SELL"}},"Asset Type":{"select":{"name":"Forex"}},"Status":{"select":{"name":"WIN"}},"Result RR":{"number":3.0},"Date":{"date":{"start":"2026-10-10"}}}},{"id":"t-1","properties":{"Symbol":{"title":[{"plain_text":"XAUUSD"}]},"Direction":{"select":{"name":"BUY"}},"Asset Type":{"select":{"name":"Gold"}},"Status":{"select":{"name":"WIN"}},"Result RR":{"number":2.0},"Date":{"date":{"start":"2026-10-10"}}}}]}`))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/pages":
			_, _ = w.Write([]byte(`{"id":"t-2"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("token")
	client.baseURL = srv.URL + "/v1"

	memberRepo := NewMemberRepo(client, "parent-1")
	memberRepo.cache[555] = &domain.Member{
		TelegramID:   555,
		Username:     "trader",
		NotionPageID: "page-100",
		NotionDBID:   "db-100",
	}

	tradeCache := newMockTradeCacheStore()
	tradeRepo := NewTradeRepo(client, memberRepo, tradeCache)

	ctx := context.Background()

	// 1. First GetTrades -> hits Notion API (queryCalls == 1) and populates cache
	trades1, err := tradeRepo.GetTrades(ctx, 555)
	if err != nil {
		t.Fatalf("first GetTrades failed: %v", err)
	}
	if len(trades1) != 1 || atomic.LoadInt32(&queryCalls) != 1 {
		t.Fatalf("expected 1 trade and 1 Notion query call, got %d trades, %d calls", len(trades1), queryCalls)
	}

	// 2. Second GetTrades -> served from cache (queryCalls still == 1)
	trades2, err := tradeRepo.GetTrades(ctx, 555)
	if err != nil {
		t.Fatalf("second GetTrades failed: %v", err)
	}
	if len(trades2) != 1 || atomic.LoadInt32(&queryCalls) != 1 {
		t.Fatalf("expected cached trade with 1 Notion query call total, got %d trades, %d calls", len(trades2), queryCalls)
	}

	// 3. SaveTrade -> invalidates cache so next GetTrades fetches fresh data (2 trades, queryCalls == 2)
	err = tradeRepo.SaveTrade(ctx, 555, &domain.Trade{
		Date:      time.Now().UTC(),
		Symbol:    "EURUSD",
		AssetType: domain.AssetForex,
		Direction: domain.DirSell,
		Status:    domain.StatusWin,
		ResultRR:  3.0,
	})
	if err != nil {
		t.Fatalf("SaveTrade failed: %v", err)
	}

	trades3, err := tradeRepo.GetTrades(ctx, 555)
	if err != nil {
		t.Fatalf("third GetTrades failed: %v", err)
	}
	if len(trades3) != 2 || atomic.LoadInt32(&queryCalls) != 2 {
		t.Fatalf("expected 2 trades and 2 Notion query calls after cache invalidation, got %d trades, %d calls", len(trades3), queryCalls)
	}
}

func TestTradeRepo_UpdateAndDeleteTrade(t *testing.T) {
	var updatedPageID string
	var archivedPageID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch && r.URL.Path == "/v1/pages/t-99" {
			updatedPageID = "t-99"
			_, _ = w.Write([]byte(`{"id":"t-99"}`))
			return
		}
		if r.Method == http.MethodPatch && r.URL.Path == "/v1/pages/t-del" {
			archivedPageID = "t-del"
			_, _ = w.Write([]byte(`{"id":"t-del","archived":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClient("token")
	client.baseURL = srv.URL + "/v1"
	memberRepo := NewMemberRepo(client, "parent-1")
	tradeCache := newMockTradeCacheStore()
	_ = tradeCache.SaveCachedTrades(context.Background(), 555, []domain.Trade{{ID: "t-99"}})

	tradeRepo := NewTradeRepo(client, memberRepo, tradeCache)

	err := tradeRepo.UpdateTrade(context.Background(), 555, &domain.Trade{
		ID:        "t-99",
		Symbol:    "XAUUSD",
		AssetType: domain.AssetGold,
		Direction: domain.DirBuy,
		Status:    domain.StatusWin,
		ResultRR:  2.5,
	})
	if err != nil {
		t.Fatalf("UpdateTrade failed: %v", err)
	}
	if updatedPageID != "t-99" {
		t.Errorf("expected PATCH /v1/pages/t-99, got %q", updatedPageID)
	}
	if _, hit, _ := tradeCache.GetCachedTrades(context.Background(), 555); hit {
		t.Errorf("expected cache invalidated after UpdateTrade")
	}

	_ = tradeCache.SaveCachedTrades(context.Background(), 555, []domain.Trade{{ID: "t-del"}})
	if err := tradeRepo.DeleteTrade(context.Background(), 555, "t-del"); err != nil {
		t.Fatalf("DeleteTrade failed: %v", err)
	}
	if archivedPageID != "t-del" {
		t.Errorf("expected PATCH /v1/pages/t-del for archive, got %q", archivedPageID)
	}
	if _, hit, _ := tradeCache.GetCachedTrades(context.Background(), 555); hit {
		t.Errorf("expected cache invalidated after DeleteTrade")
	}
}

// TestTradeRepo_ParseSingleTrade_ConcatenatesMultiSegmentRichText verifies that
// multi-segment Notion title/rich_text arrays are concatenated rather than
// truncated to segment 0.
func TestTradeRepo_ParseSingleTrade_ConcatenatesMultiSegmentRichText(t *testing.T) {
	repo := NewTradeRepo(NewClient("token"), nil)
	raw := []byte(`{
		"id":"t-multi",
		"properties":{
			"Symbol":{"title":[{"plain_text":"XAU"},{"plain_text":"USD"}]},
			"Confluence":{"rich_text":[{"plain_text":"FVG + "},{"plain_text":"OB mitigation"},{"plain_text":" on 15m"}]},
			"Notes":{"rich_text":[{"plain_text":"Part 1 - "},{"plain_text":"Part 2"}]}
		}
	}`)
	tr, err := repo.parseSingleTrade(raw)
	if err != nil {
		t.Fatalf("parseSingleTrade failed: %v", err)
	}
	if tr.Symbol != "XAUUSD" {
		t.Errorf("Symbol = %q, want XAUUSD", tr.Symbol)
	}
	if tr.Confluence != "FVG + OB mitigation on 15m" {
		t.Errorf("Confluence = %q, want %q", tr.Confluence, "FVG + OB mitigation on 15m")
	}
	if tr.Notes != "Part 1 - Part 2" {
		t.Errorf("Notes = %q, want %q", tr.Notes, "Part 1 - Part 2")
	}
}

// TestTradeRepo_ParseSingleTrade_CreatedTime pins the same-day tie-breaker
// used by /undo: the Notion page creation time must be parsed onto the trade.
func TestTradeRepo_ParseSingleTrade_CreatedTime(t *testing.T) {
	repo := NewTradeRepo(NewClient("token"), nil)
	tr, err := repo.parseSingleTrade([]byte(`{
		"id":"t-ct",
		"created_time":"2026-10-12T09:30:00.000Z",
		"properties":{"Symbol":{"title":[{"plain_text":"XAUUSD"}]}}
	}`))
	if err != nil {
		t.Fatalf("parseSingleTrade failed: %v", err)
	}
	want := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	if !tr.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", tr.CreatedAt, want)
	}
}

// TestTradeRepo_UpdateTradeClearsOptionalProperties pins the regression where
// an update could never clear Time Window/Confluence/Notes because empty
// values were simply omitted from the PATCH payload, leaving the old value in
// Notion forever.
func TestTradeRepo_UpdateTradeClearsOptionalProperties(t *testing.T) {
	var payload map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"t-clear"}`))
	}))
	defer srv.Close()

	client := NewClient("token")
	client.baseURL = srv.URL + "/v1"
	repo := NewTradeRepo(client, NewMemberRepo(client, "parent-1"))

	err := repo.UpdateTrade(context.Background(), 555, &domain.Trade{
		ID:        "t-clear",
		Symbol:    "XAUUSD",
		AssetType: domain.AssetGold,
		Direction: domain.DirBuy,
		Status:    domain.StatusBE,
		ResultRR:  0,
	})
	if err != nil {
		t.Fatalf("UpdateTrade failed: %v", err)
	}

	props, ok := payload["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("PATCH payload has no properties: %+v", payload)
	}
	tw, ok := props["Time Window"].(map[string]interface{})
	if !ok || tw["select"] != nil {
		t.Errorf("Time Window on update must clear with select:null, got %+v", props["Time Window"])
	}
	conf, ok := props["Confluence"].(map[string]interface{})
	if !ok || len(conf["rich_text"].([]interface{})) != 0 {
		t.Errorf("Confluence on update must clear with an empty rich_text, got %+v", props["Confluence"])
	}
	notes, ok := props["Notes"].(map[string]interface{})
	if !ok || len(notes["rich_text"].([]interface{})) != 0 {
		t.Errorf("Notes on update must clear with an empty rich_text, got %+v", props["Notes"])
	}

	// A create must still omit the empty optionals instead of clearing them.
	create := repo.buildTradeProperties(&domain.Trade{
		Symbol: "XAUUSD", AssetType: domain.AssetGold, Direction: domain.DirBuy, Status: domain.StatusOpen,
	}, false)
	for _, key := range []string{"Time Window", "Confluence", "Notes"} {
		if _, present := create[key]; present {
			t.Errorf("create payload must omit empty %s, got %+v", key, create[key])
		}
	}
}
