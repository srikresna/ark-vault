package badger

import (
	"context"
	"testing"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// TestMemberCacheRepo_DeleteCachedMember pins per-member eviction: /reset
// removes the mapping of every journal it archives, and must leave the other
// members' mappings — and therefore their live journals — untouched.
func TestMemberCacheRepo_DeleteCachedMember(t *testing.T) {
	store := newTestStore(t)
	repo := NewMemberCacheRepo(store)
	ctx := context.Background()

	for _, m := range []*domain.Member{
		{TelegramID: 11, Username: "archived", NotionPageID: "page-11", NotionDBID: "db-11"},
		{TelegramID: 22, Username: "live", NotionPageID: "page-22", NotionDBID: "db-22"},
	} {
		if err := repo.SaveCachedMember(ctx, m); err != nil {
			t.Fatalf("SaveCachedMember(%d) failed: %v", m.TelegramID, err)
		}
	}

	if err := repo.DeleteCachedMember(ctx, 11); err != nil {
		t.Fatalf("DeleteCachedMember failed: %v", err)
	}

	evicted, err := repo.GetCachedMember(ctx, 11)
	if err != nil {
		t.Fatalf("GetCachedMember(11) failed: %v", err)
	}
	if evicted != nil {
		t.Fatalf("member 11 = %+v, want it evicted", evicted)
	}

	kept, err := repo.GetCachedMember(ctx, 22)
	if err != nil {
		t.Fatalf("GetCachedMember(22) failed: %v", err)
	}
	if kept == nil || kept.NotionPageID != "page-22" || kept.NotionDBID != "db-22" {
		t.Fatalf("member 22 = %+v, want the untouched mapping", kept)
	}

	members, err := repo.ListCachedMembers(ctx)
	if err != nil {
		t.Fatalf("ListCachedMembers failed: %v", err)
	}
	if len(members) != 1 || members[0].TelegramID != 22 {
		t.Fatalf("ListCachedMembers = %+v, want only member 22", members)
	}

	// Reset may revisit a member it already evicted; a missing key is a no-op.
	if err := repo.DeleteCachedMember(ctx, 11); err != nil {
		t.Fatalf("repeated DeleteCachedMember failed: %v", err)
	}
}
