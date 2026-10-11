package badger

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
		WithBloomFalsePositive(0.01)

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
func (s *Store) SetWithTTL(key string, value interface{}, ttl time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.db.Update(func(txn *badger.Txn) error {
		e := badger.NewEntry([]byte(key), data).WithTTL(ttl)
		return txn.SetEntry(e)
	})
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

// Reset deletes all keys in the BadgerDB store safely within write transactions.
func (s *Store) Reset() error {
	var keys [][]byte
	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
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
