package notion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// MemberCacheStore persists member mappings across bot restarts.
type MemberCacheStore interface {
	GetCachedMember(ctx context.Context, telegramID int64) (*domain.Member, error)
	SaveCachedMember(ctx context.Context, member *domain.Member) error
	ListCachedMembers(ctx context.Context) ([]domain.Member, error)
	// DeleteCachedMember drops a single member mapping. Reset needs it to keep
	// the persisted roster in step with the journals it archives: a mapping
	// left behind points at an archived page, so the next EnsureMember would
	// reuse a database that no longer accepts writes.
	DeleteCachedMember(ctx context.Context, telegramID int64) error
}

// MemberRepo implements service.MemberRepository using Notion + optional persistent cache.
type MemberRepo struct {
	client   *Client
	parentID string // parent page ID for the Trade Journal workspace
	store    MemberCacheStore

	// In-memory cache: telegramID → Member (to avoid repeated API calls).
	mu    sync.RWMutex
	cache map[int64]*domain.Member

	// inflight serializes EnsureMember per Telegram ID. The bot dispatches
	// every update in its own goroutine, so a member's first two messages can
	// race: without this lock both callers miss the caches and both create a
	// Notion member page + inline trades database, orphaning one of them.
	inflightMu    sync.Mutex
	inflightLocks map[int64]*sync.Mutex

	// gate keeps journal writes from overlapping ResetAll's archival phase.
	// ResetAll takes it for writing; journaling paths take it for reading and
	// fail fast with ErrResetInProgress while a reset is running, so a trade
	// can never be written into a journal that is being archived.
	gate sync.RWMutex
}

// ErrResetInProgress is returned when a journal write is attempted while
// ResetAll is archiving journals. The caller should ask the user to retry.
var ErrResetInProgress = errors.New("member journal reset in progress")

// beginJournaling reserves the journal for a write and returns the release
// function. It fails fast (instead of blocking) while a reset is archiving, so
// a user command returns an error rather than hanging on an owner-triggered
// reset.
func (r *MemberRepo) beginJournaling() (func(), error) {
	if !r.gate.TryRLock() {
		return nil, ErrResetInProgress
	}
	return r.gate.RUnlock, nil
}

// NewMemberRepo creates a MemberRepo.
func NewMemberRepo(client *Client, parentID string, stores ...MemberCacheStore) *MemberRepo {
	var s MemberCacheStore
	if len(stores) > 0 {
		s = stores[0]
	}
	return &MemberRepo{
		client:        client,
		parentID:      parentID,
		store:         s,
		cache:         make(map[int64]*domain.Member),
		inflightLocks: make(map[int64]*sync.Mutex),
	}
}

// lookupCached resolves the member from the in-memory map or the persistent
// store, resolving a missing inline DB ID along the way. Returns nil when
// neither cache holds a mapping.
//
// A read failure while recovering the database ID is returned instead of being
// folded into a miss: EnsureMember uses this lookup to decide whether to
// provision a page, and GetMember uses it to decide whether a member has any
// trades. A transient Notion failure must produce neither a duplicate journal
// nor a silently empty history.
//
// The returned member may still have an empty NotionDBID when its page
// genuinely has no Trades database; callers repair that under the per-member
// lock (see ensureTradeDatabase) before returning it.
func (r *MemberRepo) lookupCached(ctx context.Context, telegramID int64) (*domain.Member, error) {
	// Check in-memory cache first.
	r.mu.RLock()
	cached, ok := r.cache[telegramID]
	r.mu.RUnlock()
	if ok && cached != nil {
		cp := *cached
		if cp.NotionDBID != "" {
			return &cp, nil
		}
		if cp.NotionPageID != "" {
			return r.resolveCachedMember(ctx, &cp)
		}
	}

	// Check persistent cache store.
	if r.store == nil {
		return nil, nil
	}
	persisted, err := r.store.GetCachedMember(ctx, telegramID)
	if err != nil || persisted == nil {
		// The persistent cache is an optimization; a read failure degrades to
		// the authoritative Notion lookup.
		return nil, nil
	}
	if persisted.NotionDBID != "" {
		cp := *persisted
		r.mu.Lock()
		r.cache[telegramID] = &cp
		r.mu.Unlock()
		return persisted, nil
	}
	if persisted.NotionPageID == "" {
		return nil, nil
	}
	return r.resolveCachedMember(ctx, persisted)
}

