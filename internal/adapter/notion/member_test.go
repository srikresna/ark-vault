package notion

import (
	"context"
	"encoding/json"
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
	// mu guards members: lookupCached probes this store on the fast path
	// (outside the per-ID single-flight lock) while SaveCachedMember writes
	// under it, so the mock must be as thread-safe as real BadgerDB.
	mu      sync.RWMutex
	members map[int64]domain.Member
	deleted []int64
	saves   int
	// deleteErr forces every DeleteCachedMember call to fail; deleteErrs is
	// consumed one entry per call first, so a transient failure followed by a
	// successful retry can be modelled.
	deleteErr  error
	deleteErrs []error
}

func newMockMemberCacheStore() *mockMemberCacheStore {
	return &mockMemberCacheStore{members: make(map[int64]domain.Member)}
}

func (m *mockMemberCacheStore) GetCachedMember(_ context.Context, telegramID int64) (*domain.Member, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if mem, ok := m.members[telegramID]; ok {
		cp := mem
		return &cp, nil
	}
	return nil, nil
}

func (m *mockMemberCacheStore) SaveCachedMember(_ context.Context, member *domain.Member) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.members[member.TelegramID] = *member
	m.saves++
	return nil
}

func (m *mockMemberCacheStore) ListCachedMembers(_ context.Context) ([]domain.Member, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []domain.Member
	for _, mem := range m.members {
		out = append(out, mem)
	}
	return out, nil
}

func (m *mockMemberCacheStore) DeleteCachedMember(_ context.Context, telegramID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.deleteErrs) > 0 {
		err := m.deleteErrs[0]
		m.deleteErrs = m.deleteErrs[1:]
		if err != nil {
			return err
		}
	}
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.members, telegramID)
	m.deleted = append(m.deleted, telegramID)
	return nil
}

// deletedMembers reports which Telegram IDs had their mapping evicted.
func (m *mockMemberCacheStore) deletedMembers() []int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]int64(nil), m.deleted...)
}

// saveCount reports how many mappings were written to the store.
func (m *mockMemberCacheStore) saveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.saves
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
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/parent-1/children":
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
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
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/parent-1/children":
			// The parent-scoped fallback sees the same foreign page.
			_, _ = w.Write([]byte(`{"results":[{"id":"page-other","type":"child_page"}],"has_more":false}`))
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

func (errMemberCacheStore) DeleteCachedMember(context.Context, int64) error {
	return errors.New("badger unavailable")
}

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

