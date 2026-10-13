package badger

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// GamificationRepo implements ports.GamificationStore backed by BadgerDB.
type GamificationRepo struct {
	store *Store
}

// NewGamificationRepo creates a new GamificationRepo.
func NewGamificationRepo(store *Store) *GamificationRepo {
	return &GamificationRepo{store: store}
}

func profileKey(id int64) string   { return fmt.Sprintf("gam:profile:%d", id) }
func streakKey(id int64) string    { return fmt.Sprintf("gam:streak:%d", id) }
func xpLogKey(id int64, ts int64) string {
	return fmt.Sprintf("gam:xp_log:%d:%d:%d", id, ts, rand.Int63n(1000000))
}
func xpLogPrefix(id int64) string  { return fmt.Sprintf("gam:xp_log:%d:", id) }
func tradeXPGrantKey(id int64, tradeID string) string {
	return fmt.Sprintf("gam:trade_xp:%d:%s", id, tradeID)
}

// xpLogScanPrefix spans every member's XP log; the prune job uses it because
// event timestamps live in values, not in a globally sortable key position.
const xpLogScanPrefix = "gam:xp_log:"
const tradeXPGrantScanPrefix = "gam:trade_xp:"

// GetProfile returns the gamification profile for the given Telegram ID.
// Returns nil (not an error) when the profile does not exist yet.
func (r *GamificationRepo) GetProfile(_ context.Context, telegramID int64) (*domain.GamificationProfile, error) {
	var p domain.GamificationProfile
	err := r.store.Get(profileKey(telegramID), &p)
	if err == badgerdb.ErrKeyNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SaveProfile persists the gamification profile.
func (r *GamificationRepo) SaveProfile(_ context.Context, profile *domain.GamificationProfile) error {
	return r.store.Set(profileKey(profile.TelegramID), profile)
}

// GetStreak returns the streak data for the given Telegram ID.
// Returns nil (not an error) when no streak data exists yet.
func (r *GamificationRepo) GetStreak(_ context.Context, telegramID int64) (*domain.StreakData, error) {
	var s domain.StreakData
	err := r.store.Get(streakKey(telegramID), &s)
	if err == badgerdb.ErrKeyNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// SaveStreak persists the streak data.
func (r *GamificationRepo) SaveStreak(_ context.Context, streak *domain.StreakData) error {
	return r.store.Set(streakKey(streak.TelegramID), streak)
}

// AppendXPEvent records a single XP event keyed by member ID and unix timestamp.
func (r *GamificationRepo) AppendXPEvent(_ context.Context, event *domain.XPEvent) error {
	key := xpLogKey(event.TelegramID, event.Timestamp.UnixNano())
	return r.store.Set(key, event)
}

// GetXPEvents returns all XP events for the member since the given time.
func (r *GamificationRepo) GetXPEvents(_ context.Context, telegramID int64, since time.Time) ([]domain.XPEvent, error) {
	raw, err := r.store.Scan(xpLogPrefix(telegramID))
	if err != nil {
		return nil, err
	}
	var events []domain.XPEvent
	for _, b := range raw {
		var ev domain.XPEvent
		if err := json.Unmarshal(b, &ev); err != nil {
			return nil, err
		}
		if !ev.Timestamp.Before(since) {
			events = append(events, ev)
		}
	}
	return events, nil
}

// GetTradeXPGrant returns the trade-intrinsic XP granted for a trade.
// Returns nil (not an error) when no grant exists (legacy trades).
func (r *GamificationRepo) GetTradeXPGrant(_ context.Context, telegramID int64, tradeID string) (*domain.TradeXPGrant, error) {
	if tradeID == "" {
		return nil, nil
	}
	var g domain.TradeXPGrant
	err := r.store.Get(tradeXPGrantKey(telegramID, tradeID), &g)
	if err == badgerdb.ErrKeyNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// SaveTradeXPGrant persists the trade-intrinsic XP granted for a trade.
func (r *GamificationRepo) SaveTradeXPGrant(_ context.Context, grant *domain.TradeXPGrant) error {
	return r.store.Set(tradeXPGrantKey(grant.TelegramID, grant.TradeID), grant)
}

// DeleteTradeXPGrant removes the grant for a deleted trade.
func (r *GamificationRepo) DeleteTradeXPGrant(_ context.Context, telegramID int64, tradeID string) error {
	if tradeID == "" {
		return nil
	}
	return r.store.Delete(tradeXPGrantKey(telegramID, tradeID))
}

// PruneGamification deletes XP events and stale trade-XP grants older than the
// cutoff. XP events only feed the daily cap (today) and monthly report cards
// (current/previous month), so entries older than a couple of months have no
// consumer but do slow every GetXPEvents scan. Returns the number of deleted
// keys.
func (r *GamificationRepo) PruneGamification(_ context.Context, cutoff time.Time) (int, error) {
	type victim struct {
		key  []byte
		when time.Time
	}
	var victims []victim

	collect := func(prefix string, tsOf func([]byte) (time.Time, bool)) error {
		return r.store.db.View(func(txn *badgerdb.Txn) error {
			opts := badgerdb.DefaultIteratorOptions
			opts.Prefix = []byte(prefix)
			it := txn.NewIterator(opts)
			defer it.Close()
			pfx := []byte(prefix)
			for it.Seek(pfx); it.ValidForPrefix(pfx); it.Next() {
				item := it.Item()
				var when time.Time
				if err := item.Value(func(val []byte) error {
					ts, ok := tsOf(val)
					if ok {
						when = ts
					}
					return nil
				}); err != nil {
					continue // malformed entry: prune it too
				}
				if when.IsZero() || when.Before(cutoff) {
					victims = append(victims, victim{key: item.KeyCopy(nil), when: when})
				}
			}
			return nil
		})
	}

	xpTs := func(val []byte) (time.Time, bool) {
		var ev domain.XPEvent
		if err := json.Unmarshal(val, &ev); err != nil {
			return time.Time{}, false
		}
		return ev.Timestamp, true
	}
	grantTs := func(val []byte) (time.Time, bool) {
		var g domain.TradeXPGrant
		if err := json.Unmarshal(val, &g); err != nil {
			return time.Time{}, false
		}
		return g.GrantedAt, true
	}

	if err := collect(xpLogScanPrefix, xpTs); err != nil {
		return 0, err
	}
	if err := collect(tradeXPGrantScanPrefix, grantTs); err != nil {
		return 0, err
	}

	const batchSize = 500
	deleted := 0
	for i := 0; i < len(victims); i += batchSize {
		end := i + batchSize
		if end > len(victims) {
			end = len(victims)
		}
		if err := r.store.db.Update(func(txn *badgerdb.Txn) error {
			for _, v := range victims[i:end] {
				if err := txn.Delete(v.key); err != nil && err != badgerdb.ErrKeyNotFound {
					return err
				}
				deleted++
			}
			return nil
		}); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}
