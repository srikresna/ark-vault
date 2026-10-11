package notion

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	client := NewClient("token")
	client.baseURL = srv.URL + "/v1"
	client.retryBase = time.Millisecond // keep retry paths fast in tests
	return client
}

// TestClient_QueryDatabaseAllPaginates is the regression test for the missing
// pagination: a single Notion query returns at most 100 rows, so without
// cursor following a member with more trades than one page would silently lose
// history (and that truncated list was then cached for 10 minutes).
func TestClient_QueryDatabaseAllPaginates(t *testing.T) {
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/databases/db-1/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := atomic.AddInt32(&calls, 1)
		switch n {
		case 1:
			_, _ = w.Write([]byte(`{"results":[{"id":"1"},{"id":"2"}],"has_more":true,"next_cursor":"c1"}`))
		case 2:
			_, _ = w.Write([]byte(`{"results":[{"id":"3"}],"has_more":true,"next_cursor":"c2"}`))
		default:
			_, _ = w.Write([]byte(`{"results":[{"id":"4"}],"has_more":false,"next_cursor":null}`))
		}
	}))
	defer srv.Close()

	items, err := newTestClient(t, srv).QueryDatabaseAll(context.Background(), "db-1", nil)
	if err != nil {
		t.Fatalf("QueryDatabaseAll failed: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 rows across all pages, got %d", len(items))
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 paginated calls, got %d", got)
	}
}

// TestClient_PaginationSendsCursor verifies the cursor is actually forwarded,
// otherwise every page would return page one and the loop would never advance.
func TestClient_PaginationSendsCursor(t *testing.T) {
	var sawCursor int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		if strings.Contains(string(body), `"start_cursor":"c1"`) {
			atomic.StoreInt32(&sawCursor, 1)
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"id":"1"}],"has_more":true,"next_cursor":"c1"}`))
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv).QueryDatabaseAll(context.Background(), "db-1", nil); err != nil {
		t.Fatalf("QueryDatabaseAll failed: %v", err)
	}
	if atomic.LoadInt32(&sawCursor) != 1 {
		t.Fatal("second page request did not carry start_cursor")
	}
}

// TestClient_PaginationHasPageCap ensures a misbehaving server that always
// reports has_more cannot loop forever.
func TestClient_PaginationHasPageCap(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":"1"}],"has_more":true,"next_cursor":"always"}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).QueryDatabaseAll(context.Background(), "db-1", nil)
	if err == nil {
		t.Fatal("expected an error when pagination exceeds the page cap")
	}
	if got := atomic.LoadInt32(&calls); got != maxPages {
		t.Fatalf("expected %d calls before giving up, got %d", maxPages, got)
	}
}

// TestClient_RetriesOn429WithRetryAfter covers Notion rate limiting: the bot
// must back off and retry instead of failing the user's command.
func TestClient_RetriesOn429WithRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"page-1"}`))
	}))
	defer srv.Close()

	raw, err := newTestClient(t, srv).CreatePage(context.Background(), map[string]interface{}{"x": 1})
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if !strings.Contains(string(raw), "page-1") {
		t.Fatalf("unexpected response: %s", raw)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 attempts, got %d", got)
	}
}

// TestClient_RetriesOn5xx covers transient Notion server errors.
func TestClient_RetriesOn5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv).CreatePage(context.Background(), map[string]interface{}{"x": 1}); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

// TestClient_DoesNotRetryClientErrors pins that non-transient failures surface
// immediately instead of hammering the API: a bad request stays bad.
func TestClient_DoesNotRetryClientErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(status)
			}))
			defer srv.Close()

			if _, err := newTestClient(t, srv).CreatePage(context.Background(), map[string]interface{}{"x": 1}); err == nil {
				t.Fatalf("expected error for status %d", status)
			}
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Fatalf("status %d must not be retried, got %d attempts", status, got)
			}
		})
	}
}

// TestClient_GivesUpAfterMaxAttempts makes sure a permanently failing endpoint
// returns an error rather than retrying forever.
func TestClient_GivesUpAfterMaxAttempts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv).CreatePage(context.Background(), map[string]interface{}{"x": 1}); err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if got := atomic.LoadInt32(&calls); got != maxAttempts {
		t.Fatalf("expected %d attempts, got %d", maxAttempts, got)
	}
}

// TestClient_RetryRebuildsRequestBody guards a subtle retry bug: an io.Reader
// body is consumed by the first attempt, so retries must rebuild it from bytes
// or the server sees an empty payload.
func TestClient_RetryRebuildsRequestBody(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv).CreatePage(context.Background(), map[string]interface{}{"marker": "payload"}); err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(bodies))
	}
	for i, b := range bodies {
		if !strings.Contains(b, "payload") {
			t.Errorf("attempt %d sent an empty/unusable body: %q", i+1, b)
		}
	}
}

// TestClient_ContextCancelStopsRetry pins that shutting down the bot is not
// fought by retry backoff.
func TestClient_ContextCancelStopsRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	client.retryBase = 5 * time.Second // would stall the test if not cancelled

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	if _, err := client.CreatePage(ctx, map[string]interface{}{"x": 1}); err == nil {
		t.Fatal("expected error on cancelled context")
	}
	if got := atomic.LoadInt32(&calls); got > 2 {
		t.Fatalf("expected cancellation to stop retries early, got %d attempts", got)
	}
}

// TestTradeRepo_GetTradesPaginatesAllRows is the end-to-end consequence: a
// member with more than one page of trades must see their whole history.
func TestTradeRepo_GetTradesPaginatesAllRows(t *testing.T) {
	var calls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/databases/db-9/query":
			if atomic.AddInt32(&calls, 1) == 1 {
				_, _ = w.Write([]byte(`{"results":[{"id":"a","properties":{"Symbol":{"title":[{"plain_text":"XAUUSD"}]},"Status":{"select":{"name":"WIN"}},"Date":{"date":{"start":"2026-10-01"}}}}],"has_more":true,"next_cursor":"next"}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"b","properties":{"Symbol":{"title":[{"plain_text":"EURUSD"}]},"Status":{"select":{"name":"LOSS"}},"Date":{"date":{"start":"2026-10-02"}}}}],"has_more":false}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	memberRepo := NewMemberRepo(client, "parent-1")
	memberRepo.cache[42] = &domain.Member{
		TelegramID:   42,
		Username:     "trader",
		NotionPageID: "page-42",
		NotionDBID:   "db-9",
	}
	tradeRepo := NewTradeRepo(client, memberRepo)

	trades, err := tradeRepo.GetTrades(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetTrades failed: %v", err)
	}
	if len(trades) != 2 {
		t.Fatalf("expected trades from both pages, got %d", len(trades))
	}
}
