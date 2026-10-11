package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// MemberCacheStore persists member mappings across bot restarts.
type MemberCacheStore interface {
	GetCachedMember(ctx context.Context, telegramID int64) (*domain.Member, error)
	SaveCachedMember(ctx context.Context, member *domain.Member) error
	ListCachedMembers(ctx context.Context) ([]domain.Member, error)
}

// MemberRepo implements service.MemberRepository using Notion + optional persistent cache.
type MemberRepo struct {
	client   *Client
	parentID string // parent page ID for the Trade Journal workspace
	store    MemberCacheStore

	// In-memory cache: telegramID → Member (to avoid repeated API calls).
	mu    sync.RWMutex
	cache map[int64]*domain.Member
}

// NewMemberRepo creates a MemberRepo.
func NewMemberRepo(client *Client, parentID string, stores ...MemberCacheStore) *MemberRepo {
	var s MemberCacheStore
	if len(stores) > 0 {
		s = stores[0]
	}
	return &MemberRepo{
		client:   client,
		parentID: parentID,
		store:    s,
		cache:    make(map[int64]*domain.Member),
	}
}

// EnsureMember creates the member page + inline trade database if it doesn't exist.
func (r *MemberRepo) EnsureMember(ctx context.Context, m *domain.Member) (*domain.Member, error) {
	// Check in-memory cache first
	r.mu.RLock()
	if cached, ok := r.cache[m.TelegramID]; ok && cached.NotionDBID != "" {
		cp := *cached
		r.mu.RUnlock()
		return &cp, nil
	}
	r.mu.RUnlock()

	// Check persistent cache store
	if r.store != nil {
		if persisted, err := r.store.GetCachedMember(ctx, m.TelegramID); err == nil && persisted != nil {
			if persisted.NotionDBID == "" && persisted.NotionPageID != "" {
				persisted.NotionDBID = r.resolveTradeDBID(ctx, persisted.NotionPageID)
				_ = r.store.SaveCachedMember(ctx, persisted)
			}
			if persisted.NotionDBID != "" {
				cp := *persisted
				r.mu.Lock()
				r.cache[m.TelegramID] = &cp
				r.mu.Unlock()
				return persisted, nil
			}
		}
	}

	// Search Notion for existing page with matching Telegram ID
	existing, err := r.findMemberPage(ctx, m.TelegramID)
	if err == nil && existing != nil {
		if existing.Username == "" {
			existing.Username = m.Username
		}
		if existing.FirstName == "" {
			existing.FirstName = m.FirstName
		}
		r.saveMemberToCache(ctx, existing)
		return existing, nil
	}

	// Create member page
	title := fmt.Sprintf("@%s — Trade Journal", m.Username)
	if m.Username == "" {
		title = fmt.Sprintf("%s — Trade Journal", m.FirstName)
	}

	pagePayload := map[string]interface{}{
		"parent": map[string]interface{}{
			"page_id": r.parentID,
		},
		"properties": map[string]interface{}{
			"title": map[string]interface{}{
				"title": []map[string]interface{}{
					{"text": map[string]interface{}{"content": title}},
				},
			},
		},
		"children": []map[string]interface{}{
			{
				"object": "block",
				"type":   "callout",
				"callout": map[string]interface{}{
					"icon": map[string]interface{}{"emoji": "📊"},
					"rich_text": []map[string]interface{}{
						{"text": map[string]interface{}{"content": fmt.Sprintf("Telegram ID: %d | Username: @%s", m.TelegramID, m.Username)}},
					},
				},
			},
		},
	}

	raw, err := r.client.CreatePage(ctx, pagePayload)
	if err != nil {
		return nil, fmt.Errorf("create member page: %w", err)
	}

	var pageResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &pageResp); err != nil {
		return nil, fmt.Errorf("parse page response: %w", err)
	}
	m.NotionPageID = pageResp.ID

	// Create inline database for trades inside this member page
	dbID, err := r.createTradeDatabase(ctx, pageResp.ID)
	if err != nil {
		return nil, fmt.Errorf("create trade db: %w", err)
	}
	m.NotionDBID = dbID

	r.saveMemberToCache(ctx, m)

	return m, nil
}

