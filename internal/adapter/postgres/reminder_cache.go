package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// ReminderRepo stores daily/community reminder preferences.
type ReminderRepo struct {
	db      *DB
	members *MemberRepo
}

// NewReminderRepo builds the reminder repository.
func NewReminderRepo(db *DB, members *MemberRepo) *ReminderRepo {
	return &ReminderRepo{db: db, members: members}
}

// GetReminderPref returns a member's reminder preference, or nil when unset.
func (r *ReminderRepo) GetReminderPref(ctx context.Context, telegramID int64) (*domain.ReminderPreference, error) {
	var p domain.ReminderPreference
	err := r.db.pool.QueryRow(ctx, `
		SELECT m.telegram_id, r.enabled, r.hour, r.chat_id, r.thread_id, r.is_community
		  FROM reminder_prefs r
		  JOIN members m ON m.id = r.member_id
		 WHERE m.telegram_id = $1`, telegramID,
	).Scan(&p.TelegramID, &p.Enabled, &p.Hour, &p.ChatID, &p.ThreadID, &p.IsCommunity)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get reminder pref %d: %w", telegramID, err)
	}
	return &p, nil
}

// SaveReminderPref upserts the preference.
func (r *ReminderRepo) SaveReminderPref(ctx context.Context, pref *domain.ReminderPreference) error {
	if pref == nil {
		return fmt.Errorf("save reminder pref: preference is required")
	}
	mid, err := r.members.memberID(ctx, pref.TelegramID)
	if err != nil {
		return err
	}
	if _, err := r.db.pool.Exec(ctx, `
		INSERT INTO reminder_prefs (member_id, enabled, hour, chat_id,
		                            thread_id, is_community, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (member_id) DO UPDATE
		   SET enabled = EXCLUDED.enabled, hour = EXCLUDED.hour,
		       chat_id = EXCLUDED.chat_id, thread_id = EXCLUDED.thread_id,
		       is_community = EXCLUDED.is_community, updated_at = now()`,
		mid, pref.Enabled, pref.Hour, pref.ChatID, pref.ThreadID,
		pref.IsCommunity); err != nil {
		return fmt.Errorf("save reminder pref %d: %w", pref.TelegramID, err)
	}
	return nil
}

// ListEnabledReminders returns every enabled preference, ordered by member so a
// scheduler run is deterministic.
func (r *ReminderRepo) ListEnabledReminders(ctx context.Context) ([]domain.ReminderPreference, error) {
	rows, err := r.db.pool.Query(ctx, `
		SELECT m.telegram_id, r.enabled, r.hour, r.chat_id, r.thread_id, r.is_community
		  FROM reminder_prefs r
		  JOIN members m ON m.id = r.member_id
		 WHERE r.enabled
		 ORDER BY m.telegram_id`)
	if err != nil {
		return nil, fmt.Errorf("list enabled reminders: %w", err)
	}
	defer rows.Close()

	var out []domain.ReminderPreference
	for rows.Next() {
		var p domain.ReminderPreference
		if err := rows.Scan(&p.TelegramID, &p.Enabled, &p.Hour, &p.ChatID,
			&p.ThreadID, &p.IsCommunity); err != nil {
			return nil, fmt.Errorf("list enabled reminders: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list enabled reminders: %w", err)
	}
	return out, nil
}

// AnalyticsCacheRepo caches AI analytics results.
type AnalyticsCacheRepo struct {
	db      *DB
	members *MemberRepo
}

// NewAnalyticsCacheRepo builds the analytics cache repository.
func NewAnalyticsCacheRepo(db *DB, members *MemberRepo) *AnalyticsCacheRepo {
	return &AnalyticsCacheRepo{db: db, members: members}
}

// GetAnalyticsCache returns the cached analytics for a member, or nil when the
// entry is absent or expired.
func (r *AnalyticsCacheRepo) GetAnalyticsCache(ctx context.Context, telegramID int64) (*domain.TradeAnalytics, error) {
	var raw []byte
	err := r.db.pool.QueryRow(ctx, `
		SELECT a.payload
		  FROM analytics_cache a
		  JOIN members m ON m.id = a.member_id
		 WHERE m.telegram_id = $1 AND a.expires_at > now()`, telegramID).Scan(&raw)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get analytics cache %d: %w", telegramID, err)
	}
	var out domain.TradeAnalytics
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode analytics cache %d: %w", telegramID, err)
	}
	return &out, nil
}

// SaveAnalyticsCache upserts the cached analytics.
func (r *AnalyticsCacheRepo) SaveAnalyticsCache(ctx context.Context, analytics *domain.TradeAnalytics) error {
	if analytics == nil {
		return fmt.Errorf("save analytics cache: analytics is required")
	}
	mid, err := r.members.memberID(ctx, analytics.TelegramID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(analytics)
	if err != nil {
		return fmt.Errorf("encode analytics cache %d: %w", analytics.TelegramID, err)
	}
	if _, err := r.db.pool.Exec(ctx, `
		INSERT INTO analytics_cache (member_id, payload, source_hash, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (member_id) DO UPDATE
		   SET payload = EXCLUDED.payload, source_hash = EXCLUDED.source_hash,
		       created_at = EXCLUDED.created_at, expires_at = EXCLUDED.expires_at`,
		mid, raw, analytics.SourceHash, analytics.GeneratedAt,
		analytics.ExpiresAt); err != nil {
		return fmt.Errorf("save analytics cache %d: %w", analytics.TelegramID, err)
	}
	return nil
}

// ReportCardRepo stores monthly report cards.
type ReportCardRepo struct {
	db      *DB
	members *MemberRepo
}

// NewReportCardRepo builds the report card repository.
func NewReportCardRepo(db *DB, members *MemberRepo) *ReportCardRepo {
	return &ReportCardRepo{db: db, members: members}
}

// GetReportCard returns the stored report card, or nil when absent.
func (r *ReportCardRepo) GetReportCard(ctx context.Context, telegramID int64, yearMonth string) (*domain.MonthlyReportCard, error) {
	var raw []byte
	err := r.db.pool.QueryRow(ctx, `
		SELECT r.payload
		  FROM report_cards r
		  JOIN members m ON m.id = r.member_id
		 WHERE m.telegram_id = $1 AND r.year_month = $2`,
		telegramID, yearMonth).Scan(&raw)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get report card %d/%s: %w", telegramID, yearMonth, err)
	}
	var out domain.MonthlyReportCard
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode report card %d/%s: %w", telegramID, yearMonth, err)
	}
	return &out, nil
}

// SaveReportCard upserts the report card.
func (r *ReportCardRepo) SaveReportCard(ctx context.Context, report *domain.MonthlyReportCard) error {
	if report == nil || report.YearMonth == "" {
		return fmt.Errorf("save report card: year month is required")
	}
	mid, err := r.members.memberID(ctx, report.TelegramID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode report card %d: %w", report.TelegramID, err)
	}
	if _, err := r.db.pool.Exec(ctx, `
		INSERT INTO report_cards (member_id, year_month, payload, source_hash, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (member_id, year_month) DO UPDATE
		   SET payload = EXCLUDED.payload, source_hash = EXCLUDED.source_hash,
		       created_at = EXCLUDED.created_at`,
		mid, report.YearMonth, raw, report.SourceHash, report.GeneratedAt); err != nil {
		return fmt.Errorf("save report card %d/%s: %w", report.TelegramID, report.YearMonth, err)
	}
	return nil
}
