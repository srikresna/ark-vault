package badger

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
)

// Store wraps a BadgerDB instance with helpers for JSON serialisation.
type Store struct {
	db        *badger.DB
	stopGC    chan struct{}
	closeOnce sync.Once
}

// OpenStore opens (or creates) a BadgerDB at path with production-tuned options
// and starts a background value-log GC goroutine.
func OpenStore(path string) (*Store, error) {
	opts := badger.DefaultOptions(path).
		WithLoggingLevel(badger.WARNING).
		WithCompactL0OnClose(true).
		WithValueLogFileSize(64 << 20). // 64 MB
		WithBloomFalsePositive(0.01).
		// The update ledger acknowledges work to Telegram: an offset advanced
		// before the records it covers reach disk would lose those updates on
		// power loss. Badger only syncs when SyncWrites is set (default false),
		// and it is a database-wide option, so it is enabled here. This bot
		// writes a few records per message; the fsync cost is acceptable for
		// not losing accepted work.
		WithSyncWrites(true)

	db, err := badger.Open(opts)
	if err != nil {
		return nil, err
	}

	s := &Store{
		db:     db,
		stopGC: make(chan struct{}),
	}
	go s.runGC()
	return s, nil
}

// runGC periodically triggers value-log garbage collection.
func (s *Store) runGC() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_ = s.db.RunValueLogGC(0.5)
		case <-s.stopGC:
			return
		}
	}
}

// Close stops the background GC and closes the database.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		close(s.stopGC)
	})
	return s.db.Close()
}

// DB returns the underlying badger.DB for advanced usage.
func (s *Store) DB() *badger.DB {
	return s.db
}

// Get reads the value for key and JSON-unmarshals it into dest.
// Returns badger.ErrKeyNotFound when the key does not exist.
func (s *Store) Get(key string, dest interface{}) error {
	return s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(key))
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, dest)
		})
	})
}

// Set JSON-marshals value and stores it under key.
func (s *Store) Set(key string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(key), data)
	})
}

// SetWithTTL is like Set but the entry expires after ttl.
//
// A ttl of zero or less means "never expires". This matters: Badger's
// Entry.WithTTL(0) sets ExpiresAt to the current second, so the value would be
// invisible immediately. Durable ledger rows (admitted, running) are written
// with ttl 0 and MUST NOT vanish.
func (s *Store) SetWithTTL(key string, value interface{}, ttl time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.db.Update(func(txn *badger.Txn) error {
		if ttl <= 0 {
			return txn.Set([]byte(key), data)
		}
		e := badger.NewEntry([]byte(key), data).WithTTL(ttl)
		return txn.SetEntry(e)
	})
}

// ApplyBatch writes insertIfAbsent and put in ONE transaction.
//
// It is the primitive batch admission needs: the caller can make a set of
// payload rows and the cursor that acknowledges them durable together, so a
// crash can never leave the cursor ahead of the records it covers. Keys already
// present are skipped and reported as not inserted, which makes a duplicate
// delivery a no-op. Badger reports read-write conflicts at commit, so the
// transaction is retried a bounded number of times to keep the check-and-set
// semantics.
func (s *Store) ApplyBatch(insertIfAbsent, put map[string][]byte) ([]string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		inserted := make([]string, 0, len(insertIfAbsent))
		err := s.db.Update(func(txn *badger.Txn) error {
			inserted = inserted[:0]
			for _, key := range sortedKeys(insertIfAbsent) {
				if _, err := txn.Get([]byte(key)); err == nil {
					continue
				} else if !errors.Is(err, badger.ErrKeyNotFound) {
					return err
				}
				if err := txn.Set([]byte(key), insertIfAbsent[key]); err != nil {
					return err
				}
				inserted = append(inserted, key)
			}
			for _, key := range sortedKeys(put) {
				if err := txn.Set([]byte(key), put[key]); err != nil {
					return err
				}
			}
			return nil
		})
		if errors.Is(err, badger.ErrConflict) {
			continue
		}
		return inserted, err
	}
	return nil, badger.ErrConflict
}

// sortedKeys returns the map keys in ascending order so a transaction touches
// them deterministically (stable tests, stable conflict behaviour).
func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Scan iterates over all keys with the given prefix and returns their raw
// JSON-encoded values.
func (s *Store) Scan(prefix string) ([][]byte, error) {
	var results [][]byte
	pfx := []byte(prefix)
	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = pfx
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(pfx); it.ValidForPrefix(pfx); it.Next() {
			item := it.Item()
			if err := item.Value(func(val []byte) error {
				cp := make([]byte, len(val))
				copy(cp, val)
				results = append(results, cp)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return results, err
}

// Delete removes the entry for key. It is a no-op if the key does not exist.
func (s *Store) Delete(key string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		err := txn.Delete([]byte(key))
		if err == badger.ErrKeyNotFound {
			return nil
		}
		return err
	})
}

// IdempotencyPrefix is the key namespace for durable per-update idempotency
// records (claims and recovery notices). Store.Reset preserves it: wiping the
// ledger during /reset would let an interrupted Telegram update replay its side
// effects — a second trade page, a second /undo — with nothing left to detect
// it. The Telegram adapter uses the same prefix; its tests assert the match.
const IdempotencyPrefix = "gam:idem:"

// Reset deletes all keys in the BadgerDB store safely within write
// transactions, except the idempotency ledger under IdempotencyPrefix.
func (s *Store) Reset() error {
	var keys [][]byte
	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			// Reset clears member state, not the record of which updates were
			// already applied: dropping a claim would let a redelivery run a
			// non-idempotent action a second time.
			if strings.HasPrefix(string(it.Item().Key()), IdempotencyPrefix) {
				continue
			}
			k := it.Item().KeyCopy(nil)
			keys = append(keys, k)
		}
		return nil
	})
	if err != nil {
		return err
	}

	const batchSize = 500
	for i := 0; i < len(keys); i += batchSize {
		end := i + batchSize
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[i:end]
		if err := s.db.Update(func(txn *badger.Txn) error {
			for _, k := range batch {
				if err := txn.Delete(k); err != nil && err != badger.ErrKeyNotFound {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// BackupToDir streams a consistent snapshot of the database compressed with
// gzip into dir, writes it atomically via a temporary file, and prunes older
// backups so only the keep newest files remain.
func (s *Store) BackupToDir(dir string, keep int) (string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}

	tmpFile, err := os.CreateTemp(dir, "badger-backup-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp backup file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath) // cleans up if rename was not reached
	}()

	gzWriter := gzip.NewWriter(tmpFile)
	if _, err := s.db.Backup(gzWriter, 0); err != nil {
		_ = gzWriter.Close()
		return "", fmt.Errorf("badger backup stream: %w", err)
	}
	if err := gzWriter.Close(); err != nil {
		return "", fmt.Errorf("close gzip writer: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}

	targetName := fmt.Sprintf("badger-%s.bak.gz", time.Now().UTC().Format("20060102-150405"))
	targetPath := filepath.Join(dir, targetName)
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return "", fmt.Errorf("commit backup file: %w", err)
	}

	if keep > 0 {
		s.pruneBackups(dir, keep)
	}

	return targetPath, nil
}

func (s *Store) pruneBackups(dir string, keep int) {
	matches, err := filepath.Glob(filepath.Join(dir, "badger-*.bak.gz"))
	if err != nil || len(matches) <= keep {
		return
	}
	sort.Strings(matches) // chronological by timestamp in name
	toRemove := matches[:len(matches)-keep]
	for _, f := range toRemove {
		_ = os.Remove(f)
	}
}
