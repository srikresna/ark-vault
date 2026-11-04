package postgres

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// SessionRepo stores web sessions and OAuth state.
//
// Session ids and OAuth state values are random 256-bit tokens; the database
// stores them verbatim because they are already unguessable and a stolen
// database is a bigger problem than a stolen session. Cookies carry them with
// HttpOnly, Secure and SameSite=Lax.
type SessionRepo struct {
	db *DB
}

// NewSessionRepo builds the session repository.
func NewSessionRepo(db *DB) *SessionRepo { return &SessionRepo{db: db} }

// SessionTTL is how long a login lasts before the member must sign in again.
const SessionTTL = 30 * 24 * time.Hour

// oauthStateTTL bounds how long a started OAuth flow may complete.
const oauthStateTTL = 10 * time.Minute

// NewToken returns a URL-safe random token with 256 bits of entropy.
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// CreateOAuthState stores a pending OAuth attempt and returns its state value.
func (r *SessionRepo) CreateOAuthState(ctx context.Context, provider, verifier, redirectTo string) (string, error) {
	state, err := NewToken()
	if err != nil {
		return "", err
	}
	if _, err := r.db.pool.Exec(ctx, `
		INSERT INTO oauth_states (state, provider, code_verifier, redirect_to, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		state, provider, verifier, redirectTo, time.Now().Add(oauthStateTTL)); err != nil {
		return "", fmt.Errorf("create oauth state: %w", err)
	}
	return state, nil
}

// OAuthState is a pending login attempt.
type OAuthState struct {
	Provider     string
	CodeVerifier string
	RedirectTo   string
}

// ConsumeOAuthState reads and deletes a state value, so a replayed callback
// cannot complete twice. An unknown or expired state is an error.
func (r *SessionRepo) ConsumeOAuthState(ctx context.Context, state string) (*OAuthState, error) {
	var out OAuthState
	err := pgx.BeginFunc(ctx, r.db.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			DELETE FROM oauth_states
			 WHERE state = $1 AND expires_at > now()
			 RETURNING provider, code_verifier, redirect_to`, state,
		).Scan(&out.Provider, &out.CodeVerifier, &out.RedirectTo)
		if err == pgx.ErrNoRows {
			return errNotFound
		}
		return err
	})
	if err != nil {
		if errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("oauth state is unknown or expired")
		}
		return nil, fmt.Errorf("consume oauth state: %w", err)
	}
	return &out, nil
}

// CreateSession starts a session for a member and returns its id.
func (r *SessionRepo) CreateSession(ctx context.Context, memberID string) (string, error) {
	id, err := NewToken()
	if err != nil {
		return "", err
	}
	if _, err := r.db.pool.Exec(ctx, `
		INSERT INTO sessions (id, member_id, expires_at)
		VALUES ($1, $2, $3)`, id, memberID, time.Now().Add(SessionTTL)); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return id, nil
}

// GetSessionMember returns the member behind a live session, or nil.
func (r *SessionRepo) GetSessionMember(ctx context.Context, sessionID string) (*domain.Member, error) {
	if sessionID == "" {
		return nil, nil
	}
	row := r.db.pool.QueryRow(ctx, `
		SELECT `+memberColumns+`
		  FROM sessions s
		  JOIN members m ON m.id = s.member_id
		 WHERE s.id = $1 AND s.expires_at > now()`, sessionID)
	m, err := scanMember(row)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session member: %w", err)
	}
	return m, nil
}

// DeleteSession ends a session. Deleting an unknown session is not an error.
func (r *SessionRepo) DeleteSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	if _, err := r.db.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sessionID); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// SweepExpired removes expired sessions and OAuth states.
func (r *SessionRepo) SweepExpired(ctx context.Context) (int64, error) {
	var total int64
	for _, stmt := range []string{
		`DELETE FROM sessions WHERE expires_at <= now()`,
		`DELETE FROM oauth_states WHERE expires_at <= now()`,
	} {
		tag, err := r.db.pool.Exec(ctx, stmt)
		if err != nil {
			return total, fmt.Errorf("sweep expired: %w", err)
		}
		total += tag.RowsAffected()
	}
	return total, nil
}

// Audit records an action for later review. Failures are not fatal to the
// caller's action, but they are returned so a broken audit trail is visible.
func (r *SessionRepo) Audit(ctx context.Context, memberID *string, action string, detail any) error {
	if _, err := r.db.pool.Exec(ctx, `
		INSERT INTO audit_log (member_id, action, detail)
		VALUES ($1, $2, COALESCE($3, '{}'::jsonb))`,
		memberID, action, detail); err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}
