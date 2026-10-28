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

func profileKey(id int64) string { return fmt.Sprintf("gam:profile:%d", id) }
func streakKey(id int64) string  { return fmt.Sprintf("gam:streak:%d", id) }
func xpLogKey(id int64, ts int64) string {
	return fmt.Sprintf("gam:xp_log:%d:%d:%d", id, ts, rand.Int63n(1000000))
}
func xpLogPrefix(id int64) string { return fmt.Sprintf("gam:xp_log:%d:", id) }
func tradeXPGrantKey(id int64, tradeID string) string {
	return fmt.Sprintf("gam:trade_xp:%d:%s", id, tradeID)
}

// xpLogScanPrefix spans every member's XP log; the prune job uses it because
// event timestamps live in values, not in a globally sortable key position.
const xpLogScanPrefix = "gam:xp_log:"

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
// Returns nil (not an error) when no grant exists: the trade predates grant
// tracking, or its record commit failed and the compensating zero grant could
// not be written. Callers must treat that as "contribution unknown".
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

// CommitTradeRecord atomically persists every gamification write produced by
// recording one trade. All entries are marshalled up front, then applied in a
// single Badger write transaction, so a failure cannot leave the profile ahead
// of the grant it is later reconciled against or hide the XP event the daily
// cap counts. profile, streak, grant and event are each skipped when nil.
func (r *GamificationRepo) CommitTradeRecord(_ context.Context, profile *domain.GamificationProfile, streak *domain.StreakData, grant *domain.TradeXPGrant, event *domain.XPEvent) error {
	type entry struct {
		key   []byte
		value []byte
	}
	var entries []entry

	add := func(key string, value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		entries = append(entries, entry{key: []byte(key), value: data})
		return nil
	}

	if profile != nil {
		if err := add(profileKey(profile.TelegramID), profile); err != nil {
			return err
		}
	}
	if streak != nil {
		if err := add(streakKey(streak.TelegramID), streak); err != nil {
			return err
		}
	}
	if grant != nil && grant.TradeID != "" {
		if err := add(tradeXPGrantKey(grant.TelegramID, grant.TradeID), grant); err != nil {
			return err
		}
	}
	if event != nil {
		if err := add(xpLogKey(event.TelegramID, event.Timestamp.UnixNano()), event); err != nil {
			return err
		}
	}

	return r.store.db.Update(func(txn *badgerdb.Txn) error {
		for _, e := range entries {
			if err := txn.Set(e.key, e.value); err != nil {
				return err
			}
		}
		return nil
	})
}

// CommitTradeAdjustment atomically persists every gamification write produced
// by editing or deleting one trade. All values are marshalled up front, then
// applied in a single Badger write transaction, so a failure cannot leave the
// profile ahead of its XP event or a deleted trade's grant behind. profile and
// event are skipped when nil; grant is upserted when non-nil and deleteGrant
// removes the grant it identifies when non-nil. Deleting a grant that does not
// exist is not an error.
func (r *GamificationRepo) CommitTradeAdjustment(_ context.Context, profile *domain.GamificationProfile, event *domain.XPEvent, grant, deleteGrant *domain.TradeXPGrant) error {
	type entry struct {
		key   []byte
		value []byte
	}
	var sets []entry
	var deletes [][]byte

	add := func(key string, value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		sets = append(sets, entry{key: []byte(key), value: data})
		return nil
	}

	if profile != nil {
		if err := add(profileKey(profile.TelegramID), profile); err != nil {
			return err
		}
	}
	if event != nil {
		if err := add(xpLogKey(event.TelegramID, event.Timestamp.UnixNano()), event); err != nil {
			return err
		}
	}
	if grant != nil && grant.TradeID != "" {
		if err := add(tradeXPGrantKey(grant.TelegramID, grant.TradeID), grant); err != nil {
			return err
		}
	}
	if deleteGrant != nil && deleteGrant.TradeID != "" {
		deletes = append(deletes, []byte(tradeXPGrantKey(deleteGrant.TelegramID, deleteGrant.TradeID)))
	}
	if len(sets) == 0 && len(deletes) == 0 {
		return nil
	}

	return r.store.db.Update(func(txn *badgerdb.Txn) error {
		for _, e := range sets {
			if err := txn.Set(e.key, e.value); err != nil {
				return err
			}
		}
		for _, key := range deletes {
			if err := txn.Delete(key); err != nil && err != badgerdb.ErrKeyNotFound {
				return err
			}
		}
		return nil
	})
}

// PruneGamification deletes XP events older than the cutoff. XP events only
// feed the daily cap (today) and monthly report cards (current/previous
// month), so entries older than a couple of months have no consumer but do
// slow every GetXPEvents scan. Returns the number of deleted keys.
//
// Trade-XP grants are deliberately never pruned, however old they are: a grant
// records what its trade actually earned (possibly nothing under the daily
// cap) and any edit or delete of that trade can happen long after it was
// recorded. Pruning a grant would turn a known contribution into an unknown
// one, so the trade could never be reconciled again. A grant is removed only
// when its trade is deleted (DeleteTradeXPGrant).
func (r *GamificationRepo) PruneGamification(_ context.Context, cutoff time.Time) (int, error) {
	var victims [][]byte

	if err := r.store.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = []byte(xpLogScanPrefix)
		it := txn.NewIterator(opts)
		defer it.Close()
		pfx := []byte(xpLogScanPrefix)
		for it.Seek(pfx); it.ValidForPrefix(pfx); it.Next() {
			item := it.Item()
			prune := false
			if err := item.Value(func(val []byte) error {
				var ev domain.XPEvent
				if err := json.Unmarshal(val, &ev); err != nil {
					prune = true // malformed entry: prune it too
					return nil
				}
				prune = ev.Timestamp.IsZero() || ev.Timestamp.Before(cutoff)
				return nil
			}); err != nil {
				return err
			}
			if prune {
				victims = append(victims, item.KeyCopy(nil))
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}

	const batchSize = 500
	deleted := 0
	for i := 0; i < len(victims); i += batchSize {
		batch := victims[i:min(i+batchSize, len(victims))]
		if err := r.store.db.Update(func(txn *badgerdb.Txn) error {
			for _, key := range batch {
				if err := txn.Delete(key); err != nil && err != badgerdb.ErrKeyNotFound {
					return err
				}
			}
			return nil
		}); err != nil {
			return deleted, err
		}
		deleted += len(batch)
	}
	return deleted, nil
}