// resolveCachedMember recovers a missing inline Trades database ID by reading
// the cached member's journal page once.
//
// Read failures are surfaced so callers abort instead of acting on a mapping
// they could not verify. A page that genuinely has no Trades database is
// returned as-is so the caller can repair it; only fully-resolved mappings are
// memoized, so an unusable journal is never served as if it were usable.
func (r *MemberRepo) resolveCachedMember(ctx context.Context, m *domain.Member) (*domain.Member, error) {
	dbID, _, err := r.readMemberPage(ctx, m.NotionPageID)
	if err != nil {
		return nil, fmt.Errorf("read cached member page %s: %w", m.NotionPageID, err)
	}
	if dbID == "" {
		return m, nil
	}
	cp := *m
	cp.NotionDBID = dbID
	r.saveMemberToCache(ctx, &cp)
	return &cp, nil
}

// memberLock returns the per-Telegram-ID mutex serializing provisioning and
// repair of a member's journal.
func (r *MemberRepo) memberLock(telegramID int64) *sync.Mutex {
	r.inflightMu.Lock()
	defer r.inflightMu.Unlock()
	l, ok := r.inflightLocks[telegramID]
	if !ok {
		l = &sync.Mutex{}
		r.inflightLocks[telegramID] = l
	}
	return l
}

// EnsureMember creates the member page + inline trade database if it doesn't exist.
//
// A Notion lookup failure is returned to the caller instead of being treated as
// a cache miss: provisioning a page after an unverified lookup is exactly how
// duplicate journals — and trade history split across two databases — happen.
//
// The journal page is recorded in the persistent store immediately after it is
// created, before its Trades database exists, because Notion search indexes page
// titles only and the Telegram ID lives in a callout block. A retry therefore
// finds the leftover page through that pending mapping, or — if the mapping
// could not be written — through parent-scoped callout discovery, and repairs it
// in place instead of provisioning a second journal. Page creation and database
// creation are separate Notion calls, so a failure in between (or a manually
// deleted database) otherwise leaves the member permanently unable to record or
// read trades.
func (r *MemberRepo) EnsureMember(ctx context.Context, m *domain.Member) (*domain.Member, error) {
	found, err := r.lookupCached(ctx, m.TelegramID)
	if err != nil {
		return nil, err
	}
	if found != nil && found.NotionDBID != "" {
		return found, nil
	}

	// Refuse to provision while ResetAll is archiving journals: a page created
	// now could be archived in the same sweep, and a journal write must never
	// race the archival.
	release, err := r.beginJournaling()
	if err != nil {
		return nil, err
	}
	defer release()

	// Single-flight per Telegram ID: once serialized, re-check the caches so
	// the loser of a race reuses the winner's page instead of creating a
	// duplicate member page + trades database.
	lock := r.memberLock(m.TelegramID)
	lock.Lock()
	defer lock.Unlock()

	found, err = r.lookupCached(ctx, m.TelegramID)
	if err != nil {
		return nil, err
	}
	if found != nil && found.NotionDBID != "" {
		return found, nil
	}

	// A cached mapping without a Trades database is already a verified journal
	// page, so it can be repaired without searching Notion again. Otherwise
	// search for an existing page with matching Telegram ID. A failed lookup
	// must abort the call: treating "search/inspection failed" as "no existing
	// page" provisions a second member page + trades database for a member who
	// already has one, and trades recorded afterwards land in the orphaned copy.
	existing := found
	if existing == nil {
		existing, err = r.findMemberPage(ctx, m.TelegramID)
		if err != nil {
			return nil, fmt.Errorf("look up member page: %w", err)
		}
	}
	if existing != nil {
		return r.adoptMember(ctx, m, existing)
	}

	// Create member page
	title := fmt.Sprintf("@%s — Trade Journal", m.Username)
	if m.Username == "" {
		title = fmt.Sprintf("%s — Trade Journal", m.FirstName)
	}

	pagePayload := map[string]interface{}{
		"parent": map[string]interface{}{
			"page_id": r.parentID,
		},
		"properties": map[string]interface{}{
			"title": map[string]interface{}{
				"title": []map[string]interface{}{
					{"text": map[string]interface{}{"content": title}},
				},
			},
		},
		"children": []map[string]interface{}{
			{
				"object": "block",
				"type":   "callout",
				"callout": map[string]interface{}{
					"icon": map[string]interface{}{"emoji": "📊"},
					"rich_text": []map[string]interface{}{
						{"text": map[string]interface{}{"content": fmt.Sprintf("Telegram ID: %d | Username: @%s", m.TelegramID, m.Username)}},
					},
				},
			},
		},
	}

	raw, err := r.client.CreatePage(ctx, pagePayload)
	if err != nil {
		// A failed create may still have been applied (the response was lost,
		// or Notion returned a 5xx after committing). Creating again would
		// orphan the first journal, so reconcile by looking for a page under
		// the parent page whose callout identifies this member.
		if !IsAmbiguousWriteError(err) {
			return nil, fmt.Errorf("create member page: %w", err)
		}
		return r.reconcilePageCreate(ctx, m, err)
	}

	var pageResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &pageResp); err != nil {
		// The page was created but its body is unusable, so the page ID is
		// unknown. Reconcile through the parent page instead of reporting a
		// clean failure (which would invite a duplicate journal on retry).
		return r.reconcilePageCreate(ctx, m, fmt.Errorf("parse page response: %w", err))
	}
	if pageResp.ID == "" {
		return r.reconcilePageCreate(ctx, m, fmt.Errorf("parse page response: empty page id"))
	}
	m.NotionPageID = pageResp.ID

	// Record the page before creating the Trades database. Notion search
	// indexes page titles only, so a page whose Telegram ID lives in a callout
	// is not findable by that ID: without this mapping, a failed database
	// creation would be invisible to the retry, which would then provision a
	// second journal for the same member.
	r.persistPendingMember(ctx, m)

	// Create inline database for trades inside this member page. The page now
	// exists, so a failure here leaves a partially provisioned journal that the
	// next EnsureMember call repairs through the pending mapping (or, if that
	// mapping could not be written, through parent-scoped discovery).
	m, err = r.ensureTradeDatabase(ctx, m)
	if err != nil {
		return nil, err
	}

	r.saveMemberToCache(ctx, m)

	return m, nil
}

