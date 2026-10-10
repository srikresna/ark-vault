package notion

import (
	"context"
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
