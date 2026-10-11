package notion

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
			_, _ = w.Write([]byte(`{"results":[{"id":"page-existing","object":"page"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/blocks/page-existing/children":
			_, _ = w.Write([]byte(`{"results":[{"id":"callout-1","type":"callout"},{"id":"db-resolved-999","type":"child_database"}]}`))
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