// reconcilePageCreate recovers from a create whose outcome is unknown: the page
// may exist even though no usable response was read.
//
// It looks for a verified journal under the parent page and adopts it. When no
// page can be found the error is returned marked as ambiguous, so the caller
// knows a journal may exist and must not simply retry.
func (r *MemberRepo) reconcilePageCreate(ctx context.Context, m *domain.Member, cause error) (*domain.Member, error) {
	recovered, reconcileErr := r.findMemberPageUnderParent(ctx, m.TelegramID)
	if reconcileErr == nil && recovered != nil {
		return r.adoptMember(ctx, m, recovered)
	}
	if reconcileErr != nil {
		return nil, AmbiguousWriteError(fmt.Errorf("create member page: %w (reconcile: %v)", cause, reconcileErr))
	}
	return nil, AmbiguousWriteError(fmt.Errorf("create member page: %w", cause))
}

// adoptMember takes over an existing verified journal page: it fills in the
// identity fields Notion search cannot supply, records the page before repairing
// it, creates the missing Trades database, and caches the result.
func (r *MemberRepo) adoptMember(ctx context.Context, m, existing *domain.Member) (*domain.Member, error) {
	if existing.Username == "" {
		existing.Username = m.Username
	}
	if existing.FirstName == "" {
		existing.FirstName = m.FirstName
	}
	// Record the page before attempting the repair, so a second failure (or a
	// restart in between) still finds it instead of searching — and possibly
	// duplicating — again.
	r.persistPendingMember(ctx, existing)
	repaired, err := r.ensureTradeDatabase(ctx, existing)
	if err != nil {
		return nil, err
	}
	r.saveMemberToCache(ctx, repaired)
	return repaired, nil
}

// persistPendingMember records a just-created journal page that has no Trades
// database yet, so a retry after a failed database creation repairs that page
// instead of provisioning a duplicate one.
//
// The mapping goes to the persistent store only: lookupCached resolves a
// mapping without a database by reading its page and leaves it to the caller to
// repair, whereas saveMemberToCache only ever memoizes usable members. A store
// failure is not fatal — the parent-scoped discovery in findMemberPage covers
// the retry — but it must not block the database creation that follows.
func (r *MemberRepo) persistPendingMember(ctx context.Context, m *domain.Member) {
	if r.store == nil || m.NotionPageID == "" || m.NotionDBID != "" {
		// Nothing to record: no store, no page yet, or the journal is already
		// complete (never downgrade a usable mapping).
		return
	}
	pending := *m
	pending.NotionDBID = ""
	_ = r.store.SaveCachedMember(ctx, &pending)
}

