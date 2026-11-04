package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// MemberRepo is the member store. It replaces the Notion member page plus the
// Badger mapping cache: there is one row per member, keyed by the Telegram ID
// the bot already speaks.
type MemberRepo struct {
	db *DB
}

// NewMemberRepo builds the member repository.
func NewMemberRepo(db *DB) *MemberRepo { return &MemberRepo{db: db} }

// memberColumns returns the shared member projection qualified by a table alias.
//
// The qualifier is not optional: `id` exists on sessions and members alike, so an
// unqualified projection makes every JOIN fail with "column reference is
// ambiguous", which surfaced as every authenticated request returning 500.
func memberColumns(alias string) string {
	return alias + `.id::text, ` + alias + `.telegram_id,
	COALESCE(` + alias + `.discord_id, ''), ` + alias + `.username,
	` + alias + `.first_name, ` + alias + `.role, ` + alias + `.joined_at,
	` + alias + `.created_at, ` + alias + `.updated_at`
}

func scanMember(row rowScanner) (*domain.Member, error) {
	var m domain.Member
	var role string
	if err := row.Scan(&m.ID, &m.TelegramID, &m.DiscordID, &m.Username,
		&m.FirstName, &role, &m.JoinDate, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, err
	}
	m.Role = domain.Role(role)
	return &m, nil
}

// EnsureMember inserts the member if absent and returns the stored row.
//
// It never fails because of a transient read: there is one authoritative store,
// so the duplicate-journal class of bug that came from reconciling two systems
// cannot happen here. Identity fields are only overwritten when the incoming
// value is non-empty, so a later call with a blank username cannot erase one.
func (r *MemberRepo) EnsureMember(ctx context.Context, m *domain.Member) (*domain.Member, error) {
	if m == nil || m.TelegramID == 0 {
		return nil, errors.New("ensure member: telegram id is required")
	}
	joined := m.JoinDate
	if joined.IsZero() {
		joined = time.Now()
	}
	row := r.db.pool.QueryRow(ctx, `
		INSERT INTO members (telegram_id, username, first_name, joined_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (telegram_id) DO UPDATE
		   SET username   = CASE WHEN EXCLUDED.username <> ''
		                         THEN EXCLUDED.username ELSE members.username END,
		       first_name = CASE WHEN EXCLUDED.first_name <> ''
		                         THEN EXCLUDED.first_name ELSE members.first_name END,
		       updated_at = now()
		RETURNING `+memberColumns("members"),
		m.TelegramID, m.Username, m.FirstName, joined)
	out, err := scanMember(row)
	if err != nil {
		return nil, fmt.Errorf("ensure member %d: %w", m.TelegramID, err)
	}
	return out, nil
}

// GetMember returns the member for a Telegram ID, or nil when absent.
func (r *MemberRepo) GetMember(ctx context.Context, telegramID int64) (*domain.Member, error) {
	row := r.db.pool.QueryRow(ctx,
		`SELECT `+memberColumns("members")+` FROM members WHERE telegram_id = $1`, telegramID)
	m, err := scanMember(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get member %d: %w", telegramID, err)
	}
	return m, nil
}

// GetMemberByID returns the member for a surrogate ID, or nil when absent.
func (r *MemberRepo) GetMemberByID(ctx context.Context, id string) (*domain.Member, error) {
	row := r.db.pool.QueryRow(ctx,
		`SELECT `+memberColumns("members")+` FROM members WHERE id = $1`, id)
	m, err := scanMember(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get member %s: %w", id, err)
	}
	return m, nil
}

// GetMemberByDiscordID returns the member linked to a Discord account, or nil.
func (r *MemberRepo) GetMemberByDiscordID(ctx context.Context, discordID string) (*domain.Member, error) {
	if discordID == "" {
		return nil, nil
	}
	row := r.db.pool.QueryRow(ctx,
		`SELECT `+memberColumns("members")+` FROM members WHERE discord_id = $1`, discordID)
	m, err := scanMember(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get member by discord id: %w", err)
	}
	return m, nil
}

// LinkDiscord attaches a Discord account to an existing member.
//
// A Discord id already linked to another member is rejected instead of moved:
// silently stealing the link would hand one person another's journal.
func (r *MemberRepo) LinkDiscord(ctx context.Context, memberID, discordID string) error {
	if memberID == "" || discordID == "" {
		return errors.New("link discord: member id and discord id are required")
	}
	var owner string
	err := r.db.pool.QueryRow(ctx,
		`SELECT id::text FROM members WHERE discord_id = $1`, discordID).Scan(&owner)
	if err == nil && owner != memberID {
		return fmt.Errorf("link discord: account already linked to another member")
	}
	if err != nil && err != pgx.ErrNoRows {
		return fmt.Errorf("link discord: check existing link: %w", err)
	}
	tag, err := r.db.pool.Exec(ctx,
		`UPDATE members SET discord_id = $2, updated_at = now() WHERE id = $1`,
		memberID, discordID)
	if err != nil {
		return fmt.Errorf("link discord: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("link discord: member %s does not exist", memberID)
	}
	return nil
}

// ListMembers returns every member, oldest first so a leaderboard is stable
// between calls when several members have the same score.
func (r *MemberRepo) ListMembers(ctx context.Context) ([]domain.Member, error) {
	rows, err := r.db.pool.Query(ctx,
		`SELECT `+memberColumns("members")+` FROM members ORDER BY joined_at, telegram_id`)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()

	var out []domain.Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, fmt.Errorf("list members: %w", err)
		}
		out = append(out, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	return out, nil
}

// SetRole changes a member's web role.
func (r *MemberRepo) SetRole(ctx context.Context, memberID string, role domain.Role) error {
	if !role.Valid() {
		return fmt.Errorf("set role: unknown role %q", role)
	}
	tag, err := r.db.pool.Exec(ctx,
		`UPDATE members SET role = $2, updated_at = now() WHERE id = $1`,
		memberID, string(role))
	if err != nil {
		return fmt.Errorf("set role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set role: member %s does not exist", memberID)
	}
	return nil
}

// memberID resolves the surrogate member id for a Telegram ID.
func (r *MemberRepo) memberID(ctx context.Context, telegramID int64) (string, error) {
	var id string
	err := r.db.pool.QueryRow(ctx,
		`SELECT id::text FROM members WHERE telegram_id = $1`, telegramID).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", fmt.Errorf("member %d does not exist", telegramID)
	}
	if err != nil {
		return "", fmt.Errorf("resolve member %d: %w", telegramID, err)
	}
	return id, nil
}
