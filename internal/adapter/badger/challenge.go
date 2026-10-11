package badger

import (
	"context"
	"fmt"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// ChallengeRepo implements ports.ChallengeStore backed by BadgerDB.
type ChallengeRepo struct {
	store *Store
}

// NewChallengeRepo creates a new ChallengeRepo.
func NewChallengeRepo(store *Store) *ChallengeRepo {
	return &ChallengeRepo{store: store}
}

func challengeKey(yearWeek string) string {
	return fmt.Sprintf("gam:challenge:%s", yearWeek)
}

// GetChallenge returns the weekly challenge for the given year-week.
// Returns nil (not an error) when no challenge exists.
func (r *ChallengeRepo) GetChallenge(_ context.Context, yearWeek string) (*domain.WeeklyChallenge, error) {
	var c domain.WeeklyChallenge
	err := r.store.Get(challengeKey(yearWeek), &c)
	if err == badgerdb.ErrKeyNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// SaveChallenge persists a weekly challenge.
func (r *ChallengeRepo) SaveChallenge(_ context.Context, challenge *domain.WeeklyChallenge) error {
	return r.store.Set(challengeKey(challenge.YearWeek), challenge)
}
