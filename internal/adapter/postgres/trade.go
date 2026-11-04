package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// TradeRepo is the trade store. It replaces the Notion Trades database and the
// Badger trade cache: rows are the history, so there is nothing to invalidate
// after a write.
type TradeRepo struct {
	db      *DB
	members *MemberRepo
}

// NewTradeRepo builds the trade repository.
func NewTradeRepo(db *DB, members *MemberRepo) *TradeRepo {
	return &TradeRepo{db: db, members: members}
}

// SaveTrade inserts a trade and returns its generated ID.
//
// The trade's ID field is left untouched by callers that do not have one; the
// generated uuid is written back so the handler can attach a screenshot and
// gamification can key its grant on it.
func (r *TradeRepo) SaveTrade(ctx context.Context, memberID int64, trade *domain.Trade) error {
	if trade == nil {
		return errors.New("save trade: trade is required")
	}
	mid, err := r.members.memberID(ctx, memberID)
	if err != nil {
		return err
	}
	date := trade.Date
	if date.IsZero() {
		date = time.Now()
	}
	err = r.db.pool.QueryRow(ctx, `
		INSERT INTO trades (
			member_id, symbol, direction, asset_type, status, trade_date,
			result_rr, time_window, confluence, notes, screenshot_key,
			source_update_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id::text, created_at`,
		mid, trade.Symbol, string(trade.Direction), trade.AssetType.String(),
		string(trade.Status), date, trade.ResultRR, string(trade.TimeWindow),
		trade.Confluence, trade.Notes, trade.ScreenshotURL, nullableUpdateID(trade.SourceUpdateID),
	).Scan(&trade.ID, &trade.CreatedAt)
	if err != nil {
		return fmt.Errorf("save trade for member %d: %w", memberID, err)
	}
	return nil
}

