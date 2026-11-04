package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// BadgeRepo stores badge awards. ClaimBadge is a single INSERT ... ON CONFLICT
// DO NOTHING RETURNING, so the check-then-award race the service documents is
// resolved by the database instead of by a mutex.
type BadgeRepo struct {
	db      *DB
	members *MemberRepo
}

// NewBadgeRepo builds the badge repository.
func NewBadgeRepo(db *DB, members *MemberRepo) *BadgeRepo {
	return &BadgeRepo{db: db, members: members}
}

// GetBadges returns every badge a member has earned, oldest first.
func (r *BadgeRepo) GetBadges(ctx context.Context, telegramID int64) ([]domain.BadgeAward, error) {
	rows, err := r.db.pool.Query(ctx, `
		SELECT m.telegram_id, b.badge_id, b.awarded_at
		  FROM badges b
		  JOIN members m ON m.id = b.member_id
		 WHERE m.telegram_id = $1
		 ORDER BY b.awarded_at, b.badge_id`, telegramID)
	if err != nil {
		return nil, fmt.Errorf("get badges %d: %w", telegramID, err)
	}
	defer rows.Close()

	var awards []domain.BadgeAward
	for rows.Next() {
		var a domain.BadgeAward
		if err := rows.Scan(&a.TelegramID, &a.BadgeID, &a.AwardedAt); err != nil {
			return nil, fmt.Errorf("get badges %d: %w", telegramID, err)
		}
		awards = append(awards, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get badges %d: %w", telegramID, err)
	}
	return awards, nil
}

// HasBadge reports whether a member already earned a badge.
func (r *BadgeRepo) HasBadge(ctx context.Context, telegramID int64, badgeID domain.BadgeID) (bool, error) {
	var exists bool
	err := r.db.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM badges b
			  JOIN members m ON m.id = b.member_id
			 WHERE m.telegram_id = $1 AND b.badge_id = $2)`,
		telegramID, string(badgeID)).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("has badge %d/%s: %w", telegramID, badgeID, err)
	}
	return exists, nil
}

// AwardBadge grants a badge. Re-awarding an existing badge is a no-op, not an
// error: the award is a fact, and the caller only cares that it now holds.
func (r *BadgeRepo) AwardBadge(ctx context.Context, award *domain.BadgeAward) error {
	if award == nil {
		return errors.New("award badge: award is required")
	}
	mid, err := r.members.memberID(ctx, award.TelegramID)
	if err != nil {
		return err
	}
	at := award.AwardedAt
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := r.db.pool.Exec(ctx, `
		INSERT INTO badges (member_id, badge_id, awarded_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (member_id, badge_id) DO NOTHING`,
		mid, string(award.BadgeID), at); err != nil {
		return fmt.Errorf("award badge %d/%s: %w", award.TelegramID, award.BadgeID, err)
	}
	return nil
}

// ClaimBadge awards a badge only if the member does not have it yet, reporting
// whether this call won the claim. Two concurrent calls can never both win.
func (r *BadgeRepo) ClaimBadge(ctx context.Context, award *domain.BadgeAward) (bool, error) {
	if award == nil {
		return false, errors.New("claim badge: award is required")
	}
	mid, err := r.members.memberID(ctx, award.TelegramID)
	if err != nil {
		return false, err
	}
	at := award.AwardedAt
	if at.IsZero() {
		at = time.Now()
	}
	var claimed bool
	err = r.db.pool.QueryRow(ctx, `
		INSERT INTO badges (member_id, badge_id, awarded_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (member_id, badge_id) DO NOTHING
		RETURNING true`,
		mid, string(award.BadgeID), at).Scan(&claimed)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim badge %d/%s: %w", award.TelegramID, award.BadgeID, err)
	}
	return claimed, nil
}