// ensureTradeDatabase returns the member with a usable inline Trades database,
// creating one inside the existing journal page when it is missing.
//
// It is the repair path for a partially provisioned journal: page creation and
// database creation are separate Notion calls, so a failure in between (or a
// manually deleted database) would otherwise leave the member permanently
// unable to record or read trades. Creation failures are returned so callers
// never cache an unusable mapping or report an empty trade history.
//
// An ambiguous create failure (network error, unreadable response, 5xx) may
// still have created the database. It is reconciled by reading the page: a
// database that exists there is adopted instead of creating a second one, which
// would split the member's trade history in two.
//
// Callers must hold the per-Telegram-ID lock so two concurrent repairs cannot
// create two Trades databases in the same journal.
func (r *MemberRepo) ensureTradeDatabase(ctx context.Context, m *domain.Member) (*domain.Member, error) {
	if m.NotionDBID != "" {
		return m, nil
	}
	if m.NotionPageID == "" {
		return nil, fmt.Errorf("member %d has no journal page", m.TelegramID)
	}
	dbID, err := r.createTradeDatabase(ctx, m.NotionPageID)
	if err != nil {
		if !IsAmbiguousWriteError(err) {
			return nil, fmt.Errorf("create missing trade database for member %d: %w", m.TelegramID, err)
		}
		created, _, readErr := r.readMemberPage(ctx, m.NotionPageID)
		if readErr != nil || created == "" {
			return nil, AmbiguousWriteError(fmt.Errorf("create missing trade database for member %d: %w", m.TelegramID, err))
		}
		dbID = created
	}
	if dbID == "" {
		// A 2xx without a database id still means the database may exist, so
		// reconcile by reading the page before reporting a failure.
		created, _, readErr := r.readMemberPage(ctx, m.NotionPageID)
		if readErr != nil || created == "" {
			return nil, AmbiguousWriteError(fmt.Errorf("create missing trade database for member %d: empty database id", m.TelegramID))
		}
		dbID = created
	}
	cp := *m
	cp.NotionDBID = dbID
	return &cp, nil
}

func (r *MemberRepo) saveMemberToCache(ctx context.Context, m *domain.Member) {
	if m.NotionDBID == "" {
		// Never memoize a journal without a usable Trades database: serving it
		// later would look like a member with an empty trade history (and a
		// journal that can never record another trade).
		return
	}
	cp := *m
	r.mu.Lock()
	if prev, ok := r.cache[m.TelegramID]; ok && prev != nil {
		// A member discovered through Notion search carries no identity data;
		// keep the richer cached values instead of overwriting them.
		if cp.Username == "" {
			cp.Username = prev.Username
		}
		if cp.FirstName == "" {
			cp.FirstName = prev.FirstName
		}
		if cp.JoinDate.IsZero() {
			cp.JoinDate = prev.JoinDate
		}
	}
	r.cache[m.TelegramID] = &cp
	r.mu.Unlock()
	if r.store != nil {
		_ = r.store.SaveCachedMember(ctx, &cp)
	}
}

// GetMember retrieves a member by Telegram ID.
//
// A journal that exists without its inline Trades database is repaired here as
// well: /history, /stats and exports must not report an empty history for a
// member whose database creation failed once.
func (r *MemberRepo) GetMember(ctx context.Context, telegramID int64) (*domain.Member, error) {
	m, err := r.lookupCached(ctx, telegramID)
	if err != nil {
		return nil, err
	}
	if m != nil && m.NotionDBID != "" {
		return m, nil
	}

	// Repair under the same gate and per-member lock EnsureMember uses, so a
	// concurrent provisioning or repair cannot create a second Trades database,
	// and no repair can overlap ResetAll archiving the journal it touches. The
	// gate is taken before the member lock to keep one lock order: acquiring a
	// read lock while holding a member lock would deadlock against a pending
	// reset writer.
	release, err := r.beginJournaling()
	if err != nil {
		return nil, err
	}
	defer release()

	lock := r.memberLock(telegramID)
	lock.Lock()
	defer lock.Unlock()

	if cached, err := r.lookupCached(ctx, telegramID); err != nil {
		return nil, err
	} else if cached != nil && cached.NotionDBID != "" {
		return cached, nil
	} else if cached != nil {
		m = cached
	}

	if m == nil {
		found, err := r.findMemberPage(ctx, telegramID)
		if err != nil {
			return nil, err
		}
		if found == nil {
			return nil, fmt.Errorf("member %d not found", telegramID)
		}
		m = found
	}

	// Record the page before repairing it: if the repair fails, the retry must
	// find this journal through the mapping instead of scanning (and possibly
	// duplicating) again.
	r.persistPendingMember(ctx, m)
	m, err = r.ensureTradeDatabase(ctx, m)
	if err != nil {
		return nil, err
	}
	r.saveMemberToCache(ctx, m)
	return m, nil
}

