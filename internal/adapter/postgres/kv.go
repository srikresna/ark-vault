package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// KV is the durable key/value surface the Telegram delivery ledger needs. It
// replaces the Badger store for that purpose and preserves its exact
// semantics, so the queue's admission, TTL and reset behaviour do not change.
//
// Rows past their expiry are treated as absent everywhere. Cleanup is lazy
// because a bot restart after downtime must not see a burst of expired rows as
// live work, and a background sweeper is not worth its own goroutine here.
type KV struct {
	db *DB
}

// NewKV builds the key/value repository.
func NewKV(db *DB) *KV { return &KV{db: db} }

// Get decodes the value under key into dest. It reports found=false when the
// key is absent or expired, and never folds a read failure into a miss: the
// caller treats "cannot decide" as "refuse to run".
func (k *KV) Get(ctx context.Context, key string, dest interface{}) (bool, error) {
	var raw []byte
	err := k.db.pool.QueryRow(ctx,
		`SELECT value FROM replay_kv
		  WHERE key = $1 AND (expires_at IS NULL OR expires_at > now())`,
		key).Scan(&raw)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read replay key %q: %w", key, err)
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return false, fmt.Errorf("decode replay key %q: %w", key, err)
	}
	return true, nil
}

// SetWithTTL stores value under key. A non-positive ttl means the row never
// expires, matching the Badger adapter: a zero TTL must not expire immediately,
// or a running ledger row would vanish and its update would be replayed.
func (k *KV) SetWithTTL(ctx context.Context, key string, value interface{}, ttl time.Duration) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode replay key %q: %w", key, err)
	}
	var expires any
	if ttl > 0 {
		expires = time.Now().Add(ttl)
	}
	if _, err := k.db.pool.Exec(ctx, `
		INSERT INTO replay_kv (key, value, expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE
		   SET value = EXCLUDED.value, expires_at = EXCLUDED.expires_at`,
		key, raw, expires); err != nil {
		return fmt.Errorf("write replay key %q: %w", key, err)
	}
	return nil
}

// Delete removes key. Deleting a missing key is not an error.
func (k *KV) Delete(ctx context.Context, key string) error {
	if _, err := k.db.pool.Exec(ctx, `DELETE FROM replay_kv WHERE key = $1`, key); err != nil {
		return fmt.Errorf("delete replay key %q: %w", key, err)
	}
	return nil
}

// Scan returns every live value whose key starts with prefix.
//
// The prefix is escaped so a key containing % or _ cannot widen the match; the
// ledger scans by structured prefixes such as "gam:idem:bot:1:notice:".
func (k *KV) Scan(ctx context.Context, prefix string) ([][]byte, error) {
	rows, err := k.db.pool.Query(ctx, `
		SELECT value FROM replay_kv
		 WHERE key LIKE $1 ESCAPE '\'
		   AND (expires_at IS NULL OR expires_at > now())
		 ORDER BY key`,
		escapeLike(prefix)+"%")
	if err != nil {
		return nil, fmt.Errorf("scan replay keys %q: %w", prefix, err)
	}
	defer rows.Close()

	var out [][]byte
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan replay keys %q: %w", prefix, err)
		}
		out = append(out, raw)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan replay keys %q: %w", prefix, err)
	}
	return out, nil
}

// ApplyBatch writes insertIfAbsent and put in one transaction and returns the
// keys that were actually inserted.
//
// Admission depends on this: the batch payloads and the fetch cursor become
// durable together, or Telegram could forget an update this bot never stored.
// put always overwrites (the cursor must advance); insertIfAbsent never
// overwrites, so a duplicate delivery cannot resurrect or re-enqueue a row.
func (k *KV) ApplyBatch(ctx context.Context, insertIfAbsent, put map[string][]byte) ([]string, error) {
	inserted := make([]string, 0, len(insertIfAbsent))
	err := pgx.BeginFunc(ctx, k.db.pool, func(tx pgx.Tx) error {
		for key, raw := range insertIfAbsent {
			var got string
			err := tx.QueryRow(ctx, `
				INSERT INTO replay_kv (key, value)
				VALUES ($1, $2)
				ON CONFLICT (key) DO NOTHING
				RETURNING key`, key, raw).Scan(&got)
			if err == pgx.ErrNoRows {
				continue // already present: a duplicate delivery
			}
			if err != nil {
				return fmt.Errorf("insert replay key %q: %w", key, err)
			}
			inserted = append(inserted, got)
		}
		for key, raw := range put {
			if _, err := tx.Exec(ctx, `
				INSERT INTO replay_kv (key, value)
				VALUES ($1, $2)
				ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
				key, raw); err != nil {
				return fmt.Errorf("write replay key %q: %w", key, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inserted, nil
}

// SweepExpired deletes rows whose TTL has passed. Safe to call periodically.
func (k *KV) SweepExpired(ctx context.Context) (int64, error) {
	tag, err := k.db.pool.Exec(ctx,
		`DELETE FROM replay_kv WHERE expires_at IS NOT NULL AND expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("sweep expired replay keys: %w", err)
	}
	return tag.RowsAffected(), nil
}

// escapeLike neutralises LIKE wildcards in a literal prefix.
func escapeLike(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '\\', '%', '_':
			out = append(out, '\\', r)
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