func (r *MemberRepo) saveMemberToCache(ctx context.Context, m *domain.Member) {
	cp := *m
	r.mu.Lock()
	r.cache[m.TelegramID] = &cp
	r.mu.Unlock()
	if r.store != nil {
		_ = r.store.SaveCachedMember(ctx, &cp)
	}
}

// GetMember retrieves a member by Telegram ID.
func (r *MemberRepo) GetMember(ctx context.Context, telegramID int64) (*domain.Member, error) {
	r.mu.RLock()
	if cached, ok := r.cache[telegramID]; ok && cached.NotionDBID != "" {
		cp := *cached
		r.mu.RUnlock()
		return &cp, nil
	}
	r.mu.RUnlock()

	if r.store != nil {
		if persisted, err := r.store.GetCachedMember(ctx, telegramID); err == nil && persisted != nil {
			if persisted.NotionDBID == "" && persisted.NotionPageID != "" {
				persisted.NotionDBID = r.resolveTradeDBID(ctx, persisted.NotionPageID)
			}
			r.saveMemberToCache(ctx, persisted)
			return persisted, nil
		}
	}

	m, err := r.findMemberPage(ctx, telegramID)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("member %d not found", telegramID)
	}
	r.saveMemberToCache(ctx, m)
	return m, nil
}

// ListMembers returns all known members, warming from the persistent store if the in-memory cache is cold.
func (r *MemberRepo) ListMembers(ctx context.Context) ([]domain.Member, error) {
	r.mu.RLock()
	n := len(r.cache)
	r.mu.RUnlock()

	if n == 0 && r.store != nil {
		if persisted, err := r.store.ListCachedMembers(ctx); err == nil && len(persisted) > 0 {
			r.mu.Lock()
			for i := range persisted {
				cp := persisted[i]
				r.cache[cp.TelegramID] = &cp
			}
			r.mu.Unlock()
		}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	members := make([]domain.Member, 0, len(r.cache))
	for _, m := range r.cache {
		members = append(members, *m)
	}
	return members, nil
}

// findMemberPage searches for a member page by Telegram ID in the callout text and resolves its trade database ID.
func (r *MemberRepo) findMemberPage(ctx context.Context, telegramID int64) (*domain.Member, error) {
	query := fmt.Sprintf("Telegram ID: %d", telegramID)
	raw, err := r.client.SearchPages(ctx, query)
	if err != nil {
		return nil, err
	}

	var result struct {
		Results []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}

	if len(result.Results) == 0 {
		return nil, nil
	}

	pageID := result.Results[0].ID
	dbID := r.resolveTradeDBID(ctx, pageID)

	return &domain.Member{
		TelegramID:   telegramID,
		NotionPageID: pageID,
		NotionDBID:   dbID,
	}, nil
}

// resolveTradeDBID inspects child blocks of a member page to find its inline Trades database ID.
func (r *MemberRepo) resolveTradeDBID(ctx context.Context, pageID string) string {
	if pageID == "" {
		return ""
	}
	raw, err := r.client.GetBlockChildren(ctx, pageID)
	if err != nil {
		return ""
	}
	var children struct {
		Results []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &children); err != nil {
		return ""
	}
	for _, blk := range children.Results {
		if blk.Type == "child_database" && blk.ID != "" {
			return blk.ID
		}
	}
	return ""
}

// createTradeDatabase creates the inline Notion database for trades.
func (r *MemberRepo) createTradeDatabase(ctx context.Context, parentPageID string) (string, error) {
	dbPayload := map[string]interface{}{
		"parent": map[string]interface{}{
			"page_id": parentPageID,
		},
		"title": []map[string]interface{}{
			{"text": map[string]interface{}{"content": "Trades"}},
		},
		"is_inline": true,
		"properties": map[string]interface{}{
			"Symbol": map[string]interface{}{
				"title": map[string]interface{}{},
			},
			"Date": map[string]interface{}{
				"date": map[string]interface{}{},
			},
			"Asset Type": map[string]interface{}{
				"select": map[string]interface{}{
					"options": []map[string]interface{}{
						{"name": "Forex", "color": "blue"},
						{"name": "Gold", "color": "yellow"},
						{"name": "Indices", "color": "green"},
						{"name": "Crypto", "color": "purple"},
					},
				},
			},
			"Direction": map[string]interface{}{
				"select": map[string]interface{}{
					"options": []map[string]interface{}{
						{"name": "BUY", "color": "green"},
						{"name": "SELL", "color": "red"},
					},
				},
			},
			"Status": map[string]interface{}{
				"select": map[string]interface{}{
					"options": []map[string]interface{}{
						{"name": "OPEN", "color": "default"},
						{"name": "WIN", "color": "green"},
						{"name": "LOSS", "color": "red"},
						{"name": "BE", "color": "gray"},
					},
				},
			},
			"Result RR": map[string]interface{}{
				"number": map[string]interface{}{"format": "number"},
			},
			"Time Window": map[string]interface{}{
				"select": map[string]interface{}{
					"options": []map[string]interface{}{
						{"name": "Asia", "color": "blue"},
						{"name": "London", "color": "green"},
						{"name": "NY AM", "color": "yellow"},
						{"name": "NY PM", "color": "orange"},
					},
				},
			},
			"Confluence": map[string]interface{}{
				"rich_text": map[string]interface{}{},
			},
			"Notes": map[string]interface{}{
				"rich_text": map[string]interface{}{},
			},
		},
	}

	raw, err := r.client.CreateDatabase(ctx, dbPayload)
	if err != nil {
		return "", err
	}

	var dbResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &dbResp); err != nil {
		return "", err
	}
	return dbResp.ID, nil
}

