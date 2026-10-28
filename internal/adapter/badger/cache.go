package badger

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/arkcode369/ark-vault/internal/domain"
)

const (
	memberCachePrefix = "gam:member:"
	tradeCacheTTL     = 10 * time.Minute
)

func memberCacheKey(telegramID int64) string {
	return fmt.Sprintf("gam:member:%d", telegramID)
}

func tradeCacheKey(telegramID int64) string {
	return fmt.Sprintf("gam:trades_cache:%d", telegramID)
}

// MemberCacheRepo persists discovered Notion member mappings in BadgerDB.
type MemberCacheRepo struct {
	store *Store
}

// NewMemberCacheRepo creates a new MemberCacheRepo.
func NewMemberCacheRepo(store *Store) *MemberCacheRepo {
	return &MemberCacheRepo{store: store}
}

// GetCachedMember retrieves a persisted member by Telegram ID.
func (r *MemberCacheRepo) GetCachedMember(_ context.Context, telegramID int64) (*domain.Member, error) {
	var m domain.Member
	err := r.store.Get(memberCacheKey(telegramID), &m)
	if err == badgerdb.ErrKeyNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// SaveCachedMember persists a member record in BadgerDB.
func (r *MemberCacheRepo) SaveCachedMember(_ context.Context, member *domain.Member) error {
	return r.store.Set(memberCacheKey(member.TelegramID), member)
}

// DeleteCachedMember removes a single persisted member mapping.
//
// Missing keys are not an error: Reset deletes the mappings of the journals it
// archived, and a member discovered from Notion rather than from the cache has
// no mapping to begin with.
func (r *MemberCacheRepo) DeleteCachedMember(_ context.Context, telegramID int64) error {
	return r.store.Delete(memberCacheKey(telegramID))
}

// ListCachedMembers returns all persisted members from BadgerDB.
func (r *MemberCacheRepo) ListCachedMembers(_ context.Context) ([]domain.Member, error) {
	raw, err := r.store.Scan(memberCachePrefix)
	if err != nil {
		return nil, err
	}
	members := make([]domain.Member, 0, len(raw))
	for _, b := range raw {
		var m domain.Member
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, nil
}

// TradeCacheRepo caches a member's trade list in BadgerDB with a short TTL.
type TradeCacheRepo struct {
	store *Store
}

// NewTradeCacheRepo creates a new TradeCacheRepo.
func NewTradeCacheRepo(store *Store) *TradeCacheRepo {
	return &TradeCacheRepo{store: store}
}

// GetCachedTrades returns cached trades for a member if present and not expired.
func (r *TradeCacheRepo) GetCachedTrades(_ context.Context, memberID int64) ([]domain.Trade, bool, error) {
	var trades []domain.Trade
	err := r.store.Get(tradeCacheKey(memberID), &trades)
	if err == badgerdb.ErrKeyNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return trades, true, nil
}

// SaveCachedTrades stores a member's trades in BadgerDB with a 10-minute TTL.
func (r *TradeCacheRepo) SaveCachedTrades(_ context.Context, memberID int64, trades []domain.Trade) error {
	return r.store.SetWithTTL(tradeCacheKey(memberID), trades, tradeCacheTTL)
}

// InvalidateCachedTrades removes the cached trades for a member when a new trade is saved.
func (r *TradeCacheRepo) InvalidateCachedTrades(_ context.Context, memberID int64) error {
	return r.store.Delete(tradeCacheKey(memberID))
}
