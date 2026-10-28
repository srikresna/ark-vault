package notion

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
		if tr == nil {
			return nil, true, nil
		}
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

// TestTradeRepo_ParseSingleTrade_NormalizesAssetType pins the regression where
// the raw Notion select label ("Indices", "Gold", "Commodities") was cast
// straight into domain.AssetType, leaving mixed-case values that failed
// IsValid and split asset statistics into separate map keys.
func TestTradeRepo_ParseSingleTrade_NormalizesAssetType(t *testing.T) {
	repo := NewTradeRepo(NewClient("token"), nil)

	cases := []struct {
		selectName string
		want       domain.AssetType
	}{
		{"Forex", domain.AssetForex},
		{"Gold", domain.AssetGold},
		{"Indices", domain.AssetIndices},
		{"Crypto", domain.AssetCrypto},
		{"Commodities", domain.AssetCommodity},
		{"  INDICES  ", domain.AssetIndices},
		{"Index", domain.AssetIndices},
	}

	for _, tc := range cases {
		t.Run(tc.selectName, func(t *testing.T) {
			raw := []byte(`{"id":"t-at","properties":{` +
				`"Symbol":{"title":[{"plain_text":"XAUUSD"}]},` +
				`"Asset Type":{"select":{"name":"` + tc.selectName + `"}}}}`)
			tr, err := repo.parseSingleTrade(raw)
			if err != nil {
				t.Fatalf("parseSingleTrade failed: %v", err)
			}
			if tr.AssetType != tc.want {
				t.Errorf("AssetType = %q, want %q", tr.AssetType, tc.want)
			}
			if !tr.AssetType.IsValid() {
				t.Errorf("AssetType %q is not valid", tr.AssetType)
			}
		})
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

// TestTradeRepo_GetTrades_EmptyDatabaseReturnsEmptyAndCaches pins that a member
// whose database exists but holds no trades gets an empty history (not an
// error) and that the empty result is cached like any other.
func TestTradeRepo_GetTrades_EmptyDatabaseReturnsEmptyAndCaches(t *testing.T) {
	var queryCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/v1/databases/db-empty/query" {
			queryCalls.Add(1)
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClient("token")
	client.baseURL = srv.URL + "/v1"
	memberRepo := NewMemberRepo(client, "parent-1")
	memberRepo.cache[555] = &domain.Member{
		TelegramID:   555,
		Username:     "empty",
		NotionPageID: "page-empty",
		NotionDBID:   "db-empty",
	}
	tradeCache := newMockTradeCacheStore()
	tradeRepo := NewTradeRepo(client, memberRepo, tradeCache)
	ctx := context.Background()

	trades, err := tradeRepo.GetTrades(ctx, 555)
	if err != nil {
		t.Fatalf("GetTrades on an empty database failed: %v", err)
	}
	if trades == nil || len(trades) != 0 {
		t.Fatalf("GetTrades = %+v, want an empty non-nil history", trades)
	}
	if got := queryCalls.Load(); got != 1 {
		t.Fatalf("Notion query calls = %d, want 1", got)
	}
	if _, hit, err := tradeCache.GetCachedTrades(ctx, 555); err != nil || !hit {
		t.Errorf("empty history was not cached (hit=%v, err=%v)", hit, err)
	}

	// The cached empty history must not trigger another Notion query.
	if _, err := tradeRepo.GetTrades(ctx, 555); err != nil {
		t.Fatalf("second GetTrades failed: %v", err)
	}
	if got := queryCalls.Load(); got != 1 {
		t.Errorf("Notion query calls after cache hit = %d, want 1", got)
	}
}

// TestTradeRepo_GetTrades_RefetchesNilCachedHistory pins that a cache entry
// holding nil (legacy/corrupt data) is treated as a miss instead of being
// served as a silent empty history.
func TestTradeRepo_GetTrades_RefetchesNilCachedHistory(t *testing.T) {
	var queryCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/v1/databases/db-nil/query" {
			queryCalls.Add(1)
			_, _ = w.Write([]byte(`{"results":[{"id":"t-1","properties":{"Symbol":{"title":[{"plain_text":"XAUUSD"}]},"Status":{"select":{"name":"WIN"}}}}],"has_more":false}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClient("token")
	client.baseURL = srv.URL + "/v1"
	memberRepo := NewMemberRepo(client, "parent-1")
	memberRepo.cache[555] = &domain.Member{
		TelegramID:   555,
		Username:     "nil-cache",
		NotionPageID: "page-nil",
		NotionDBID:   "db-nil",
	}
	tradeCache := newMockTradeCacheStore()
	tradeCache.trades[555] = nil
	tradeRepo := NewTradeRepo(client, memberRepo, tradeCache)
	ctx := context.Background()

	trades, err := tradeRepo.GetTrades(ctx, 555)
	if err != nil {
		t.Fatalf("GetTrades failed: %v", err)
	}
	if len(trades) != 1 || trades[0].Symbol != "XAUUSD" {
		t.Fatalf("trades = %+v, want the history refetched from Notion", trades)
	}
	if got := queryCalls.Load(); got != 1 {
		t.Errorf("Notion query calls = %d, want 1", got)
	}
	cached, hit, err := tradeCache.GetCachedTrades(ctx, 555)
	if err != nil || !hit || len(cached) != 1 {
		t.Errorf("cache after refetch = %+v (hit=%v, err=%v), want the fetched history", cached, hit, err)
	}
}

// TestTradeRepo_GetTrades_MissingTradeDatabaseErrorsInsteadOfEmptyHistory pins
// that a journal whose Trades database cannot be created yields an error rather
// than an empty history: /stats, /report and the leaderboard must not silently
// claim the member has no trades.
func TestTradeRepo_GetTrades_MissingTradeDatabaseErrorsInsteadOfEmptyHistory(t *testing.T) {
	var queryCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[{"id":"page-partial","object":"page"}],"has_more":false}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			_, _ = w.Write([]byte(`{"results":[{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 555 | Username: @broken"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"object":"error","status":400,"message":"database creation failed"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/query"):
			queryCalls.Add(1)
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("token")
	client.baseURL = srv.URL + "/v1"
	tradeCache := newMockTradeCacheStore()
	tradeRepo := NewTradeRepo(client, NewMemberRepo(client, "parent-1"), tradeCache)

	trades, err := tradeRepo.GetTrades(context.Background(), 555)
	if err == nil {
		t.Fatalf("expected GetTrades to fail for an unusable journal, got %+v", trades)
	}
	if trades != nil {
		t.Errorf("GetTrades returned trades alongside the error: %+v", trades)
	}
	if got := queryCalls.Load(); got != 0 {
		t.Errorf("queried the trade database %d times without a database ID, want 0", got)
	}
	if _, hit, _ := tradeCache.GetCachedTrades(context.Background(), 555); hit {
		t.Error("failed history read was cached")
	}
}

// TestTradeRepo_GetTrades_PropagatesMemberLookupFailure pins that a failed
// member lookup surfaces instead of being reported as an empty history.
func TestTradeRepo_GetTrades_PropagatesMemberLookupFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"object":"error","status":400,"message":"search unavailable"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClient("token")
	client.baseURL = srv.URL + "/v1"
	tradeCache := newMockTradeCacheStore()
	tradeRepo := NewTradeRepo(client, NewMemberRepo(client, "parent-1"), tradeCache)

	trades, err := tradeRepo.GetTrades(context.Background(), 555)
	if err == nil {
		t.Fatalf("expected GetTrades to surface the member lookup failure, got %+v", trades)
	}
	if trades != nil {
		t.Errorf("GetTrades returned trades alongside the error: %+v", trades)
	}
	if _, hit, _ := tradeCache.GetCachedTrades(context.Background(), 555); hit {
		t.Error("failed member lookup cached a trade history")
	}
}

// TestTradeRepo_ParseTrades_RejectsMalformedEntry pins that one malformed
// Notion row fails the whole read. Skipping it would return and cache a
// truncated history with no error anywhere.
func TestTradeRepo_ParseTrades_RejectsMalformedEntry(t *testing.T) {
	repo := NewTradeRepo(NewClient("token"), nil)
	items := []json.RawMessage{
		json.RawMessage(`{"id":"t-ok","properties":{"Symbol":{"title":[{"plain_text":"XAUUSD"}]}}}`),
		json.RawMessage(`["not","a","page"]`),
	}
	if trades, err := repo.parseTrades(items); err == nil {
		t.Fatalf("parseTrades returned %+v, want an error for the malformed row", trades)
	}
}

// TestTradeRepo_SaveTrade_DoesNotRetryAmbiguousCreate pins that a 5xx on the
// trade create is not retried (which would duplicate the trade) and that the
// error tells the user the trade may already be saved.
func TestTradeRepo_SaveTrade_DoesNotRetryAmbiguousCreate(t *testing.T) {
	var creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages") {
			creates.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClient("token")
	client.baseURL = srv.URL + "/v1"
	memberRepo := NewMemberRepo(client, "parent-1")
	memberRepo.cache[555] = &domain.Member{
		TelegramID:   555,
		Username:     "trader",
		NotionPageID: "page-555",
		NotionDBID:   "db-555",
	}
	tradeCache := newMockTradeCacheStore()
	// Seed a cached history: an ambiguous save must drop it, or the next read
	// would serve a list that cannot contain the trade that may exist.
	_ = tradeCache.SaveCachedTrades(context.Background(), 555, []domain.Trade{{ID: "t-old"}})
	tradeRepo := NewTradeRepo(client, memberRepo, tradeCache)

	err := tradeRepo.SaveTrade(context.Background(), 555, &domain.Trade{
		Symbol:    "XAUUSD",
		AssetType: domain.AssetGold,
		Direction: domain.DirBuy,
		Status:    domain.StatusWin,
		ResultRR:  2,
	})
	if err == nil {
		t.Fatal("expected SaveTrade to fail on a 5xx create")
	}
	if !strings.Contains(err.Error(), "may have been saved") {
		t.Errorf("error %q must warn that the trade may already be saved", err)
	}
	if got := creates.Load(); got != 1 {
		t.Errorf("trade create attempts = %d, want 1 (no retry after an ambiguous failure)", got)
	}
	if _, hit, cacheErr := tradeCache.GetCachedTrades(context.Background(), 555); cacheErr != nil || hit {
		t.Errorf("cached history survived an ambiguous save (hit=%v, err=%v)", hit, cacheErr)
	}
}

// TestTradeRepo_SaveTrade_UnusableCreateResponseIsAmbiguous pins the same rule
// for a 2xx whose body cannot be used: the trade page exists but its ID is
// unknown, so the error must be flagged as ambiguous (the handler then points at
// /history instead of inviting a duplicate retry).
func TestTradeRepo_SaveTrade_UnusableCreateResponseIsAmbiguous(t *testing.T) {
	for _, body := range []string{`{}`, `not-json`} {
		t.Run(body, func(t *testing.T) {
			var creates atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages") {
					creates.Add(1)
					_, _ = w.Write([]byte(body))
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer srv.Close()

			client := NewClient("token")
			client.baseURL = srv.URL + "/v1"
			memberRepo := NewMemberRepo(client, "parent-1")
			memberRepo.cache[555] = &domain.Member{
				TelegramID:   555,
				Username:     "trader",
				NotionPageID: "page-555",
				NotionDBID:   "db-555",
			}
			tradeCache := newMockTradeCacheStore()
			_ = tradeCache.SaveCachedTrades(context.Background(), 555, []domain.Trade{{ID: "t-old"}})
			tradeRepo := NewTradeRepo(client, memberRepo, tradeCache)

			err := tradeRepo.SaveTrade(context.Background(), 555, &domain.Trade{
				Symbol:    "XAUUSD",
				AssetType: domain.AssetGold,
				Direction: domain.DirBuy,
				Status:    domain.StatusWin,
				ResultRR:  2,
			})
			if err == nil {
				t.Fatal("expected SaveTrade to fail on an unusable create response")
			}
			if !IsAmbiguousWriteError(err) {
				t.Errorf("error %v must be reported as ambiguous", err)
			}
			if got := creates.Load(); got != 1 {
				t.Errorf("trade create attempts = %d, want 1", got)
			}
			if _, hit, _ := tradeCache.GetCachedTrades(context.Background(), 555); hit {
				t.Error("cached history survived an ambiguous save")
			}
		})
	}
}
