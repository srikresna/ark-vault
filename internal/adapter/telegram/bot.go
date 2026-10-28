package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const telegramAPI = "https://api.telegram.org/bot"

// redactTokenErr rewrites transport errors so the bot token never reaches
// logs. Every Bot API call embeds the token in the URL path, and
// http.Client.Do returns *url.Error whose Error() renders the full request
// URL — a DNS failure, timeout or refused connection would otherwise log the
// raw token verbatim.
func redactTokenErr(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.URL != "" {
		ue.URL = strings.ReplaceAll(ue.URL, token, "REDACTED")
	}
	return err
}

const (
	// maxConcurrentUpdates bounds how many handlers may run at the same time.
	// It is enforced when a handler starts, not when an update is queued, so
	// one user's backlog can never occupy the execution budget other users
	// need.
	maxConcurrentUpdates = 32
	// maxPendingUpdates bounds accepted-but-unfinished updates (running or
	// queued) so a burst cannot exhaust memory. It must comfortably exceed
	// Telegram's batch size (100) so one full batch plus in-flight work still
	// fits; beyond it Enqueue blocks, which stops fetching while the already
	// admitted remainder stays durable.
	maxPendingUpdates = 1024
	// shutdownDrainTimeout is how long Start lets running handlers finish before
	// it cancels them. Aborting is safe now: the update row stays running and the
	// next start records a verification notice, so a slow handler costs a notice,
	// never a duplicate or a lost update.
	shutdownDrainTimeout = 8 * time.Second
	// replayClaimTTL bounds terminal rows only. Non-terminal rows (admitted,
	// running, verify) are written WITHOUT a TTL: expiring one would silently lose
	// the update it represents.
	replayClaimTTL = 72 * time.Hour
	// replayNoticeInterval is how often undelivered notices are retried.
	replayNoticeInterval = time.Minute
	// replayNoticeTimeout bounds a single notice delivery attempt.
	replayNoticeTimeout = 10 * time.Second
)

// Bot manages the Telegram bot lifecycle (long polling).
type Bot struct {
	token        string
	handler      *Handler
	client       *http.Client
	logger       *slog.Logger
	offset       int
	baseAPI      string        // Bot API prefix; tests point it at an httptest server
	drainTimeout time.Duration // overrides shutdownDrainTimeout in tests
	replay       *replayGuard  // durable ledger; nil disables admission
	noticeEvery  time.Duration // overrides replayNoticeInterval in tests
}

// NewBot creates a new Bot.
func NewBot(token string, handler *Handler, logger *slog.Logger) *Bot {
	return &Bot{
		token:        token,
		handler:      handler,
		client:       &http.Client{Timeout: 60 * time.Second},
		logger:       logger,
		baseAPI:      telegramAPI,
		drainTimeout: shutdownDrainTimeout,
	}
}

// replayStore is the durable key/value surface the replay guard needs. It is
// declared here (consumer-side) so the Telegram adapter never imports the
// Badger adapter; main wires the concrete store in.
//
// Get reports found=false only when the key is genuinely absent. Any other
// failure MUST be returned as an error: the guard treats an undecidable read as
// "refuse to execute", so a misreported miss could replay side effects.
type replayStore interface {
	Get(key string, dest interface{}) (found bool, err error)
	SetWithTTL(key string, value interface{}, ttl time.Duration) error
	Delete(key string) error
	Scan(prefix string) ([][]byte, error)
	// ApplyBatch writes insertIfAbsent and put in ONE transaction and returns
	// the keys actually inserted. Admission depends on it: the batch payloads
	// and the fetch cursor must become durable together, or Telegram could
	// forget an update this bot never stored.
	ApplyBatch(insertIfAbsent, put map[string][]byte) (inserted []string, err error)
}

// ReplayStoreFuncs adapts plain functions to replayStore, letting main pass the
// Badger store without this package depending on it.
type ReplayStoreFuncs struct {
	GetFn        func(key string, dest interface{}) (bool, error)
	SetFn        func(key string, value interface{}, ttl time.Duration) error
	DeleteFn     func(key string) error
	ScanFn       func(prefix string) ([][]byte, error)
	ApplyBatchFn func(insertIfAbsent, put map[string][]byte) ([]string, error)
}

// Get implements replayStore.
func (f ReplayStoreFuncs) Get(key string, dest interface{}) (bool, error) {
	return f.GetFn(key, dest)
}

// SetWithTTL implements replayStore.
func (f ReplayStoreFuncs) SetWithTTL(key string, value interface{}, ttl time.Duration) error {
	return f.SetFn(key, value, ttl)
}

// Delete implements replayStore.
func (f ReplayStoreFuncs) Delete(key string) error { return f.DeleteFn(key) }

// Scan implements replayStore.
func (f ReplayStoreFuncs) Scan(prefix string) ([][]byte, error) { return f.ScanFn(prefix) }

// ApplyBatch implements replayStore.
func (f ReplayStoreFuncs) ApplyBatch(insertIfAbsent, put map[string][]byte) ([]string, error) {
	return f.ApplyBatchFn(insertIfAbsent, put)
}

// updateState is the durable lifecycle of one Telegram update.
//
// The distinction that matters is whether the handler ever started: an admitted
// row provably produced no side effects and is always safe to replay, while a
// running row may already have written to Notion and must never be replayed
// automatically.
type updateState string

const (
	// stateAdmitted: durably owned by this bot, handler never started.
	stateAdmitted updateState = "admitted"
	// stateRunning: the handler was invoked and its outcome is unknown.
	stateRunning updateState = "running"
	// stateDone: the handler returned. NOT proof of success — HandleUpdate
	// returns no error — only proof that repeating it would duplicate effects.
	stateDone updateState = "done"
	// stateVerify: interrupted work whose outcome is ambiguous; the user has
	// been (or will be) told to check /history.
	stateVerify updateState = "verify"
)

// States written by the previous claim design, kept only so an existing
// deployment can migrate without losing or re-running work.
const (
	legacyStatePending = "pending"
	legacyStateHandled = "handled"
)

// errNoNoticeTarget reports an update with no chat to notify (a system update).
// It is a permanent condition, not a transient failure.
var errNoNoticeTarget = errors.New("update has no chat to notify")

// errDispatcherStopped reports that a fatal condition fenced the dispatcher.
var errDispatcherStopped = errors.New("update dispatcher stopped after a fatal error")

// errResetBlocked reports a /reset that must wait for older, still-unfinished
// updates. Running it first would wipe state that those updates then recreate,
// resurrecting pre-reset actions.
var errResetBlocked = errors.New("reset must wait for older pending updates")

// errNotAdmitted reports a record that is missing when the dispatcher is about
// to run it. The handler must not run in that case: without a durable admitted
// row there is nothing to prevent a redelivery from duplicating the effects.
var errNotAdmitted = errors.New("update has no durable admission record")

// replayPrefix is the ledger namespace. It MUST match
// badger.IdempotencyPrefix, which Store.Reset preserves; a test in this package
// asserts the two agree.
const replayPrefix = "gam:idem:"

// Every ledger key is scoped by bot identity:
//
//	gam:idem:<scope>:upd:<update_id>   updateRecord
//	gam:idem:<scope>:cursor            fetchCursor
//	gam:idem:<scope>:notice:<update_id> replayNotice
const (
	replayClaimPrefix  = replayPrefix
	replayNoticePrefix = replayPrefix
)

// botScope derives the stable, non-secret identity of a bot from its token.
//
// A Telegram bot token is "<bot id>:<secret>". The numeric bot id survives
// token rotation — and rotation is exactly what used to split the ledger: the
// new token produced a new scope, so every row written before the rotation
// became invisible to the running bot (admitted rows were never replayed,
// notices were never delivered, and the fetch cursor restarted from 0). The bot
// id is public (it prefixes every Bot API URL), so scoping by it leaks nothing.
//
// A token without a numeric prefix (a test double, a mock) falls back to the
// previous derivation. Rows already written under that derivation are adopted on
// the next start by adoptLegacyScopes.
func botScope(token string) string {
	if token == "" {
		return "anon"
	}
	// The "bot:" prefix cannot appear in a legacy 16-hex scope, so a live scope
	// and a legacy scope never overlap and a migration can never read its own
	// output back.
	if id, _, ok := strings.Cut(token, ":"); ok && isNumeric(id) {
		return "bot:" + id
	}
	return legacyBotScope(token)
}