// TestMemberRepo_EnsureMember_LookupErrorDoesNotProvisionDuplicate pins that a
// failed Notion lookup aborts EnsureMember. Falling through to creation after a
// search error provisions a second member page + inline trades database for a
// member whose journal already exists, splitting their trades across two
// databases.
func TestMemberRepo_EnsureMember_LookupErrorDoesNotProvisionDuplicate(t *testing.T) {
	var pageCreates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"object":"error","status":400,"message":"search unavailable"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			pageCreates.Add(1)
			_, _ = w.Write([]byte(`{"id":"page-new"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			_, _ = w.Write([]byte(`{"id":"db-new"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1")

	if _, err := repo.EnsureMember(context.Background(), &domain.Member{
		TelegramID: 6006,
		Username:   "lookup",
	}); err == nil {
		t.Fatal("expected EnsureMember to surface the failed member lookup, got nil")
	}
	if got := pageCreates.Load(); got != 0 {
		t.Fatalf("EnsureMember created %d member pages after a failed lookup; want 0", got)
	}
}

// TestMemberRepo_EnsureMember_VerificationErrorDoesNotProvisionDuplicate covers
// the other half of the lookup: the search succeeds but a candidate page cannot
// be read. An unverifiable result must abort, not be treated as "no existing
// page", or the member gets a duplicate journal.
func TestMemberRepo_EnsureMember_VerificationErrorDoesNotProvisionDuplicate(t *testing.T) {
	var pageCreates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[{"id":"page-candidate","object":"page"}],"has_more":false}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"object":"error","status":400,"message":"block read failed"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			pageCreates.Add(1)
			_, _ = w.Write([]byte(`{"id":"page-new"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			_, _ = w.Write([]byte(`{"id":"db-new"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1")

	if _, err := repo.EnsureMember(context.Background(), &domain.Member{
		TelegramID: 6007,
		Username:   "unverifiable",
	}); err == nil {
		t.Fatal("expected EnsureMember to surface the failed page verification, got nil")
	}
	if got := pageCreates.Load(); got != 0 {
		t.Fatalf("EnsureMember created %d member pages after a failed verification; want 0", got)
	}
}

// TestMemberJournalTelegramID guards the marker parse that keeps /reset from
// archiving the owner's own pages under the shared journal parent page, and
// that identifies which persisted mapping to evict.
func TestMemberJournalTelegramID(t *testing.T) {
	tests := []struct {
		callout string
		wantID  int64
		wantOK  bool
	}{
		{"Telegram ID: 1001 | Username: @kresna", 1001, true},
		{"Telegram ID: 1001 | Username: @", 1001, true},
		{"Telegram ID: 1001", 1001, true},
		{"  Telegram ID: 42 | Username: @x  ", 42, true},
		{"Telegram ID: ", 0, false},
		{"Telegram ID: abc | Username: @x", 0, false},
		{"Telegram ID: 1001 notes", 0, false},
		{"Journal notes", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		id, ok := memberJournalTelegramID(tt.callout)
		if ok != tt.wantOK || (ok && id != tt.wantID) {
			t.Errorf("memberJournalTelegramID(%q) = (%d, %v), want (%d, %v)", tt.callout, id, ok, tt.wantID, tt.wantOK)
		}
	}
}

// TestMemberRepo_ResetAll_ArchivesOnlyVerifiedMemberJournals pins that reset
// archives the bot's journals and nothing else: the parent page also holds the
// owner's notes and databases, and the previous implementation archived every
// child page/database plus every search hit titled "Trade Journal".
func TestMemberRepo_ResetAll_ArchivesOnlyVerifiedMemberJournals(t *testing.T) {
	var (
		mu       sync.Mutex
		archived []string
	)

	children := map[string]string{
		"parent-1": `{"results":[
			{"id":"page-member","type":"child_page","archived":false},
			{"id":"page-notes","type":"child_page","archived":false},
			{"id":"db-owner","type":"child_database","archived":false}
		],"has_more":false}`,
		"page-member": `{"results":[
			{"id":"callout-member","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 1001 | Username: @member"}]}},
			{"id":"db-member","type":"child_database"}
		],"has_more":false}`,
		"page-found": `{"results":[
			{"id":"callout-found","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 2002 | Username: @found"}]}},
			{"id":"db-found","type":"child_database"}
		],"has_more":false}`,
		"page-notes": `{"results":[
			{"id":"para-notes","type":"paragraph"}
		],"has_more":false}`,
		"page-found-notes": `{"results":[
			{"id":"callout-notes","type":"callout","callout":{"rich_text":[{"plain_text":"Trade Journal planning notes"}]}}
		],"has_more":false}`,
		"page-cached-notes": `{"results":[
			{"id":"para-cached","type":"paragraph"}
		],"has_more":false}`,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children")
			if body, ok := children[id]; ok {
				_, _ = w.Write([]byte(body))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[
				{"id":"page-found","object":"page","archived":false,"parent":{"type":"page_id","page_id":"parent-1"}},
				{"id":"page-found-notes","object":"page","archived":false,"parent":{"type":"page_id","page_id":"parent-1"}},
				{"id":"page-other-parent","object":"page","archived":false,"parent":{"type":"page_id","page_id":"parent-2"}}
			],"has_more":false}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/pages/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/pages/")
			mu.Lock()
			archived = append(archived, id)
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	persistStore := newMockMemberCacheStore()
	repo := NewMemberRepo(client, "parent-1", persistStore)
	for _, m := range []*domain.Member{
		{TelegramID: 1001, Username: "member", NotionPageID: "page-member", NotionDBID: "db-member"},
		{TelegramID: 2002, Username: "found", NotionPageID: "page-found", NotionDBID: "db-found"},
		{TelegramID: 3003, Username: "cached-notes", NotionPageID: "page-cached-notes", NotionDBID: "db-cached"},
	} {
		repo.saveMemberToCache(context.Background(), m)
	}

	count, err := repo.ResetAll(context.Background())
	if err != nil {
		t.Fatalf("ResetAll failed: %v", err)
	}
	if count != 2 {
		t.Errorf("ResetAll archived %d pages, want 2 (verified member journals only)", count)
	}

	mu.Lock()
	got := append([]string(nil), archived...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("archived pages = %v, want exactly the two member journals", got)
	}
	wantArchived := map[string]bool{"page-member": true, "page-found": true}
	for _, id := range got {
		if !wantArchived[id] {
			t.Errorf("ResetAll archived unrelated page %q", id)
		}
		delete(wantArchived, id)
	}
	if len(wantArchived) != 0 {
		t.Errorf("ResetAll did not archive member journals: %v", wantArchived)
	}

	// Persisted mappings must go with the journals they point at; the mapping
	// for the non-journal page must survive so the retry/reuse path is intact.
	wantDeleted := map[int64]bool{1001: true, 2002: true}
	for _, id := range persistStore.deletedMembers() {
		if !wantDeleted[id] {
			t.Errorf("ResetAll evicted cached member %d, whose journal was not archived", id)
		}
		delete(wantDeleted, id)
	}
	if len(wantDeleted) != 0 {
		t.Errorf("ResetAll left persisted mappings for archived journals: %v", wantDeleted)
	}
	for _, id := range []int64{1001, 2002} {
		if persisted, err := persistStore.GetCachedMember(context.Background(), id); err != nil || persisted != nil {
			t.Errorf("persisted member %d = %+v, %v; want it evicted", id, persisted, err)
		}
	}
	if persisted, err := persistStore.GetCachedMember(context.Background(), 3003); err != nil || persisted == nil {
		t.Errorf("persisted member 3003 = %+v, %v; want it preserved", persisted, err)
	}

	repo.mu.RLock()
	cached := len(repo.cache)
	_, keptInMemory := repo.cache[3003]
	repo.mu.RUnlock()
	if cached != 1 || !keptInMemory {
		t.Errorf("in-memory cache = %d members (3003 present: %v), want only the unarchived member", cached, keptInMemory)
	}
}

// TestMemberRepo_ResetAll_PropagatesDiscoveryFailure pins that a failed parent
// listing or search is reported instead of returning success: the owner must
// not be told the journals were archived when they are still live.
func TestMemberRepo_ResetAll_PropagatesDiscoveryFailure(t *testing.T) {
	tests := []struct {
		name string
		fail func(*http.Request) bool
	}{
		{
			name: "parent listing",
			fail: func(r *http.Request) bool {
				return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children")
			},
		},
		{
			name: "search",
			fail: func(r *http.Request) bool {
				return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var patches atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.fail(r) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
					_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
					_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
				case r.Method == http.MethodPatch:
					patches.Add(1)
					_, _ = w.Write([]byte(`{}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			client := NewClient("ntn_test")
			client.baseURL = srv.URL + "/v1"
			repo := NewMemberRepo(client, "parent-1")
			repo.saveMemberToCache(context.Background(), &domain.Member{
				TelegramID: 4004, Username: "kept", NotionPageID: "page-kept", NotionDBID: "db-kept",
			})

			count, err := repo.ResetAll(context.Background())
			if err == nil {
				t.Fatal("expected ResetAll to return the discovery error, got nil")
			}
			if count != 0 {
				t.Errorf("ResetAll reported %d archived pages after a discovery failure, want 0", count)
			}
			if got := patches.Load(); got != 0 {
				t.Errorf("ResetAll archived %d pages after a discovery failure, want 0", got)
			}
			repo.mu.RLock()
			_, stillCached := repo.cache[4004]
			repo.mu.RUnlock()
			if !stillCached {
				t.Error("ResetAll dropped cached members even though nothing was archived")
			}
		})
	}
}

// TestMemberRepo_ResetAll_KeepsCacheForUnarchivedMembers pins cache correctness
// on partial failure: only members whose journal was actually archived may be
// forgotten, otherwise a later EnsureMember would provision a duplicate journal
// while the original page is still live.
func TestMemberRepo_ResetAll_KeepsCacheForUnarchivedMembers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children")
			switch id {
			case "parent-1":
				_, _ = w.Write([]byte(`{"results":[
					{"id":"page-a","type":"child_page"},
					{"id":"page-b","type":"child_page"}
				],"has_more":false}`))
			case "page-a":
				_, _ = w.Write([]byte(`{"results":[{"id":"c-a","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 5005 | Username: @a"}]}}],"has_more":false}`))
			case "page-b":
				_, _ = w.Write([]byte(`{"results":[{"id":"c-b","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 5006 | Username: @b"}]}}],"has_more":false}`))
			default:
				_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/pages/"):
			if strings.HasSuffix(r.URL.Path, "/page-b") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	persistStore := newMockMemberCacheStore()
	repo := NewMemberRepo(client, "parent-1", persistStore)
	repo.saveMemberToCache(context.Background(), &domain.Member{
		TelegramID: 5005, Username: "a", NotionPageID: "page-a", NotionDBID: "db-a",
	})
	repo.saveMemberToCache(context.Background(), &domain.Member{
		TelegramID: 5006, Username: "b", NotionPageID: "page-b", NotionDBID: "db-b",
	})

	count, err := repo.ResetAll(context.Background())
	if err == nil {
		t.Fatal("expected ResetAll to surface the failed archive call, got nil")
	}
	if count != 1 {
		t.Errorf("ResetAll archived %d pages, want 1", count)
	}

	repo.mu.RLock()
	_, aCached := repo.cache[5005]
	_, bCached := repo.cache[5006]
	repo.mu.RUnlock()
	if aCached {
		t.Error("member 5005 was archived but is still cached")
	}
	if !bCached {
		t.Error("member 5006 archive failed but was dropped from the cache")
	}

	// The persisted mapping is evicted *before* the archive attempt, so both
	// mappings are gone. That is what makes the failure safe: member 5005's
	// journal really is archived, and member 5006's is still live, so the next
	// EnsureMember rediscovers it under the parent page instead of reusing an
	// archived one.
	for _, id := range []int64{5005, 5006} {
		if persisted, err := persistStore.GetCachedMember(context.Background(), id); err != nil || persisted != nil {
			t.Errorf("persisted member %d = %+v, %v; want the mapping evicted before archiving", id, persisted, err)
		}
	}

	// The in-memory mapping for the still-live journal keeps the running
	// process from re-reading Notion.
	if found, err := repo.lookupCached(context.Background(), 5006); err != nil || found == nil || found.NotionDBID != "db-b" {
		t.Errorf("lookupCached(5006) = %+v, %v; want the live journal still resolvable", found, err)
	}
}

// TestMemberRepo_ResetAll_PropagatesVerificationFailure pins that a page whose
// content cannot be read aborts the reset. Treating "could not verify" as "not
// a member journal" would archive nothing yet report success, and treating it
// as verified would archive a page the bot cannot prove it owns.
func TestMemberRepo_ResetAll_PropagatesVerificationFailure(t *testing.T) {
	var patches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children")
			if id == "parent-1" {
				_, _ = w.Write([]byte(`{"results":[{"id":"page-member","type":"child_page"}],"has_more":false}`))
				return
			}
			w.WriteHeader(http.StatusBadRequest)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodPatch:
			patches.Add(1)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1")
	repo.saveMemberToCache(context.Background(), &domain.Member{
		TelegramID: 7007, Username: "unreadable", NotionPageID: "page-member", NotionDBID: "db-member",
	})

	count, err := repo.ResetAll(context.Background())
	if err == nil {
		t.Fatal("expected ResetAll to return the verification error, got nil")
	}
	if count != 0 {
		t.Errorf("ResetAll reported %d archived pages after a verification failure, want 0", count)
	}
	if got := patches.Load(); got != 0 {
		t.Errorf("ResetAll archived %d pages after a verification failure, want 0", got)
	}
	repo.mu.RLock()
	_, stillCached := repo.cache[7007]
	repo.mu.RUnlock()
	if !stillCached {
		t.Error("ResetAll dropped cached members even though nothing was archived")
	}
}

// TestMemberRepo_ResetAll_KeepsJournalLiveWhenMappingCannotBeEvicted pins the
// evict-before-archive ordering: if the persisted mapping cannot be removed the
// journal must stay live (and reusable) instead of being archived behind a
// mapping that still points at it, which would hand the next EnsureMember an
// archived page.
func TestMemberRepo_ResetAll_KeepsJournalLiveWhenMappingCannotBeEvicted(t *testing.T) {
	var patches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children")
			if id == "parent-1" {
				_, _ = w.Write([]byte(`{"results":[{"id":"page-a","type":"child_page"}],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"c-a","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 8008 | Username: @a"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/pages/"):
			patches.Add(1)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	persistStore := newMockMemberCacheStore()
	persistStore.deleteErr = errors.New("badger write failed")
	repo := NewMemberRepo(client, "parent-1", persistStore)
	repo.saveMemberToCache(context.Background(), &domain.Member{
		TelegramID: 8008, Username: "a", NotionPageID: "page-a", NotionDBID: "db-a",
	})

	count, err := repo.ResetAll(context.Background())
	if err == nil {
		t.Fatal("expected ResetAll to report the failed mapping eviction, got nil")
	}
	if count != 0 {
		t.Errorf("ResetAll archived %d pages, want 0 (mapping could not be evicted)", count)
	}
	if got := patches.Load(); got != 0 {
		t.Errorf("ResetAll archived %d pages behind a stale mapping, want 0", got)
	}

	// The journal is still live, so its mapping must remain intact and usable.
	persisted, err := persistStore.GetCachedMember(context.Background(), 8008)
	if err != nil {
		t.Fatalf("GetCachedMember failed: %v", err)
	}
	if persisted == nil || persisted.NotionPageID != "page-a" || persisted.NotionDBID != "db-a" {
		t.Fatalf("persisted mapping = %+v, %v; want it preserved for the live journal", persisted, err)
	}
	repo.mu.RLock()
	_, stillCached := repo.cache[8008]
	repo.mu.RUnlock()
	if !stillCached {
		t.Error("member 8008's journal was not archived but was dropped from the in-memory cache")
	}
}

// TestMemberRepo_ResetAll_RetriesTransientEvictionFailure pins that one
// transient store failure does not abort the reset: the delete is retried, the
// journal is archived, and no mapping survives it.
func TestMemberRepo_ResetAll_RetriesTransientEvictionFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children")
			if id == "parent-1" {
				_, _ = w.Write([]byte(`{"results":[{"id":"page-a","type":"child_page"}],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"c-a","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 8200 | Username: @a"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/pages/"):
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	persistStore := newMockMemberCacheStore()
	persistStore.deleteErrs = []error{errors.New("badger busy")}
	repo := NewMemberRepo(client, "parent-1", persistStore)
	repo.saveMemberToCache(context.Background(), &domain.Member{
		TelegramID: 8200, Username: "a", NotionPageID: "page-a", NotionDBID: "db-a",
	})

	count, err := repo.ResetAll(context.Background())
	if err != nil {
		t.Fatalf("ResetAll failed after a transient eviction failure: %v", err)
	}
	if count != 1 {
		t.Errorf("ResetAll archived %d pages, want 1", count)
	}
	persisted, err := persistStore.GetCachedMember(context.Background(), 8200)
	if err != nil {
		t.Fatalf("GetCachedMember failed: %v", err)
	}
	if persisted != nil {
		t.Errorf("persisted member = %+v, want it deleted by the retry", persisted)
	}
}

// TestMemberRepo_ResetAll_ArchiveFailureLeavesJournalRediscoverable pins the
// other half of the ordering: the mapping is evicted first, so a failed archive
// leaves a live journal that parent-scoped discovery finds again. The retry must
// reuse it, never provision a duplicate.
func TestMemberRepo_ResetAll_ArchiveFailureLeavesJournalRediscoverable(t *testing.T) {
	var (
		pageCreates atomic.Int32
		pageExists  atomic.Bool
	)
	pageExists.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/parent-1/children":
			if pageExists.Load() {
				_, _ = w.Write([]byte(`{"results":[{"id":"page-a","type":"child_page"}],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/page-a/children":
			_, _ = w.Write([]byte(`{"results":[
				{"id":"c-a","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 8300 | Username: @a"}]}},
				{"id":"db-a","type":"child_database"}
			],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/pages/"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"object":"error","status":400,"message":"archive failed"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			pageCreates.Add(1)
			_, _ = w.Write([]byte(`{"id":"page-duplicate"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	persistStore := newMockMemberCacheStore()
	repo := NewMemberRepo(client, "parent-1", persistStore)
	repo.saveMemberToCache(context.Background(), &domain.Member{
		TelegramID: 8300, Username: "a", NotionPageID: "page-a", NotionDBID: "db-a",
	})

	count, err := repo.ResetAll(context.Background())
	if err == nil {
		t.Fatal("expected ResetAll to surface the failed archive call, got nil")
	}
	if count != 0 {
		t.Errorf("ResetAll reported %d archived pages, want 0", count)
	}
	if persisted, err := persistStore.GetCachedMember(context.Background(), 8300); err != nil || persisted != nil {
		t.Errorf("persisted mapping = %+v, %v; want it evicted before the archive attempt", persisted, err)
	}

	// The journal is still live and must be rediscovered, not duplicated. Use a
	// cold repo to model a restart: the mapping was evicted, so only
	// parent-scoped discovery can find the journal again.
	cold := NewMemberRepo(client, "parent-1", persistStore)
	m, err := cold.EnsureMember(context.Background(), &domain.Member{TelegramID: 8300, Username: "a"})
	if err != nil {
		t.Fatalf("EnsureMember after a failed archive failed: %v", err)
	}
	if m.NotionPageID != "page-a" || m.NotionDBID != "db-a" {
		t.Fatalf("member = %+v, want the still-live journal reused", m)
	}
	if got := pageCreates.Load(); got != 0 {
		t.Errorf("EnsureMember created %d duplicate journals, want 0", got)
	}
}

// TestMemberRepo_ResetAll_BlocksConcurrentJournaling pins the journaling gate:
// while journals are being archived, a journal write fails fast with
// ErrResetInProgress instead of landing in a page that is about to disappear.
func TestMemberRepo_ResetAll_BlocksConcurrentJournaling(t *testing.T) {
	archiving := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children")
			if id == "parent-1" {
				_, _ = w.Write([]byte(`{"results":[{"id":"page-a","type":"child_page"}],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"c-a","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 8400 | Username: @a"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/pages/"):
			once.Do(func() { close(archiving) })
			<-release
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			_, _ = w.Write([]byte(`{"id":"db-a"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1")
	repo.saveMemberToCache(context.Background(), &domain.Member{
		TelegramID: 8400, Username: "a", NotionPageID: "page-a", NotionDBID: "db-a",
	})
	tradeRepo := NewTradeRepo(client, repo, newMockTradeCacheStore())
	ctx := context.Background()

	resetDone := make(chan error, 1)
	go func() {
		_, err := repo.ResetAll(ctx)
		resetDone <- err
	}()

	select {
	case <-archiving:
	case <-time.After(2 * time.Second):
		t.Fatal("ResetAll never reached the archival phase")
	}

	if _, err := repo.EnsureMember(ctx, &domain.Member{TelegramID: 8500, Username: "late"}); !errors.Is(err, ErrResetInProgress) {
		t.Errorf("EnsureMember during reset error = %v, want ErrResetInProgress", err)
	}
	if err := tradeRepo.SaveTrade(ctx, 8400, &domain.Trade{Symbol: "XAUUSD"}); !errors.Is(err, ErrResetInProgress) {
		t.Errorf("SaveTrade during reset error = %v, want ErrResetInProgress", err)
	}
	if err := tradeRepo.DeleteTrade(ctx, 8400, "t-1"); !errors.Is(err, ErrResetInProgress) {
		t.Errorf("DeleteTrade during reset error = %v, want ErrResetInProgress", err)
	}
	if err := tradeRepo.UpdateTrade(ctx, 8400, &domain.Trade{ID: "t-1"}); !errors.Is(err, ErrResetInProgress) {
		t.Errorf("UpdateTrade during reset error = %v, want ErrResetInProgress", err)
	}

	close(release)
	select {
	case err := <-resetDone:
		if err != nil {
			t.Fatalf("ResetAll failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ResetAll did not finish after the archive call was released")
	}

	// Once the reset is done, journaling works again: the archived journal is
	// gone from the cache, so the member is reprovisioned under the parent.
	m, err := repo.EnsureMember(ctx, &domain.Member{TelegramID: 8400, Username: "a"})
	if err != nil {
		t.Fatalf("EnsureMember after reset failed: %v", err)
	}
	if m.NotionDBID != "db-a" {
		t.Errorf("member after reset = %+v, want a usable trade database", m)
	}
}

// TestMemberRepo_EnsureMember_RepairsJournalMissingTradeDB pins the repair of a
// partially provisioned journal discovered through Notion search: the page was
// created but its inline Trades database never was, so EnsureMember must create
// the database inside that page instead of returning an unusable member or
// provisioning a duplicate page.
func TestMemberRepo_EnsureMember_RepairsJournalMissingTradeDB(t *testing.T) {
	var (
		pageCreates atomic.Int32
		dbCreates   atomic.Int32
		dbParent    string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[{"id":"page-existing","object":"page"}],"has_more":false}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			_, _ = w.Write([]byte(`{"results":[{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 9009 | Username: @stuck"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			dbCreates.Add(1)
			var payload struct {
				Parent struct {
					PageID string `json:"page_id"`
				} `json:"parent"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			dbParent = payload.Parent.PageID
			_, _ = w.Write([]byte(`{"id":"db-repaired"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			pageCreates.Add(1)
			_, _ = w.Write([]byte(`{"id":"page-duplicate"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	persistStore := newMockMemberCacheStore()
	repo := NewMemberRepo(client, "parent-1", persistStore)

	m, err := repo.EnsureMember(context.Background(), &domain.Member{
		TelegramID: 9009,
		Username:   "stuck",
		FirstName:  "Stuck",
	})
	if err != nil {
		t.Fatalf("EnsureMember failed: %v", err)
	}
	if m.NotionPageID != "page-existing" || m.NotionDBID != "db-repaired" {
		t.Fatalf("member = %+v, want page-existing/db-repaired", m)
	}
	if m.Username != "stuck" {
		t.Errorf("Username = %q, want the caller's username filled in", m.Username)
	}
	if got := pageCreates.Load(); got != 0 {
		t.Errorf("EnsureMember created %d duplicate member pages, want 0", got)
	}
	if got := dbCreates.Load(); got != 1 {
		t.Errorf("EnsureMember created %d trade databases, want 1", got)
	}
	if dbParent != "page-existing" {
		t.Errorf("trade database parent = %q, want the existing journal page", dbParent)
	}
	persisted, err := persistStore.GetCachedMember(context.Background(), 9009)
	if err != nil || persisted == nil || persisted.NotionDBID != "db-repaired" {
		t.Errorf("persisted member = %+v, %v; want the repaired mapping", persisted, err)
	}
}

// TestMemberRepo_EnsureMember_FailedDBCreationThenRetryRepairs pins the retry
// path after page creation succeeds but database creation fails. Notion search
// indexes page titles only, so the leftover page is not findable by the
// Telegram ID in its callout: the retry must find it through the pending
// mapping (and, failing that, parent-scoped discovery) and repair it instead of
// provisioning a second journal.
func TestMemberRepo_EnsureMember_FailedDBCreationThenRetryRepairs(t *testing.T) {
	var (
		pageCreates atomic.Int32
		dbAttempts  atomic.Int32
		searches    atomic.Int32
		pageExists  atomic.Bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			// Realistic Notion behaviour: "Telegram ID: <id>" matches no page
			// title, so search never returns the journal.
			searches.Add(1)
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			if strings.Contains(r.URL.Path, "/parent-1/") {
				if pageExists.Load() {
					_, _ = w.Write([]byte(`{"results":[{"id":"page-orphan","type":"child_page"}],"has_more":false}`))
					return
				}
				_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 9100 | Username: @retry"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			pageCreates.Add(1)
			pageExists.Store(true)
			_, _ = w.Write([]byte(`{"id":"page-orphan"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			if dbAttempts.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"object":"error","status":400,"message":"database creation failed"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"db-retry"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	persistStore := newMockMemberCacheStore()
	repo := NewMemberRepo(client, "parent-1", persistStore)
	ctx := context.Background()

	if _, err := repo.EnsureMember(ctx, &domain.Member{TelegramID: 9100, Username: "retry"}); err == nil {
		t.Fatal("expected the first EnsureMember to fail when database creation fails")
	}

	// The page is persisted immediately, before the database exists, so the
	// retry can find it. It must not be served as a usable member, though.
	repo.mu.RLock()
	_, cached := repo.cache[9100]
	repo.mu.RUnlock()
	if cached {
		t.Error("failed provisioning was cached as a usable member")
	}
	pending, err := persistStore.GetCachedMember(ctx, 9100)
	if err != nil {
		t.Fatalf("GetCachedMember failed: %v", err)
	}
	if pending == nil || pending.NotionPageID != "page-orphan" || pending.NotionDBID != "" {
		t.Fatalf("pending mapping = %+v, %v; want the page recorded without a database", pending, err)
	}

	m, err := repo.EnsureMember(ctx, &domain.Member{TelegramID: 9100, Username: "retry"})
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if m.NotionPageID != "page-orphan" || m.NotionDBID != "db-retry" {
		t.Fatalf("retried member = %+v, want the orphaned page repaired", m)
	}
	if got := pageCreates.Load(); got != 1 {
		t.Errorf("retry created %d member pages, want 1 total (no duplicate)", got)
	}
	if got := dbAttempts.Load(); got != 2 {
		t.Errorf("database creation attempts = %d, want 2 (failed + retried)", got)
	}
	if got := searches.Load(); got != 1 {
		t.Errorf("Notion searches = %d, want 1 (the retry must use the pending mapping)", got)
	}
}

// TestMemberRepo_EnsureMember_RepairsCachedJournalWithoutSearch pins that a
// persisted mapping whose Trades database was never created is repaired in
// place: the cached page is a verified journal, so no search (and therefore no
// duplicate page) is needed.
func TestMemberRepo_EnsureMember_RepairsCachedJournalWithoutSearch(t *testing.T) {
	var (
		searches    atomic.Int32
		pageCreates atomic.Int32
		dbCreates   atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			searches.Add(1)
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			_, _ = w.Write([]byte(`{"results":[{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 9500 | Username: @cached"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			dbCreates.Add(1)
			_, _ = w.Write([]byte(`{"id":"db-cached-repair"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			pageCreates.Add(1)
			_, _ = w.Write([]byte(`{"id":"page-duplicate"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	persistStore := newMockMemberCacheStore()
	_ = persistStore.SaveCachedMember(context.Background(), &domain.Member{
		TelegramID: 9500, Username: "cached", NotionPageID: "page-cached",
	})
	repo := NewMemberRepo(client, "parent-1", persistStore)

	m, err := repo.EnsureMember(context.Background(), &domain.Member{
		TelegramID: 9500,
		Username:   "cached",
	})
	if err != nil {
		t.Fatalf("EnsureMember failed: %v", err)
	}
	if m.NotionPageID != "page-cached" || m.NotionDBID != "db-cached-repair" {
		t.Fatalf("member = %+v, want the cached page repaired", m)
	}
	if m.Username != "cached" {
		t.Errorf("Username = %q, want the cached identity preserved", m.Username)
	}
	if got := searches.Load(); got != 0 {
		t.Errorf("EnsureMember searched Notion %d times for a cached journal, want 0", got)
	}
	if got := pageCreates.Load(); got != 0 {
		t.Errorf("EnsureMember created %d member pages, want 0", got)
	}
	if got := dbCreates.Load(); got != 1 {
		t.Errorf("EnsureMember created %d trade databases, want 1", got)
	}
}

// TestMemberRepo_GetMember_RepairsJournalMissingTradeDB pins the read path:
// /history and /stats must repair a partially provisioned journal instead of
// reporting an empty history.
func TestMemberRepo_GetMember_RepairsJournalMissingTradeDB(t *testing.T) {
	var (
		pageCreates atomic.Int32
		dbParent    string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[{"id":"page-existing","object":"page"}],"has_more":false}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			_, _ = w.Write([]byte(`{"results":[{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 9300 | Username: @reader"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			var payload struct {
				Parent struct {
					PageID string `json:"page_id"`
				} `json:"parent"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			dbParent = payload.Parent.PageID
			_, _ = w.Write([]byte(`{"id":"db-fixed"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			pageCreates.Add(1)
			_, _ = w.Write([]byte(`{"id":"page-duplicate"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1")

	m, err := repo.GetMember(context.Background(), 9300)
	if err != nil {
		t.Fatalf("GetMember failed: %v", err)
	}
	if m.NotionPageID != "page-existing" || m.NotionDBID != "db-fixed" {
		t.Fatalf("member = %+v, want page-existing/db-fixed", m)
	}
	if dbParent != "page-existing" {
		t.Errorf("trade database parent = %q, want the existing journal page", dbParent)
	}
	if got := pageCreates.Load(); got != 0 {
		t.Errorf("GetMember created %d member pages, want 0", got)
	}
}

// TestMemberRepo_GetMember_ResolvesPersistedDatabaseID pins that a persisted
// mapping missing only the database ID is completed from the page (and written
// back) instead of being returned broken or triggering a search.
func TestMemberRepo_GetMember_ResolvesPersistedDatabaseID(t *testing.T) {
	var (
		searches  atomic.Int32
		dbCreates atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			searches.Add(1)
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			_, _ = w.Write([]byte(`{"results":[
				{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 9400 | Username: @resolved"}]}},
				{"id":"db-resolved","type":"child_database"}
			],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			dbCreates.Add(1)
			_, _ = w.Write([]byte(`{"id":"db-unexpected"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	persistStore := newMockMemberCacheStore()
	_ = persistStore.SaveCachedMember(context.Background(), &domain.Member{
		TelegramID: 9400, Username: "resolved", NotionPageID: "page-resolved",
	})
	repo := NewMemberRepo(client, "parent-1", persistStore)

	m, err := repo.GetMember(context.Background(), 9400)
	if err != nil {
		t.Fatalf("GetMember failed: %v", err)
	}
	if m.NotionDBID != "db-resolved" {
		t.Fatalf("NotionDBID = %q, want db-resolved", m.NotionDBID)
	}
	if got := searches.Load(); got != 0 {
		t.Errorf("GetMember searched Notion %d times for a cached mapping, want 0", got)
	}
	if got := dbCreates.Load(); got != 0 {
		t.Errorf("GetMember created %d trade databases for an existing one, want 0", got)
	}
	persisted, err := persistStore.GetCachedMember(context.Background(), 9400)
	if err != nil || persisted == nil || persisted.NotionDBID != "db-resolved" {
		t.Errorf("persisted member = %+v, %v; want the resolved database ID", persisted, err)
	}
}

// TestMemberRepo_GetMember_ReadFailureDoesNotServeBrokenMember pins that a
// failure to read a cached journal is surfaced and leaves the cache untouched.
// The old code swallowed the read error, returned a member with no database,
// and re-persisted that unusable mapping; GetTrades then reported an empty
// history.
func TestMemberRepo_GetMember_ReadFailureDoesNotServeBrokenMember(t *testing.T) {
	var searches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"object":"error","status":400,"message":"block read failed"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			searches.Add(1)
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	persistStore := newMockMemberCacheStore()
	_ = persistStore.SaveCachedMember(context.Background(), &domain.Member{
		TelegramID: 9200, Username: "cached", NotionPageID: "page-cached",
	})
	repo := NewMemberRepo(client, "parent-1", persistStore)

	m, err := repo.GetMember(context.Background(), 9200)
	if err == nil {
		t.Fatalf("expected GetMember to surface the page read failure, got member %+v", m)
	}
	if m != nil {
		t.Errorf("GetMember returned a member alongside the error: %+v", m)
	}
	if got := searches.Load(); got != 0 {
		t.Errorf("GetMember fell through to search %d times after a read failure, want 0", got)
	}
	repo.mu.RLock()
	_, cached := repo.cache[9200]
	repo.mu.RUnlock()
	if cached {
		t.Error("unverifiable mapping was promoted into the in-memory cache")
	}
	if persisted, err := persistStore.GetCachedMember(context.Background(), 9200); err != nil || persisted == nil || persisted.NotionDBID != "" {
		t.Errorf("persisted member = %+v, %v; want the original partial mapping", persisted, err)
	}
	if got := persistStore.saveCount(); got != 1 {
		t.Errorf("cache writes = %d, want only the test's initial save (no empty-DB rewrite)", got)
	}
}

// TestMemberRepo_GetMember_ConcurrentRepairCreatesOneDatabase pins that two
// concurrent reads of a partially provisioned journal create exactly one Trades
// database: repair runs under the same per-member lock as provisioning.
func TestMemberRepo_GetMember_ConcurrentRepairCreatesOneDatabase(t *testing.T) {
	var (
		dbCreates atomic.Int32
		searches  atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			_, _ = w.Write([]byte(`{"results":[{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 9600 | Username: @racer"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			dbCreates.Add(1)
			// Hold the first repair open so the second caller is guaranteed to
			// wait on the per-member lock.
			time.Sleep(100 * time.Millisecond)
			_, _ = w.Write([]byte(`{"id":"db-once"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			searches.Add(1)
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	persistStore := newMockMemberCacheStore()
	_ = persistStore.SaveCachedMember(context.Background(), &domain.Member{
		TelegramID: 9600, Username: "racer", NotionPageID: "page-racer",
	})
	repo := NewMemberRepo(client, "parent-1", persistStore)

	var (
		wg      sync.WaitGroup
		results [2]*domain.Member
		errs    [2]error
	)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = repo.GetMember(context.Background(), 9600)
		}()
	}
	wg.Wait()

	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("GetMember[%d] failed: %v", i, errs[i])
		}
		if results[i].NotionDBID != "db-once" {
			t.Errorf("GetMember[%d].NotionDBID = %q, want db-once", i, results[i].NotionDBID)
		}
	}
	if got := dbCreates.Load(); got != 1 {
		t.Errorf("repair created %d Trades databases, want 1", got)
	}
	if got := searches.Load(); got != 0 {
		t.Errorf("repair searched Notion %d times for a cached journal, want 0", got)
	}
}

// TestMemberRepo_EnsureMember_TitleOnlySearchFindsJournalViaParent models real
// Notion search, which indexes page titles only: the Telegram ID lives in a
// callout, so searching for it returns nothing. Discovery must fall back to the
// parent page's children, verify the callout, and repair the journal in place.
func TestMemberRepo_EnsureMember_TitleOnlySearchFindsJournalViaParent(t *testing.T) {
	var (
		searches    atomic.Int32
		pageCreates atomic.Int32
		dbCreates   atomic.Int32
		dbParent    string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			searches.Add(1)
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/parent-1/children":
			_, _ = w.Write([]byte(`{"results":[
				{"id":"page-notes","type":"child_page","archived":false},
				{"id":"page-archived","type":"child_page","archived":true},
				{"id":"page-journal","type":"child_page","archived":false}
			],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/page-notes/children":
			_, _ = w.Write([]byte(`{"results":[{"id":"p-1","type":"paragraph"}],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/page-journal/children":
			_, _ = w.Write([]byte(`{"results":[{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 9700 | Username: @journal"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			dbCreates.Add(1)
			var payload struct {
				Parent struct {
					PageID string `json:"page_id"`
				} `json:"parent"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			dbParent = payload.Parent.PageID
			_, _ = w.Write([]byte(`{"id":"db-found"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			pageCreates.Add(1)
			_, _ = w.Write([]byte(`{"id":"page-duplicate"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	persistStore := newMockMemberCacheStore()
	repo := NewMemberRepo(client, "parent-1", persistStore)

	m, err := repo.EnsureMember(context.Background(), &domain.Member{
		TelegramID: 9700,
		Username:   "journal",
	})
	if err != nil {
		t.Fatalf("EnsureMember failed: %v", err)
	}
	if m.NotionPageID != "page-journal" || m.NotionDBID != "db-found" {
		t.Fatalf("member = %+v, want the existing journal page repaired", m)
	}
	if got := pageCreates.Load(); got != 0 {
		t.Errorf("EnsureMember created %d duplicate journals, want 0", got)
	}
	if got := dbCreates.Load(); got != 1 {
		t.Errorf("trade database creations = %d, want 1", got)
	}
	if dbParent != "page-journal" {
		t.Errorf("trade database parent = %q, want the discovered journal page", dbParent)
	}
	if got := searches.Load(); got != 1 {
		t.Errorf("Notion searches = %d, want 1", got)
	}
	if persisted, err := persistStore.GetCachedMember(context.Background(), 9700); err != nil || persisted == nil || persisted.NotionDBID != "db-found" {
		t.Errorf("persisted member = %+v, %v; want the repaired mapping", persisted, err)
	}
}

// TestMemberRepo_EnsureMember_PendingMappingLostFallsBackToParentDiscovery
// covers a cold process with no persistent store: the page created by a failed
// provisioning attempt is not in any cache, so discovery must find it under the
// parent page rather than provisioning a duplicate.
func TestMemberRepo_EnsureMember_PendingMappingLostFallsBackToParentDiscovery(t *testing.T) {
	var (
		pageCreates atomic.Int32
		dbAttempts  atomic.Int32
		searches    atomic.Int32
		pageExists  atomic.Bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			searches.Add(1)
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/parent-1/children":
			if pageExists.Load() {
				_, _ = w.Write([]byte(`{"results":[{"id":"page-orphan","type":"child_page"}],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/page-orphan/children":
			_, _ = w.Write([]byte(`{"results":[{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 9800 | Username: @cold"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			pageCreates.Add(1)
			pageExists.Store(true)
			_, _ = w.Write([]byte(`{"id":"page-orphan"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			if dbAttempts.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"object":"error","status":400,"message":"database creation failed"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"db-recovered"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	ctx := context.Background()

	// No persistent store: nothing survives the failed attempt.
	if _, err := NewMemberRepo(client, "parent-1").EnsureMember(ctx, &domain.Member{
		TelegramID: 9800,
		Username:   "cold",
	}); err == nil {
		t.Fatal("expected the first EnsureMember to fail when database creation fails")
	}

	m, err := NewMemberRepo(client, "parent-1").EnsureMember(ctx, &domain.Member{
		TelegramID: 9800,
		Username:   "cold",
	})
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if m.NotionPageID != "page-orphan" || m.NotionDBID != "db-recovered" {
		t.Fatalf("retried member = %+v, want the discovered page repaired", m)
	}
	if got := pageCreates.Load(); got != 1 {
		t.Errorf("member pages created = %d, want 1 (no duplicate)", got)
	}
	if got := dbAttempts.Load(); got != 2 {
		t.Errorf("database creation attempts = %d, want 2", got)
	}
	if got := searches.Load(); got != 2 {
		t.Errorf("Notion searches = %d, want 2 (one per EnsureMember call)", got)
	}
}

// TestMemberRepo_EnsureMember_ParentDiscoveryFailureDoesNotProvision pins the
// fail-closed rule: if the parent page cannot be listed, an existing journal
// may still be there, so provisioning would duplicate it.
func TestMemberRepo_EnsureMember_ParentDiscoveryFailureDoesNotProvision(t *testing.T) {
	var pageCreates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/parent-1/children":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"object":"error","status":400,"message":"block read failed"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			pageCreates.Add(1)
			_, _ = w.Write([]byte(`{"id":"page-duplicate"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			_, _ = w.Write([]byte(`{"id":"db-duplicate"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1")

	if _, err := repo.EnsureMember(context.Background(), &domain.Member{
		TelegramID: 9900,
		Username:   "unlistable",
	}); err == nil {
		t.Fatal("expected EnsureMember to surface the failed parent listing, got nil")
	}
	if got := pageCreates.Load(); got != 0 {
		t.Fatalf("EnsureMember created %d journals after an unverified discovery, want 0", got)
	}
}

// TestMemberRepo_EnsureMember_UnverifiableChildWithoutMatchAborts pins that an
// unreadable child page blocks provisioning when no verified match was found:
// it may well be the member's journal.
func TestMemberRepo_EnsureMember_UnverifiableChildWithoutMatchAborts(t *testing.T) {
	var pageCreates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/parent-1/children":
			_, _ = w.Write([]byte(`{"results":[{"id":"page-unreadable","type":"child_page"}],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/page-unreadable/children":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"object":"error","status":400,"message":"block read failed"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			pageCreates.Add(1)
			_, _ = w.Write([]byte(`{"id":"page-duplicate"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1")

	if _, err := repo.EnsureMember(context.Background(), &domain.Member{
		TelegramID: 9901,
		Username:   "unreadable",
	}); err == nil {
		t.Fatal("expected EnsureMember to surface the unverifiable child page, got nil")
	}
	if got := pageCreates.Load(); got != 0 {
		t.Errorf("EnsureMember created %d journals after an unverifiable child, want 0", got)
	}
}

// TestMemberRepo_EnsureMember_UnreadableChildDoesNotDiscardMatch pins the other
// half of the rule: a read failure must not discard a match found later in the
// same parent listing, which would otherwise abort a member whose journal is
// perfectly reachable.
func TestMemberRepo_EnsureMember_UnreadableChildDoesNotDiscardMatch(t *testing.T) {
	var (
		pageCreates atomic.Int32
		dbCreates   atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/parent-1/children":
			_, _ = w.Write([]byte(`{"results":[
				{"id":"page-unreadable","type":"child_page"},
				{"id":"page-journal","type":"child_page"}
			],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/page-unreadable/children":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"object":"error","status":400,"message":"block read failed"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/page-journal/children":
			_, _ = w.Write([]byte(`{"results":[{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 9902 | Username: @reachable"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			dbCreates.Add(1)
			_, _ = w.Write([]byte(`{"id":"db-reachable"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			pageCreates.Add(1)
			_, _ = w.Write([]byte(`{"id":"page-duplicate"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1")

	m, err := repo.EnsureMember(context.Background(), &domain.Member{
		TelegramID: 9902,
		Username:   "reachable",
	})
	if err != nil {
		t.Fatalf("EnsureMember failed: %v", err)
	}
	if m.NotionPageID != "page-journal" || m.NotionDBID != "db-reachable" {
		t.Fatalf("member = %+v, want the reachable journal repaired", m)
	}
	if got := pageCreates.Load(); got != 0 {
		t.Errorf("EnsureMember created %d journals, want 0", got)
	}
	if got := dbCreates.Load(); got != 1 {
		t.Errorf("trade database creations = %d, want 1", got)
	}
}

// TestMemberRepo_EnsureMember_ConcurrentRetryAfterFailedDBCreation pins that a
// failed database creation followed by two concurrent retries still yields one
// journal and one Trades database: the retry repairs under the per-member lock.
func TestMemberRepo_EnsureMember_ConcurrentRetryAfterFailedDBCreation(t *testing.T) {
	var (
		pageCreates atomic.Int32
		dbAttempts  atomic.Int32
		pageExists  atomic.Bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/parent-1/children":
			if pageExists.Load() {
				_, _ = w.Write([]byte(`{"results":[{"id":"page-orphan","type":"child_page"}],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/page-orphan/children":
			_, _ = w.Write([]byte(`{"results":[{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 9903 | Username: @concurrent"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			pageCreates.Add(1)
			pageExists.Store(true)
			_, _ = w.Write([]byte(`{"id":"page-orphan"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			if dbAttempts.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"object":"error","status":400,"message":"database creation failed"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"db-once"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	persistStore := newMockMemberCacheStore()
	repo := NewMemberRepo(client, "parent-1", persistStore)
	ctx := context.Background()

	// Seed the failed state: page exists, no Trades database yet.
	if _, err := repo.EnsureMember(ctx, &domain.Member{TelegramID: 9903, Username: "concurrent"}); err == nil {
		t.Fatal("expected the first EnsureMember to fail when database creation fails")
	}

	var (
		wg      sync.WaitGroup
		results [2]*domain.Member
		errs    [2]error
		start   = make(chan struct{})
	)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = repo.EnsureMember(ctx, &domain.Member{TelegramID: 9903, Username: "concurrent"})
		}()
	}
	close(start)
	wg.Wait()

	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("concurrent retry %d failed: %v", i, errs[i])
		}
		if results[i].NotionPageID != "page-orphan" || results[i].NotionDBID != "db-once" {
			t.Errorf("retry %d result = %+v, want the repaired journal", i, results[i])
		}
	}
	if got := pageCreates.Load(); got != 1 {
		t.Errorf("member pages created = %d, want 1", got)
	}
	if got := dbAttempts.Load(); got != 2 {
		t.Errorf("database creation attempts = %d, want 2 (one failed, one retried)", got)
	}
}

// TestMemberRepo_EnsureMember_ReconcilesAmbiguousPageCreate pins the fix for a
// 5xx (or lost response) on the member-page create: the page may exist, so the
// retry must not create a second journal. It must find the page under the parent
// and adopt it.
//
// The parent page is empty until the create is attempted, which is what forces
// the create path; the POST then commits server-side and fails the response.
func TestMemberRepo_EnsureMember_ReconcilesAmbiguousPageCreate(t *testing.T) {
	var (
		pageCreates    atomic.Int32
		dbCreates      atomic.Int32
		parentListings atomic.Int32
		pageExists     atomic.Bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/parent-1/children":
			parentListings.Add(1)
			if !pageExists.Load() {
				_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"page-committed","type":"child_page"}],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/page-committed/children":
			_, _ = w.Write([]byte(`{"results":[{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 9950 | Username: @ambiguous"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			// The write commits server-side but the response is lost.
			pageCreates.Add(1)
			pageExists.Store(true)
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			dbCreates.Add(1)
			_, _ = w.Write([]byte(`{"id":"db-adopted"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1", newMockMemberCacheStore())

	m, err := repo.EnsureMember(context.Background(), &domain.Member{
		TelegramID: 9950,
		Username:   "ambiguous",
	})
	if err != nil {
		t.Fatalf("EnsureMember failed: %v", err)
	}
	if m.NotionPageID != "page-committed" || m.NotionDBID != "db-adopted" {
		t.Fatalf("member = %+v, want the committed page adopted", m)
	}
	if got := pageCreates.Load(); got != 1 {
		t.Errorf("member-page create attempts = %d, want 1 (no retry after an ambiguous failure)", got)
	}
	if got := dbCreates.Load(); got != 1 {
		t.Errorf("trade database creations = %d, want 1", got)
	}
	if got := parentListings.Load(); got < 2 {
		t.Errorf("parent listings = %d, want the ambiguous create reconciled by re-reading the parent page", got)
	}
}

// TestMemberRepo_EnsureMember_ReconcilesAmbiguousDatabaseCreate pins the same
// rule for the Trades database: a 5xx may mean the database exists, so the
// repair must adopt it by reading the page instead of creating a second one
// (which would split the member's history).
func TestMemberRepo_EnsureMember_ReconcilesAmbiguousDatabaseCreate(t *testing.T) {
	var (
		dbCreates atomic.Int32
		pageReads atomic.Int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/parent-1/children":
			_, _ = w.Write([]byte(`{"results":[{"id":"page-half","type":"child_page"}],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/page-half/children":
			// After the ambiguous create the database is visible.
			if dbCreates.Load() > 0 {
				pageReads.Add(1)
				_, _ = w.Write([]byte(`{"results":[
					{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 9960 | Username: @half"}]}},
					{"id":"db-committed","type":"child_database"}
				],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 9960 | Username: @half"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			dbCreates.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1")

	m, err := repo.EnsureMember(context.Background(), &domain.Member{
		TelegramID: 9960,
		Username:   "half",
	})
	if err != nil {
		t.Fatalf("EnsureMember failed: %v", err)
	}
	if m.NotionDBID != "db-committed" {
		t.Fatalf("NotionDBID = %q, want the committed database adopted", m.NotionDBID)
	}
	if got := dbCreates.Load(); got != 1 {
		t.Errorf("trade database creations = %d, want 1 (no retry after an ambiguous failure)", got)
	}
	if got := pageReads.Load(); got != 1 {
		t.Errorf("reconciliation page reads = %d, want 1", got)
	}
}

// TestMemberRepo_ResetAllWith_RunsFinalizeUnderGate pins that the follow-up step
// (the BadgerDB wipe in /reset) runs while the journaling gate is still held, so
// no journal write can be accepted between the archive and the wipe.
func TestMemberRepo_ResetAllWith_RunsFinalizeUnderGate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children")
			if id == "parent-1" {
				_, _ = w.Write([]byte(`{"results":[{"id":"page-a","type":"child_page"}],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"c-a","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 8500 | Username: @a"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/pages/"):
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1")
	repo.saveMemberToCache(context.Background(), &domain.Member{
		TelegramID: 8500, Username: "a", NotionPageID: "page-a", NotionDBID: "db-a",
	})
	tradeRepo := NewTradeRepo(client, repo, newMockTradeCacheStore())

	finalizeRan := false
	finalize := func() error {
		finalizeRan = true
		if _, err := repo.beginJournaling(); !errors.Is(err, ErrResetInProgress) {
			t.Errorf("journaling was allowed during finalize: err = %v", err)
		}
		if err := tradeRepo.SaveTrade(context.Background(), 8500, &domain.Trade{Symbol: "XAUUSD"}); !errors.Is(err, ErrResetInProgress) {
			t.Errorf("SaveTrade during finalize error = %v, want ErrResetInProgress", err)
		}
		return nil
	}

	count, err := repo.ResetAllWith(context.Background(), finalize)
	if err != nil {
		t.Fatalf("ResetAllWith failed: %v", err)
	}
	if count != 1 {
		t.Errorf("archived %d pages, want 1", count)
	}
	if !finalizeRan {
		t.Fatal("finalize was not called")
	}
}

// TestMemberRepo_ResetAllWith_SkipsFinalizeOnArchiveFailure pins that a partial
// archive never wipes the persistent store: the journals still live keep their
// mappings, and the owner is told to retry.
func TestMemberRepo_ResetAllWith_SkipsFinalizeOnArchiveFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children")
			if id == "parent-1" {
				_, _ = w.Write([]byte(`{"results":[{"id":"page-a","type":"child_page"}],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"c-a","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 8600 | Username: @a"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/pages/"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"object":"error","status":400,"message":"archive failed"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1")
	repo.saveMemberToCache(context.Background(), &domain.Member{
		TelegramID: 8600, Username: "a", NotionPageID: "page-a", NotionDBID: "db-a",
	})

	finalizeCalled := false
	count, err := repo.ResetAllWith(context.Background(), func() error {
		finalizeCalled = true
		return nil
	})
	if err == nil {
		t.Fatal("expected ResetAllWith to surface the failed archive, got nil")
	}
	if count != 0 {
		t.Errorf("archived %d pages, want 0", count)
	}
	if finalizeCalled {
		t.Error("finalize ran after a failed archive; the persistent store must be left intact")
	}
}

// TestMemberRepo_ResetAllWith_SurfacesFinalizeFailure pins that a failing
// follow-up step is reported so the owner does not see a success message after a
// half-completed reset.
func TestMemberRepo_ResetAllWith_SurfacesFinalizeFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children")
			if id == "parent-1" {
				_, _ = w.Write([]byte(`{"results":[{"id":"page-a","type":"child_page"}],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"c-a","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 8700 | Username: @a"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/pages/"):
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1")
	repo.saveMemberToCache(context.Background(), &domain.Member{
		TelegramID: 8700, Username: "a", NotionPageID: "page-a", NotionDBID: "db-a",
	})

	wantErr := errors.New("badger unavailable")
	count, err := repo.ResetAllWith(context.Background(), func() error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("ResetAllWith error = %v, want %v", err, wantErr)
	}
	if count != 1 {
		t.Errorf("archived %d pages, want 1", count)
	}
}

// TestMemberRepo_EnsureMember_ReconcilesUnusableCreateResponse pins that a 2xx
// create with an unusable body (no page id) is reconciled through the parent
// page instead of being reported as a clean failure: the journal exists, and a
// retry would duplicate it.
func TestMemberRepo_EnsureMember_ReconcilesUnusableCreateResponse(t *testing.T) {
	var (
		pageCreates atomic.Int32
		dbCreates   atomic.Int32
		pageExists  atomic.Bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/parent-1/children":
			if !pageExists.Load() {
				_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"page-committed","type":"child_page"}],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/page-committed/children":
			_, _ = w.Write([]byte(`{"results":[{"id":"callout-1","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 9970 | Username: @unusable"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			pageCreates.Add(1)
			pageExists.Store(true)
			// 2xx without an id: the page exists but cannot be addressed.
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/databases"):
			dbCreates.Add(1)
			_, _ = w.Write([]byte(`{"id":"db-adopted"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1")

	m, err := repo.EnsureMember(context.Background(), &domain.Member{
		TelegramID: 9970,
		Username:   "unusable",
	})
	if err != nil {
		t.Fatalf("EnsureMember failed: %v", err)
	}
	if m.NotionPageID != "page-committed" || m.NotionDBID != "db-adopted" {
		t.Fatalf("member = %+v, want the committed page adopted", m)
	}
	if got := pageCreates.Load(); got != 1 {
		t.Errorf("member-page create attempts = %d, want 1", got)
	}
	if got := dbCreates.Load(); got != 1 {
		t.Errorf("trade database creations = %d, want 1", got)
	}
}

// TestMemberRepo_EnsureMember_UnusableCreateResponseWithoutPageIsAmbiguous pins
// the fallback when reconciliation finds nothing: the error must still be
// flagged as ambiguous, because a journal may exist that the parent listing
// could not show yet.
func TestMemberRepo_EnsureMember_UnusableCreateResponseWithoutPageIsAmbiguous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/parent-1/children":
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pages"):
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	repo := NewMemberRepo(client, "parent-1")

	_, err := repo.EnsureMember(context.Background(), &domain.Member{
		TelegramID: 9971,
		Username:   "unusable-empty",
	})
	if err == nil {
		t.Fatal("expected EnsureMember to fail when no page can be found")
	}
	if !IsAmbiguousWriteError(err) {
		t.Errorf("error %v must be reported as ambiguous", err)
	}
}

// TestMemberRepo_ListMembers_WaitsForReset pins that the roster is never built
// from a half-reset state: while ResetAll is archiving, ListMembers blocks, and
// it must not re-add the archived member afterwards.
func TestMemberRepo_ListMembers_WaitsForReset(t *testing.T) {
	archiving := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children")
			if id == "parent-1" {
				_, _ = w.Write([]byte(`{"results":[{"id":"page-a","type":"child_page"}],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"c-a","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 8800 | Username: @a"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/pages/"):
			once.Do(func() { close(archiving) })
			<-release
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	persistStore := newMockMemberCacheStore()
	repo := NewMemberRepo(client, "parent-1", persistStore)
	repo.saveMemberToCache(context.Background(), &domain.Member{
		TelegramID: 8800, Username: "a", NotionPageID: "page-a", NotionDBID: "db-a",
	})

	resetDone := make(chan error, 1)
	go func() {
		_, err := repo.ResetAll(context.Background())
		resetDone <- err
	}()

	select {
	case <-archiving:
	case <-time.After(2 * time.Second):
		t.Fatal("ResetAll never reached the archival phase")
	}

	rosterDone := make(chan []domain.Member, 1)
	go func() {
		members, err := repo.ListMembers(context.Background())
		if err != nil {
			t.Errorf("ListMembers failed: %v", err)
		}
		rosterDone <- members
	}()

	select {
	case members := <-rosterDone:
		t.Fatalf("ListMembers returned %+v while a reset was archiving; want it to wait", members)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-resetDone:
		if err != nil {
			t.Fatalf("ResetAll failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ResetAll did not finish")
	}

	select {
	case members := <-rosterDone:
		for _, m := range members {
			if m.TelegramID == 8800 {
				t.Errorf("archived member leaked into the roster: %+v", m)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListMembers did not return after the reset finished")
	}
}

// TestMemberRepo_ResetAll_AmbiguousArchiveConfirmedArchived pins that an
// ambiguous archive response is reconciled by reading the page: when Notion did
// archive it, the reset counts it and forgets the cached mapping instead of
// leaving a stale entry pointing at a hidden journal.
func TestMemberRepo_ResetAll_AmbiguousArchiveConfirmedArchived(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children")
			if id == "parent-1" {
				_, _ = w.Write([]byte(`{"results":[{"id":"page-a","type":"child_page"}],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"c-a","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 8900 | Username: @a"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/pages/"):
			// The archive is applied, but the response is lost.
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"object":"error","status":500}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/pages/page-a":
			_, _ = w.Write([]byte(`{"id":"page-a","archived":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	client.retryBase = time.Millisecond
	repo := NewMemberRepo(client, "parent-1")
	repo.saveMemberToCache(context.Background(), &domain.Member{
		TelegramID: 8900, Username: "a", NotionPageID: "page-a", NotionDBID: "db-a",
	})

	count, err := repo.ResetAll(context.Background())
	if err != nil {
		t.Fatalf("ResetAll failed although the page was verified archived: %v", err)
	}
	if count != 1 {
		t.Errorf("archived %d pages, want 1", count)
	}
	repo.mu.RLock()
	_, cached := repo.cache[8900]
	repo.mu.RUnlock()
	if cached {
		t.Error("mapping for a verified-archived journal survived in the cache")
	}
}

// TestMemberRepo_ResetAll_AmbiguousArchiveUnverifiedForgetsMapping pins the
// conservative branch: when the archive outcome cannot be verified, the cached
// mapping is dropped anyway. A live page is rediscovered by parent-scoped
// lookup, while an archived one must never be handed out again.
func TestMemberRepo_ResetAll_AmbiguousArchiveUnverifiedForgetsMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/blocks/"), "/children")
			if id == "parent-1" {
				_, _ = w.Write([]byte(`{"results":[{"id":"page-a","type":"child_page"}],"has_more":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"results":[{"id":"c-a","type":"callout","callout":{"rich_text":[{"plain_text":"Telegram ID: 8950 | Username: @a"}]}}],"has_more":false}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_, _ = w.Write([]byte(`{"results":[],"has_more":false}`))
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/pages/"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"object":"error","status":500}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/pages/page-a":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"object":"error","status":400,"message":"read failed"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClient("ntn_test")
	client.baseURL = srv.URL + "/v1"
	client.retryBase = time.Millisecond
	repo := NewMemberRepo(client, "parent-1")
	repo.saveMemberToCache(context.Background(), &domain.Member{
		TelegramID: 8950, Username: "a", NotionPageID: "page-a", NotionDBID: "db-a",
	})

	count, err := repo.ResetAll(context.Background())
	if err == nil {
		t.Fatal("expected ResetAll to report the unverified archive, got nil")
	}
	if count != 0 {
		t.Errorf("archived %d pages, want 0 (outcome unverified)", count)
	}
	repo.mu.RLock()
	_, cached := repo.cache[8950]
	repo.mu.RUnlock()
	if cached {
		t.Error("mapping survived an unverified archive; it may point at a hidden journal")
	}
}