// GetTrades returns every live trade for a member, newest first.
//
// A member with no trades gets an empty slice, not nil: callers distinguish
// "no history" from "failed read" by the error, and returning nil here would
// make an unreadable history look empty.
func (r *TradeRepo) GetTrades(ctx context.Context, memberID int64) ([]domain.Trade, error) {
	mid, err := r.members.memberID(ctx, memberID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.pool.Query(ctx, `
		SELECT id::text, member_id::text, trade_date, created_at, asset_type,
		       symbol, direction, result_rr, status, time_window, confluence,
		       notes, screenshot_key, COALESCE(source_update_id, 0)
		  FROM trades
		 WHERE member_id = $1 AND deleted_at IS NULL
		 ORDER BY trade_date DESC, created_at DESC`, mid)
	if err != nil {
		return nil, fmt.Errorf("query trades for member %d: %w", memberID, err)
	}
	defer rows.Close()

	trades := make([]domain.Trade, 0, 16)
	for rows.Next() {
		t, err := scanTrade(rows)
		if err != nil {
			return nil, fmt.Errorf("scan trade for member %d: %w", memberID, err)
		}
		trades = append(trades, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query trades for member %d: %w", memberID, err)
	}
	return trades, nil
}

// UpdateTrade overwrites the mutable fields of an existing trade.
func (r *TradeRepo) UpdateTrade(ctx context.Context, memberID int64, trade *domain.Trade) error {
	if trade == nil || trade.ID == "" {
		return errors.New("update trade: trade id is required")
	}
	mid, err := r.members.memberID(ctx, memberID)
	if err != nil {
		return err
	}
	tag, err := r.db.pool.Exec(ctx, `
		UPDATE trades
		   SET symbol = $3, direction = $4, asset_type = $5, status = $6,
		       trade_date = $7, result_rr = $8, time_window = $9,
		       confluence = $10, notes = $11, screenshot_key = $12,
		       updated_at = now()
		 WHERE id = $1 AND member_id = $2 AND deleted_at IS NULL`,
		trade.ID, mid, trade.Symbol, string(trade.Direction), trade.AssetType.String(),
		string(trade.Status), trade.Date, trade.ResultRR, string(trade.TimeWindow),
		trade.Confluence, trade.Notes, trade.ScreenshotURL)
	if isInvalidUUID(err) {
		return fmt.Errorf("update trade %s: %w", trade.ID, errNotFound)
	}
	if err != nil {
		return fmt.Errorf("update trade %s: %w", trade.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("update trade %s: %w", trade.ID, errNotFound)
	}
	return nil
}

// DeleteTrade soft-deletes a trade. The row stays so XP grants and audit trail
// keep a referent; a hard delete would orphan them.
func (r *TradeRepo) DeleteTrade(ctx context.Context, memberID int64, tradeID string) error {
	mid, err := r.members.memberID(ctx, memberID)
	if err != nil {
		return err
	}
	tag, err := r.db.pool.Exec(ctx, `
		UPDATE trades SET deleted_at = now(), updated_at = now()
		 WHERE id = $1 AND member_id = $2 AND deleted_at IS NULL`, tradeID, mid)
	if isInvalidUUID(err) {
		return fmt.Errorf("delete trade %s: %w", tradeID, errNotFound)
	}
	if err != nil {
		return fmt.Errorf("delete trade %s: %w", tradeID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete trade %s: %w", tradeID, errNotFound)
	}
	return nil
}

// GetTrade returns one trade by ID, or nil when it does not exist.
func (r *TradeRepo) GetTrade(ctx context.Context, memberID int64, tradeID string) (*domain.Trade, error) {
	mid, err := r.members.memberID(ctx, memberID)
	if err != nil {
		return nil, err
	}
	row := r.db.pool.QueryRow(ctx, `
		SELECT id::text, member_id::text, trade_date, created_at, asset_type,
		       symbol, direction, result_rr, status, time_window, confluence,
		       notes, screenshot_key, COALESCE(source_update_id, 0)
		  FROM trades
		 WHERE id = $1 AND member_id = $2 AND deleted_at IS NULL`, tradeID, mid)
	t, err := scanTrade(row)
	if err == pgx.ErrNoRows || isInvalidUUID(err) {
		// A malformed id cannot name a trade, so it is "not found", not a
		// server error: Postgres rejects it at the type level.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get trade %s: %w", tradeID, err)
	}
	return t, nil
}

// SetScreenshot stores the object key of a trade's screenshot.
func (r *TradeRepo) SetScreenshot(ctx context.Context, memberID int64, tradeID, key string) error {
	mid, err := r.members.memberID(ctx, memberID)
	if err != nil {
		return err
	}
	tag, err := r.db.pool.Exec(ctx, `
		UPDATE trades SET screenshot_key = $3, updated_at = now()
		 WHERE id = $1 AND member_id = $2 AND deleted_at IS NULL`, tradeID, mid, key)
	if isInvalidUUID(err) {
		return fmt.Errorf("set screenshot for trade %s: %w", tradeID, errNotFound)
	}
	if err != nil {
		return fmt.Errorf("set screenshot for trade %s: %w", tradeID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set screenshot for trade %s: %w", tradeID, errNotFound)
	}
	return nil
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows, so one scan function
// serves single-row and list queries.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTrade(row rowScanner) (*domain.Trade, error) {
	var (
		t        domain.Trade
		memberID string
	)
	err := row.Scan(
		&t.ID, &memberID, &t.Date, &t.CreatedAt, &t.AssetType,
		&t.Symbol, &t.Direction, &t.ResultRR, &t.Status, &t.TimeWindow,
		&t.Confluence, &t.Notes, &t.ScreenshotURL, &t.SourceUpdateID,
	)
	if err != nil {
		return nil, err
	}
	if v, convErr := parseInt64(memberID); convErr == nil {
		t.MemberID = v
	}
	return &t, nil
}

// nullableUpdateID maps the zero value to SQL NULL so the partial unique index
// on source_update_id does not treat every legacy trade as a duplicate of 0.
func nullableUpdateID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