// ListMembers returns all known members.
//
// The persistent store is always merged in (not only when the in-memory map is
// cold): previously a single member cached in memory — e.g. the one who just
// sent a command — suppressed the Badger roster entirely, so leaderboards,
// weekly reports and challenge standings silently ranked a subset of the
// community. In-memory entries win on conflict because they are fresher.
//
// The journaling gate is held for reading, so the roster cannot be built from a
// half-reset state: reading the caches while ResetAll is archiving would merge a
// mapping that is about to be evicted (or report a member whose journal is
// already gone), and the merge itself would repopulate the in-memory cache with
// a stale mapping.
func (r *MemberRepo) ListMembers(ctx context.Context) ([]domain.Member, error) {
	r.gate.RLock()
	defer r.gate.RUnlock()

	if r.store != nil {
		persisted, err := r.store.ListCachedMembers(ctx)
		if err != nil {
			return nil, fmt.Errorf("list cached members: %w", err)
		}
		if len(persisted) > 0 {
			r.mu.Lock()
			for i := range persisted {
				id := persisted[i].TelegramID
				if persisted[i].NotionDBID == "" {
					// Pending mapping: the journal page exists but its Trades
					// database does not, so the member has no readable history
					// yet. It is repaired on the next journaling call; until
					// then it must not appear in community rankings.
					continue
				}
				if _, ok := r.cache[id]; ok {
					continue // in-memory copy is authoritative
				}
				cp := persisted[i]
				r.cache[id] = &cp
			}
			r.mu.Unlock()
		}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	members := make([]domain.Member, 0, len(r.cache))
	for _, m := range r.cache {
		members = append(members, *m)
	}
	return members, nil
}

// findMemberPage locates an existing journal page for a Telegram ID.
//
// Notion search indexes page titles only, while the Telegram ID lives in a
// callout block, so search is a best-effort first attempt (it still finds pages
// a user renamed to contain the ID) and the authoritative lookup walks the
// configured parent page's child pages. Both abort on read failures:
// provisioning after an unverified lookup creates a duplicate journal.
func (r *MemberRepo) findMemberPage(ctx context.Context, telegramID int64) (*domain.Member, error) {
	found, err := r.searchMemberPage(ctx, telegramID)
	if err != nil || found != nil {
		return found, err
	}
	return r.findMemberPageUnderParent(ctx, telegramID)
}

// searchMemberPage searches for a member page by Telegram ID and verifies the
// match before using it.
//
// Notion search is fuzzy: it can return unrelated pages, and a result page
// whose callout holds a different Telegram ID must never be used — writing a
// trade to the wrong member's database is silent data corruption. Every
// candidate is therefore confirmed by reading its callout text, and an
// ambiguous or unverifiable result is reported as not-found instead of being
// guessed from results[0].
//
// Read failures are returned as errors rather than being folded into
// "not found": EnsureMember uses this lookup to decide whether to provision a
// page, and a transient Notion failure must not produce a duplicate journal.
func (r *MemberRepo) searchMemberPage(ctx context.Context, telegramID int64) (*domain.Member, error) {
	items, err := r.client.SearchPagesAll(ctx, fmt.Sprintf("Telegram ID: %d", telegramID))
	if err != nil {
		return nil, err
	}

	want := fmt.Sprintf("Telegram ID: %d", telegramID)

	for _, item := range items {
		var page struct {
			ID       string `json:"id"`
			Archived bool   `json:"archived"`
		}
		if err := json.Unmarshal(item, &page); err != nil {
			return nil, fmt.Errorf("parse search result: %w", err)
		}
		if page.ID == "" || page.Archived {
			// An archived page must never be adopted: it was removed from the
			// bot's journals, and writing trades into it hides them.
			continue
		}

		dbID, callout, err := r.readMemberPage(ctx, page.ID)
		if err != nil {
			return nil, fmt.Errorf("inspect page %s: %w", page.ID, err)
		}
		if !containsTelegramID(callout, want) {
			continue // search matched some other member's page
		}
		return &domain.Member{
			TelegramID:   telegramID,
			NotionPageID: page.ID,
			NotionDBID:   dbID,
		}, nil
	}

	return nil, nil
}

// findMemberPageUnderParent walks the direct child pages of the configured
// parent page and verifies each callout against the wanted Telegram ID.
//
// This is the lookup search cannot provide: the ID is stored in a callout
// block, which Notion search does not index, so a journal left behind by an
// earlier failed provisioning attempt is only discoverable here. A page that
// cannot be read is remembered and reported when no verified match was found —
// treating "could not read" as "not a member page" is exactly what provisions a
// duplicate journal — while a match already found is still returned.
func (r *MemberRepo) findMemberPageUnderParent(ctx context.Context, telegramID int64) (*domain.Member, error) {
	if r.parentID == "" {
		return nil, nil
	}

	items, err := r.client.GetBlockChildrenAll(ctx, r.parentID)
	if err != nil {
		return nil, fmt.Errorf("list parent page children: %w", err)
	}

	want := fmt.Sprintf("Telegram ID: %d", telegramID)
	var firstErr error
	for _, item := range items {
		var blk struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Archived bool   `json:"archived"`
		}
		if err := json.Unmarshal(item, &blk); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("parse parent page child block: %w", err)
			}
			continue
		}
		if blk.Archived || blk.Type != "child_page" || blk.ID == "" {
			continue
		}
		dbID, callout, err := r.readMemberPage(ctx, blk.ID)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("inspect page %s: %w", blk.ID, err)
			}
			continue
		}
		if !containsTelegramID(callout, want) {
			continue
		}
		return &domain.Member{
			TelegramID:   telegramID,
			NotionPageID: blk.ID,
			NotionDBID:   dbID,
		}, nil
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, nil
}