func normalizeNotionID(id string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(id), "-", ""))
}

// ResetAll archives all member journal pages under the configured parent page and clears the cache.
func (r *MemberRepo) ResetAll(ctx context.Context) (int, error) {
	toArchive := make(map[string]bool)
	normParent := normalizeNotionID(r.parentID)

	// 1. Direct children of parent page
	if r.parentID != "" {
		if raw, err := r.client.GetBlockChildren(ctx, r.parentID); err == nil {
			var children struct {
				Results []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Archived bool   `json:"archived"`
				} `json:"results"`
			}
			if json.Unmarshal(raw, &children) == nil {
				for _, blk := range children.Results {
					if !blk.Archived && (blk.Type == "child_page" || blk.Type == "child_database") && blk.ID != "" {
						toArchive[blk.ID] = true
					}
				}
			}
		}
	}

	// 2. Search pages matching "Trade Journal" under parent page
	if raw, err := r.client.SearchPages(ctx, "Trade Journal"); err == nil {
		var searchResp struct {
			Results []struct {
				ID       string `json:"id"`
				Archived bool   `json:"archived"`
				Parent   struct {
					Type   string `json:"type"`
					PageID string `json:"page_id"`
				} `json:"parent"`
			} `json:"results"`
		}
		if json.Unmarshal(raw, &searchResp) == nil {
			for _, p := range searchResp.Results {
				if p.Archived || p.ID == "" {
					continue
				}
				if normParent == "" || normalizeNotionID(p.Parent.PageID) == normParent {
					toArchive[p.ID] = true
				}
			}
		}
	}

	// 3. Also include any cached member page IDs
	r.mu.RLock()
	for _, m := range r.cache {
		if m != nil && m.NotionPageID != "" {
			toArchive[m.NotionPageID] = true
		}
	}
	r.mu.RUnlock()

	archivedCount := 0
	var firstErr error
	for pageID := range toArchive {
		_, err := r.client.UpdatePage(ctx, pageID, map[string]interface{}{"archived": true})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		archivedCount++
	}

	r.mu.Lock()
	r.cache = make(map[int64]*domain.Member)
	r.mu.Unlock()

	return archivedCount, firstErr
}
