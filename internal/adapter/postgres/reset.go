package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Resetter wipes the journal and gamification state of every member.
//
// It replaces the two-step Notion archive plus Badger wipe with one transaction:
// either everything is gone or nothing is. That removes the window the old
// implementation needed a journaling lease to cover, because there is no second
// store that can be left behind.
//
// The Telegram delivery ledger is deliberately preserved: it is transport state,
// not user data, and wiping it would let a redelivered update re-run an action
// whose effects are gone.
type Resetter struct {
	db *DB
}

// NewResetter builds the reset repository.
func NewResetter(db *DB) *Resetter { return &Resetter{db: db} }

// ResetAll deletes trades, grants, XP events, badges, goals, reminders,
// analytics and report cards, and resets every profile and streak.
//
// Members themselves survive: they are identities, and deleting them would
// break web sessions and audit history. Their journal simply becomes empty.
// It returns the number of trades removed, which is what the owner sees.
func (r *Resetter) ResetAll(ctx context.Context) (int, error) {
	deletedTrades := 0
	err := pgx.BeginFunc(ctx, r.db.pool, func(tx pgx.Tx) error {
		// Order matters: children before the rows they reference, even though
		// ON DELETE CASCADE would handle it. Explicit order keeps the intent
		// readable and works the same if a constraint is ever relaxed.
		for _, stmt := range []string{
			`DELETE FROM trade_xp_grants`,
			`DELETE FROM xp_events`,
			`DELETE FROM badges`,
			`DELETE FROM goals`,
			`DELETE FROM reminder_prefs`,
			`DELETE FROM analytics_cache`,
			`DELETE FROM report_cards`,
			`DELETE FROM challenges`,
		} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("reset: %w", err)
			}
		}
		tag, err := tx.Exec(ctx, `DELETE FROM trades`)
		if err != nil {
			return fmt.Errorf("reset trades: %w", err)
		}
		deletedTrades = int(tag.RowsAffected())

		if _, err := tx.Exec(ctx, `
			UPDATE profiles SET total_xp = 0, level = 1, title = 'Retail',
			                    updated_at = now()`); err != nil {
			return fmt.Errorf("reset profiles: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE streaks SET current_streak = 0, longest_streak = 0,
			                    last_log_date = NULL, updated_at = now()`); err != nil {
			return fmt.Errorf("reset streaks: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO audit_log (member_id, action, detail)
			VALUES (NULL, 'reset_all', jsonb_build_object('trades', $1::int))`,
			deletedTrades); err != nil {
			return fmt.Errorf("reset audit: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deletedTrades, nil
}
