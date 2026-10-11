package badger

import (
	"context"
	"encoding/json"
	"fmt"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// BadgeRepo implements ports.BadgeStore backed by BadgerDB.
type BadgeRepo struct {
	store *Store
}

// NewBadgeRepo creates a new BadgeRepo.
func NewBadgeRepo(store *Store) *BadgeRepo {
	return &BadgeRepo{store: store}
}

func badgeKey(telegramID int64, badgeID domain.BadgeID) string {
	return fmt.Sprintf("gam:badge:%d:%s", telegramID, badgeID)
}

func badgePrefix(telegramID int64) string {
	return fmt.Sprintf("gam:badge:%d:", telegramID)
}

// GetBadges returns all badge awards for a member.
func (r *BadgeRepo) GetBadges(_ context.Context, telegramID int64) ([]domain.BadgeAward, error) {
	raw, err := r.store.Scan(badgePrefix(telegramID))
	if err != nil {
		return nil, err
	}
	var awards []domain.BadgeAward
	for _, b := range raw {
		var a domain.BadgeAward
		if err := json.Unmarshal(b, &a); err != nil {
			return nil, err
		}
		awards = append(awards, a)
	}
	return awards, nil
}

// HasBadge checks if a member has already earned a specific badge.
func (r *BadgeRepo) HasBadge(_ context.Context, telegramID int64, badgeID domain.BadgeID) (bool, error) {
	var a domain.BadgeAward
	err := r.store.Get(badgeKey(telegramID, badgeID), &a)
	if err == badgerdb.ErrKeyNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// AwardBadge persists a badge award.
func (r *BadgeRepo) AwardBadge(_ context.Context, award *domain.BadgeAward) error {
	return r.store.Set(badgeKey(award.TelegramID, award.BadgeID), award)
}

// ClaimBadge awards a badge only if it has not been awarded yet, and reports
// whether this call performed the award.
//
// The existence check and the write happen inside a single Badger transaction.
// Doing HasBadge followed by AwardBadge as two separate calls is a TOCTOU race:
// two concurrent handlers (the bot runs one goroutine per Telegram update) can
// both observe "not awarded" and both award the badge and its XP.
func (r *BadgeRepo) ClaimBadge(_ context.Context, award *domain.BadgeAward) (bool, error) {
	data, err := json.Marshal(award)
	if err != nil {
		return false, err
	}
	claimed := false
	err = r.store.db.Update(func(txn *badgerdb.Txn) error {
		key := []byte(badgeKey(award.TelegramID, award.BadgeID))
		switch _, err := txn.Get(key); {
		case err == nil:
			return nil // already awarded by someone else
		case err != badgerdb.ErrKeyNotFound:
			return err
		}
		if err := txn.Set(key, data); err != nil {
			return err
		}
		claimed = true
		return nil
	})
	return claimed, err
}
