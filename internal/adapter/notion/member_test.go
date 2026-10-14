package notion

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

type mockMemberCacheStore struct {
	members map[int64]domain.Member
}

func newMockMemberCacheStore() *mockMemberCacheStore {
	return &mockMemberCacheStore{members: make(map[int64]domain.Member)}
}

func (m *mockMemberCacheStore) GetCachedMember(_ context.Context, telegramID int64) (*domain.Member, error) {
	if mem, ok := m.members[telegramID]; ok {
		cp := mem
		return &cp, nil
	}
	return nil, nil
}

func (m *mockMemberCacheStore) SaveCachedMember(_ context.Context, member *domain.Member) error {
	m.members[member.TelegramID] = *member
	return nil
}

func (m *mockMemberCacheStore) ListCachedMembers(_ context.Context) ([]domain.Member, error) {
	var out []domain.Member
	for _, mem := range m.members {
		out = append(out, mem)
	}
	return out, nil
}

func TestMemberRepo_PersistentIndexAcrossRestart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/search":
			_, _ = w.Write([]byte(`{"results":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/pages":
			_, _ = w.Write([]byte(`{"id":"page-111"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/databases":
			_, _ = w.Write([]byte(`{"id":"db-111"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("token")
	client.baseURL = srv.URL + "/v1"

	persistStore := newMockMemberCacheStore()

	// First instance creates member
	repo1 := NewMemberRepo(client, "parent-1", persistStore)
	_, err := repo1.EnsureMember(context.Background(), &domain.Member{
		TelegramID: 1001,
		Username:   "kresna",
		FirstName:  "Kresna",
	})
	if err != nil {
		t.Fatalf("EnsureMember failed: %v", err)
	}

	// Second instance simulates bot restart with cold in-memory cache
	repo2 := NewMemberRepo(client, "parent-1", persistStore)
	members, err := repo2.ListMembers(context.Background())
	if err != nil {
		t.Fatalf("ListMembers after restart failed: %v", err)
	}
	if len(members) != 1 || members[0].TelegramID != 1001 || members[0].NotionDBID != "db-111" {
		t.Fatalf("expected persisted member with NotionDBID=db-111 after restart, got %+v", members)
	}
}

func TestMemberRepo_FindMemberPage_ResolvesInlineDBID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/search":
			_, _ = w.Write([]byte(`{"results":[{"id":"page-existing","object":"page"}],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/page-existing/children":
			_, _ = w.Write([]byte(`{"results":[
				{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 2002 | Username: @kresna"}]}},
				{"id":"db-resolved-999","type":"child_database"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("token")
	client.baseURL = srv.URL + "/v1"

	repo := NewMemberRepo(client, "parent-1")
	m, err := repo.GetMember(context.Background(), 2002)
	if err != nil {
		t.Fatalf("GetMember failed: %v", err)
	}
	if m.NotionPageID != "page-existing" || m.NotionDBID != "db-resolved-999" {
		t.Fatalf("expected NotionPageID=page-existing and NotionDBID=db-resolved-999, got %+v", m)
	}
}

// TestMemberRepo_FindMemberPage_RejectsWrongMember pins that a fuzzy Notion
// search hit for another member is never accepted: writing a trade into the
// wrong member's database is silent data corruption that the old
// "take results[0]" behaviour made possible.
func TestMemberRepo_FindMemberPage_RejectsWrongMember(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/search":
			// Search returns somebody else's page first (and only).
			_, _ = w.Write([]byte(`{"results":[{"id":"page-other","object":"page"}],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/page-other/children":
			_, _ = w.Write([]byte(`{"results":[
				{"id":"callout-x","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 7777 | Username: @someone-else"}]}},
				{"id":"db-other","type":"child_database"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("token")
	client.baseURL = srv.URL + "/v1"

	repo := NewMemberRepo(client, "parent-1")
	m, err := repo.findMemberPage(context.Background(), 2002)
	if err != nil {
		t.Fatalf("findMemberPage failed: %v", err)
	}
	if m != nil {
		t.Fatalf("expected no match for member 2002, got %+v", m)
	}
}

// TestContainsTelegramID guards the digit-boundary check that keeps member
// 1001 from matching a callout for member 100.
func TestContainsTelegramID(t *testing.T) {
	tests := []struct {
		callout string
		want    string
		match   bool
	}{
		{"Telegram ID: 1001 | Username: @a", "Telegram ID: 1001", true},
		{"Telegram ID: 1001", "Telegram ID: 1001", true},
		{"Telegram ID: 10012 | Username: @a", "Telegram ID: 1001", false},
		{"Telegram ID: 100 | Username: @a", "Telegram ID: 1001", false},
		{"", "Telegram ID: 1001", false},
	}
	for _, tt := range tests {
		if got := containsTelegramID(tt.callout, tt.want); got != tt.match {
			t.Errorf("containsTelegramID(%q, %q) = %v, want %v", tt.callout, tt.want, got, tt.match)
		}
	}
}

func TestMemberRepo_DefensiveCopy(t *testing.T) {
	repo := NewMemberRepo(NewClient("token"), "parent-1")
	orig := &domain.Member{
		TelegramID:   3003,
		Username:     "original",
		NotionPageID: "p-3",
		NotionDBID:   "db-3",
	}
	repo.saveMemberToCache(context.Background(), orig)

	got, err := repo.GetMember(context.Background(), 3003)
	if err != nil {
		t.Fatalf("GetMember failed: %v", err)
	}
	got.Username = "mutated"

	got2, err := repo.GetMember(context.Background(), 3003)
	if err != nil {
		t.Fatalf("second GetMember failed: %v", err)
	}
	if got2.Username != "original" {
		t.Fatalf("expected cached member username to remain 'original', got %q", got2.Username)
	}
}

// TestMemberRepo_ListMembersUnionsPersistedRoster guards the regression where a
// warm in-memory cache suppressed the persisted roster: leaderboards, weekly
// reports and challenge standings would silently rank only the members the
// process had already touched since the last restart.
func TestMemberRepo_ListMembersUnionsPersistedRoster(t *testing.T) {
	persistStore := newMockMemberCacheStore()
	for _, id := range []int64{10, 20, 30} {
		_ = persistStore.SaveCachedMember(context.Background(), &domain.Member{
			TelegramID:   id,
			Username:     "member",
			NotionPageID: "p",
			NotionDBID:   "db",
		})
	}

	repo := NewMemberRepo(NewClient("token"), "parent-1", persistStore)
	// One member already resident in memory (e.g. just recorded a trade).
	repo.saveMemberToCache(context.Background(), &domain.Member{
		TelegramID:   10,
		Username:     "fresh",
		NotionPageID: "p10",
		NotionDBID:   "db10",
	})

	members, err := repo.ListMembers(context.Background())
	if err != nil {
		t.Fatalf("ListMembers failed: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("ListMembers returned %d members, want 3 (union of memory + persisted)", len(members))
	}

	byID := make(map[int64]domain.Member, len(members))
	for _, m := range members {
		byID[m.TelegramID] = m
	}
	if got := byID[10].Username; got != "fresh" {
		t.Errorf("member 10 username = %q, want in-memory value %q", got, "fresh")
	}
}

// TestMemberRepo_ListMembersPropagatesStoreError pins that a broken roster
// lookup surfaces instead of silently returning a partial member list.
func TestMemberRepo_ListMembersPropagatesStoreError(t *testing.T) {
	repo := NewMemberRepo(NewClient("token"), "parent-1", errMemberCacheStore{})
	if _, err := repo.ListMembers(context.Background()); err == nil {
		t.Fatal("expected ListMembers to return the store error, got nil")
	}
}

type errMemberCacheStore struct{}

func (errMemberCacheStore) GetCachedMember(context.Context, int64) (*domain.Member, error) {
	return nil, nil
}

func (errMemberCacheStore) SaveCachedMember(context.Context, *domain.Member) error { return nil }

func (errMemberCacheStore) ListCachedMembers(context.Context) ([]domain.Member, error) {
	return nil, errors.New("badger unavailable")
}

// TestMemberRepo_EnsureMember_ConcurrentSingleFlight pins the TOCTOU fix: the
// bot dispatches each update in its own goroutine, so a member's first two
// messages can race EnsureMember. Without single-flight both callers miss the
// caches, both search Notion, and both create a member page + inline trades
// database — trades then land in an orphaned database and vanish from
// /history, /stats and exports.
func TestMemberRepo_EnsureMember_ConcurrentSingleFlight(t *testing.T) {
	var pageCreates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			if pageCreates.Add(1) == 1 {
				// Hold the first create open so a racy second caller is
				// guaranteed to also reach its own create attempt.
				time.Sleep(300 * time.Millisecond)
			}
			_, _ = w.Write([]byte(`{"id":"page-1"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			_, _ = w.Write([]byte(`{"id":"db-1"}`))
		default:
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1", newMockMemberCacheStore())

	m := &domain.Member{TelegramID: 555, Username: "racer"}
	var (
		wg         sync.WaitGroup
		start      = make(chan struct{})
		res1, res2 *domain.Member
		err1, err2 error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		res1, err1 = repo.EnsureMember(context.Background(), m)
	}()
	go func() {
		defer wg.Done()
		<-start
		res2, err2 = repo.EnsureMember(context.Background(), m)
	}()
	close(start)
	wg.Wait()

	if err1 != nil || err2 != nil {
		t.Fatalf("EnsureMember failed: %v / %v", err1, err2)
	}
	if got := pageCreates.Load(); got != 1 {
		t.Errorf("EnsureMember created %d member pages for one Telegram ID; want 1", got)
	}
	if res1.NotionPageID != res2.NotionPageID || res1.NotionDBID != res2.NotionDBID {
		t.Errorf("concurrent EnsureMember returned divergent members: %+v vs %+v", res1, res2)
	}
}
