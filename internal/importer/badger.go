package importer

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// Store reads the gamification state out of a BadgerDB directory.
//
// It must point at a COPY of the production volume, never the live one: Badger
// takes an exclusive lock, and the running bot holds it. A copy also lets the
// migration replay without touching anything the bot still reads.
type Store struct {
	db *badgerdb.DB
}

// OpenStore opens a Badger directory for reading. The directory is opened
// read-write because Badger refuses to replay a log left truncated by a hard
// stop in read-only mode; on a copy that write is harmless.
func OpenStore(dir string) (*Store, error) {
	opts := badgerdb.DefaultOptions(dir).
		WithLogger(nil).
		WithValueLogFileSize(1 << 20)
	db, err := badgerdb.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open badger copy at %s: %w", dir, err)
	}
	return &Store{db: db}, nil
}

// Close releases the store.
func (s *Store) Close() {
	if s != nil && s.db != nil {
		_ = s.db.Close()
	}
}

// State is everything the old store knew, decoded into domain types.
type State struct {
	Profiles  []domain.GamificationProfile
	Streaks   []domain.StreakData
	XPEvents  []domain.XPEvent
	Grants    []domain.TradeXPGrant
	Badges    []domain.BadgeAward
	Goals     []domain.MonthlyGoal
	Reminders []domain.ReminderPreference
	Challenges []domain.WeeklyChallenge
	// Skipped lists keys this migration deliberately ignores, with the reason,
	// so the report is complete instead of quietly smaller than the store.
	Skipped map[string]int
}

// Read decodes every gam: key of the copy.
func (s *Store) Read() (*State, error) {
	st := &State{Skipped: map[string]int{}}

	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.PrefetchValues = true
		it := txn.NewIterator(opts)
		defer it.Close()

		prefix := []byte("gam:")
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			key := string(item.Key())
			var raw []byte
			if err := item.Value(func(v []byte) error {
				raw = append([]byte(nil), v...)
				return nil
			}); err != nil {
				return fmt.Errorf("read %s: %w", key, err)
			}
			if err := st.decode(key, raw); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

// decode routes one key to its slice.
//
// The Telegram ID is always the second segment, except for the challenge key,
// which is keyed by year-week only. Unknown keys are counted, not failed: the
// ledger and the caches are intentionally dropped, and a future key should show
// up in the report rather than abort a migration.
func (st *State) decode(key string, raw []byte) error {
	parts := strings.Split(key, ":")
	if len(parts) < 2 {
		st.Skipped["unparseable"]++
		return nil
	}
	kind := parts[1]

	switch kind {
	case "profile":
		var v domain.GamificationProfile
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("decode %s: %w", key, err)
		}
		st.Profiles = append(st.Profiles, v)
	case "streak":
		var v domain.StreakData
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("decode %s: %w", key, err)
		}
		st.Streaks = append(st.Streaks, v)
	case "xp_log":
		var v domain.XPEvent
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("decode %s: %w", key, err)
		}
		st.XPEvents = append(st.XPEvents, v)
	case "trade_xp":
		var v domain.TradeXPGrant
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("decode %s: %w", key, err)
		}
		st.Grants = append(st.Grants, v)
	case "badge":
		var v domain.BadgeAward
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("decode %s: %w", key, err)
		}
		st.Badges = append(st.Badges, v)
	case "goal":
		var v domain.MonthlyGoal
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("decode %s: %w", key, err)
		}
		st.Goals = append(st.Goals, v)
	case "reminder_pref":
		var v domain.ReminderPreference
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("decode %s: %w", key, err)
		}
		st.Reminders = append(st.Reminders, v)
	case "challenge":
		var v domain.WeeklyChallenge
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("decode %s: %w", key, err)
		}
		st.Challenges = append(st.Challenges, v)
	case "idem":
		// The Telegram delivery ledger is transport state. Replaying it into a
		// fresh deployment would suppress updates this bot has never seen.
		st.Skipped["ledger"]++
	case "member":
		// Members are rebuilt from Notion and from the live bot; the cache held
		// Notion page ids that no longer exist after the cutover.
		st.Skipped["member_cache"]++
	case "trades_cache":
		st.Skipped["trade_cache"]++
	case "analytics_cache":
		st.Skipped["analytics_cache"]++
	case "report":
		st.Skipped["report_card"]++
	default:
		st.Skipped["unknown:"+kind]++
	}
	return nil
}

// ParseMemberID reads the Telegram ID embedded in a key, for callers that need
// the numeric owner of a record.
func ParseMemberID(key string) (int64, error) {
	parts := strings.Split(key, ":")
	if len(parts) < 3 {
		return 0, fmt.Errorf("key %q has no id segment", key)
	}
	return strconv.ParseInt(parts[2], 10, 64)
}

// ZeroTime is the value the old store wrote for an unset timestamp.
var ZeroTime = time.Time{}