// containsTelegramID reports whether callout text identifies the wanted member.
//
// The comparison is digit-boundary aware so "Telegram ID: 100" does not match a
// callout for "Telegram ID: 1001".
func containsTelegramID(callout, want string) bool {
	idx := strings.Index(callout, want)
	if idx < 0 {
		return false
	}
	end := idx + len(want)
	if end >= len(callout) {
		return true
	}
	next := callout[end]
	return next < '0' || next > '9'
}

// memberJournalTelegramID extracts the Telegram ID from the callout marker
// EnsureMember writes on every journal page it provisions, reporting ok=false
// for any other callout.
//
// Reset uses it to tell bot-created journals apart from the owner's own pages
// that happen to live under the same parent page — archiving those would destroy
// content the bot never created — and to know which persisted member mapping to
// evict once the page is archived. The username suffix is optional because it is
// informational, but the "Telegram ID: <digits>" prefix must be exact.
func memberJournalTelegramID(callout string) (int64, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(callout), "Telegram ID: ")
	if !ok {
		return 0, false
	}
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	if tail := rest[end:]; tail != "" && !strings.HasPrefix(tail, " | Username: @") {
		return 0, false
	}
	id, err := strconv.ParseInt(rest[:end], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// readMemberPage reads a member page's children once, returning the inline
// Trades database ID and the callout text used to identify the member.
//
// It surfaces read and parse failures, so callers that base provisioning or
// archival decisions on the result can abort instead of acting on a page they
// could not verify.
func (r *MemberRepo) readMemberPage(ctx context.Context, pageID string) (dbID, callout string, err error) {
	if pageID == "" {
		return "", "", fmt.Errorf("empty page ID")
	}
	items, err := r.client.GetBlockChildrenAll(ctx, pageID)
	if err != nil {
		return "", "", err
	}
	for _, item := range items {
		var blk struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Callout *struct {
				RichText []struct {
					PlainText string `json:"plain_text"`
				} `json:"rich_text"`
			} `json:"callout"`
		}
		if err := json.Unmarshal(item, &blk); err != nil {
			return "", "", fmt.Errorf("parse block of page %s: %w", pageID, err)
		}
		switch {
		case blk.Type == "child_database" && blk.ID != "" && dbID == "":
			dbID = blk.ID
		case blk.Type == "callout" && blk.Callout != nil && callout == "":
			var sb strings.Builder
			for _, rt := range blk.Callout.RichText {
				sb.WriteString(rt.PlainText)
			}
			callout = sb.String()
		}
	}
	return dbID, callout, nil
}

// createTradeDatabase creates the inline Notion database for trades.
func (r *MemberRepo) createTradeDatabase(ctx context.Context, parentPageID string) (string, error) {
	dbPayload := map[string]interface{}{
		"parent": map[string]interface{}{
			"page_id": parentPageID,
		},
		"title": []map[string]interface{}{
			{"text": map[string]interface{}{"content": "Trades"}},
		},
		"is_inline": true,
		"properties": map[string]interface{}{
			"Symbol": map[string]interface{}{
				"title": map[string]interface{}{},
			},
			"Date": map[string]interface{}{
				"date": map[string]interface{}{},
			},
			"Asset Type": map[string]interface{}{
				"select": map[string]interface{}{
					"options": []map[string]interface{}{
						{"name": "Forex", "color": "blue"},
						{"name": "Gold", "color": "yellow"},
						{"name": "Indices", "color": "green"},
						{"name": "Crypto", "color": "purple"},
						{"name": "Commodities", "color": "orange"},
					},
				},
			},
			"Direction": map[string]interface{}{
				"select": map[string]interface{}{
					"options": []map[string]interface{}{
						{"name": "BUY", "color": "green"},
						{"name": "SELL", "color": "red"},
					},
				},
			},
			"Status": map[string]interface{}{
				"select": map[string]interface{}{
					"options": []map[string]interface{}{
						{"name": "OPEN", "color": "default"},
						{"name": "WIN", "color": "green"},
						{"name": "LOSS", "color": "red"},
						{"name": "BE", "color": "gray"},
					},
				},
			},
			"Result RR": map[string]interface{}{
				"number": map[string]interface{}{"format": "number"},
			},
			"Time Window": map[string]interface{}{
				"select": map[string]interface{}{
					"options": []map[string]interface{}{
						{"name": "Asia", "color": "blue"},
						{"name": "London", "color": "green"},
						{"name": "NY AM", "color": "yellow"},
						{"name": "NY PM", "color": "orange"},
					},
				},
			},
			"Confluence": map[string]interface{}{
				"rich_text": map[string]interface{}{},
			},
			"Notes": map[string]interface{}{
				"rich_text": map[string]interface{}{},
			},
		},
	}

	raw, err := r.client.CreateDatabase(ctx, dbPayload)
	if err != nil {
		return "", err
	}

	var dbResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &dbResp); err != nil {
		// The database was created; only its id is unknown, so the caller must
		// reconcile instead of creating a second one.
		return "", AmbiguousWriteError(fmt.Errorf("parse database response: %w", err))
	}
	return dbResp.ID, nil
}

