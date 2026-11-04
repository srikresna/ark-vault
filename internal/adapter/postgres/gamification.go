package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// GamificationRepo is the gamification store. Every multi-row write the service
// layer used to spread across several Badger keys is one SQL transaction here,
// so the atomicity the services document is enforced by the database instead of
// by adapter code.
type GamificationRepo struct {
	db      *DB
	members *MemberRepo
}

// NewGamificationRepo builds the gamification repository.
func NewGamificationRepo(db *DB, members *MemberRepo) *GamificationRepo {
	return &GamificationRepo{db: db, members: members}
}

// GetProfile returns the profile for a member, or nil when none exists yet.
func (r *GamificationRepo) GetProfile(ctx context.Context, telegramID int64) (*domain.GamificationProfile, error) {
	var p domain.GamificationProfile
	err := r.db.pool.QueryRow(ctx, `
		SELECT m.telegram_id, p.total_xp, p.level, p.title, p.updated_at
		  FROM profiles p
		  JOIN members m ON m.id = p.member_id
		 WHERE m.telegram_id = $1`, telegramID,
	).Scan(&p.TelegramID, &p.TotalXP, &p.Level, &p.Title, &p.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get profile %d: %w", telegramID, err)
	}
	return &p, nil
}

// SaveProfile upserts the profile.
func (r *GamificationRepo) SaveProfile(ctx context.Context, profile *domain.GamificationProfile) error {
	if profile == nil {
		return errors.New("save profile: profile is required")
	}
	mid, err := r.members.memberID(ctx, profile.TelegramID)
	if err != nil {
		return err
	}
	if _, err := r.db.pool.Exec(ctx, `
		INSERT INTO profiles (member_id, total_xp, level, title, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (member_id) DO UPDATE
		   SET total_xp = EXCLUDED.total_xp, level = EXCLUDED.level,
		       title = EXCLUDED.title, updated_at = EXCLUDED.updated_at`,
		mid, profile.TotalXP, profile.Level, profile.Title, profile.UpdatedAt); err != nil {
		return fmt.Errorf("save profile %d: %w", profile.TelegramID, err)
	}
	return nil
}

// GetStreak returns the streak for a member, or nil when none exists yet.
func (r *GamificationRepo) GetStreak(ctx context.Context, telegramID int64) (*domain.StreakData, error) {
	var s domain.StreakData
	var last *time.Time
	err := r.db.pool.QueryRow(ctx, `
		SELECT m.telegram_id, s.current_streak, s.longest_streak, s.last_log_date,
		       s.updated_at
		  FROM streaks s
		  JOIN members m ON m.id = s.member_id
		 WHERE m.telegram_id = $1`, telegramID,
	).Scan(&s.TelegramID, &s.CurrentStreak, &s.LongestStreak, &last, &s.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get streak %d: %w", telegramID, err)
	}
	if last != nil {
		s.LastLogDate = last.Format("2006-01-02")
	}
	return &s, nil
}

// SaveStreak upserts the streak.
func (r *GamificationRepo) SaveStreak(ctx context.Context, streak *domain.StreakData) error {
	if streak == nil {
		return errors.New("save streak: streak is required")
	}
	mid, err := r.members.memberID(ctx, streak.TelegramID)
	if err != nil {
		return err
	}
	var last any
	if streak.LastLogDate != "" {
		d, err := time.Parse("2006-01-02", streak.LastLogDate)
		if err != nil {
			return fmt.Errorf("save streak %d: invalid last log date %q: %w",
				streak.TelegramID, streak.LastLogDate, err)
		}
		last = d
	}
	if _, err := r.db.pool.Exec(ctx, `
		INSERT INTO streaks (member_id, current_streak, longest_streak,
		                     last_log_date, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (member_id) DO UPDATE
		   SET current_streak = EXCLUDED.current_streak,
		       longest_streak = EXCLUDED.longest_streak,
		       last_log_date = EXCLUDED.last_log_date,
		       updated_at = EXCLUDED.updated_at`,
		mid, streak.CurrentStreak, streak.LongestStreak, last, streak.UpdatedAt); err != nil {
		return fmt.Errorf("save streak %d: %w", streak.TelegramID, err)
	}
	return nil
}

// AppendXPEvent records a single XP event.
func (r *GamificationRepo) AppendXPEvent(ctx context.Context, event *domain.XPEvent) error {
	if event == nil {
		return errors.New("append xp event: event is required")
	}
	mid, err := r.members.memberID(ctx, event.TelegramID)
	if err != nil {
		return err
	}
	if _, err := r.db.pool.Exec(ctx, `
		INSERT INTO xp_events (member_id, amount, reason, created_at)
		VALUES ($1, $2, $3, $4)`,
		mid, event.Amount, event.Reason, event.Timestamp); err != nil {
		return fmt.Errorf("append xp event for %d: %w", event.TelegramID, err)
	}
	return nil
}

