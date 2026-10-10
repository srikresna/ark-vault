package badger

import (
	"encoding/json"
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