func normalizeNotionID(id string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(id), "-", ""))
}

// ResetAll archives every verified bot member journal under the configured
// parent page and evicts each archived member from both caches.
//
// Only pages carrying the "Telegram ID: <id>" callout that EnsureMember writes
// are archived. The parent page is shared workspace content, and archiving all
// of its children — or every search hit titled "Trade Journal" — destroys notes
// and databases the bot never created. Child databases are never archived
// directly either: each member's Trades database is inline in its journal page,
// so archiving the page already takes it along.
//
// Discovery failures are returned instead of being swallowed: reporting a
// successful reset after the parent page or search could not be read would tell
// the owner their journals are gone while they are still live in Notion. The
// caches are left untouched on a discovery failure, so a later EnsureMember
// reuses the live journals instead of provisioning duplicates.
//
// Archived journals are evicted from both caches, and the persisted mapping is
// evicted *before* the page is archived. That ordering is what keeps the two
// stores consistent across a failure: if the mapping cannot be removed the page
// is left live (and reusable) and the reset reports an error, instead of leaving
// a mapping that points at an archived journal. If archiving fails after the
// mapping is gone, the journal is still live and is rediscovered by
// parent-scoped lookup, so the retry cannot duplicate it.
//
// The archival phase holds the journaling gate, so no trade can be written into
// a journal while it is being archived; concurrent journal writes fail fast with
// ErrResetInProgress instead of landing in a page that is about to disappear.
//
// The gate is taken before discovery, not after: a journal provisioned between
// discovery and archival would otherwise be created while the reset already
// believes it has the full candidate set.
func (r *MemberRepo) ResetAll(ctx context.Context) (int, error) {
	return r.ResetAllWith(ctx, nil)
}