// GetXPEvents returns every XP event at or after since, oldest first.
func (r *GamificationRepo) GetXPEvents(ctx context.Context, telegramID int64, since time.Time) ([]domain.XPEvent, error) {
	rows, err := r.db.pool.Query(ctx, `
		SELECT m.telegram_id, e.amount, e.reason, e.created_at
		  FROM xp_events e
		  JOIN members m ON m.id = e.member_id
		 WHERE m.telegram_id = $1 AND e.created_at >= $2
		 ORDER BY e.created_at, e.id`, telegramID, since)
	if err != nil {
		return nil, fmt.Errorf("get xp events %d: %w", telegramID, err)
	}
	defer rows.Close()

	var events []domain.XPEvent
	for rows.Next() {
		var e domain.XPEvent
		if err := rows.Scan(&e.TelegramID, &e.Amount, &e.Reason, &e.Timestamp); err != nil {
			return nil, fmt.Errorf("get xp events %d: %w", telegramID, err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get xp events %d: %w", telegramID, err)
	}
	return events, nil
}

// GetTradeXPGrant returns the grant for a trade, or nil when there is none.
//
// Absence is meaningful: it means the trade's contribution is unknown, which is
// why the service must not invent a nominal base for it.
func (r *GamificationRepo) GetTradeXPGrant(ctx context.Context, telegramID int64, tradeID string) (*domain.TradeXPGrant, error) {
	if tradeID == "" {
		return nil, nil
	}
	var g domain.TradeXPGrant
	err := r.db.pool.QueryRow(ctx, `
		SELECT m.telegram_id, g.trade_id::text, g.base, g.screenshot, g.granted_at
		  FROM trade_xp_grants g
		  JOIN members m ON m.id = g.member_id
		 WHERE g.trade_id = $1 AND m.telegram_id = $2`, tradeID, telegramID,
	).Scan(&g.TelegramID, &g.TradeID, &g.Base, &g.Screenshot, &g.GrantedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get trade xp grant %s: %w", tradeID, err)
	}
	return &g, nil
}

// SaveTradeXPGrant upserts a grant.
func (r *GamificationRepo) SaveTradeXPGrant(ctx context.Context, grant *domain.TradeXPGrant) error {
	if grant == nil || grant.TradeID == "" {
		return errors.New("save trade xp grant: trade id is required")
	}
	mid, err := r.members.memberID(ctx, grant.TelegramID)
	if err != nil {
		return err
	}
	if _, err := r.db.pool.Exec(ctx, `
		INSERT INTO trade_xp_grants (trade_id, member_id, base, screenshot, granted_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (trade_id) DO UPDATE
		   SET base = EXCLUDED.base, screenshot = EXCLUDED.screenshot,
		       granted_at = EXCLUDED.granted_at`,
		grant.TradeID, mid, grant.Base, grant.Screenshot, grant.GrantedAt); err != nil {
		return fmt.Errorf("save trade xp grant %s: %w", grant.TradeID, err)
	}
	return nil
}

// DeleteTradeXPGrant removes a grant. Deleting a missing grant is not an error.
func (r *GamificationRepo) DeleteTradeXPGrant(ctx context.Context, telegramID int64, tradeID string) error {
	if tradeID == "" {
		return nil
	}
	if _, err := r.db.pool.Exec(ctx, `
		DELETE FROM trade_xp_grants g
		 USING members m
		 WHERE g.trade_id = $1 AND m.telegram_id = $2 AND g.member_id = m.id`,
		tradeID, telegramID); err != nil {
		return fmt.Errorf("delete trade xp grant %s: %w", tradeID, err)
	}
	return nil
}

// CommitTradeRecord writes the profile, streak, grant and XP event as one
// transaction. Any nil argument is skipped; the rest still commit together.
func (r *GamificationRepo) CommitTradeRecord(ctx context.Context, profile *domain.GamificationProfile, streak *domain.StreakData, grant *domain.TradeXPGrant, event *domain.XPEvent) error {
	return r.commit(ctx, profile, streak, grant, nil, event, nil)
}

// CommitTradeAdjustment writes the profile, XP event and one grant mutation as
// one transaction. grant is upserted when non-nil; deleteGrant removes the grant
// it names when non-nil. The two are mutually exclusive.
func (r *GamificationRepo) CommitTradeAdjustment(ctx context.Context, profile *domain.GamificationProfile, event *domain.XPEvent, grant, deleteGrant *domain.TradeXPGrant) error {
	if grant != nil && deleteGrant != nil {
		return errors.New("commit trade adjustment: grant and delete grant are mutually exclusive")
	}
	return r.commit(ctx, profile, nil, grant, deleteGrant, event, nil)
}

// commit is the shared transactional body. streak is only used by
// CommitTradeRecord; deleteGrant only by CommitTradeAdjustment.
func (r *GamificationRepo) commit(ctx context.Context, profile *domain.GamificationProfile, streak *domain.StreakData, grant, deleteGrant *domain.TradeXPGrant, event *domain.XPEvent, extra func(ctx context.Context, tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, r.db.pool, func(tx pgx.Tx) error {
		if profile != nil {
			mid, err := memberIDTx(ctx, tx, profile.TelegramID)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO profiles (member_id, total_xp, level, title, updated_at)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (member_id) DO UPDATE
				   SET total_xp = EXCLUDED.total_xp, level = EXCLUDED.level,
				       title = EXCLUDED.title, updated_at = EXCLUDED.updated_at`,
				mid, profile.TotalXP, profile.Level, profile.Title, profile.UpdatedAt); err != nil {
				return fmt.Errorf("commit profile %d: %w", profile.TelegramID, err)
			}
		}
		if streak != nil {
			mid, err := memberIDTx(ctx, tx, streak.TelegramID)
			if err != nil {
				return err
			}
			var last any
			if streak.LastLogDate != "" {
				d, err := time.Parse("2006-01-02", streak.LastLogDate)
				if err != nil {
					return fmt.Errorf("commit streak %d: invalid last log date: %w", streak.TelegramID, err)
				}
				last = d
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO streaks (member_id, current_streak, longest_streak,
				                     last_log_date, updated_at)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (member_id) DO UPDATE
				   SET current_streak = EXCLUDED.current_streak,
				       longest_streak = EXCLUDED.longest_streak,
				       last_log_date = EXCLUDED.last_log_date,
				       updated_at = EXCLUDED.updated_at`,
				mid, streak.CurrentStreak, streak.LongestStreak, last, streak.UpdatedAt); err != nil {
				return fmt.Errorf("commit streak %d: %w", streak.TelegramID, err)
			}
		}
		if grant != nil && grant.TradeID != "" {
			mid, err := memberIDTx(ctx, tx, grant.TelegramID)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO trade_xp_grants (trade_id, member_id, base, screenshot, granted_at)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (trade_id) DO UPDATE
				   SET base = EXCLUDED.base, screenshot = EXCLUDED.screenshot,
				       granted_at = EXCLUDED.granted_at`,
				grant.TradeID, mid, grant.Base, grant.Screenshot, grant.GrantedAt); err != nil {
				return fmt.Errorf("commit trade xp grant %s: %w", grant.TradeID, err)
			}
		}
		if deleteGrant != nil && deleteGrant.TradeID != "" {
			if _, err := tx.Exec(ctx,
				`DELETE FROM trade_xp_grants WHERE trade_id = $1`,
				deleteGrant.TradeID); err != nil {
				return fmt.Errorf("commit delete trade xp grant %s: %w", deleteGrant.TradeID, err)
			}
		}
		if event != nil {
			mid, err := memberIDTx(ctx, tx, event.TelegramID)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO xp_events (member_id, amount, reason, created_at)
				VALUES ($1, $2, $3, $4)`,
				mid, event.Amount, event.Reason, event.Timestamp); err != nil {
				return fmt.Errorf("commit xp event for %d: %w", event.TelegramID, err)
			}
		}
		if extra != nil {
			return extra(ctx, tx)
		}
		return nil
	})
}

// memberIDTx resolves a Telegram ID to its surrogate member id inside tx.
func memberIDTx(ctx context.Context, tx pgx.Tx, telegramID int64) (string, error) {
	var id string
	err := tx.QueryRow(ctx,
		`SELECT id::text FROM members WHERE telegram_id = $1`, telegramID).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", fmt.Errorf("member %d does not exist", telegramID)
	}
	if err != nil {
		return "", fmt.Errorf("resolve member %d: %w", telegramID, err)
	}
	return id, nil
}

// PruneGamification deletes XP events older than cutoff and returns how many
// rows went away. Trade XP grants are deliberately untouched: a trade can be
// edited or deleted at any later time, so its grant must outlive any cutoff.
func (r *GamificationRepo) PruneGamification(ctx context.Context, cutoff time.Time) (int, error) {
	tag, err := r.db.pool.Exec(ctx,
		`DELETE FROM xp_events WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune xp events: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
