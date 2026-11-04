package importer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/arkcode369/ark-vault/internal/adapter/postgres"
)

// Report is what the migration did, printed at the end so an operator can
// compare it against Notion instead of trusting a silent success.
type Report struct {
	Members       int
	Trades        int
	Screenshots       int
	ScreenshotsStored int
	ImageDir          string
	Profiles      int
	Streaks       int
	XPEvents      int
	Grants        int
	GrantsDropped int
	Badges        int
	Goals         int
	Reminders     int
	Challenges    int
	UnknownMember int
	Skipped       map[string]int
	Warnings      []string
}

func (r *Report) warn(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// SkippedKeys returns the skip counters sorted for a stable report.
func (r *Report) SkippedKeys() []string {
	keys := make([]string, 0, len(r.Skipped))
	for k := range r.Skipped {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Options configures one migration run.
type Options struct {
	DatabaseURL  string
	NotionToken  string
	NotionParent string
	// BadgerDir points at a COPY of the production Badger volume. Badger holds an
	// exclusive lock, so the live directory cannot be read while the bot runs.
	// Empty skips gamification, which is only right for a fresh community.
	BadgerDir string
	// ImageDir is the screenshot root the new deployment serves from. Empty
	// skips screenshots, which is only right when Notion holds none.
	ImageDir string
	// DryRun reads both sources and reports counts without writing anything.
	DryRun bool
}

// Run migrates Notion journals and the Badger copy into Postgres.
//
// It refuses to run against a journal that already holds trades: trades carry no
// external identity column, so a second run would duplicate rows instead of
// converging.
func Run(ctx context.Context, opts Options) (*Report, error) {
	report := &Report{Skipped: map[string]int{}}
	if opts.ImageDir != "" {
		report.ImageDir = opts.ImageDir
	}

	nc := NewNotionClient(opts.NotionToken)
	members, skippedPages, err := DiscoverMembers(ctx, nc, opts.NotionParent)
	if err != nil {
		return nil, err
	}
	for _, s := range skippedPages {
		report.warn("skipped Notion page: %s", s)
	}
	for i := range members {
		trades, err := LoadTrades(ctx, nc, members[i])
		if err != nil {
			return nil, err
		}
		if err := AttachImages(ctx, nc, trades); err != nil {
			return nil, err
		}
		members[i].Trades = trades
		report.Trades += len(trades)
	}

	if opts.ImageDir != "" {
		var all []Trade
		for _, m := range members {
			all = append(all, m.Trades...)
		}
		stage, err := os.MkdirTemp("", "ark-migrate-images-")
		if err != nil {
			return nil, fmt.Errorf("create image staging dir: %w", err)
		}
		defer func() { _ = os.RemoveAll(stage) }()
		if err := downloadImages(ctx, stage, all); err != nil {
			return nil, err
		}
		// downloadImages wrote into its own slice; carry the paths back by page
		// id so the insert loop can attach them.
		staged := make(map[string]string, len(all))
		for _, t := range all {
			if t.ImagePath != "" {
				staged[t.NotionPageID] = t.ImagePath
				report.Screenshots++
			}
		}
		for mi := range members {
			for ti := range members[mi].Trades {
				if p, ok := staged[members[mi].Trades[ti].NotionPageID]; ok {
					members[mi].Trades[ti].ImagePath = p
				}
			}
		}
	}
	report.Members = len(members)

	var state *State
	if opts.BadgerDir != "" {
		store, err := OpenStore(opts.BadgerDir)
		if err != nil {
			return nil, err
		}
		state, err = store.Read()
		store.Close()
		if err != nil {
			return nil, err
		}
		report.Skipped = state.Skipped
	}

	if opts.DryRun {
		if state != nil {
			report.Profiles = len(state.Profiles)
			report.Streaks = len(state.Streaks)
			report.XPEvents = len(state.XPEvents)
			report.Grants = len(state.Grants)
			report.Badges = len(state.Badges)
			report.Goals = len(state.Goals)
			report.Reminders = len(state.Reminders)
			report.Challenges = len(state.Challenges)
		}
		return report, nil
	}

	db, err := postgres.Open(ctx, opts.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open target database: %w", err)
	}
	defer db.Close()

	var existing int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM trades`).Scan(&existing); err != nil {
		return nil, fmt.Errorf("count existing trades: %w", err)
	}
	if existing > 0 {
		return nil, fmt.Errorf(
			"journal already holds %d trades; refusing to import into a non-empty journal",
			existing)
	}

	var store *postgres.ImageStore
	if opts.ImageDir != "" {
		store, err = postgres.NewImageStore(opts.ImageDir)
		if err != nil {
			return nil, err
		}
	}

	// memberIDs maps a Telegram ID to its new uuid. tradeIDs maps an old Notion
	// trade page id to the uuid it became, so a grant recorded against the old id
	// still lands on the right trade.
	memberIDs := map[int64]string{}
	tradeIDs := map[string]string{}

	err = pgx.BeginFunc(ctx, db.Pool(), func(tx pgx.Tx) error {
		for _, m := range members {
			var memberID string
			if err := tx.QueryRow(ctx, `
				INSERT INTO members (telegram_id, username, first_name, role)
				VALUES ($1, $2, $2, 'member')
				ON CONFLICT (telegram_id) DO UPDATE SET username = EXCLUDED.username
				RETURNING id::text`, m.TelegramID, m.Username).Scan(&memberID); err != nil {
				return fmt.Errorf("insert member %d: %w", m.TelegramID, err)
			}
			memberIDs[m.TelegramID] = memberID

			for _, t := range m.Trades {
				var newID string
				// created_at is taken from Notion, not now(): it is the tie-break
				// for "latest trade" and for ordering trades recorded the same day.
				if err := tx.QueryRow(ctx, `
					INSERT INTO trades (
						member_id, symbol, direction, asset_type, status, trade_date,
						result_rr, time_window, confluence, notes, created_at, updated_at
					) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11)
					RETURNING id::text`,
					memberID, t.Trade.Symbol, string(t.Trade.Direction),
					t.Trade.AssetType.String(), string(t.Trade.Status), t.Trade.Date,
					t.Trade.ResultRR, string(t.Trade.TimeWindow), t.Trade.Confluence,
					t.Trade.Notes, t.CreatedAt).Scan(&newID); err != nil {
					return fmt.Errorf("insert trade %s: %w", t.NotionPageID, err)
				}
				if t.ImagePath != "" && store != nil {
					data, err := os.ReadFile(t.ImagePath)
					if err != nil {
						return fmt.Errorf("read staged screenshot for trade %s: %w", t.NotionPageID, err)
					}
					key, err := store.Upload(ctx, newID, filepath.Base(t.ImagePath), data)
					if err != nil {
						return fmt.Errorf("store screenshot for trade %s: %w", t.NotionPageID, err)
					}
					if _, err := tx.Exec(ctx,
						`UPDATE trades SET screenshot_key = $2 WHERE id = $1`, newID, key); err != nil {
						return fmt.Errorf("attach screenshot to trade %s: %w", t.NotionPageID, err)
					}
					report.ScreenshotsStored++
				}
				tradeIDs[t.NotionPageID] = newID
			}
		}

		if state == nil {
			return nil
		}
		return importState(ctx, tx, state, memberIDs, tradeIDs, report)
	})
	if err != nil {
		return nil, err
	}
	return report, nil
}