// ResetAllWith performs the same reset and, when finalize is non-nil, runs it
// while the journaling gate is still held.
//
// A caller that wipes the persistent store after archiving (the bot's /reset)
// must pass that wipe as finalize: releasing the gate first would let a new
// journal write land in the store being cleared, leaving a Notion journal with
// no mapping — or a mapping with no gamification state.
func (r *MemberRepo) ResetAllWith(ctx context.Context, finalize func() error) (int, error) {
	// Take the gate before discovery so nothing can be provisioned into the
	// candidate set while it is being computed and archived.
	r.gate.Lock()
	defer r.gate.Unlock()

	candidates := make(map[string]struct{})

	if r.parentID != "" {
		normParent := normalizeNotionID(r.parentID)

		// 1. Direct child pages of the parent page. The parent also holds the
		// owner's own content, so these are only candidates until verified.
		items, err := r.client.GetBlockChildrenAll(ctx, r.parentID)
		if err != nil {
			return 0, fmt.Errorf("list parent page children: %w", err)
		}
		for _, item := range items {
			var blk struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Archived bool   `json:"archived"`
			}
			if err := json.Unmarshal(item, &blk); err != nil {
				return 0, fmt.Errorf("parse parent page child block: %w", err)
			}
			if blk.Archived || blk.Type != "child_page" || blk.ID == "" {
				continue
			}
			candidates[blk.ID] = struct{}{}
		}

		// 2. Search pages matching "Trade Journal", restricted to pages that
		// actually live under the configured parent page.
		results, err := r.client.SearchPagesAll(ctx, "Trade Journal")
		if err != nil {
			return 0, fmt.Errorf("search member journals: %w", err)
		}
		for _, item := range results {
			var p struct {
				ID       string `json:"id"`
				Archived bool   `json:"archived"`
				Parent   struct {
					Type   string `json:"type"`
					PageID string `json:"page_id"`
				} `json:"parent"`
			}
			if err := json.Unmarshal(item, &p); err != nil {
				return 0, fmt.Errorf("parse member journal search result: %w", err)
			}
			if p.Archived || p.ID == "" {
				continue
			}
			if normalizeNotionID(p.Parent.PageID) != normParent {
				continue // another page, another database, or another workspace
			}
			candidates[p.ID] = struct{}{}
		}
	}

	// 3. Cached member page IDs are known bot artifacts, but they are verified
	// like every other candidate: a stale cache entry must not archive a page
	// the bot no longer owns.
	r.mu.RLock()
	for _, m := range r.cache {
		if m != nil && m.NotionPageID != "" {
			candidates[m.NotionPageID] = struct{}{}
		}
	}
	r.mu.RUnlock()

	// The journaling gate is already held (taken before discovery), so no
	// member can have a journal provisioned, repaired or written while these
	// candidates are verified and archived.

	verified := make(map[string]int64, len(candidates))
	for pageID := range candidates {
		_, callout, err := r.readMemberPage(ctx, pageID)
		if err != nil {
			return 0, fmt.Errorf("verify member journal %s: %w", pageID, err)
		}
		if telegramID, ok := memberJournalTelegramID(callout); ok {
			verified[pageID] = telegramID
		}
	}

	archived := make(map[string]struct{}, len(verified))
	archivedCount := 0
	var firstErr error
	for pageID, telegramID := range verified {
		// Evict the mapping first. Archiving first would leave a mapping that
		// resolves to an archived journal whenever the store is unavailable,
		// and the next EnsureMember would then write trades into a page nobody
		// can see. Evicting first makes the failure safe: the journal stays
		// live and reusable.
		if r.store != nil {
			if err := r.evictCachedMember(ctx, telegramID); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
		}

		if _, err := r.client.UpdatePage(ctx, pageID, map[string]interface{}{"archived": true}); err != nil {
			// The persisted mapping is already gone, so the page can only be
			// reused through the in-memory cache. An ambiguous failure may
			// still have archived it, which would make that cached mapping
			// stale, so verify the page's archived flag.
			if IsAmbiguousWriteError(err) {
				isArchived, verifyErr := r.pageArchived(ctx, pageID)
				switch {
				case verifyErr == nil && isArchived:
					// Confirmed archived: count it and forget the mapping.
					archived[normalizeNotionID(pageID)] = struct{}{}
					archivedCount++
					continue
				case verifyErr != nil:
					// Unknown outcome: forget the mapping anyway. A live page
					// is rediscovered by parent-scoped lookup, while an
					// archived one must never be handed out again.
					archived[normalizeNotionID(pageID)] = struct{}{}
				}
			}
			// The page is still live (or its state is unknown), so a retry
			// repairs it rather than provisioning a duplicate.
			if firstErr == nil {
				firstErr = fmt.Errorf("archive member journal %s: %w", pageID, err)
			}
			continue
		}
		archived[normalizeNotionID(pageID)] = struct{}{}
		archivedCount++
	}

	r.mu.Lock()
	for id, m := range r.cache {
		if m == nil {
			delete(r.cache, id)
			continue
		}
		// Only members whose journal was actually archived are forgotten;
		// everyone else keeps their live journal mapping for the retry.
		if _, ok := archived[normalizeNotionID(m.NotionPageID)]; ok {
			delete(r.cache, id)
		}
	}
	r.mu.Unlock()

	if firstErr != nil {
		// Never wipe the persistent store after a partial archive: the owner
		// retries /reset, and the journals still live must keep their mappings
		// until they are actually archived.
		return archivedCount, firstErr
	}
	if finalize != nil {
		if err := finalize(); err != nil {
			// The gate stays held until this function returns, so the caller's
			// store wipe cannot overlap a new journal write.
			return archivedCount, err
		}
	}

	return archivedCount, nil
}

// pageArchived reads a page's archived flag.
//
// Reset uses it to resolve an ambiguous archive response: Notion may have
// applied the archive even though no successful response was read, and a stale
// mapping for an archived journal would send the next trades into a page nobody
// can see.
func (r *MemberRepo) pageArchived(ctx context.Context, pageID string) (bool, error) {
	raw, err := r.client.GetPage(ctx, pageID)
	if err != nil {
		return false, err
	}
	var page struct {
		Archived bool `json:"archived"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return false, fmt.Errorf("parse page %s: %w", pageID, err)
	}
	return page.Archived, nil
}

// evictCachedMember drops a persisted mapping before its journal is archived.
//
// A plain delete is retried once so a transient store failure does not abort the
// reset. If the key still cannot be removed the error is returned and the caller
// leaves the journal live: a mapping must never outlive the page it points at,
// because the next EnsureMember would then reuse an archived journal.
func (r *MemberRepo) evictCachedMember(ctx context.Context, telegramID int64) error {
	err := r.store.DeleteCachedMember(ctx, telegramID)
	if err == nil {
		return nil
	}
	if retryErr := r.store.DeleteCachedMember(ctx, telegramID); retryErr == nil {
		return nil
	}
	return fmt.Errorf("evict cached member %d: %w", telegramID, err)
}