// legacyBotScope is the previous derivation: a truncated hash of the whole
// token. It changes on every rotation, which is why it is only kept as a
// migration source, never as the live scope.
func legacyBotScope(token string) string {
	if token == "" {
		return "anon"
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// isNumeric reports whether s is a non-empty run of ASCII digits.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := range s {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ledgerClaimKey is the update-record key for one update in one ledger scope.
func ledgerClaimKey(scope string, updateID int) string {
	return replayClaimPrefix + scope + ":upd:" + strconv.Itoa(updateID)
}

// ledgerNoticeKey is the pending-notice key for one update in one scope.
func ledgerNoticeKey(scope string, updateID int) string {
	return replayNoticePrefix + scope + ":notice:" + strconv.Itoa(updateID)
}

// ledgerNoticePrefix is the notice scan prefix of one scope.
func ledgerNoticePrefix(scope string) string {
	return replayNoticePrefix + scope + ":notice:"
}

// ledgerRecordPrefix is the update-record scan prefix of one scope.
func ledgerRecordPrefix(scope string) string {
	return replayClaimPrefix + scope + ":upd:"
}

// ledgerCursorKey is the fetch-cursor key of one scope.
func ledgerCursorKey(scope string) string {
	return replayClaimPrefix + scope + ":cursor"
}

// claimKey is the ledger key for one update of this bot.
func (g *replayGuard) claimKey(updateID int) string {
	return ledgerClaimKey(g.scope, updateID)
}

// noticeKey is the pending-notice key for one update of this bot.
func (g *replayGuard) noticeKey(updateID int) string {
	return ledgerNoticeKey(g.scope, updateID)
}

// noticeScanPrefix scopes the notice scan to this bot only.
func (g *replayGuard) noticeScanPrefix() string {
	return ledgerNoticePrefix(g.scope)
}

// recordScanPrefix scopes the update-record scan to this bot only.
func (g *replayGuard) recordScanPrefix() string {
	return ledgerRecordPrefix(g.scope)
}

// cursorKey is the durable fetch cursor for this bot.
func (g *replayGuard) cursorKey() string {
	return ledgerCursorKey(g.scope)
}

// updateRecord is the durable ledger row for one update id.
//
// Payload is the exact Update JSON, stored at admission so a row can be replayed
// after a restart without asking Telegram for it again. Rows that are not
// terminal (admitted, running) MUST NOT carry a TTL: expiring them would silently
// lose the update.
type updateRecord struct {
	UpdateID int64           `json:"update_id"`
	State    updateState     `json:"state"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	ChatID   int64           `json:"chat_id,omitempty"`
	ThreadID int             `json:"thread_id,omitempty"`
	// UserID is the ordering scope of the update (see updateKey), stored so a
	// per-user readiness check does not have to decode the payload. Rows written
	// before it existed leave it 0; olderPendingForUser falls back to the payload
	// for those.
	UserID int64     `json:"user_id,omitempty"`
	At     time.Time `json:"at"`
}

// fetchCursor is the next getUpdates offset, advanced only inside the same
// transaction that admits the batch it covers.
//
// UpdatedAt is when the cursor was last advanced. It exists because a cursor is
// only meaningful for as long as Telegram's update-id sequence is: Telegram
// documents that ids may restart from a lower value when a bot has been offline
// for a long time (about a week). Polling with an offset above the restarted
// numbering is not merely stale, it is destructive — Telegram treats every new
// update as already acknowledged and forgets it, so the bot goes silent with no
// error to observe.
type fetchCursor struct {
	Offset    int       `json:"offset"`
	UpdatedAt time.Time `json:"updated_at"`
}

// telegramUpdateIDResetWindow bounds how long a stored fetch cursor may be
// trusted. Past it the offset is discarded and polling restarts at 0, which
// costs at most one round of duplicate deliveries that the ledger already
// dedupes, and is the only way to observe a restarted numbering at all.
//
// Telegram describes the restart as happening after "about a week" without
// stating a boundary, so this deliberately undercuts it: a cursor trusted for
// the full week could still be above a sequence that restarted at day five,
// which silently suppresses every new update (and, worse, the long poll would
// keep returning nothing while the user sees a dead bot). Erring early only
// costs duplicate deliveries.
const telegramUpdateIDResetWindow = 5 * 24 * time.Hour

// replayNotice is a durable, retried message telling the user that an
// interrupted update was not re-run and needs manual verification.
type replayNotice struct {
	UpdateID int       `json:"update_id"`
	ChatID   int64     `json:"chat_id"`
	ThreadID int       `json:"thread_id,omitempty"`
	Text     string    `json:"text"`
	At       time.Time `json:"at"`
}

// replayGuard owns the durable ledger: which updates this bot has taken
// ownership of, how far Telegram has been acknowledged, and which interrupted
// updates need manual verification.
//
// Two cursors, deliberately separate:
//   - the fetch cursor (Telegram's offset) advances inside the admission
//     transaction, so polling never waits for handlers and Telegram may forget
//     the updates immediately;
//   - the per-update state machine is the processing record of truth.
//
// Tradeoff — at-most-once per update id for side effects, not exactly-once.
// Notion and Badger cannot commit together, so no local record can prove whether
// an interrupted write landed. The guard therefore never replays a running row:
// it records a durable notice asking the user to verify. An admitted row never
// started, so it is always safe to replay.
type replayGuard struct {
	store  replayStore
	logger *slog.Logger
	scope  string // bot identity, see botScope
	// migrateFrom lists ledger scopes written by an earlier release that must
	// be adopted into scope. A token rotation changes the old derivation (a
	// hash of the whole token), and without this the rows of the previous
	// token — pending work, undelivered notices, the fetch cursor — would be
	// invisible to the running bot: silently never replayed and never
	// delivered.
	migrateFrom []string
	// cursorAt is when the stored fetch cursor was last advanced. The poll loop
	// uses it to notice that the cursor outlived Telegram's update-id reset
	// window even while the process kept running: after a long quiet period
	// Telegram may restart its ids, and polling with an offset above the new
	// numbering makes it drop every update silently.
	cursorAt time.Time
}

// admitBatch durably records updates and advances the fetch cursor in one
// transaction, returning the ids that were newly inserted.
//
// Telegram must never forget an update before this returns nil: the cursor is
// what makes Telegram drop everything below it. Records already present are left
// untouched, so a duplicate delivery cannot resurrect or re-enqueue a row.
func (g *replayGuard) admitBatch(updates []Update, nextOffset int) ([]int64, error) {
	insert := make(map[string][]byte, len(updates))
	byID := make(map[int64]Update, len(updates))
	for _, u := range updates {
		payload, err := json.Marshal(u)
		if err != nil {
			return nil, fmt.Errorf("encode update %d: %w", u.UpdateID, err)
		}
		chatID, threadID, _ := noticeTarget(u)
		userID, _ := updateKey(u)
		rec := updateRecord{
			UpdateID: int64(u.UpdateID),
			State:    stateAdmitted,
			Payload:  payload,
			ChatID:   chatID,
			ThreadID: threadID,
			UserID:   userID,
			At:       time.Now(),
		}
		raw, err := json.Marshal(rec)
		if err != nil {
			return nil, fmt.Errorf("encode record %d: %w", u.UpdateID, err)
		}
		insert[g.claimKey(u.UpdateID)] = raw
		byID[int64(u.UpdateID)] = u
	}
	cursorRaw, err := json.Marshal(fetchCursor{Offset: nextOffset, UpdatedAt: time.Now()})
	if err != nil {
		return nil, fmt.Errorf("encode fetch cursor: %w", err)
	}
	inserted, err := g.store.ApplyBatch(insert, map[string][]byte{g.cursorKey(): cursorRaw})
	if err != nil {
		return nil, fmt.Errorf("admit batch: %w", err)
	}
	ids := make([]int64, 0, len(inserted))
	for _, key := range inserted {
		// Keys are "<prefix><scope>:upd:<id>"; the record carries the id, so
		// parse from the value instead of the key to stay independent of the
		// key format.
		var rec updateRecord
		if err := json.Unmarshal(insert[key], &rec); err != nil {
			continue
		}
		if _, ok := byID[rec.UpdateID]; ok {
			ids = append(ids, rec.UpdateID)
		}
	}
	// Ledger keys sort lexicographically ("10" < "9"), but processing must
	// follow numeric update_id order or per-user FIFO would invert.
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// recover returns the durable fetch cursor and every non-done row, ordered by
// update id so a replay preserves per-user arrival order.
//
// Legacy rows from the previous claim design carry no payload: "handled" maps to
// done, and "pending" is ambiguous but unnotifiable, so it is reported to the
// operator and closed instead of being replayed.
func (g *replayGuard) recover() (int, []updateRecord, error) {
	if err := g.adoptLegacyScopes(); err != nil {
		return 0, nil, err
	}

	cursor := 0
	var cur fetchCursor
	found, err := g.store.Get(g.cursorKey(), &cur)
	if err != nil {
		return 0, nil, fmt.Errorf("read fetch cursor: %w", err)
	}
	if found {
		cursor = cur.Offset
		g.cursorAt = cur.UpdatedAt
		switch {
		case cur.Offset <= 0:
			cursor = 0
		case cur.UpdatedAt.IsZero() || time.Since(cur.UpdatedAt) > telegramUpdateIDResetWindow:
			// Telegram may have restarted its update ids while this bot was
			// away. Trusting the stored offset would make it discard every new
			// update, so poll from the beginning instead: Telegram redelivers
			// whatever it still holds, and admission dedupes the repeats.
			//
			// The terminal rows of the previous numbering go with it. They can
			// no longer be redelivered (Telegram keeps updates for a day, and
			// this cursor is older than a week), while keeping them would make a
			// NEW update that reuses an old id look already processed, so
			// admission would skip it and the bot would go quiet.
			g.logger.Warn("fetch cursor is older than Telegram's update-id reset window; polling from 0",
				"offset", cur.Offset, "updated_at", cur.UpdatedAt)
			if err := g.purgeTerminal(); err != nil {
				// Fail closed: a stale terminal row can swallow a new update
				// that reuses its id, and that failure is silent. Refusing to
				// start is the only way it surfaces.
				return 0, nil, fmt.Errorf("purge terminal rows after an update-id reset: %w", err)
			}
			cursor = 0
		}
	}

	raw, err := g.store.Scan(g.recordScanPrefix())
	if err != nil {
		return 0, nil, fmt.Errorf("scan ledger: %w", err)
	}
	pending := make([]updateRecord, 0, len(raw))
	for _, entry := range raw {
		var rec updateRecord
		if err := json.Unmarshal(entry, &rec); err != nil {
			g.logger.Warn("could not decode ledger row", "error", err)
			continue
		}
		switch rec.State {
		case stateDone:
			continue
		case legacyStateHandled:
			continue
		case legacyStatePending:
			// Ambiguous under the old design (claim written before enqueue) and
			// unnotifiable (no payload, no chat): report once, then close so it
			// does not resurface on every start.
			g.logger.Warn("closing legacy pending claim with unknown outcome; verify manually",
				"update_id", rec.UpdateID)
			if err := g.markDone(int(rec.UpdateID)); err != nil {
				g.logger.Warn("could not close legacy claim", "update_id", rec.UpdateID, "error", err)
			}
			continue
		}
		pending = append(pending, rec)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].UpdateID < pending[j].UpdateID })
	return cursor, pending, nil
}

// adoptLegacyScopes copies the ledger of an earlier release into the live scope,
// so a token rotation keeps pending work, undelivered notices and the fetch
// cursor instead of starting from an empty ledger.
//
// The copy is one transaction and never overwrites a row the live scope already
// has (insert-if-absent), so a repeated start — or a rotation that is rolled
// back — is harmless. Only after the copy is durable are the old keys deleted;
// leaving them behind would be invisible but would keep a stale cursor around
// for a future rollback.
func (g *replayGuard) adoptLegacyScopes() error {
	for _, from := range g.migrateFrom {
		if from == "" || from == g.scope {
			continue
		}
		if err := g.adoptScope(from); err != nil {
			return err
		}
	}
	return nil
}

// adoptScope adopts one legacy scope into g.scope.
func (g *replayGuard) adoptScope(from string) error {
	insert := make(map[string][]byte)
	var stale []string

	rows, err := g.store.Scan(ledgerRecordPrefix(from))
	if err != nil {
		return fmt.Errorf("scan ledger of scope %q: %w", from, err)
	}
	for _, entry := range rows {
		var rec updateRecord
		if err := json.Unmarshal(entry, &rec); err != nil {
			g.logger.Warn("could not decode ledger row of a previous scope", "scope", from, "error", err)
			continue
		}
		key := ledgerClaimKey(g.scope, int(rec.UpdateID))
		insert[key] = entry
		stale = append(stale, ledgerClaimKey(from, int(rec.UpdateID)))
	}

	notices, err := g.store.Scan(ledgerNoticePrefix(from))
	if err != nil {
		return fmt.Errorf("scan notices of scope %q: %w", from, err)
	}
	for _, entry := range notices {
		var rec replayNotice
		if err := json.Unmarshal(entry, &rec); err != nil {
			g.logger.Warn("could not decode notice of a previous scope", "scope", from, "error", err)
			continue
		}
		key := ledgerNoticeKey(g.scope, rec.UpdateID)
		insert[key] = entry
		stale = append(stale, ledgerNoticeKey(from, rec.UpdateID))
	}

	var cur fetchCursor
	found, err := g.store.Get(ledgerCursorKey(from), &cur)
	if err != nil {
		return fmt.Errorf("read fetch cursor of scope %q: %w", from, err)
	}
	if found {
		raw, err := json.Marshal(cur)
		if err != nil {
			return fmt.Errorf("encode adopted fetch cursor: %w", err)
		}
		insert[ledgerCursorKey(g.scope)] = raw
		stale = append(stale, ledgerCursorKey(from))
	}

	if len(insert) == 0 {
		return nil
	}
	if _, err := g.store.ApplyBatch(insert, nil); err != nil {
		return fmt.Errorf("adopt ledger scope %q: %w", from, err)
	}
	g.logger.Info("adopted ledger rows from a previous token scope", "from", from, "rows", len(insert))
	for _, key := range stale {
		if err := g.store.Delete(key); err != nil {
			g.logger.Warn("could not remove adopted ledger key", "key", key, "error", err)
		}
	}
	return nil
}

// purgeTerminal deletes every finished row of this bot.
//
// It is only called when Telegram's update-id sequence is known to have
// restarted. Those ids are gone for good, so a terminal row protects nothing —
// while keeping it would make a NEW update that reuses an old id look already
// processed, and admission would silently drop it.
func (g *replayGuard) purgeTerminal() error {
	raw, err := g.store.Scan(g.recordScanPrefix())
	if err != nil {
		return fmt.Errorf("scan ledger to purge terminal rows: %w", err)
	}
	for _, entry := range raw {
		var rec updateRecord
		if err := json.Unmarshal(entry, &rec); err != nil {
			continue
		}
		switch rec.State {
		case stateDone, legacyStateHandled:
		default:
			continue
		}
		if err := g.store.Delete(g.claimKey(int(rec.UpdateID))); err != nil {
			return fmt.Errorf("purge terminal row %d: %w", rec.UpdateID, err)
		}
	}
	return nil
}

// olderPending reports how many non-terminal rows precede updateID. A /reset is
// only safe once that count is zero: otherwise an older update (from any user)
// can still write after the wipe and resurrect pre-reset state.
func (g *replayGuard) olderPending(updateID int) (int, error) {
	return g.countOlder(updateID, nil)
}

// olderPendingForUser reports how many non-terminal rows of one user precede
// updateID.
//
// This is the readiness rule for every non-reset update: a user's updates are
// ordered only against each other (the dispatcher guarantees it in-process, and
// this keeps it true across a deferral and a restart). Applying the global
// olderPending here instead made one user's unfinished row park every other
// user's deferred update, i.e. one update per retry tick for the whole bot.
func (g *replayGuard) olderPendingForUser(updateID int, userID int64) (int, error) {
	return g.countOlder(updateID, &userID)
}

// countOlder counts unfinished rows older than updateID, optionally restricted
// to one user. A nil userID counts every user (the reset barrier).
func (g *replayGuard) countOlder(updateID int, userID *int64) (int, error) {
	raw, err := g.store.Scan(g.recordScanPrefix())
	if err != nil {
		return 0, fmt.Errorf("scan ledger for pending work: %w", err)
	}
	count := 0
	for _, entry := range raw {
		var rec updateRecord
		if err := json.Unmarshal(entry, &rec); err != nil {
			continue
		}
		switch rec.State {
		case stateAdmitted, stateRunning, stateVerify:
		default:
			continue
		}
		if rec.UpdateID >= int64(updateID) {
			continue
		}
		if userID != nil {
			// Rows written before UserID existed carry only the payload; decode
			// it rather than treating an unknown owner as "someone else", which
			// would let a later update overtake an older one.
			if rec.UserID == 0 {
				var u Update
				if err := json.Unmarshal(rec.Payload, &u); err != nil {
					continue
				}
				rec.UserID, _ = updateKey(u)
			}
			if rec.UserID != *userID {
				continue
			}
		}
		count++
	}
	return count, nil
}

// effectiveText returns the text HandleUpdate routes on: the message text, or
// the photo caption when the message carries no text.
//
// Reset recognition MUST use this instead of reading Message.Text directly: a
// "/reset confirm" sent as a photo caption reaches cmdReset exactly like a text
// command, so the barrier would otherwise miss a wipe that really happens and
// let it run beside in-flight updates.
func effectiveText(msg *Message) string {
	if msg == nil {
		return ""
	}
	if text := strings.TrimSpace(msg.Text); text != "" {
		return text
	}
	return strings.TrimSpace(msg.Caption)
}

// isDestructiveReset reports whether an update actually wipes state: a
// confirmed /reset or its confirm button.
//
// The plain /reset prompt only renders a keyboard and reset:cancel does nothing,
// so neither may raise the global barrier — doing so would stall every user for
// a no-op.
//
// Recognition MUST go through parseCommandName, the same parser the handler uses
// to route the command. Anything this function accepts and the handler does not
// would raise a barrier for an update that never wipes anything (stalling every
// user), while anything the handler wipes state for and this function misses
// would let a wipe run without the barrier. That covers capitalisation
// ("/RESET CONFIRM"), the group-chat "@BotName" suffix ("/reset@MyBot
// confirm"), and a photo caption in both directions.
func isDestructiveReset(u Update) bool {
	if u.CallbackQuery != nil {
		return u.CallbackQuery.Data == "reset:confirm"
	}
	if u.Message == nil {
		return false
	}
	text := effectiveText(u.Message)
	name, ok := parseCommandName(text)
	if !ok || name != "reset" {
		return false
	}
	// Same argument the handler's cmdReset reads: the second whitespace field,
	// compared case-insensitively.
	fields := strings.Fields(text)
	return len(fields) >= 2 && strings.EqualFold(fields[1], "confirm")
}

// isOwner reports whether the update comes from the configured owner. A
// non-owner's /reset is silently ignored by the handler, so it must not raise
// the barrier either.
func (h *Handler) isOwner(u Update) bool {
	if h == nil || h.ownerID == 0 {
		return false
	}
	if u.Message != nil && u.Message.From != nil {
		return u.Message.From.ID == h.ownerID
	}
	if u.CallbackQuery != nil && u.CallbackQuery.From != nil {
		return u.CallbackQuery.From.ID == h.ownerID
	}
	return false
}

// resetBarrier reports whether u must hold the global reset barrier.
func (b *Bot) resetBarrier(u Update) bool {
	return isDestructiveReset(u) && b.handler.isOwner(u)
}

// markRunning records that the handler is about to be invoked. It must succeed
// before the handler runs: without it a crash would leave no evidence that side
// effects were possible.
func (g *replayGuard) markRunning(updateID int) error {
	var rec updateRecord
	found, err := g.store.Get(g.claimKey(updateID), &rec)
	if err != nil {
		return fmt.Errorf("read update %d before running: %w", updateID, err)
	}
	if !found {
		return fmt.Errorf("update %d: %w", updateID, errNotAdmitted)
	}
	if rec.State != stateAdmitted {
		return fmt.Errorf("update %d is %q, refusing to run", updateID, rec.State)
	}
	rec.State = stateRunning
	rec.At = time.Now()
	// No TTL: a running row must survive until it is resolved.
	if err := g.store.SetWithTTL(g.claimKey(updateID), rec, 0); err != nil {
		return fmt.Errorf("mark update %d running: %w", updateID, err)
	}
	return nil
}

// markDone closes a row. "Done" means the handler returned, not that the write
// succeeded (HandleUpdate returns no error); it only promises that a replay
// would duplicate the effects.
func (g *replayGuard) markDone(updateID int) error {
	var rec updateRecord
	found, err := g.store.Get(g.claimKey(updateID), &rec)
	if err != nil {
		return fmt.Errorf("read update %d before completing: %w", updateID, err)
	}
	if !found {
		return fmt.Errorf("update %d: %w", updateID, errNotAdmitted)
	}
	rec.State = stateDone
	rec.At = time.Now()
	rec.Payload = nil // terminal rows need no replay data
	if err := g.store.SetWithTTL(g.claimKey(updateID), rec, replayClaimTTL); err != nil {
		return fmt.Errorf("mark update %d done: %w", updateID, err)
	}
	return nil
}

// markVerified records the verification notice and closes the row, in that
// order: a row must never become terminal before the user's only signal is
// durable. It is used for work whose outcome is ambiguous but will not be
// retried (a contained panic).
func (g *replayGuard) markVerified(u Update) error {
	if err := g.recordNotice(u); err != nil {
		if !errors.Is(err, errNoNoticeTarget) {
			return err
		}
		g.logger.Error("ambiguous update has no chat to notify; closing it", "update_id", u.UpdateID)
	}
	return g.markDone(u.UpdateID)
}

// resolveInterrupted turns rows left running by a previous process into
// verification notices, then closes them. It never re-executes them.
func (g *replayGuard) resolveInterrupted(rec updateRecord) error {
	if len(rec.Payload) == 0 {
		g.logger.Warn("interrupted update has no stored payload; cannot notify",
			"update_id", rec.UpdateID, "state", string(rec.State))
		return g.markDone(int(rec.UpdateID))
	}
	var u Update
	if err := json.Unmarshal(rec.Payload, &u); err != nil {
		g.logger.Warn("could not decode interrupted update", "update_id", rec.UpdateID, "error", err)
		return g.markDone(int(rec.UpdateID))
	}
	g.logger.Warn("interrupted update needs manual verification", "update_id", rec.UpdateID)
	if err := g.recordNotice(u); err != nil {
		if errors.Is(err, errNoNoticeTarget) {
			g.logger.Error("interrupted update has no chat to notify; closing it",
				"update_id", rec.UpdateID)
			return g.markDone(int(rec.UpdateID))
		}
		// Leave the row as-is so the next start retries the notice.
		return err
	}
	return g.markDone(int(rec.UpdateID))
}

// noticeTarget extracts where a recovery notice should go.
func noticeTarget(u Update) (chatID int64, threadID int, ok bool) {
	if u.Message != nil && u.Message.Chat != nil {
		return u.Message.Chat.ID, noticeThreadID(u.Message.Chat, u.Message.MessageThreadID), true
	}
	if u.CallbackQuery != nil && u.CallbackQuery.Message != nil && u.CallbackQuery.Message.Chat != nil {
		chat := u.CallbackQuery.Message.Chat
		return chat.ID, noticeThreadID(chat, u.CallbackQuery.Message.MessageThreadID), true
	}
	return 0, 0, false
}

// interruptedText explains, without claiming success or failure, what happened
// and what the user should do. It never says the action succeeded.
//
// A reset needs its own wording: /history cannot describe the state a half-run
// wipe leaves behind. Both stores are touched — Notion pages are archived before
// BadgerDB is wiped — so an interruption can leave some pages archived and some
// data already deleted. Saying "check /history" there would understate the
// damage and invite the owner to assume nothing happened.
func interruptedText(u Update) (chatID int64, threadID int, text string, ok bool) {
	chatID, threadID, ok = noticeTarget(u)
	if !ok {
		return 0, 0, "", false
	}
	if isDestructiveReset(u) {
		return chatID, threadID,
			"⚠️ Reset terputus di tengah jalan dan TIDAK diulang otomatis, supaya tidak menghapus dua kali.\n\n" +
				"Keadaannya bisa sebagian:\n" +
				"• Notion: sebagian halaman jurnal mungkin sudah diarsipkan.\n" +
				"• BadgerDB: sebagian data gamifikasi, XP, trade & cache mungkin sudah terhapus.\n\n" +
				"Cek kedua tempat itu sebelum memakai bot lagi. Bila ingin keadaan bersih, jalankan /reset lagi.", true
	}
	if u.Message != nil {
		return chatID, threadID,
			"⚠️ Pesan ini terputus saat diproses dan tidak diulang otomatis, supaya tidak tercatat dua kali. " +
				"Cek /history: kalau trade-nya belum ada, kirim ulang pesannya.", true
	}
	return chatID, threadID,
		"⚠️ Aksi ini terputus saat diproses dan tidak diulang otomatis, supaya tidak terjadi dua kali. " +
			"Cek /history untuk memastikan hasilnya.", true
}

// noticeThreadID returns the topic a notice belongs to, or 0 when the chat has
// no topics. message_thread_id is only meaningful for forum supergroups; sending
// it to a private chat or a plain group makes the Bot API reject the message, so
// the notice would be lost.
//
// The notice text never contains trade details (no symbol, no amounts): in a
// group the message is visible to every member, so it only points the requester
// at /history, which is answered privately.
func noticeThreadID(chat *Chat, threadID int) int {
	if chat == nil || !chat.IsForum || threadID <= 0 {
		return 0
	}
	return threadID
}

// recordNotice durably stores the recovery notice for an update whose handling
// did not complete.
func (g *replayGuard) recordNotice(u Update) error {
	chatID, threadID, text, ok := interruptedText(u)
	if !ok {
		return errNoNoticeTarget
	}
	rec := replayNotice{
		UpdateID: u.UpdateID,
		ChatID:   chatID,
		ThreadID: threadID,
		Text:     text,
		At:       time.Now(),
	}
	// No TTL: an undelivered notice is the user's only signal that an action was
	// interrupted. Expiring it would drop that signal silently, so it lives
	// until the send succeeds and deliverNotice deletes it.
	if err := g.store.SetWithTTL(g.noticeKey(u.UpdateID), rec, 0); err != nil {
		return fmt.Errorf("record replay notice %d: %w", u.UpdateID, err)
	}
	return nil
}

// deliverNotice attempts to send one stored notice, deleting it on success.
func (g *replayGuard) deliverNotice(ctx context.Context, sender *Sender, rec replayNotice) {
	if sender == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, replayNoticeTimeout)
	defer cancel()
	if _, err := sender.SendText(ctx, rec.ChatID, rec.Text, rec.ThreadID); err != nil {
		g.logger.Warn("could not deliver replay notice", "update_id", rec.UpdateID, "error", err)
		return
	}
	if err := g.store.Delete(g.noticeKey(rec.UpdateID)); err != nil {
		g.logger.Warn("could not clear delivered replay notice", "update_id", rec.UpdateID, "error", err)
	}
}

// flushNotices delivers every stored notice once.
func (g *replayGuard) flushNotices(ctx context.Context, sender *Sender) {
	raw, err := g.store.Scan(g.noticeScanPrefix())
	if err != nil {
		g.logger.Warn("could not scan replay notices", "error", err)
		return
	}
	for _, entry := range raw {
		var rec replayNotice
		if err := json.Unmarshal(entry, &rec); err != nil {
			g.logger.Warn("could not decode replay notice", "error", err)
			continue
		}
		if ctx.Err() != nil {
			return
		}
		g.deliverNotice(ctx, sender, rec)
	}
}

// retryDeferred re-enqueues updates whose start was deferred: a /reset waiting
// for older work, and the updates that must wait behind that reset or behind an
// older deferred update of their own user.
//
// It must be joined before the dispatcher is waited on and before the store is
// closed (it calls EnqueueRetry, which registers work with the dispatcher). Every
// failure that would strand a durable row is reported as fatal instead of being
// logged and skipped.
func (b *Bot) retryDeferred(ctx context.Context, deferred chan Update, dispatcher *updateDispatcher, replay *replayGuard, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(replayNoticeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !b.retryDeferredOnce(ctx, deferred, dispatcher, replay) {
			return
		}
	}
}

// retryDeferredOnce retries every deferred update that was queued when it was
// called, at most once each.
//
// The pass is bounded by the queue length measured up front: an entry that is
// re-queued (still blocked) must not be read again in the same pass, or the loop
// would spin forever. Cancellation is checked on every iteration so shutdown is
// never delayed by a long pass.
func (b *Bot) retryDeferredOnce(ctx context.Context, deferred chan Update, dispatcher *updateDispatcher, replay *replayGuard) bool {
	for range len(deferred) {
		if ctx.Err() != nil {
			return false
		}
		var u Update
		select {
		case u = <-deferred:
		default:
			return true // the queue drained earlier than expected
		}

		// While a reset is waiting, only the reset and the OLDER updates it
		// depends on may be retried. Newer updates would run first and then be
		// wiped; older ones are what the reset is waiting for, so holding them
		// back would deadlock the barrier.
		if gate := dispatcher.gateUpdate(); gate != 0 && u.UpdateID > gate {
			if !requeue(deferred, u) {
				dispatcher.fail(fmt.Errorf("deferred update %d could not be re-queued", u.UpdateID))
				return false
			}
			continue
		}
		// Readiness is scoped to what the update actually orders against:
		//   - a destructive reset waits for the WHOLE ledger (olderPending),
		//     because a write from any user after the wipe would resurrect
		//     pre-reset state;
		//   - every other update waits only for its own user's older rows, which
		//     is the same FIFO the dispatcher enforces in-process. Using the
		//     global rule here parked unrelated users' deferred updates behind
		//     one another and let at most one run per retry tick.
		// An update without a user has no ordering scope, so it falls back to the
		// global rule.
		var (
			older int
			err   error
		)
		switch key, keyed := updateKey(u); {
		case isDestructiveReset(u):
			older, err = replay.olderPending(u.UpdateID)
		case keyed:
			older, err = replay.olderPendingForUser(u.UpdateID, key)
		default:
			older, err = replay.olderPending(u.UpdateID)
		}
		if err != nil {
			// Cannot decide whether it is safe to run: fail closed.
			dispatcher.fail(fmt.Errorf("deferred update %d: %w", u.UpdateID, err))
			return false
		}
		if older > 0 {
			if !requeue(deferred, u) {
				dispatcher.fail(fmt.Errorf("deferred update %d could not be re-queued", u.UpdateID))
				return false
			}
			continue
		}
		// The update still holds its pending budget from admission, so the
		// retry must not take a second one.
		if err := dispatcher.EnqueueRetry(ctx, u); err != nil {
			if ctx.Err() != nil {
				return false // shutting down: the row is durable and replays
			}
			dispatcher.fail(fmt.Errorf("deferred update %d could not be re-enqueued: %w", u.UpdateID, err))
			return false
		}
	}
	return true
}

// requeue puts u back for a later tick, reporting false when the buffer is full
// (the caller must fail-stop: a dropped update would be stranded until restart).
func requeue(deferred chan Update, u Update) bool {
	select {
	case deferred <- u:
		return true
	default:
		return false
	}
}

// flushNoticesSafely delivers pending notices without letting a panic escape
// the background loop: a crash there would strand every later notice.
func (b *Bot) flushNoticesSafely(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error("panic while delivering replay notices", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	b.replay.flushNotices(ctx, b.handler.sender)
}

// newReplayGuard builds the guard for one bot token, including the ledger scope
// of the previous release when it differs — which it does for every numeric
// token, because the old derivation hashed the whole token. Adoption is a no-op
// once the old scope is empty.
func newReplayGuard(store replayStore, logger *slog.Logger, token string) *replayGuard {
	scope := botScope(token)
	g := &replayGuard{store: store, logger: logger, scope: scope}
	if legacy := legacyBotScope(token); legacy != scope {
		g.migrateFrom = append(g.migrateFrom, legacy)
	}
	return g
}

// SetReplayGuard installs the durable per-update claim store. Without it the
// dispatcher keeps its previous behaviour (no replay protection).
func (b *Bot) SetReplayGuard(store replayStore) {
	if store == nil {
		b.replay = nil
		return
	}
	b.replay = newReplayGuard(store, b.logger, b.token)
}

// Start begins the long-polling loop. Blocks until ctx is cancelled.
//
// Polling is decoupled from processing: a fetched batch is admitted to the
// durable ledger (payloads + fetch cursor in one transaction), the Telegram
// offset advances immediately, and handlers run in the background. A slow
// handler therefore never delays fetching, which is what used to stall every
// user behind the slowest update in the batch.
func (b *Bot) Start(ctx context.Context) error {
	b.logger.Info("starting telegram bot polling")
	// Cancelling ctx means "stop fetching", not "abandon owned work": the
	// ledger already holds every accepted update, so an aborted handler is
	// recovered on the next start instead of being lost.
	workCtx, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()
	dispatcher := newUpdateDispatcher(workCtx, maxConcurrentUpdates, b.handler.HandleUpdate, b.logUpdatePanic)
	dispatcher.resetCheck = b.resetBarrier
	// A fatal ledger condition must interrupt a long poll immediately, not
	// after its 30s timeout: cancel the fetch context and prefer the fatal
	// error when reporting why Start returned.
	pollCtx, cancelPoll := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-dispatcher.Fatal():
			cancelPoll()
		case <-pollCtx.Done():
		}
	}()
	defer func() {
		cancelPoll()
		<-watchDone
	}()
	drainTimeout := b.drainTimeout
	if drainTimeout <= 0 {
		drainTimeout = shutdownDrainTimeout
	}

	// Undelivered recovery notices are retried while the bot runs. The loop
	// must stop before Start returns: main closes the store right after.
	noticeCtx, stopNotices := context.WithCancel(context.WithoutCancel(ctx))
	noticeDone := make(chan struct{})
	if b.replay != nil {
		go func() {
			defer close(noticeDone)
			interval := b.noticeEvery
			if interval <= 0 {
				interval = replayNoticeInterval
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			b.flushNoticesSafely(noticeCtx)
			for {
				select {
				case <-noticeCtx.Done():
					return
				case <-ticker.C:
					b.flushNoticesSafely(noticeCtx)
				}
			}
		}()
	} else {
		close(noticeDone)
	}

	defer func() {
		stopNotices()
		<-noticeDone
		// Give running handlers a short chance to finish so their rows become
		// done without a needless notice, then cancel. Aborting is now safe:
		// the row stays running and the next start turns it into a verification
		// notice. WaitAll is still mandatory — main closes BadgerDB as soon as
		// Start returns.
		warned := time.AfterFunc(drainTimeout, func() {
			b.logger.Warn("handlers still running at shutdown; they will be recovered on the next start",
				"drain_timeout", drainTimeout)
		})
		finished := dispatcher.Wait(drainTimeout)
		warned.Stop()
		if !finished {
			cancelWork()
		}
		dispatcher.WaitAll()
	}()

	// Hooks are batch-independent: they run for replayed and freshly admitted
	// updates alike.
	if b.replay != nil {
		replay := b.replay
		// Mark the row running before the handler can touch anything. If this
		// fails the handler must NOT run: without a durable running row a crash
		// would leave no evidence that side effects were possible.
		dispatcher.setOnStart(func(u Update) error {
			// A /reset must not run ahead of older, still-unfinished updates:
			// they would write after the wipe and resurrect pre-reset state.
			if b.resetBarrier(u) {
				older, err := replay.olderPending(u.UpdateID)
				if err != nil {
					return err
				}
				if older > 0 {
					b.logger.Info("deferring reset until older updates finish",
						"update_id", u.UpdateID, "older_pending", older)
					return errResetBlocked
				}
			}
			if err := replay.markRunning(u.UpdateID); err != nil {
				b.logger.Error("could not mark update running; stopping so it is not overtaken",
					"update_id", u.UpdateID, "error", err)
				return err
			}
			return nil
		})
		// Deferred updates are retried in-process; they stay admitted in the
		// meantime, so a crash before the retry still replays them.
		//
		// The buffer is sized to the admission budget and cannot overflow: a
		// deferred update keeps its pending token, so at most maxPendingUpdates
		// updates can be deferred at once. Overflow is still treated as fatal
		// rather than silently dropped, because dropping one would strand a
		// durable row until the next restart.
		deferred := make(chan Update, maxPendingUpdates)
		deferDone := make(chan struct{})
		dispatcher.setOnDefer(func(u Update) {
			if !requeue(deferred, u) {
				// Dropping it would strand a durable row until the next start.
				dispatcher.fail(fmt.Errorf("deferred update %d could not be queued", u.UpdateID))
			}
		})
		go b.retryDeferred(noticeCtx, deferred, dispatcher, replay, deferDone)
		// Join the retry goroutine before the dispatcher is waited on: it calls
		// Enqueue, which must not race WaitGroup.Wait, and it uses the store that
		// main closes right after Start returns. Registered after the shutdown
		// defer, so it runs first (LIFO); it cancels the shared context itself.
		defer func() {
			stopNotices()
			<-noticeDone
			<-deferDone
		}()
		dispatcher.setOnDone(func(u Update, outcome updateOutcome) {
			switch {
			case !outcome.Started:
				// Never started, so the row is still admitted and the next start
				// replays it. Nothing to record.
			case outcome.Completed && !outcome.Aborted:
				if err := replay.markDone(u.UpdateID); err != nil {
					// The row is stuck running: it would block the reset barrier
					// and never notify anyone. Fail closed so the next start
					// resolves it with a verification notice. The handler is
					// never re-run.
					dispatcher.fail(fmt.Errorf("could not complete update %d: %w", u.UpdateID, err))
					return
				}
				// A finished reset releases the barrier — only now, after its row
				// is durably terminal. Everything deferred behind it retries on
				// the next tick.
				if b.resetBarrier(u) {
					dispatcher.clearGateFor(u.UpdateID)
				}
			case outcome.Aborted:
				// Shutdown cut it short: the row stays running and the next
				// start records the verification notice. Never replay it.
				b.logger.Warn("update aborted at shutdown; it will need manual verification",
					"update_id", u.UpdateID)
			default:
				// A contained panic: the effects are ambiguous. Resolve it NOW
				// (durable notice, then close) instead of leaving a running row
				// that blocks resets and tells nobody until the next restart.
				if err := replay.markVerified(u); err != nil {
					dispatcher.fail(fmt.Errorf("could not resolve panicked update %d: %w", u.UpdateID, err))
					return
				}
				b.logger.Error("update panicked; user asked to verify", "update_id", u.UpdateID)
			}
		})
	}

	// Age of the fetch cursor this process is resuming from. The poll loop
	// discards the offset once it outlives Telegram's update-id reset window,
	// even when this process then sits idle long enough for Telegram to restart
	// the numbering.
	var cursorAt time.Time
	if b.replay != nil {
		cursor, pending, err := b.replay.recover()
		if err != nil {
			// Fail closed: without the ledger we cannot tell admitted from
			// already-processed work.
			return err
		}
		b.offset = cursor
		cursorAt = b.replay.cursorAt

		// Resolve interrupted rows before replaying anything, so their notices
		// are queued first. A failure here is fatal: the notice is the user's
		// only signal that an interrupted action needs manual verification, and
		// logging it would leave them believing it either succeeded or never
		// happened — with nothing retrying it until the next restart. Refusing
		// to start keeps that silent window from opening at all.
		for _, rec := range pending {
			if rec.State != stateRunning && rec.State != stateVerify {
				continue
			}
			if err := b.replay.resolveInterrupted(rec); err != nil {
				return fmt.Errorf("resolve interrupted update %d: %w", rec.UpdateID, err)
			}
		}
		// Replay admitted rows in update_id order BEFORE the first poll, or a
		// fresh update from the same user could overtake an older replayed one
		// and break per-user FIFO.
		for _, rec := range pending {
			if rec.State != stateAdmitted {
				continue
			}
			var u Update
			if err := json.Unmarshal(rec.Payload, &u); err != nil {
				b.logger.Error("could not decode admitted update; skipping", "update_id", rec.UpdateID, "error", err)
				continue
			}
			if err := dispatcher.Enqueue(ctx, u); err != nil {
				b.logger.Info("bot polling stopped")
				return err
			}
			b.logger.Info("replayed admitted update", "update_id", rec.UpdateID)
		}
	}

	for {
		if ctx.Err() != nil {
			b.logger.Info("bot polling stopped")
			return ctx.Err()
		}
		// Telegram may restart update ids after a long inactivity, and polling
		// with an offset above the restarted numbering makes it drop every new
		// update without any error to observe. A cursor that has not advanced
		// for the reset window is therefore discarded here too, not only at
		// startup: this process can sit idle long enough for it to happen.
		if b.offset > 0 && !cursorAt.IsZero() && time.Since(cursorAt) > telegramUpdateIDResetWindow {
			b.logger.Warn("fetch cursor is older than Telegram's update-id reset window; polling from 0",
				"offset", b.offset, "updated_at", cursorAt)
			if b.replay != nil {
				if err := b.replay.purgeTerminal(); err != nil {
					// Fail closed: a stale terminal row would silently swallow
					// a new update that reuses its id.
					fatal := fmt.Errorf("purge terminal rows after an update-id reset: %w", err)
					dispatcher.fail(fatal)
					return fatal
				}
			}
			b.offset = 0
			cursorAt = time.Now()
		}
		// Fail closed: a durable state write failed, so continuing could run an
		// update whose predecessor was never recorded as running.
		select {
		case <-dispatcher.Fatal():
			err := dispatcher.FatalErr()
			b.logger.Error("stopping polling after a fatal ledger error", "error", err)
			return err
		default:
		}
		updates, err := b.getUpdates(pollCtx)
		if err != nil {
			select {
			case <-dispatcher.Fatal():
				fatal := dispatcher.FatalErr()
				b.logger.Error("stopping polling after a fatal ledger error", "error", fatal)
				return fatal
			default:
			}
			if ctx.Err() != nil {
				b.logger.Info("bot polling stopped")
				return ctx.Err()
			}
			b.logger.Error("get updates failed", "error", err)
			select {
			case <-ctx.Done():
				b.logger.Info("bot polling stopped")
				return ctx.Err()
			case <-dispatcher.Fatal():
				err := dispatcher.FatalErr()
				b.logger.Error("stopping polling after a fatal ledger error", "error", err)
				return err
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if len(updates) == 0 {
			continue
		}

		// Telegram's offset is a single watermark: the next getUpdates with a
		// higher offset forgets everything below it. It may only advance inside
		// the admission transaction, so an update is never dropped before this
		// bot owns a durable copy of it.
		sort.Slice(updates, func(i, j int) bool { return updates[i].UpdateID < updates[j].UpdateID })
		next := 0
		for _, u := range updates {
			if u.UpdateID+1 > next {
				next = u.UpdateID + 1
			}
		}
		// A batch entirely below the stored cursor means Telegram restarted its
		// update ids. Keeping the stale watermark would filter out every new
		// update, so adopt the batch's, and drop the terminal rows of the
		// previous numbering: those ids cannot be redelivered any more, while
		// keeping them would make a new update that reuses an old id look
		// already processed.
		if next < b.offset {
			b.logger.Warn("update ids restarted below the stored cursor; adopting the batch watermark",
				"cursor", b.offset, "batch_next", next)
			if b.replay != nil {
				if err := b.replay.purgeTerminal(); err != nil {
					fatal := fmt.Errorf("purge terminal rows after an update-id reset: %w", err)
					dispatcher.fail(fatal)
					return fatal
				}
			}
		}

		if b.replay == nil {
			// No ledger configured: keep the old conservative behaviour rather
			// than advancing an offset with nothing durable behind it.
			for _, u := range updates {
				if err := dispatcher.Enqueue(ctx, u); err != nil {
					b.logger.Info("bot polling stopped")
					return err
				}
			}
			b.offset = next
			continue
		}

		inserted, err := b.replay.admitBatch(updates, next)
		if err != nil {
			// Nothing was written, so the offset is unchanged and Telegram will
			// redeliver the batch.
			b.logger.Error("could not admit batch; not advancing offset", "error", err)
			select {
			case <-ctx.Done():
				b.logger.Info("bot polling stopped")
				return ctx.Err()
			case <-dispatcher.Fatal():
				err := dispatcher.FatalErr()
				b.logger.Error("stopping polling after a fatal ledger error", "error", err)
				return err
			case <-time.After(2 * time.Second):
			}
			continue
		}
		b.offset = next
		cursorAt = time.Now()

		// Enqueue only the rows this call inserted: a redelivery of an
		// already-known id must not be processed twice.
		byID := make(map[int64]Update, len(updates))
		for _, u := range updates {
			byID[int64(u.UpdateID)] = u
		}
		for _, id := range inserted {
			u, ok := byID[id]
			if !ok {
				continue
			}
			if err := dispatcher.Enqueue(ctx, u); err != nil {
				// The rows are already durable, so they replay on the next
				// start; do not mark them done.
				b.logger.Info("bot polling stopped")
				return err
			}
		}
	}
}

// logUpdatePanic records a handler panic recovered by the dispatcher. The
// stack is captured while the panicking goroutine is still unwinding, so it
// points at the failure site. HandleUpdate already recovers its own panics, so
// this is the backstop that keeps one bad update from killing its user's
// worker.
func (b *Bot) logUpdatePanic(update Update, r any) {
	b.logger.Error("panic in update worker",
		"update_id", update.UpdateID,
		"panic", r,
		"stack", string(debug.Stack()),
	)
	if b.handler != nil {
		b.handler.RecordPanic(update.UpdateID, r)
	}
}

// updateKey returns the ordering scope for an update: the Telegram user the
// update belongs to. Updates without a user (rare system updates) are not
// keyed and may run concurrently.
func updateKey(u Update) (int64, bool) {
	if u.Message != nil && u.Message.From != nil {
		return u.Message.From.ID, true
	}
	if u.CallbackQuery != nil && u.CallbackQuery.From != nil {
		return u.CallbackQuery.From.ID, true
	}
	return 0, false
}

// updateQueue holds the updates waiting for one user, in arrival order.
type updateQueue struct {
	items   []Update
	running bool
}

// updateDispatcher serialises updates per user while keeping distinct users
// concurrent.
//
// Telegram delivers updates in update_id order, but processing them in
// independent goroutines lets a slow handler (e.g. a Notion round trip)
// finish after a later update from the same user. That reorders a guided
// answer, a callback press and a following command, corrupting per-user
// state (GuidedFlow / TradeEditSession). Each user therefore gets a FIFO
// queue drained by at most one worker, while different users run in
// parallel under the execution semaphore.
//
// Two separate bounds keep memory finite without starving users: the pending
// budget is taken when an update is accepted (backpressure on the poll loop),
// and the execution budget only while a handler actually runs, so one user's
// backlog cannot occupy slots another user needs.
type updateDispatcher struct {
	workCtx context.Context
	handle  func(context.Context, Update)
	onPanic func(Update, any)
	exec    chan struct{}
	pending chan struct{}
	wait    *waitSet
	mu      sync.Mutex
	queues  map[int64]*updateQueue
	hookMu  sync.Mutex
	onStart func(Update) error
	onDefer func(Update)
	onDone  func(Update, updateOutcome)
	// fatal broadcasts a condition the bot cannot continue past (a durable
	// state write failed). fatalErr retains it: a channel carrying the error
	// would be consumed by whichever goroutine read first, leaving the others
	// waiting forever on a cancelled poll.
	fatalCh   chan struct{}
	fatalErr  error
	fatalOnce sync.Once
	// stopped fences admission and handler start as soon as a fatal condition
	// is known, so a later update cannot overtake the failed one while the poll
	// loop is still busy.
	stopped atomic.Bool
	// resetCheck decides whether an update is a destructive owner reset.
	resetCheck func(Update) bool

	gateMu  sync.Mutex
	gateIDs map[int]struct{} // pending resets; the barrier is their minimum
	// gateActive is the reset that currently holds exclusive execution, or 0.
	// It is set by beginRun and cleared either when that reset defers (its start
	// hook could not run it yet) or when its completion is recorded. While a
	// reset is pending or active, NO other update may start ahead of it — it
	// would run first and be wiped — or beside it, because a concurrent write
	// could land after the wipe. running counts handlers currently executing, so
	// a reset only starts once nothing else is in flight.
	gateActive int
	running    int

	// holdMu guards held: the update ids per user that were deferred and have
	// not started yet. A newly admitted update for that user must not start
	// ahead of them, or lifting a barrier would let a later update overtake an
	// older deferred one and break per-user FIFO (the retry tick can be a minute
	// away). Ids are kept sorted ascending; an update releases its own entry the
	// moment it starts, so a hold only spans deferral, never execution.
	holdMu sync.Mutex
	held   map[int64][]int
}

func newUpdateDispatcher(workCtx context.Context, maxConcurrent int, handle func(context.Context, Update), onPanic func(Update, any)) *updateDispatcher {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	if workCtx == nil {
		workCtx = context.Background()
	}
	return &updateDispatcher{
		workCtx: workCtx,
		handle:  handle,
		onPanic: onPanic,
		exec:    make(chan struct{}, maxConcurrent),
		pending: make(chan struct{}, maxPendingUpdates),
		wait:    newWaitSet(),
		queues:  make(map[int64]*updateQueue),
		fatalCh: make(chan struct{}),
		gateIDs: make(map[int]struct{}),
		held:    make(map[int64][]int),
		// Default rule; Bot narrows it to destructive, owner-issued resets.
		resetCheck: isDestructiveReset,
	}
}

// updateOutcome is what the dispatcher reports for one accepted update.
//
// The three flags are independent because the transport no longer waits on
// processing: the ledger already owns the update, so the only questions are
// whether the handler was allowed to start, whether it finished, and whether
// shutdown cut it short.
type updateOutcome struct {
	// Started is true once onStart succeeded and the handler was invoked.
	Started bool
	// Completed is true only when the handler returned without panicking.
	Completed bool
	// Aborted is true when the work context was cancelled mid-flight.
	Aborted bool
}

// setOnStart installs the pre-handler hook. A non-nil error means the handler
// MUST NOT run: the caller could not durably record that it is running.
func (d *updateDispatcher) setOnStart(fn func(Update) error) {
	d.hookMu.Lock()
	d.onStart = fn
	d.hookMu.Unlock()
}

// setOnDefer installs the hook called when an update is deliberately not run yet
// (a /reset waiting for older work). The row stays admitted and is retried.
func (d *updateDispatcher) setOnDefer(fn func(Update)) {
	d.hookMu.Lock()
	d.onDefer = fn
	d.hookMu.Unlock()
}

func (d *updateDispatcher) notifyDefer(u Update) {
	// Remember that this update is waiting, so a later update from the same user
	// cannot be admitted ahead of it while the retry tick is up to a minute
	// away. Parking happens before the hook: the hook may re-enqueue on the
	// spot (a bounded pass), and the bookkeeping must already be in place.
	if key, ok := updateKey(u); ok {
		d.hold(key, u.UpdateID)
	}
	d.hookMu.Lock()
	fn := d.onDefer
	d.hookMu.Unlock()
	if fn != nil {
		fn(u)
	}
}

// hold records that updateID of user key is deferred and awaiting a retry.
func (d *updateDispatcher) hold(key int64, updateID int) {
	d.holdMu.Lock()
	defer d.holdMu.Unlock()
	ids := d.held[key]
	if len(ids) > 0 && ids[len(ids)-1] >= updateID {
		// Defensive: keep the slice ordered even if a retry is admitted out of
		// order, so heldBefore can stop at the first candidate.
		i := sort.SearchInts(ids, updateID)
		if i < len(ids) && ids[i] == updateID {
			return
		}
		ids = append(ids, 0)
		copy(ids[i+1:], ids[i:])
		ids[i] = updateID
		d.held[key] = ids
		return
	}
	d.held[key] = append(ids, updateID)
}

// releaseHold drops updateID once it is actually running. It is called by the
// worker that started it, not by Enqueue: admission and a deferral can
// interleave, and a release racing a fresh deferral would drop the hold and let
// a newer update of the same user overtake this one.
func (d *updateDispatcher) releaseHold(key int64, updateID int) {
	d.holdMu.Lock()
	defer d.holdMu.Unlock()
	ids := d.held[key]
	i := sort.SearchInts(ids, updateID)
	if i >= len(ids) || ids[i] != updateID {
		return
	}
	ids = append(ids[:i], ids[i+1:]...)
	if len(ids) == 0 {
		delete(d.held, key)
		return
	}
	d.held[key] = ids
}

// heldBefore reports whether a deferred update of user key is older than
// updateID, i.e. whether starting updateID now would invert that user's order.
func (d *updateDispatcher) heldBefore(key int64, updateID int) bool {
	d.holdMu.Lock()
	defer d.holdMu.Unlock()
	ids := d.held[key]
	i := sort.SearchInts(ids, updateID)
	return i > 0
}

// setOnDone installs the completion hook called once per accepted update.
func (d *updateDispatcher) setOnDone(fn func(Update, updateOutcome)) {
	d.hookMu.Lock()
	d.onDone = fn
	d.hookMu.Unlock()
}

func (d *updateDispatcher) notifyStart(u Update) (err error) {
	d.hookMu.Lock()
	fn := d.onStart
	d.hookMu.Unlock()
	if fn == nil {
		return nil
	}
	// The hook runs inside the worker; contain a panic so one bad hook cannot
	// take the process down. A panic means the running state was NOT recorded,
	// so it must surface as an error: returning nil here would run the handler
	// without durable evidence that side effects were possible.
	defer func() {
		if r := recover(); r != nil {
			if d.onPanic != nil {
				d.onPanic(u, r)
			}
			err = fmt.Errorf("start hook panicked for update %d: %v", u.UpdateID, r)
		}
	}()
	return fn(u)
}

func (d *updateDispatcher) notifyDone(u Update, outcome updateOutcome) {
	d.hookMu.Lock()
	fn := d.onDone
	d.hookMu.Unlock()
	if fn == nil {
		return
	}
	// This runs inside run's deferred cleanup, so a panicking hook must not take
	// the process down.
	defer func() {
		if r := recover(); r != nil && d.onPanic != nil {
			d.onPanic(u, r)
		}
	}()
	fn(u, outcome)
}

// Enqueue accepts an update for processing in arrival order. It blocks while
// maxPendingUpdates updates are already accepted, so a burst cannot exhaust
// memory. It returns ctx.Err() when the context ends first.
//
// ctx gates admission only. Handlers run under the dispatcher's workCtx, so
// cancelling the polling context stops new updates without aborting the ones
// already accepted.
func (d *updateDispatcher) Enqueue(ctx context.Context, u Update) error {
	return d.enqueue(ctx, u, true)
}

// EnqueueRetry re-admits an update that was deferred and therefore still holds
// its pending budget. Taking another token here would count one update twice
// and, once the budget filled with retries, block the very loop that drains it.
func (d *updateDispatcher) EnqueueRetry(ctx context.Context, u Update) error {
	return d.enqueue(ctx, u, false)
}

// enqueue accepts u for processing, optionally taking a pending-budget token.
func (d *updateDispatcher) enqueue(ctx context.Context, u Update, takeBudget bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.stopped.Load() {
		<-d.Fatal()
		return fmt.Errorf("%w: %w", errDispatcherStopped, d.FatalErr())
	}
	// Raise the reset barrier here, at admission time: Start enqueues in
	// update_id order, so a newer update can never be queued — let alone start —
	// before the barrier exists. Raising it only in the reset's worker would
	// leave a window where a later update runs and is then wiped.
	if d.resetCheck != nil && d.resetCheck(u) {
		d.addGate(u.UpdateID)
	}
	if takeBudget {
		select {
		case d.pending <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		case <-d.Fatal():
			return fmt.Errorf("%w: %w", errDispatcherStopped, d.FatalErr())
		}
	}
	if d.stopped.Load() {
		if takeBudget {
			<-d.pending
		}
		<-d.Fatal()
		return fmt.Errorf("%w: %w", errDispatcherStopped, d.FatalErr())
	}

	key, keyed := updateKey(u)
	if !keyed {
		d.wait.add()
		go func() {
			defer d.wait.done()
			d.execute(u)
		}()
		return nil
	}

	d.mu.Lock()
	q := d.queues[key]
	if q == nil {
		q = &updateQueue{}
		d.queues[key] = q
	}
	q.items = append(q.items, u)
	if !q.running {
		q.running = true
		d.wait.add()
		go d.drain(key, q)
	}
	d.mu.Unlock()
	return nil
}

// drain processes one user's queue until it is empty. The queue is removed
// under the same lock that decides whether to start a worker, so a new update
// can never race a dying worker and produce two concurrent workers for the
// same user.
func (d *updateDispatcher) drain(key int64, q *updateQueue) {
	defer d.wait.done()
	for {
		d.mu.Lock()
		if len(q.items) == 0 {
			q.running = false
			if d.queues[key] == q {
				delete(d.queues, key)
			}
			d.mu.Unlock()
			return
		}
		u := q.items[0]
		q.items[0] = Update{} // release pointers while the queue drains
		q.items = q.items[1:]
		d.mu.Unlock()

		if !d.execute(u) {
			d.abandon(key, q)
			return
		}
	}
}

// abandon stops a user's worker after shutdown was requested: the remaining
// updates are reported as never-started (so their ledger rows stay admitted and
// replay on the next start) and their pending budget is released.
func (d *updateDispatcher) abandon(key int64, q *updateQueue) {
	d.mu.Lock()
	rest := q.items
	q.items = nil
	q.running = false
	if d.queues[key] == q {
		delete(d.queues, key)
	}
	d.mu.Unlock()
	for i := range rest {
		d.finish(rest[i], updateOutcome{})
	}
}

// execute runs one update under the execution bound. It reports false when
// shutdown was requested before the handler could start; the update is then
// reported as never-started and its ledger row stays admitted.
func (d *updateDispatcher) execute(u Update) bool {
	select {
	case d.exec <- struct{}{}:
	case <-d.workCtx.Done():
		d.finish(u, updateOutcome{})
		return false
	}
	// A fatal condition may have landed while this update waited for its slot:
	// starting it now would let it overtake the failed one.
	if d.stopped.Load() {
		<-d.exec
		d.finish(u, updateOutcome{})
		return false
	}
	// Cancellation may have landed between admission and the slot; do not
	// start work that would immediately abort.
	if d.workCtx.Err() != nil {
		<-d.exec
		d.finish(u, updateOutcome{})
		return false
	}
	return d.run(d.workCtx, u)
}

// run executes one update and always frees its execution slot, even when the
// handler panics: a panic must never strand the rest of that user's queue or
// permanently shrink the concurrency budget. The pending budget is released
// unless the update was deferred — a deferred update is still accepted work, so
// keeping its token is what makes a barrier backlog apply backpressure to the
// poll loop instead of overflowing the deferred buffer (which is fatal).
func (d *updateDispatcher) run(ctx context.Context, u Update) (consumed bool) {
	var (
		started   bool
		completed bool
		deferred  bool
		counting  bool
		reset     = d.resetCheck != nil && d.resetCheck(u)
	)
	defer func() {
		if r := recover(); r != nil && d.onPanic != nil {
			d.onPanic(u, r)
		}
		if counting {
			d.endRun()
		}
		// A reset that claimed exclusive execution but never started (its start
		// hook deferred it because older work is still pending) must hand the
		// claim back. Keeping it would make beginRun treat the wipe as under way
		// and defer the very updates the reset waits for, so olderPending could
		// never reach zero and the barrier would deadlock.
		if reset && !started {
			d.releaseGateClaim(u.UpdateID)
		}
		<-d.exec
		outcome := updateOutcome{
			Started:   started,
			Completed: completed,
			Aborted:   ctx.Err() != nil,
		}
		// A deferred update keeps its pending budget: it is still accepted
		// work, and keeping the token is what turns a barrier backlog into
		// admission backpressure instead of a fatal buffer overflow.
		if deferred {
			d.finishDeferred(u, outcome)
		} else {
			d.finish(u, outcome)
		}
		// The barrier is released by the completion hook only after the reset is
		// durably finished; if the reset panicked or could not be recorded, it
		// stays up and the bot stops instead of running later work against a
		// half-wiped state.
		if reset && started && !completed && ctx.Err() == nil {
			// The reset failed mid-way (panic): keep the barrier up and stop, so
			// later updates never run against a half-wiped state. A shutdown
			// abort is excluded: the process is exiting and the ledger replays
			// the reset as unfinished.
			d.fail(fmt.Errorf("reset %d did not complete; blocking later updates", u.UpdateID))
		}
		// A panic or a deferral still consumes the update: the row stays
		// running or admitted, and the rest of this user's queue must keep
		// flowing. Only a failed start hook stops the queue, because the
		// unrecorded row would otherwise be overtaken.
		if started || deferred {
			consumed = true
		}
	}()

	switch d.beginRun(u) {
	case runStop:
		// Fenced by a fatal condition: the row stays admitted for the next
		// start and must not run here. The deferred cleanup releases the slot
		// and reports it as never-started.
		return false
	case runDefer:
		// Either the reset barrier is up, or an older update of this user is
		// still deferred. Both are retried, so the hold stays.
		d.notifyDefer(u)
		deferred = true
		return true
	}
	counting = true
	// It is running now, so it no longer holds back later updates of its user.
	// Released here rather than in Enqueue: admission and a deferral can
	// interleave, and a release racing a fresh deferral would drop the hold and
	// let a newer update overtake this one.
	if key, ok := updateKey(u); ok {
		d.releaseHold(key, u.UpdateID)
	}

	// The pre-handler hook is the durability gate. If it cannot record that the
	// update is running, the handler must not touch anything and the row stays
	// admitted; a deferred update (a /reset waiting for older work) is simply
	// retried, anything else is fatal because this user's later updates would
	// otherwise overtake it.
	if err := d.notifyStart(u); err != nil {
		if errors.Is(err, errResetBlocked) {
			// Deferred, not failed: the row stays admitted and the caller
			// retries it once the older updates are terminal. The barrier is
			// already up from admission, so nothing else can overtake it.
			d.notifyDefer(u)
			deferred = true
			return true
		}
		d.fail(fmt.Errorf("update %d cannot be started: %w", u.UpdateID, err))
		return false
	}
	started = true
	d.handle(ctx, u)
	completed = true
	return true
}

// runDecision is the outcome of deciding whether an update may start.
type runDecision int

const (
	// runProceed: the handler may run; the caller must call endRun later.
	runProceed runDecision = iota
	// runDefer: the reset barrier is up, or the reset is waiting for in-flight
	// work; the update stays admitted and is retried.
	runDefer
	// runStop: a fatal condition fenced the dispatcher.
	runStop
)

// beginRun atomically decides whether u may start and, when it may, counts it as
// running.
//
// The barrier itself is raised at admission time (see Enqueue), because that is
// the only point ordered against every later update. This function adds the
// execution rules: a reset never runs beside anything else, an earlier pending
// reset goes first, and updates older than the barrier still run because the
// reset is waiting on them.
func (d *updateDispatcher) beginRun(u Update) runDecision {
	if d.stopped.Load() {
		return runStop
	}
	reset := d.resetCheck != nil && d.resetCheck(u)
	// Per-user FIFO: a deferred update of this user is older and still waiting,
	// so starting u now would invert its order. Checked before taking any slot.
	if key, ok := updateKey(u); ok && d.heldBefore(key, u.UpdateID) {
		return runDefer
	}

	d.gateMu.Lock()
	defer d.gateMu.Unlock()
	if d.stopped.Load() {
		return runStop
	}
	gate := d.gateMin()

	if reset {
		// Defensive: admission already registered it, but a reset replayed from
		// the ledger does not go through Enqueue.
		if _, ok := d.gateIDs[u.UpdateID]; !ok {
			d.gateIDs[u.UpdateID] = struct{}{}
			gate = d.gateMin()
		}
		if d.running > 0 {
			// Nothing may run beside the wipe.
			return runDefer
		}
		if gate != u.UpdateID {
			// An earlier reset is still pending: it goes first.
			return runDefer
		}
		// Claim exclusive execution. This is NOT the same as the wipe being
		// under way: the start hook can still refuse to run the reset (older
		// work is pending), and then releaseGateClaim puts the barrier back to
		// "pending" so the older work it waits on may still start. Leaving the
		// claim up in that case deadlocks the barrier: the updates the reset
		// depends on would be deferred forever, so olderPending could never
		// reach zero.
		d.gateActive = u.UpdateID
		d.running++
		return runProceed
	}

	if gate != 0 {
		switch {
		case u.UpdateID < gate && d.gateActive == 0:
			// Older than the barrier: it MUST run. The reset waits on exactly
			// these updates (olderPending), so blocking them would defer the
			// reset forever.
		default:
			// Newer than the barrier, or the reset is already wiping: defer.
			// Nothing may write concurrently with the wipe, older or not.
			return runDefer
		}
	}
	d.running++
	return runProceed
}

// releaseGateClaim drops the exclusive claim taken by beginRun for a reset that
// did not actually start. The gate stays pending, so the barrier still holds
// later updates and the retry can start the reset as soon as the work it waits
// on is terminal.
func (d *updateDispatcher) releaseGateClaim(updateID int) {
	d.gateMu.Lock()
	if d.gateActive == updateID {
		d.gateActive = 0
	}
	d.gateMu.Unlock()
}

// endRun releases the running count taken by beginRun.
func (d *updateDispatcher) endRun() {
	d.gateMu.Lock()
	if d.running > 0 {
		d.running--
	}
	d.gateMu.Unlock()
}

// addGate registers a pending reset. The barrier is the lowest pending id, so an
// earlier reset is never overtaken by a later one, and finishing one reset moves
// the barrier to the next instead of dropping it.
func (d *updateDispatcher) addGate(updateID int) {
	d.gateMu.Lock()
	d.gateIDs[updateID] = struct{}{}
	d.gateMu.Unlock()
}

// gateMin returns the current barrier id, or 0 when no reset is pending.
// Caller must hold gateMu.
func (d *updateDispatcher) gateMin() int {
	min := 0
	for id := range d.gateIDs {
		if min == 0 || id < min {
			min = id
		}
	}
	return min
}

// gateUpdate returns the id of the pending reset holding the barrier, or 0.
func (d *updateDispatcher) gateUpdate() int {
	d.gateMu.Lock()
	defer d.gateMu.Unlock()
	return d.gateMin()
}

// clearGateFor retires a finished reset; the barrier moves to the next pending
// reset, if any.
func (d *updateDispatcher) clearGateFor(updateID int) {
	d.gateMu.Lock()
	delete(d.gateIDs, updateID)
	if d.gateActive == updateID {
		d.gateActive = 0
	}
	d.gateMu.Unlock()
}

// fail reports a condition the bot must stop on, once, and fences all further
// admission and handler starts immediately.
func (d *updateDispatcher) fail(err error) {
	d.stopped.Store(true)
	d.fatalOnce.Do(func() {
		d.fatalErr = err
		close(d.fatalCh)
	})
}

// Stopped reports whether a fatal condition has fenced the dispatcher.
func (d *updateDispatcher) Stopped() bool { return d.stopped.Load() }

// Fatal is closed once a fail-stop condition has been recorded, so every
// observer sees it (broadcast, not a single-consumer channel).
func (d *updateDispatcher) Fatal() <-chan struct{} { return d.fatalCh }

// FatalErr returns the retained fail-stop error, or nil when there is none.
func (d *updateDispatcher) FatalErr() error { return d.fatalErr }

// finish releases the update's pending budget and reports its outcome.
func (d *updateDispatcher) finish(u Update, outcome updateOutcome) {
	<-d.pending
	d.notifyDone(u, outcome)
}

// finishDeferred reports a deferred update WITHOUT releasing its pending budget.
//
// A deferred update is still accepted work, so keeping its token is what makes
// admission backpressure cover a barrier backlog: releasing it would let the
// poll loop keep admitting updates until the deferred buffer overflowed, which
// is fatal, instead of slowing fetching down. The token is released when the
// update finally runs or is abandoned, and re-admission of a deferred update
// goes through EnqueueRetry so it is never counted twice.
func (d *updateDispatcher) finishDeferred(u Update, outcome updateOutcome) {
	d.notifyDone(u, outcome)
}

// Wait blocks until every update accepted so far has finished processing or the
// timeout elapses, reporting whether the drain completed in time.
//
// Unlike sync.WaitGroup, it is safe to admit work again after Wait returned:
// each call observes the generation that was open when it started, and a later
// Enqueue opens a new one. That matters because shutdown is not the only caller
// — a Wait that timed out must never make the next admission panic, since the
// panic would take the whole bot down mid-shutdown.
func (d *updateDispatcher) Wait(timeout time.Duration) bool {
	return d.wait.wait(timeout)
}

// WaitAll blocks until every accepted update has finished processing. Start
// uses it after cancelling the remaining handlers so the caller can never
// close its stores while a worker is still using them.
func (d *updateDispatcher) WaitAll() { d.wait.waitAll() }

// waitSet tracks accepted work so callers can wait for it to drain.
//
// It replaces sync.WaitGroup because Wait doubles as a settle primitive while
// more work may still arrive: a WaitGroup panics ("WaitGroup is reused before
// previous Wait has returned") when Add lands between its counter reaching zero
// and its final state check, and a shutdown path must never panic. A wait here
// returns as soon as the generation it observed is drained; work admitted
// afterwards belongs to the next generation and is waited for by the next call.
type waitSet struct {
	mu      sync.Mutex
	n       int
	drained chan struct{} // closed once n reaches 0; nil while nothing is accepted
}

func newWaitSet() *waitSet { return &waitSet{} }

// add registers one unit of accepted work.
func (w *waitSet) add() {
	w.mu.Lock()
	if w.n == 0 {
		w.drained = make(chan struct{})
	}
	w.n++
	w.mu.Unlock()
}

// done retires one unit of accepted work.
func (w *waitSet) done() {
	w.mu.Lock()
	w.n--
	if w.n == 0 {
		close(w.drained)
	}
	w.mu.Unlock()
}

// wait reports whether the observed generation drained within timeout.
func (w *waitSet) wait(timeout time.Duration) bool {
	w.mu.Lock()
	ch := w.drained
	w.mu.Unlock()
	if ch == nil {
		return true // nothing accepted yet
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

// waitAll blocks until no accepted work remains, including work admitted while
// it was waiting.
func (w *waitSet) waitAll() {
	for {
		w.mu.Lock()
		ch, n := w.drained, w.n
		w.mu.Unlock()
		if n == 0 {
			return
		}
		<-ch
	}
}

// Update represents a Telegram update.
type Update struct {
	UpdateID      int            `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// Message represents a Telegram message.
type Message struct {
	MessageID       int         `json:"message_id"`
	MessageThreadID int         `json:"message_thread_id,omitempty"`
	From            *User       `json:"from,omitempty"`
	Chat            *Chat       `json:"chat"`
	Text            string      `json:"text,omitempty"`
	Photo           []PhotoSize `json:"photo,omitempty"`
	Caption         string      `json:"caption,omitempty"`
}

// User represents a Telegram user.
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
}

// Chat represents a Telegram chat.
type Chat struct {
	ID      int64  `json:"id"`
	Type    string `json:"type"` // "private", "group", "supergroup"
	IsForum bool   `json:"is_forum,omitempty"`
}

// PhotoSize represents one size of a photo.
type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int    `json:"file_size,omitempty"`
}

// CallbackQuery represents an inline button press.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// apiBase returns the Bot API prefix, defaulting to the public endpoint when
// the field is unset. baseAPI is overridable so tests can point the client at
// an httptest server instead of api.telegram.org.
func (b *Bot) apiBase() string {
	if b.baseAPI == "" {
		return telegramAPI
	}
	return b.baseAPI
}

// apiURL builds the Bot API URL for a method.
func (b *Bot) apiURL(method string) string {
	return fmt.Sprintf("%s%s/%s", b.apiBase(), b.token, method)
}

// fileURL builds the download URL for a file path returned by getFile.
// Telegram serves files from "/file/bot<token>/<path>", a sibling of the
// "/bot<token>" API root.
func (b *Bot) fileURL(filePath string) string {
	root := strings.TrimSuffix(b.apiBase(), "/bot")
	return fmt.Sprintf("%s/file/bot%s/%s", root, b.token, filePath)
}

func (b *Bot) getUpdates(ctx context.Context) ([]Update, error) {
	return b.getUpdatesAt(ctx, b.offset, 30, 0)
}

// getUpdatesAt performs one getUpdates call with an explicit offset. timeoutSec
// 0 makes Telegram answer immediately instead of long polling; limit 0 leaves
// Telegram's default (100).
func (b *Bot) getUpdatesAt(ctx context.Context, offset, timeoutSec, limit int) ([]Update, error) {
	// Build the query with url.Values so the JSON payload is properly
	// percent-encoded instead of relying on the server's leniency.
	q := url.Values{}
	q.Set("offset", strconv.Itoa(offset))
	q.Set("timeout", strconv.Itoa(timeoutSec))
	q.Set("allowed_updates", `["message","callback_query"]`)
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	pollURL := b.apiURL("getUpdates") + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, redactTokenErr(err, b.token)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		OK     bool     `json:"ok"`
		Result []Update `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("telegram API error: %s", string(body))
	}
	return result.Result, nil
}

// CheckChatMember checks if a user is a member of the given chat.
// Returns true if the user's status is "creator", "administrator", "member", or "restricted".
func (b *Bot) CheckChatMember(ctx context.Context, chatID int64, userID int64) (bool, error) {
	reqURL := fmt.Sprintf("%s?chat_id=%d&user_id=%d", b.apiURL("getChatMember"), chatID, userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return false, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return false, redactTokenErr(err, b.token)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return false, err
	}
	if !result.OK {
		return false, nil
	}
	switch result.Result.Status {
	case "creator", "administrator", "member", "restricted":
		return true, nil
	default:
		return false, nil
	}
}

// GetFile downloads a file from Telegram by file ID and returns the content.
// This method downloads the file directly rather than returning a URL with the bot token embedded,
// preventing token exposure when files are uploaded to external systems like Notion.
func (b *Bot) GetFile(ctx context.Context, fileID string) ([]byte, error) {
	// First, get the file path
	reqURL := fmt.Sprintf("%s?file_id=%s", b.apiURL("getFile"), url.QueryEscape(fileID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, redactTokenErr(err, b.token)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("telegram API error: %s", string(body))
	}

	// Download the actual file content
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, b.fileURL(result.Result.FilePath), nil)
	if err != nil {
		return nil, err
	}

	resp, err = b.client.Do(req)
	if err != nil {
		return nil, redactTokenErr(err, b.token)
	}
	defer func() { _ = resp.Body.Close() }()

	return io.ReadAll(resp.Body)
}
