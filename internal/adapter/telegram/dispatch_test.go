package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/adapter/badger"
	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/internal/service"
)

// resetUpdate builds a confirmed (destructive) owner reset update.
func resetUpdate(updateID int) Update {
	return Update{UpdateID: updateID, Message: &Message{
		From: &User{ID: 99, Username: "owner"},
		Chat: &Chat{ID: 99, Type: "private"},
		Text: "/reset confirm",
	}}
}

func privateUpdate(userID int64, updateID int) Update {
	return Update{UpdateID: updateID, Message: &Message{
		From: &User{ID: userID, Username: "trader"},
		Chat: &Chat{ID: userID, Type: "private"},
	}}
}

// TestUpdateDispatcher_PreservesPerUserOrder pins the core ordering guarantee:
// one user's updates are handled in arrival order even when an earlier update
// is slow. Telegram delivers updates in update_id order, so a slow handler
// finishing last used to reorder a guided answer behind the callback or
// command that followed it.
func TestUpdateDispatcher_PreservesPerUserOrder(t *testing.T) {
	var (
		mu  sync.Mutex
		got []int
	)
	d := newUpdateDispatcher(context.Background(), 4, func(_ context.Context, u Update) {
		if u.UpdateID == 1 {
			// Slow first update: a concurrent dispatcher would let 2 and 3 finish first.
			time.Sleep(40 * time.Millisecond)
		}
		mu.Lock()
		got = append(got, u.UpdateID)
		mu.Unlock()
	}, nil)

	ctx := context.Background()
	for i := range 3 {
		if err := d.Enqueue(ctx, privateUpdate(7, i+1)); err != nil {
			t.Fatalf("enqueue %d: %v", i+1, err)
		}
	}
	if !d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not drain in time")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []int{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("processed %d updates, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("per-user order broken: got %v, want %v", got, want)
		}
	}
}

// TestUpdateDispatcher_DifferentUsersRunConcurrently pins that serialisation
// is per user only: two users must be able to be in flight at the same time,
// otherwise one slow Notion call would stall the whole community.
func TestUpdateDispatcher_DifferentUsersRunConcurrently(t *testing.T) {
	var started int32
	both := make(chan struct{})
	d := newUpdateDispatcher(context.Background(), 4, func(_ context.Context, _ Update) {
		if atomic.AddInt32(&started, 1) == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-time.After(2 * time.Second):
			t.Error("updates from different users were not processed concurrently")
		}
	}, nil)

	ctx := context.Background()
	if err := d.Enqueue(ctx, privateUpdate(1, 1)); err != nil {
		t.Fatalf("enqueue user 1: %v", err)
	}
	if err := d.Enqueue(ctx, privateUpdate(2, 2)); err != nil {
		t.Fatalf("enqueue user 2: %v", err)
	}
	if !d.Wait(5 * time.Second) {
		t.Fatal("dispatcher did not drain in time")
	}
}

// TestUpdateDispatcher_RespectsConcurrencyLimit pins the semaphore bound so a
// burst of messages cannot spawn unbounded goroutines.
func TestUpdateDispatcher_RespectsConcurrencyLimit(t *testing.T) {
	var inFlight, maxInFlight int32
	d := newUpdateDispatcher(context.Background(), 1, func(_ context.Context, _ Update) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
	}, nil)

	ctx := context.Background()
	for i := range 4 {
		if err := d.Enqueue(ctx, privateUpdate(int64(i+1), i+1)); err != nil {
			t.Fatalf("enqueue %d: %v", i+1, err)
		}
	}
	if !d.Wait(5 * time.Second) {
		t.Fatal("dispatcher did not drain in time")
	}
	if got := atomic.LoadInt32(&maxInFlight); got != 1 {
		t.Fatalf("max concurrent handlers = %d, want 1", got)
	}
}

// TestUpdateDispatcher_EnqueueUnblocksOnCancel pins shutdown behaviour while
// the admission budget is saturated: Start must return promptly on ctx
// cancellation instead of hanging on a full pending queue.
func TestUpdateDispatcher_EnqueueUnblocksOnCancel(t *testing.T) {
	release := make(chan struct{})
	d := newUpdateDispatcher(context.Background(), 1, func(_ context.Context, _ Update) {
		<-release
	}, nil)
	d.pending = make(chan struct{}, 1) // one admission slot: the second update blocks

	ctx, cancel := context.WithCancel(context.Background())
	if err := d.Enqueue(ctx, privateUpdate(1, 1)); err != nil {
		t.Fatalf("enqueue first update: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- d.Enqueue(ctx, privateUpdate(2, 2)) }()

	time.Sleep(20 * time.Millisecond) // let the second enqueue block on the full budget
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected context error from a saturated enqueue")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue did not unblock after cancellation")
	}

	close(release)
	if !d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not drain after release")
	}
}

func TestUpdateDispatcher_EnqueueUnblocksOnFatal(t *testing.T) {
	d := newUpdateDispatcher(context.Background(), 1, func(context.Context, Update) {}, nil)
	d.pending = make(chan struct{}, 1)
	d.pending <- struct{}{}
	want := errors.New("ledger unavailable")
	result := make(chan error, 1)
	go func() { result <- d.Enqueue(context.Background(), privateUpdate(1, 1)) }()
	d.fail(want)
	select {
	case err := <-result:
		if !errors.Is(err, want) {
			t.Fatalf("enqueue error = %v, want %v", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue remained blocked after fatal ledger error")
	}
}

// TestUpdateDispatcher_QueuedBacklogDoesNotStarveOtherUsers pins the split
// between the admission budget and the execution budget: one user's backlog
// must not consume execution slots, so another user's update still starts
// while the first user's handler is in flight. Under the old single-semaphore
// design the third enqueue of user 1 would have blocked the poll loop.
func TestUpdateDispatcher_QueuedBacklogDoesNotStarveOtherUsers(t *testing.T) {
	release := make(chan struct{})
	otherStarted := make(chan struct{})
	var otherOnce sync.Once
	d := newUpdateDispatcher(context.Background(), 2, func(_ context.Context, u Update) {
		if u.Message.From.ID == 2 {
			otherOnce.Do(func() { close(otherStarted) })
			return
		}
		<-release
	}, nil)

	ctx := context.Background()
	for i := range 6 {
		if err := d.Enqueue(ctx, privateUpdate(1, i+1)); err != nil {
			t.Fatalf("enqueue backlog %d: %v", i+1, err)
		}
	}
	if err := d.Enqueue(ctx, privateUpdate(2, 7)); err != nil {
		t.Fatalf("enqueue other user: %v", err)
	}

	select {
	case <-otherStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("one user's queued backlog blocked another user's update")
	}
	close(release)
	if !d.Wait(5 * time.Second) {
		t.Fatal("dispatcher did not drain in time")
	}
}

// TestUpdateDispatcher_ShutdownFlushesQueuedUpdates pins that queued work is
// never started after cancellation and is reported as never-started, so its
// ledger row stays admitted and replays on the next start. Only the handler that
// was already running executes.
func TestUpdateDispatcher_ShutdownFlushesQueuedUpdates(t *testing.T) {
	workCtx, cancelWork := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	var (
		mu       sync.Mutex
		ran      []int
		outcomes []updateOutcome
	)
	d := newUpdateDispatcher(workCtx, 4, func(ctx context.Context, u Update) {
		mu.Lock()
		ran = append(ran, u.UpdateID)
		mu.Unlock()
		if u.UpdateID == 1 {
			close(started)
			<-release
		}
	}, nil)
	d.setOnDone(func(_ Update, outcome updateOutcome) {
		mu.Lock()
		outcomes = append(outcomes, outcome)
		mu.Unlock()
	})

	ctx := context.Background()
	for i := range 3 {
		if err := d.Enqueue(ctx, privateUpdate(9, i+1)); err != nil {
			t.Fatalf("enqueue %d: %v", i+1, err)
		}
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first update never started")
	}

	cancelWork()
	close(release)
	if !d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not finish after cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 1 || ran[0] != 1 {
		t.Fatalf("queued updates ran after cancellation: %v", ran)
	}
	if len(outcomes) != 3 {
		t.Fatalf("expected one completion report per accepted update, got %d", len(outcomes))
	}
	// Update 1 was cut short; 2 and 3 never started. A never-started update must
	// keep reporting Started=false so its ledger row stays admitted for replay.
	for i, o := range outcomes[1:] {
		if o.Started {
			t.Fatalf("queued update %d was reported as started", i+2)
		}
	}
}

// TestUpdateDispatcher_WaitReportsTimeout pins that the shutdown drain is
// bounded: a stuck handler must not block process exit forever.
func TestUpdateDispatcher_WaitReportsTimeout(t *testing.T) {
	release := make(chan struct{})
	d := newUpdateDispatcher(context.Background(), 1, func(_ context.Context, _ Update) {
		<-release
	}, nil)
	if err := d.Enqueue(context.Background(), privateUpdate(1, 1)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if d.Wait(30 * time.Millisecond) {
		t.Fatal("Wait must report a timeout while a handler is still running")
	}

	close(release)
	if !d.Wait(2 * time.Second) {
		t.Fatal("Wait must complete once handlers return")
	}
}

// TestUpdateDispatcher_QueuedUpdatesSurviveAdmissionCancel pins the shutdown
// contract: cancelling the context passed to Enqueue stops admission but must
// not abort updates already queued. Handlers run under the dispatcher's work
// context, so a queued #journal still reaches Notion instead of failing with
// context.Canceled after its offset was already advanced.
func TestUpdateDispatcher_QueuedUpdatesSurviveAdmissionCancel(t *testing.T) {
	pollCtx, cancelPoll := context.WithCancel(context.Background())
	defer cancelPoll()
	workCtx, cancelWork := context.WithCancel(context.WithoutCancel(pollCtx))
	defer cancelWork()

	started := make(chan struct{})
	release := make(chan struct{})
	var (
		mu   sync.Mutex
		got  []int
		errs []error
	)
	d := newUpdateDispatcher(workCtx, 4, func(ctx context.Context, u Update) {
		if u.UpdateID == 1 {
			close(started)
			<-release
		}
		mu.Lock()
		got = append(got, u.UpdateID)
		errs = append(errs, ctx.Err())
		mu.Unlock()
	}, nil)

	for i := range 3 {
		if err := d.Enqueue(pollCtx, privateUpdate(7, i+1)); err != nil {
			t.Fatalf("enqueue %d: %v", i+1, err)
		}
	}
	select {
	case <-started: // first update is in flight, the other two are queued
	case <-time.After(2 * time.Second):
		t.Fatal("first update never started")
	}

	cancelPoll()
	close(release)
	if !d.Wait(2 * time.Second) {
		t.Fatal("queued updates were abandoned after polling cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []int{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("processed %d updates, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("drain order = %v, want %v", got, want)
		}
		if errs[i] != nil {
			t.Fatalf("update %d ran with a cancelled context: %v", got[i], errs[i])
		}
	}
}

// TestUpdateDispatcher_CancelStopsStuckHandlers pins the other half of the
// bounded drain: once the budget expires the work context is cancelled, so a
// handler waiting on an HTTP round trip unwinds instead of holding the
// process past the container stop grace period.
func TestUpdateDispatcher_CancelStopsStuckHandlers(t *testing.T) {
	workCtx, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()

	stopped := make(chan struct{})
	d := newUpdateDispatcher(workCtx, 1, func(ctx context.Context, _ Update) {
		<-ctx.Done()
		close(stopped)
	}, nil)
	if err := d.Enqueue(context.Background(), privateUpdate(1, 1)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if d.Wait(30 * time.Millisecond) {
		t.Fatal("Wait must report a timeout while a handler is still running")
	}
	cancelWork()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("handler did not stop after the work context was cancelled")
	}
	if !d.Wait(time.Second) {
		t.Fatal("dispatcher did not finish after cancelling the work context")
	}
}

// TestUpdateDispatcher_PanicDoesNotStrandQueue pins liveness after a handler
// panic: the panicking update is reported, its execution slot is freed, and the
// same user's remaining updates still run in order.
func TestUpdateDispatcher_PanicDoesNotStrandQueue(t *testing.T) {
	var (
		mu       sync.Mutex
		got      []int
		panics   []int
		outcomes []updateOutcome
	)
	d := newUpdateDispatcher(context.Background(), 1, func(_ context.Context, u Update) {
		if u.UpdateID == 1 {
			panic("boom")
		}
		mu.Lock()
		got = append(got, u.UpdateID)
		mu.Unlock()
	}, func(u Update, _ any) {
		mu.Lock()
		panics = append(panics, u.UpdateID)
		mu.Unlock()
	})
	d.setOnDone(func(_ Update, outcome updateOutcome) {
		mu.Lock()
		outcomes = append(outcomes, outcome)
		mu.Unlock()
	})

	ctx := context.Background()
	for i := range 3 {
		if err := d.Enqueue(ctx, privateUpdate(5, i+1)); err != nil {
			t.Fatalf("enqueue %d: %v", i+1, err)
		}
	}
	if !d.Wait(2 * time.Second) {
		t.Fatal("a handler panic stranded the user's queue")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(panics) != 1 || panics[0] != 1 {
		t.Fatalf("panic callback got %v, want [1]", panics)
	}
	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("queue did not continue after a panic: %v", got)
	}
	if len(outcomes) != 3 {
		t.Fatalf("expected one report per update, got %v", outcomes)
	}
	// The panicking update started but never completed, so the ledger keeps it
	// running and the next start asks the user to verify.
	if !outcomes[0].Started || outcomes[0].Completed {
		t.Fatalf("panicking update reported %+v, want started but not completed", outcomes[0])
	}
	for i, o := range outcomes[1:] {
		if !o.Started || !o.Completed {
			t.Fatalf("update %d reported %+v, want started and completed", i+2, o)
		}
	}
}

// TestUpdateDispatcher_UnkeyedUpdatesAreNotSerialised pins that updates
// without a user (rare system updates) still run concurrently instead of all
// sharing one queue.
func TestUpdateDispatcher_UnkeyedUpdatesAreNotSerialised(t *testing.T) {
	var started int32
	both := make(chan struct{})
	d := newUpdateDispatcher(context.Background(), 4, func(_ context.Context, _ Update) {
		if atomic.AddInt32(&started, 1) == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-time.After(2 * time.Second):
			t.Error("unkeyed updates were serialised")
		}
	}, nil)

	ctx := context.Background()
	for i := range 2 {
		if err := d.Enqueue(ctx, Update{UpdateID: i + 1}); err != nil {
			t.Fatalf("enqueue %d: %v", i+1, err)
		}
	}
	if !d.Wait(5 * time.Second) {
		t.Fatal("dispatcher did not drain in time")
	}
}

// TestUpdateDispatcher_MessageAndCallbackShareUserQueue pins that a button
// press and a following text message from the same user are ordered against
// each other: a guided answer must never overtake the callback that advanced
// the flow to that step.
func TestUpdateDispatcher_MessageAndCallbackShareUserQueue(t *testing.T) {
	var (
		mu  sync.Mutex
		got []int
	)
	d := newUpdateDispatcher(context.Background(), 4, func(_ context.Context, u Update) {
		if u.UpdateID == 1 {
			time.Sleep(40 * time.Millisecond)
		}
		mu.Lock()
		got = append(got, u.UpdateID)
		mu.Unlock()
	}, nil)

	ctx := context.Background()
	cb := Update{UpdateID: 1, CallbackQuery: &CallbackQuery{ID: "cb", From: &User{ID: 9}}}
	if err := d.Enqueue(ctx, cb); err != nil {
		t.Fatalf("enqueue callback: %v", err)
	}
	if err := d.Enqueue(ctx, privateUpdate(9, 2)); err != nil {
		t.Fatalf("enqueue message: %v", err)
	}
	if !d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not drain in time")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("callback and message from one user were reordered: %v", got)
	}
}

// memReplayStore is an in-memory replayStore with the same semantics as the
// Badger one: insert-if-absent within a batch, recorded TTLs, injectable read and
// write failures.
type memReplayStore struct {
	mu      sync.Mutex
	data    map[string][]byte
	ttls    map[string]time.Duration
	failGet error
	failPut error
	// failSet fails single-key writes (SetWithTTL) only, so admission through
	// ApplyBatch can succeed while a later state transition fails.
	failSet func(key string) error
	// failDelete fails key deletions, so a purge that cannot be recorded is
	// observable instead of silently leaving stale rows behind.
	failDelete error
}

func newMemReplayStore() *memReplayStore {
	return &memReplayStore{data: make(map[string][]byte), ttls: make(map[string]time.Duration)}
}

func (m *memReplayStore) Get(key string, dest interface{}) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failGet != nil {
		return false, m.failGet
	}
	raw, ok := m.data[key]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return false, err
	}
	return true, nil
}

func (m *memReplayStore) SetWithTTL(key string, value interface{}, ttl time.Duration) error {
	if m.failSet != nil {
		if err := m.failSet(key); err != nil {
			return err
		}
	}
	if m.failPut != nil {
		return m.failPut
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = raw
	m.ttls[key] = ttl
	return nil
}

func (m *memReplayStore) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failDelete != nil {
		return m.failDelete
	}
	delete(m.data, key)
	delete(m.ttls, key)
	return nil
}

func (m *memReplayStore) Scan(prefix string) ([][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.data))
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([][]byte, 0, len(keys))
	for _, k := range keys {
		out = append(out, append([]byte(nil), m.data[k]...))
	}
	return out, nil
}

// ApplyBatch mirrors the Badger primitive: all-or-nothing, insert-if-absent.
func (m *memReplayStore) ApplyBatch(insertIfAbsent, put map[string][]byte) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPut != nil {
		return nil, m.failPut
	}
	inserted := make([]string, 0, len(insertIfAbsent))
	for k, v := range insertIfAbsent {
		if _, ok := m.data[k]; ok {
			continue
		}
		inserted = append(inserted, k)
		m.data[k] = append([]byte(nil), v...)
	}
	for k, v := range put {
		m.data[k] = append([]byte(nil), v...)
	}
	sort.Strings(inserted)
	return inserted, nil
}

func (m *memReplayStore) record(updateID int) (updateRecord, bool) {
	var rec updateRecord
	found, err := m.Get(testClaimKey(updateID), &rec)
	if err != nil || !found {
		return updateRecord{}, false
	}
	return rec, true
}

func (m *memReplayStore) claimState(updateID int) (updateState, bool) {
	rec, ok := m.record(updateID)
	if !ok {
		return "", false
	}
	return rec.State, true
}

func (m *memReplayStore) cursorOffset() int {
	var cur fetchCursor
	found, err := m.Get(testGuard.cursorKey(), &cur)
	if err != nil || !found {
		return 0
	}
	return cur.Offset
}

func (m *memReplayStore) seedRecord(rec updateRecord) {
	_ = m.SetWithTTL(testClaimKey(int(rec.UpdateID)), rec, 0)
}

func (m *memReplayStore) noticeCount() int {
	raw, _ := m.Scan(testGuard.noticeScanPrefix())
	return len(raw)
}

// testBotToken is the token newJournalTestBot uses; the ledger scope derives
// from it, so seeded keys match what the bot under test reads.
const testBotToken = "test-token"

var testGuard = &replayGuard{scope: botScope(testBotToken)}

func newTestGuard(store replayStore) *replayGuard {
	return &replayGuard{store: store, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), scope: testGuard.scope}
}

func testClaimKey(updateID int) string { return testGuard.claimKey(updateID) }

// seedAdmitted stores a durable admitted row for u, the state a previous process
// leaves after Telegram acknowledged the batch.
func seedAdmitted(t *testing.T, store *memReplayStore, u Update) {
	t.Helper()
	payload, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal update %d: %v", u.UpdateID, err)
	}
	store.seedRecord(updateRecord{UpdateID: int64(u.UpdateID), State: stateAdmitted, Payload: payload})
}

// ledgerHarness wires a dispatcher to a durable ledger the way Start does: the
// start hook records the row as running (deferring a destructive reset while
// older rows are unfinished), the completion hook closes the row and releases
// the reset barrier, and deferred updates are parked for the retry loop.
type ledgerHarness struct {
	d         *updateDispatcher
	deferred  chan Update
	deferrals chan int
}

func newLedgerHarness(replay *replayGuard, handle func(context.Context, Update)) *ledgerHarness {
	h := &ledgerHarness{
		deferred:  make(chan Update, 8),
		deferrals: make(chan int, 8),
	}
	h.d = newUpdateDispatcher(context.Background(), 4, handle, func(Update, any) {})
	h.d.resetCheck = isDestructiveReset
	h.d.setOnStart(func(u Update) error {
		if isDestructiveReset(u) {
			older, err := replay.olderPending(u.UpdateID)
			if err != nil {
				return err
			}
			if older > 0 {
				return errResetBlocked
			}
		}
		return replay.markRunning(u.UpdateID)
	})
	h.d.setOnDefer(func(u Update) {
		h.deferrals <- u.UpdateID
		if !requeue(h.deferred, u) {
			h.d.fail(fmt.Errorf("deferred update %d could not be queued", u.UpdateID))
		}
	})
	h.d.setOnDone(func(u Update, outcome updateOutcome) {
		if !outcome.Started {
			return
		}
		if !outcome.Completed || outcome.Aborted {
			return
		}
		if err := replay.markDone(u.UpdateID); err != nil {
			h.d.fail(fmt.Errorf("could not complete update %d: %w", u.UpdateID, err))
			return
		}
		if isDestructiveReset(u) {
			h.d.clearGateFor(u.UpdateID)
		}
	})
	return h
}

// TestReplayPrefixMatchesResetPreservation pins the contract between the ledger
// and badger.Store.Reset: every key the ledger writes must live under the prefix
// Reset preserves, or /reset would silently re-enable replay.
func TestReplayPrefixMatchesResetPreservation(t *testing.T) {
	if !strings.HasPrefix(replayClaimPrefix, badger.IdempotencyPrefix) {
		t.Fatalf("claim prefix %q is not preserved by Store.Reset (prefix %q)", replayClaimPrefix, badger.IdempotencyPrefix)
	}
	if !strings.HasPrefix(replayNoticePrefix, badger.IdempotencyPrefix) {
		t.Fatalf("notice prefix %q is not preserved by Store.Reset (prefix %q)", replayNoticePrefix, badger.IdempotencyPrefix)
	}
	g := newTestGuard(newMemReplayStore())
	for _, key := range []string{g.claimKey(1), g.cursorKey(), g.noticeKey(1)} {
		if !strings.HasPrefix(key, badger.IdempotencyPrefix) {
			t.Fatalf("ledger key %q is not preserved by Store.Reset", key)
		}
	}
}

// TestBotScope_IsStableAndHidesToken pins the bot-identity scoping: update ids
// are only unique per bot, so the ledger must be scoped by a value that survives
// a token rotation (the numeric bot id), is stable across restarts, and never
// reveals the token.
func TestBotScope_IsStableAndHidesToken(t *testing.T) {
	const (
		token    = "123456:AAH-super-secret-token"
		rotated  = "123456:BBJ-rotated-secret"
		otherBot = "999:AAH-super-secret-token"
	)
	scope := botScope(token)
	if scope == "" {
		t.Fatal("botScope returned an empty scope")
	}
	if scope != botScope(token) {
		t.Fatal("botScope must be stable for the same token")
	}
	if scope != botScope(rotated) {
		t.Fatal("a rotated secret for the same bot must keep the ledger scope")
	}
	if scope == botScope(otherBot) {
		t.Fatal("different bots must not share a scope")
	}
	if !strings.HasPrefix(scope, "bot:123456") {
		t.Fatalf("scope = %q, want it derived from the numeric bot id", scope)
	}
	for _, secret := range []string{"AAH-super-secret-token", "BBJ-rotated-secret", token, rotated} {
		if strings.Contains(scope, secret) {
			t.Fatalf("scope %q leaks the token secret", scope)
		}
	}
	// A token without a numeric prefix (test doubles) keeps the previous
	// derivation, which is what testGuard and every seeded row rely on.
	const mockToken = "test-token"
	if got, want := botScope(mockToken), legacyBotScope(mockToken); got != want || len(got) != 16 {
		t.Fatalf("mock-token scope = %q, want the legacy 16-hex derivation %q", got, want)
	}
	g1 := &replayGuard{store: newMemReplayStore(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), scope: botScope("111:secret-a")}
	g2 := &replayGuard{store: newMemReplayStore(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), scope: botScope("222:secret-a")}
	if g1.claimKey(5) == g2.claimKey(5) {
		t.Fatal("claim keys of two bots collide for the same update id")
	}
	if g1.cursorKey() == g2.cursorKey() {
		t.Fatal("two bots share one fetch cursor")
	}
	if g1.noticeKey(5) == g2.noticeKey(5) {
		t.Fatal("two bots share one notice key")
	}
}

// TestLedger_SurvivesTokenRotation pins the operator workflow: rotating the
// secret of the same bot must not orphan the durable ledger. A row admitted and
// acknowledged by the old token is still recovered and runnable by the new one,
// so a rotation neither loses work nor makes Telegram redeliver it.
func TestLedger_SurvivesTokenRotation(t *testing.T) {
	store := newMemReplayStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	oldGuard := newReplayGuard(store, logger, "123456:old-secret")
	newGuard := newReplayGuard(store, logger, "123456:new-secret")
	if oldGuard.scope != newGuard.scope {
		t.Fatalf("scopes differ across rotation: %q vs %q", oldGuard.scope, newGuard.scope)
	}

	admitted := privateUpdate(1, 7)
	if _, err := oldGuard.admitBatch([]Update{admitted}, 8); err != nil {
		t.Fatalf("admitBatch with the old token: %v", err)
	}

	cursor, pending, err := newGuard.recover()
	if err != nil {
		t.Fatalf("recover after rotation: %v", err)
	}
	if cursor != 8 {
		t.Fatalf("cursor after rotation = %d, want 8", cursor)
	}
	if len(pending) != 1 || pending[0].UpdateID != 7 || pending[0].State != stateAdmitted {
		t.Fatalf("recovered after rotation = %+v, want the admitted row 7", pending)
	}
	if newGuard.claimKey(7) != oldGuard.claimKey(7) {
		t.Fatal("the rotated token addresses a different ledger key")
	}
	// The new process must be able to drive the recovered row to completion.
	if err := newGuard.markRunning(7); err != nil {
		t.Fatalf("markRunning after rotation: %v", err)
	}
	if err := newGuard.markDone(7); err != nil {
		t.Fatalf("markDone after rotation: %v", err)
	}
	var rec updateRecord
	found, err := store.Get(newGuard.claimKey(7), &rec)
	if err != nil || !found {
		t.Fatalf("read row after rotation: found=%v err=%v", found, err)
	}
	if rec.State != stateDone {
		t.Fatalf("row after rotation = %q, want done", rec.State)
	}
}

// TestBot_StartReplaysRowAdmittedBeforeTokenRotation pins the consumer path of
// the rotation guarantee: a row admitted and acknowledged by the old token must
// still be found and replayed by the new one. Otherwise the rotation either
// loses the work or lets Telegram redeliver it after the cursor moved past it.
func TestBot_StartReplaysRowAdmittedBeforeTokenRotation(t *testing.T) {
	const (
		oldToken = "123456:old-secret"
		newToken = "123456:new-secret"
	)
	store := newMemReplayStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	oldGuard := newReplayGuard(store, logger, oldToken)
	admitted := Update{UpdateID: 1, Message: &Message{
		MessageID: 1,
		From:      &User{ID: 85, Username: "a"},
		Chat:      &Chat{ID: 85, Type: "private"},
		Text:      "#journal\nPair: EURUSD\nType: BUY\nRR: +2",
	}}
	if _, err := oldGuard.admitBatch([]Update{admitted}, 2); err != nil {
		t.Fatalf("admit with the old token: %v", err)
	}

	repo := &gatedTradeRepo{started: make(chan struct{}), release: make(chan struct{}), savedCh: make(chan savedTrade, 4)}
	close(repo.release)
	srv, _ := recordingPollServer(t, `{"ok":true,"result":[]}`)
	b := newJournalTestBotWithToken(t, newToken, srv, repo)
	b.SetReplayGuard(store)
	b.drainTimeout = 5 * time.Second
	b.noticeEvery = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	select {
	case saved := <-repo.savedCh:
		if saved.symbol != "EURUSD" {
			t.Fatalf("replayed symbol = %q, want EURUSD", saved.symbol)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a row admitted before the token rotation was never replayed")
	}
	// Stop the bot first: the completion hook records the terminal row, so the
	// state must be read after the drain rather than racing the handler.
	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not stop after cancellation")
	}
	// The replayed row is closed under the rotated scope, so the next restart
	// does not treat it as interrupted work.
	newGuard := newReplayGuard(store, logger, newToken)
	var rec updateRecord
	found, err := store.Get(newGuard.claimKey(1), &rec)
	if err != nil || !found {
		t.Fatalf("read row under the rotated token: found=%v err=%v", found, err)
	}
	if rec.State != stateDone {
		t.Fatalf("row after rotation = %q, want done", rec.State)
	}
}

// TestBot_StartAdoptsLegacyScopeRows pins upgrade compatibility: rows written by
// the release that scoped the ledger by a hash of the whole token must be adopted
// when the bot starts with the new bot-id scope, or the upgrade would silently
// drop work the old process had already acknowledged.
func TestBot_StartAdoptsLegacyScopeRows(t *testing.T) {
	const token = "123456:rotated-secret"
	store := newMemReplayStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	legacyScope := legacyBotScope(token)
	if legacyScope == botScope(token) {
		t.Fatal("test token does not exercise a scope migration")
	}

	admitted := Update{UpdateID: 1, Message: &Message{
		MessageID: 1,
		From:      &User{ID: 86, Username: "a"},
		Chat:      &Chat{ID: 86, Type: "private"},
		Text:      "#journal\nPair: XAUUSD\nType: SELL\nRR: -1",
	}}
	payload, err := json.Marshal(admitted)
	if err != nil {
		t.Fatalf("marshal admitted update: %v", err)
	}
	legacyRow, err := json.Marshal(updateRecord{UpdateID: 1, State: stateAdmitted, Payload: payload})
	if err != nil {
		t.Fatalf("marshal legacy row: %v", err)
	}
	// A bare guard with the old scope models the previous release's ledger; the
	// bot under test must find and adopt these keys.
	legacyGuard := &replayGuard{store: store, logger: logger, scope: legacyScope}
	if _, err := store.ApplyBatch(map[string][]byte{
		legacyGuard.claimKey(1): legacyRow,
	}, nil); err != nil {
		t.Fatalf("seed legacy ledger: %v", err)
	}

	repo := &gatedTradeRepo{started: make(chan struct{}), release: make(chan struct{}), savedCh: make(chan savedTrade, 4)}
	close(repo.release)
	srv, _ := recordingPollServer(t, `{"ok":true,"result":[]}`)
	b := newJournalTestBotWithToken(t, token, srv, repo)
	b.SetReplayGuard(store)
	b.drainTimeout = 5 * time.Second
	b.noticeEvery = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	select {
	case saved := <-repo.savedCh:
		if saved.symbol != "XAUUSD" {
			t.Fatalf("adopted symbol = %q, want XAUUSD", saved.symbol)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a row from the previous ledger scope was never replayed")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not stop after cancellation")
	}

	liveGuard := &replayGuard{store: store, logger: logger, scope: botScope(token)}
	var row updateRecord
	found, err := store.Get(liveGuard.claimKey(1), &row)
	if err != nil || !found {
		t.Fatalf("read adopted row under the live scope: found=%v err=%v", found, err)
	}
	if row.State != stateDone {
		t.Fatalf("adopted row = %q, want done", row.State)
	}
	var raw json.RawMessage
	if found, err := store.Get(legacyGuard.claimKey(1), &raw); err != nil || found {
		t.Fatalf("legacy row was left behind: found=%v err=%v", found, err)
	}
}

// TestLedger_AdmitIsAtomicAndIdempotent pins the core admission contract: the
// payload rows and the fetch cursor land together, a repeated admission inserts
// nothing, and the cursor only moves forward.
func TestLedger_AdmitIsAtomicAndIdempotent(t *testing.T) {
	store := newMemReplayStore()
	g := newTestGuard(store)

	ids, err := g.admitBatch([]Update{privateUpdate(1, 5), privateUpdate(1, 6)}, 7)
	if err != nil {
		t.Fatalf("admitBatch: %v", err)
	}
	if len(ids) != 2 || ids[0] != 5 || ids[1] != 6 {
		t.Fatalf("inserted = %v, want [5 6]", ids)
	}
	if got := store.cursorOffset(); got != 7 {
		t.Fatalf("cursor = %d, want 7", got)
	}
	for _, id := range []int{5, 6} {
		rec, ok := store.record(id)
		if !ok {
			t.Fatalf("record %d missing", id)
		}
		if rec.State != stateAdmitted {
			t.Fatalf("record %d state = %q, want admitted", id, rec.State)
		}
		if len(rec.Payload) == 0 {
			t.Fatalf("record %d has no payload to replay", id)
		}
		if ttl := store.ttls[testClaimKey(id)]; ttl != 0 {
			t.Fatalf("non-terminal record %d has TTL %v; it must never expire", id, ttl)
		}
	}

	// Re-admitting the same batch must insert nothing and must not rewind.
	ids, err = g.admitBatch([]Update{privateUpdate(1, 5), privateUpdate(1, 6)}, 7)
	if err != nil {
		t.Fatalf("second admitBatch: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("duplicate admission inserted %v, want none", ids)
	}
	if got := store.cursorOffset(); got != 7 {
		t.Fatalf("cursor = %d after duplicate admission, want 7", got)
	}
}

// TestLedger_AdmitFailureWritesNothing pins all-or-nothing: a failed write must
// not leave rows behind or move the cursor, so the caller can retry the batch.
func TestLedger_AdmitFailureWritesNothing(t *testing.T) {
	store := newMemReplayStore()
	store.failPut = errors.New("badger unavailable")
	g := newTestGuard(store)

	if _, err := g.admitBatch([]Update{privateUpdate(1, 5)}, 6); err == nil {
		t.Fatal("admitBatch must report the write failure")
	}
	if got := store.cursorOffset(); got != 0 {
		t.Fatalf("cursor advanced to %d despite a failed admission", got)
	}
	if _, ok := store.claimState(5); ok {
		t.Fatal("a failed admission left a record behind")
	}
}

// TestLedger_RecoverOrdersByUpdateIDAndMapsLegacy pins startup recovery:
// pending rows come back in update_id order (numeric, not lexicographic) and the
// old claim states are migrated without replaying ambiguous work.
func TestLedger_RecoverOrdersByUpdateIDAndMapsLegacy(t *testing.T) {
	store := newMemReplayStore()
	g := newTestGuard(store)

	payload, _ := json.Marshal(privateUpdate(1, 0))
	store.seedRecord(updateRecord{UpdateID: 10, State: stateAdmitted, Payload: payload})
	store.seedRecord(updateRecord{UpdateID: 9, State: stateAdmitted, Payload: payload})
	store.seedRecord(updateRecord{UpdateID: 11, State: stateDone})
	store.seedRecord(updateRecord{UpdateID: 12, State: stateRunning, Payload: payload})
	store.seedRecord(updateRecord{UpdateID: 13, State: legacyStatePending})
	store.seedRecord(updateRecord{UpdateID: 14, State: legacyStateHandled})
	_ = store.SetWithTTL(g.cursorKey(), fetchCursor{Offset: 15, UpdatedAt: time.Now()}, 0)

	cursor, pending, err := g.recover()
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if cursor != 15 {
		t.Fatalf("cursor = %d, want 15", cursor)
	}
	var got []int64
	for _, rec := range pending {
		got = append(got, rec.UpdateID)
	}
	// done (11), legacy handled (14) and legacy pending (13, unnotifiable) are
	// not replayed; 9 and 10 sort numerically before 12.
	want := []int64{9, 10, 12}
	if len(got) != len(want) {
		t.Fatalf("pending = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pending = %v, want %v", got, want)
		}
	}
}

// TestBot_StartDiscardsStaleCursorAfterUpdateIDReset pins the recovery rule for
// Telegram's restarted update ids: a cursor older than the reset window cannot be
// trusted, so it is discarded and the terminal rows of the old numbering are
// purged. Keeping them would make a NEW update that reuses an old id look already
// processed, so admission would silently drop it and the bot would go quiet.
func TestBot_StartDiscardsStaleCursorAfterUpdateIDReset(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":5,"message":{"message_id":5,"from":{"id":79,"username":"a"},"chat":{"id":79,"type":"private"},"text":"#journal\nPair: EURUSD\nType: BUY\nRR: +2"}}
	]}`
	store := newMemReplayStore()
	// A terminal row of the old numbering, sharing the id the new update reuses.
	store.seedRecord(updateRecord{UpdateID: 5, State: stateDone})
	_ = store.SetWithTTL(testGuard.cursorKey(), fetchCursor{
		Offset:    9,
		UpdatedAt: time.Now().Add(-telegramUpdateIDResetWindow - time.Hour),
	}, 0)

	repo := &gatedTradeRepo{started: make(chan struct{}), release: make(chan struct{}), savedCh: make(chan savedTrade, 4)}
	close(repo.release)
	srv, rec := recordingPollServer(t, batch)
	b := newJournalTestBot(t, srv, repo)
	b.SetReplayGuard(store)
	b.drainTimeout = 5 * time.Second
	b.noticeEvery = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	select {
	case saved := <-repo.savedCh:
		if saved.symbol != "EURUSD" {
			t.Fatalf("reused update id was processed as %q, want EURUSD", saved.symbol)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an update that reused an id from the old numbering was dropped")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not stop after cancellation")
	}
	// The stale watermark was not sent to Telegram: polling restarted at 0.
	if offsets := rec.offsetsSnapshot(); len(offsets) == 0 || offsets[0] != "0" {
		t.Fatalf("first poll offset = %v, want 0 after the reset window", offsets)
	}
	if state, ok := store.claimState(5); !ok || state != stateDone {
		t.Fatalf("reused id row = %q (found %v), want the new update recorded as done", state, ok)
	}
}

// TestLedger_PurgeFailureFailsRecovery pins fail-closed recovery: if the
// terminal rows of the old numbering cannot be removed, the bot must refuse to
// start rather than run with rows that would silently swallow a new update
// reusing an old id.
func TestLedger_PurgeFailureFailsRecovery(t *testing.T) {
	store := newMemReplayStore()
	g := newTestGuard(store)

	store.seedRecord(updateRecord{UpdateID: 5, State: stateDone})
	_ = store.SetWithTTL(g.cursorKey(), fetchCursor{
		Offset:    9,
		UpdatedAt: time.Now().Add(-telegramUpdateIDResetWindow - time.Hour),
	}, 0)
	store.failDelete = errors.New("badger delete failed")

	if _, _, err := g.recover(); err == nil {
		t.Fatal("recover returned nil despite an unrecordable purge")
	}
}

// TestLedger_CursorResetWindowBoundary pins both sides of the update-id reset
// window. A cursor just inside it is still trusted, so a restart does not
// needlessly redeliver a batch and re-dedupe it; a cursor just past it — or one
// with no timestamp at all — is discarded together with the terminal rows of the
// previous numbering, so a reused update id is not mistaken for handled work.
func TestLedger_CursorResetWindowBoundary(t *testing.T) {
	// The window must stay comfortably under Telegram's documented "about a
	// week" of update retention: a cursor is only useful while the numbering it
	// encodes is, and being too generous risks discarding updates Telegram would
	// still have redelivered.
	if telegramUpdateIDResetWindow >= 7*24*time.Hour {
		t.Fatalf("reset window = %v, want well under 7 days", telegramUpdateIDResetWindow)
	}
	cases := []struct {
		name        string
		updatedAt   time.Time
		wantCursor  int
		wantRowKept bool
	}{
		{
			name:        "just inside the window",
			updatedAt:   time.Now().Add(-telegramUpdateIDResetWindow + time.Minute),
			wantCursor:  9,
			wantRowKept: true,
		},
		{
			name:        "just past the window",
			updatedAt:   time.Now().Add(-telegramUpdateIDResetWindow - time.Minute),
			wantCursor:  0,
			wantRowKept: false,
		},
		{
			name:        "no timestamp",
			updatedAt:   time.Time{},
			wantCursor:  0,
			wantRowKept: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemReplayStore()
			g := newTestGuard(store)
			store.seedRecord(updateRecord{UpdateID: 5, State: stateDone})
			_ = store.SetWithTTL(g.cursorKey(), fetchCursor{Offset: 9, UpdatedAt: tc.updatedAt}, 0)

			cursor, pending, err := g.recover()
			if err != nil {
				t.Fatalf("recover: %v", err)
			}
			if cursor != tc.wantCursor {
				t.Fatalf("cursor = %d, want %d", cursor, tc.wantCursor)
			}
			if len(pending) != 0 {
				t.Fatalf("pending = %+v, want none (the only row is terminal)", pending)
			}
			if _, kept := store.claimState(5); kept != tc.wantRowKept {
				t.Fatalf("terminal row kept = %v, want %v", kept, tc.wantRowKept)
			}
			if tc.wantRowKept {
				return
			}
			// The discarded numbering must accept a reused id: nothing left to
			// make it look already processed.
			ids, err := g.admitBatch([]Update{privateUpdate(1, 5)}, 10)
			if err != nil {
				t.Fatalf("admitBatch after the reset: %v", err)
			}
			if len(ids) != 1 || ids[0] != 5 {
				t.Fatalf("inserted = %v, want the reused update id 5 to be admitted", ids)
			}
		})
	}
}

// TestBot_StartFailsClosedWhenStalePurgeFails pins the same rule at the poll
// loop: a purge that cannot be recorded after a restarted batch must stop the
// bot instead of letting it poll with rows that would drop reused ids.
func TestBot_StartFailsClosedWhenStalePurgeFails(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":3,"message":{"message_id":3,"from":{"id":80,"username":"a"},"chat":{"id":80,"type":"private"},"text":"#journal\nPair: EURUSD\nType: BUY\nRR: +2"}}
	]}`
	store := newMemReplayStore()
	// A terminal row of the old numbering and a cursor ahead of the batch: the
	// first poll therefore detects the restarted sequence and tries to purge.
	store.seedRecord(updateRecord{UpdateID: 1, State: stateDone})
	_ = store.SetWithTTL(testGuard.cursorKey(), fetchCursor{Offset: 9, UpdatedAt: time.Now()}, 0)
	store.failDelete = errors.New("badger delete failed")

	repo := &gatedTradeRepo{started: make(chan struct{}), release: make(chan struct{}), savedCh: make(chan savedTrade, 4)}
	close(repo.release)
	srv, _ := recordingPollServer(t, batch)
	b := newJournalTestBot(t, srv, repo)
	b.SetReplayGuard(store)
	b.drainTimeout = 5 * time.Second
	b.noticeEvery = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	select {
	case err := <-errCh:
		if err == nil || err == context.Canceled {
			t.Fatalf("Start returned %v, want a fatal purge error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start kept polling after the stale ledger could not be purged")
	}
	if got := repo.savedSymbols(); len(got) != 0 {
		t.Fatalf("handler ran while the ledger was known to be stale: %v", got)
	}
}

// TestUpdateDispatcher_WaitTimeoutDoesNotPoisonLaterAdmission pins that a Wait
// which times out cannot break the next admission. sync.WaitGroup panics
// ("WaitGroup is reused before previous Wait has returned") when Add lands while
// a Wait is still in flight, and a panic here would kill the bot during
// shutdown.
func TestUpdateDispatcher_WaitTimeoutDoesNotPoisonLaterAdmission(t *testing.T) {
	release := make(chan struct{})
	d := newUpdateDispatcher(context.Background(), 4, func(context.Context, Update) {
		<-release // keep the first update in flight past the Wait timeout
	}, nil)

	if err := d.Enqueue(context.Background(), privateUpdate(1, 1)); err != nil {
		t.Fatalf("enqueue first update: %v", err)
	}
	if d.Wait(30 * time.Millisecond) {
		t.Fatal("Wait reported a drain while an update was still running")
	}
	// The timed-out Wait must not have left a waiter behind that the next
	// admission trips over.
	if err := d.Enqueue(context.Background(), privateUpdate(2, 2)); err != nil {
		t.Fatalf("enqueue after a timed-out Wait: %v", err)
	}
	close(release)
	if !d.Wait(5 * time.Second) {
		t.Fatal("dispatcher did not drain after the release")
	}
	d.WaitAll()
}

// TestLedger_MarkRunningRequiresAdmission pins the durability gate: the handler
// may only run after the running state is durable, and only for an admitted row.
func TestLedger_MarkRunningRequiresAdmission(t *testing.T) {
	store := newMemReplayStore()
	g := newTestGuard(store)

	if err := g.markRunning(3); !errors.Is(err, errNotAdmitted) {
		t.Fatalf("markRunning on unknown id = %v, want errNotAdmitted", err)
	}
	if _, err := g.admitBatch([]Update{privateUpdate(1, 3)}, 4); err != nil {
		t.Fatalf("admitBatch: %v", err)
	}
	if err := g.markRunning(3); err != nil {
		t.Fatalf("markRunning: %v", err)
	}
	if state, _ := store.claimState(3); state != stateRunning {
		t.Fatalf("state = %q, want running", state)
	}
	if ttl := store.ttls[testClaimKey(3)]; ttl != 0 {
		t.Fatalf("running row has TTL %v; it must survive until resolved", ttl)
	}
	// Running again is refused: a second worker must not re-enter.
	if err := g.markRunning(3); err == nil {
		t.Fatal("markRunning must refuse a row that is already running")
	}
}

// TestLedger_MarkDoneFailureIsReported pins that a failed terminal write
// surfaces, so the caller can leave the row running rather than pretend it
// completed.
func TestLedger_MarkDoneFailureIsReported(t *testing.T) {
	store := newMemReplayStore()
	g := newTestGuard(store)
	if _, err := g.admitBatch([]Update{privateUpdate(1, 9)}, 10); err != nil {
		t.Fatalf("admitBatch: %v", err)
	}
	store.failPut = errors.New("badger write failed")
	if err := g.markDone(9); err == nil {
		t.Fatal("markDone must report a write failure")
	}
	store.failPut = nil
	if state, _ := store.claimState(9); state != stateAdmitted {
		t.Fatalf("state = %q, want admitted (nothing was written)", state)
	}
}

// TestLedger_ResolveInterruptedNotifiesThenCloses pins the manual-verification
// policy: a row left running is never replayed, the user gets a durable notice,
// and the row is closed only after that notice is stored.
func TestLedger_ResolveInterruptedNotifiesThenCloses(t *testing.T) {
	store := newMemReplayStore()
	g := newTestGuard(store)
	payload, _ := json.Marshal(privateUpdate(5, 21))
	store.seedRecord(updateRecord{UpdateID: 21, State: stateRunning, Payload: payload})

	rec, _ := store.record(21)
	if err := g.resolveInterrupted(rec); err != nil {
		t.Fatalf("resolveInterrupted: %v", err)
	}
	if got := store.noticeCount(); got != 1 {
		t.Fatalf("notices = %d, want 1", got)
	}
	if state, _ := store.claimState(21); state != stateDone {
		t.Fatalf("state = %q, want done", state)
	}
}

// TestLedger_ResolveInterruptedKeepsRowWhenNoticeFails pins that a failed notice
// leaves the row running, so the next start retries instead of silently dropping
// the user's only signal.
func TestLedger_ResolveInterruptedKeepsRowWhenNoticeFails(t *testing.T) {
	store := newMemReplayStore()
	g := newTestGuard(store)
	payload, _ := json.Marshal(privateUpdate(5, 22))
	store.seedRecord(updateRecord{UpdateID: 22, State: stateRunning, Payload: payload})
	store.failPut = errors.New("badger write failed")

	rec, _ := store.record(22)
	if err := g.resolveInterrupted(rec); err == nil {
		t.Fatal("resolveInterrupted must report the failed notice write")
	}
	store.failPut = nil
	if state, _ := store.claimState(22); state != stateRunning {
		t.Fatalf("state = %q, want running (retry next start)", state)
	}
}

// TestReplayGuard_NoChatIsDistinguishable pins that an update with no chat is a
// permanent condition the caller can recognise, instead of being confused with a
// transient write failure.
func TestReplayGuard_NoChatIsDistinguishable(t *testing.T) {
	g := newTestGuard(newMemReplayStore())
	err := g.recordNotice(Update{UpdateID: 1})
	if !errors.Is(err, errNoNoticeTarget) {
		t.Fatalf("err = %v, want errNoNoticeTarget", err)
	}
}

// TestNoticeThreadID pins group privacy/format: message_thread_id is only valid
// in forum supergroups, so it must be dropped elsewhere or Telegram rejects the
// notice and the user is never told.
func TestNoticeThreadID(t *testing.T) {
	if got := noticeThreadID(&Chat{ID: 1, Type: "private"}, 7); got != 0 {
		t.Fatalf("private chat thread = %d, want 0", got)
	}
	if got := noticeThreadID(&Chat{ID: 1, Type: "supergroup"}, 7); got != 0 {
		t.Fatalf("non-forum supergroup thread = %d, want 0", got)
	}
	if got := noticeThreadID(&Chat{ID: 1, Type: "supergroup", IsForum: true}, 7); got != 7 {
		t.Fatalf("forum topic thread = %d, want 7", got)
	}
	if got := noticeThreadID(&Chat{ID: 1, Type: "supergroup", IsForum: true}, 0); got != 0 {
		t.Fatalf("forum general thread = %d, want 0", got)
	}
}

// TestReplayGuard_NoticeSurvivesFailedDelivery pins that a recovery notice is
// durable: a failed send leaves it stored for the retry loop.
func TestReplayGuard_NoticeSurvivesFailedDelivery(t *testing.T) {
	store := newMemReplayStore()
	g := newTestGuard(store)

	u := privateUpdate(5, 42)
	if err := g.recordNotice(u); err != nil {
		t.Fatalf("record notice: %v", err)
	}
	// A notice must never expire before it is delivered: losing it would drop
	// the user's only signal that the action was interrupted.
	if got := store.ttls[g.noticeKey(42)]; got != 0 {
		t.Fatalf("notice TTL = %v, want 0 (persist until delivered)", got)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"

	g.flushNotices(context.Background(), sender)
	if got := store.noticeCount(); got != 1 {
		t.Fatalf("failed delivery dropped the notice (count = %d)", got)
	}

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer ok.Close()
	sender.baseAPI = ok.URL + "/bot"

	g.flushNotices(context.Background(), sender)
	if got := store.noticeCount(); got != 0 {
		t.Fatalf("delivered notice was not cleared (count = %d)", got)
	}
}

// TestReplayGuard_NoticeNeverClaimsSuccess pins the wording contract: the user
// must be told to verify, never told the action succeeded.
func TestReplayGuard_NoticeNeverClaimsSuccess(t *testing.T) {
	for _, u := range []Update{
		privateUpdate(5, 1),
		{UpdateID: 2, CallbackQuery: &CallbackQuery{ID: "cb", From: &User{ID: 5}, Message: &Message{Chat: &Chat{ID: 5}}}},
	} {
		_, _, text, ok := interruptedText(u)
		if !ok {
			t.Fatalf("update %d produced no notice", u.UpdateID)
		}
		if strings.Contains(text, "berhasil") || strings.Contains(text, "✅") {
			t.Fatalf("notice claims success: %q", text)
		}
		if !strings.Contains(text, "/history") {
			t.Fatalf("notice does not point at manual verification: %q", text)
		}
	}
}

// TestLedger_InterruptedResetNoticeExplainsPartialState pins the one notice
// whose wording cannot be generic: a reset left running by a crash may have
// archived Notion while leaving Badger untouched (or the reverse). A notice that
// only points at /history would describe a trade that does not exist and hide
// the real risk, so it must name both stores, warn that the result may be
// partial, and never claim the reset finished.
func TestLedger_InterruptedResetNoticeExplainsPartialState(t *testing.T) {
	store := newMemReplayStore()
	g := newTestGuard(store)

	reset := resetUpdate(9)
	payload, err := json.Marshal(reset)
	if err != nil {
		t.Fatalf("marshal reset update: %v", err)
	}
	store.seedRecord(updateRecord{UpdateID: 9, State: stateRunning, Payload: payload})

	rec, ok := store.record(9)
	if !ok {
		t.Fatal("seeded running reset row is missing")
	}
	if err := g.resolveInterrupted(rec); err != nil {
		t.Fatalf("resolveInterrupted: %v", err)
	}
	var notice replayNotice
	found, err := store.Get(g.noticeKey(9), &notice)
	if err != nil || !found {
		t.Fatalf("interrupted reset notice not stored: found=%v err=%v", found, err)
	}
	assertPartialResetNotice(t, notice.Text)
	// The row is never replayed; it is closed only after the notice is durable.
	if state, ok := store.claimState(9); !ok || state != stateDone {
		t.Fatalf("interrupted reset row = %q (found %v), want done after the notice", state, ok)
	}

	// The confirm button is the other way to trigger the wipe, so it must get the
	// same warning instead of the generic trade notice.
	_, _, cbText, ok := interruptedText(Update{UpdateID: 10, CallbackQuery: &CallbackQuery{
		From:    &User{ID: 99},
		Data:    "reset:confirm",
		Message: &Message{Chat: &Chat{ID: 99, Type: "private"}},
	}})
	if !ok {
		t.Fatal("reset callback produced no notice")
	}
	assertPartialResetNotice(t, cbText)
}

// assertPartialResetNotice checks the reset-specific warning contract.
func assertPartialResetNotice(t *testing.T, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, store := range []string{"notion", "badger"} {
		if !strings.Contains(lower, store) {
			t.Fatalf("interrupted reset notice does not name %s: %q", store, text)
		}
	}
	if !strings.Contains(lower, "sebagian") && !strings.Contains(lower, "partial") &&
		!strings.Contains(lower, "setengah") && !strings.Contains(lower, "belum") {
		t.Fatalf("notice does not warn that the reset may be partial: %q", text)
	}
	if strings.Contains(text, "✅") || strings.Contains(lower, "berhasil") || strings.Contains(lower, "reset selesai") {
		t.Fatalf("interrupted reset notice claims success: %q", text)
	}
}

// TestUpdateDispatcher_StartHookGatesHandler pins the durability gate at the
// dispatcher level: when onStart cannot record that the update is running, the
// handler must not touch anything, the update is reported as never-started, and
// the dispatcher fail-stops instead of letting this user's later updates
// overtake the unrecorded row.
func TestUpdateDispatcher_StartHookGatesHandler(t *testing.T) {
	var (
		mu      sync.Mutex
		ran     []int
		outcome updateOutcome
	)
	d := newUpdateDispatcher(context.Background(), 1, func(_ context.Context, u Update) {
		mu.Lock()
		ran = append(ran, u.UpdateID)
		mu.Unlock()
	}, nil)
	d.setOnStart(func(u Update) error {
		if u.UpdateID == 1 {
			return errors.New("could not mark running")
		}
		return nil
	})
	d.setOnDone(func(_ Update, o updateOutcome) {
		if o.Started {
			return
		}
		mu.Lock()
		outcome = o
		mu.Unlock()
	})

	ctx := context.Background()
	if err := d.Enqueue(ctx, privateUpdate(1, 1)); err != nil {
		t.Fatalf("enqueue gated update: %v", err)
	}
	if !d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not finish the gated update")
	}
	// Fail-stop: update 1 stays admitted for the next start, so running update 2
	// now would let it overtake work whose outcome was never recorded.
	if err := d.Enqueue(ctx, privateUpdate(1, 2)); !errors.Is(err, errDispatcherStopped) {
		t.Fatalf("enqueue after a failed start hook = %v, want errDispatcherStopped", err)
	}
	select {
	case <-d.Fatal():
	default:
		t.Fatal("a failed start hook must fail-stop the dispatcher")
	}
	if d.FatalErr() == nil {
		t.Fatal("fatal error was not retained")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 0 {
		t.Fatalf("handlers ran without a durable running state: %v", ran)
	}
	if outcome.Started || outcome.Completed {
		t.Fatalf("gated update reported %+v, want never-started", outcome)
	}
}

// TestUpdateDispatcher_PanicIsNotCompleted pins that a contained panic never
// reports Completed, so the ledger leaves the row running and the next start
// asks the user to verify.
func TestUpdateDispatcher_PanicIsNotCompleted(t *testing.T) {
	var (
		mu      sync.Mutex
		outcome updateOutcome
	)
	d := newUpdateDispatcher(context.Background(), 1, func(context.Context, Update) {
		panic("boom")
	}, func(Update, any) {})
	d.setOnDone(func(_ Update, o updateOutcome) {
		mu.Lock()
		outcome = o
		mu.Unlock()
	})
	if err := d.Enqueue(context.Background(), privateUpdate(1, 1)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if !d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if !outcome.Started {
		t.Fatal("a panicking handler did start")
	}
	if outcome.Completed {
		t.Fatal("a panicking handler must never be reported as completed")
	}
}

// TestUpdateDispatcher_CompletedOutcome pins the happy path flag triple.
func TestUpdateDispatcher_CompletedOutcome(t *testing.T) {
	var (
		mu      sync.Mutex
		outcome updateOutcome
	)
	d := newUpdateDispatcher(context.Background(), 1, func(context.Context, Update) {}, nil)
	d.setOnDone(func(_ Update, o updateOutcome) {
		mu.Lock()
		outcome = o
		mu.Unlock()
	})
	if err := d.Enqueue(context.Background(), privateUpdate(1, 1)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if !d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if !outcome.Started || !outcome.Completed || outcome.Aborted {
		t.Fatalf("outcome = %+v, want started+completed, not aborted", outcome)
	}
}

// TestHandler_HandleUpdatePropagatesPanic pins that the handler no longer
// swallows panics: the dispatcher is the single recovery point, so it can mark
// the update ambiguous instead of done.
func TestHandler_HandleUpdatePropagatesPanic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sender := NewSender(testBotToken)
	h := NewHandler(sender, Services{}, nil, nil, nil, NewRateLimiter(100, time.Minute), logger, 0, 0)
	defer h.Close()

	defer func() {
		if recover() == nil {
			t.Fatal("HandleUpdate must let the panic reach the dispatcher")
		}
	}()
	// Services{} has a nil Journal service, so reaching RecordTrade dereferences
	// it and panics — exactly the failure the dispatcher must observe.
	h.HandleUpdate(context.Background(), Update{UpdateID: 1, Message: &Message{
		Chat: &Chat{ID: 1, Type: "private"},
		From: &User{ID: 1, Username: "a"},
		Text: "#journal\nPair: EURUSD\nType: BUY\nRR: +2",
	}})
}

func TestUpdateKey(t *testing.T) {
	if key, ok := updateKey(privateUpdate(42, 1)); !ok || key != 42 {
		t.Fatalf("message key = (%d, %v), want (42, true)", key, ok)
	}
	cb := Update{UpdateID: 2, CallbackQuery: &CallbackQuery{ID: "cb", From: &User{ID: 43}}}
	if key, ok := updateKey(cb); !ok || key != 43 {
		t.Fatalf("callback key = (%d, %v), want (43, true)", key, ok)
	}
	if _, ok := updateKey(Update{UpdateID: 3}); ok {
		t.Fatal("update without a user must not be keyed")
	}
}

type savedTrade struct {
	memberID int64
	symbol   string
}

// orderingTradeRepo records the order and overlap of trade saves so the
// polling loop can be asserted to serialise one user while keeping different
// users concurrent.
type orderingTradeRepo struct {
	mu          sync.Mutex
	saved       []savedTrade
	inFlight    int
	maxInFlight int
	saves       chan savedTrade
}

func newOrderingTradeRepo() *orderingTradeRepo {
	return &orderingTradeRepo{saves: make(chan savedTrade, 8)}
}

func (r *orderingTradeRepo) SaveTrade(_ context.Context, memberID int64, trade *domain.Trade) error {
	r.mu.Lock()
	r.inFlight++
	if r.inFlight > r.maxInFlight {
		r.maxInFlight = r.inFlight
	}
	r.mu.Unlock()

	time.Sleep(40 * time.Millisecond) // slow persistence, e.g. a Notion round trip

	r.mu.Lock()
	r.inFlight--
	r.saved = append(r.saved, savedTrade{memberID: memberID, symbol: trade.Symbol})
	r.mu.Unlock()
	r.saves <- savedTrade{memberID: memberID, symbol: trade.Symbol}
	return nil
}

func (r *orderingTradeRepo) GetTrades(context.Context, int64) ([]domain.Trade, error) {
	return nil, nil
}

func (r *orderingTradeRepo) UpdateTrade(context.Context, int64, *domain.Trade) error { return nil }

func (r *orderingTradeRepo) DeleteTrade(context.Context, int64, string) error { return nil }

func (r *orderingTradeRepo) snapshot() (saved []savedTrade, maxInFlight int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]savedTrade(nil), r.saved...), r.maxInFlight
}

// pollRecorder captures the getUpdates offsets the fake Bot API observed, in
// order. acked closes on the first poll whose offset acknowledges anything.
type pollRecorder struct {
	mu      sync.Mutex
	offsets []string
	sends   []string
	acked   chan struct{}
	once    sync.Once
}

func (p *pollRecorder) offsetsSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.offsets...)
}

func (p *pollRecorder) sendsSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sends...)
}

// recordingPollServer serves batch on the first getUpdates call, then holds
// every later long poll open until the request is cancelled, mirroring the real
// endpoint.
func recordingPollServer(t *testing.T, batch string) (*httptest.Server, *pollRecorder) {
	t.Helper()
	rec := &pollRecorder{acked: make(chan struct{})}
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var message struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&message)
			rec.mu.Lock()
			rec.sends = append(rec.sends, message.Text)
			rec.mu.Unlock()
		}
		if !strings.HasSuffix(r.URL.Path, "/getUpdates") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
			return
		}
		offset := r.URL.Query().Get("offset")
		rec.mu.Lock()
		rec.offsets = append(rec.offsets, offset)
		rec.mu.Unlock()

		if atomic.AddInt32(&polls, 1) == 1 {
			_, _ = w.Write([]byte(batch))
			return
		}
		if offset != "0" {
			rec.once.Do(func() { close(rec.acked) })
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// capturingSendServer serves every Bot API method with a success reply and
// records the "text" of each sendMessage call, so a test can prove what the
// handler told the user.
func capturingSendServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu    sync.Mutex
		texts []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		if text, ok := payload["text"].(string); ok {
			mu.Lock()
			texts = append(texts, text)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), texts...)
	}
}

// sentContains reports whether any captured message contains want.
func sentContains(texts []string, want string) bool {
	for _, text := range texts {
		if strings.Contains(text, want) {
			return true
		}
	}
	return false
}

func newJournalTestBot(t *testing.T, srv *httptest.Server, tradeRepo service.TradeRepository) *Bot {
	t.Helper()
	return newJournalTestBotOpts(t, srv, tradeRepo, journalBotOpts{})
}

// newJournalTestBotWithToken builds a journal bot for a specific token, so a test
// can model a rotation of the same bot's secret against one durable ledger.
func newJournalTestBotWithToken(t *testing.T, token string, srv *httptest.Server, tradeRepo service.TradeRepository) *Bot {
	t.Helper()
	return newJournalTestBotOpts(t, srv, tradeRepo, journalBotOpts{token: token})
}

// journalBotOpts configures the test bot. ownerID matters for the reset barrier:
// a destructive reset only holds it when the update comes from the owner.
type journalBotOpts struct {
	token   string
	ownerID int64
}

func newJournalTestBotOpts(t *testing.T, srv *httptest.Server, tradeRepo service.TradeRepository, opts journalBotOpts) *Bot {
	t.Helper()
	token := opts.token
	if token == "" {
		token = testBotToken
	}
	memberRepo := &mockResettableMemberRepo{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sender := NewSender(token)
	sender.baseAPI = srv.URL + "/bot"
	h := NewHandler(sender, Services{Journal: service.NewJournalService(tradeRepo, memberRepo, nil)},
		nil, tradeRepo, memberRepo, NewRateLimiter(100, time.Minute), logger, 0, opts.ownerID)
	t.Cleanup(h.Close)

	// The token fixes the replay-ledger scope; a test seeding claims must derive
	// its keys from the same token or they will not be found.
	b := NewBot(token, h, logger)
	b.baseAPI = srv.URL + "/bot"
	return b
}

// gatedTradeRepo blocks the first SaveTrade until release is closed and
// records the context state every handler observed. Under the old shutdown
// path the polling context was already cancelled, so an in-flight handler
// reached Notion with ctx.Err() != nil.
type gatedTradeRepo struct {
	started chan struct{}
	release chan struct{}
	savedCh chan savedTrade
	once    sync.Once
	mu      sync.Mutex
	saved   []savedTrade
	ctxErrs []error
}

func (r *gatedTradeRepo) SaveTrade(ctx context.Context, memberID int64, trade *domain.Trade) error {
	r.once.Do(func() {
		close(r.started)
		<-r.release
	})
	// Observe cancellation after the gate: with the old code the polling
	// context was cancelled while this handler was blocked.
	err := ctx.Err()
	r.mu.Lock()
	r.saved = append(r.saved, savedTrade{memberID: memberID, symbol: trade.Symbol})
	r.ctxErrs = append(r.ctxErrs, err)
	r.mu.Unlock()
	if r.savedCh != nil {
		r.savedCh <- savedTrade{memberID: memberID, symbol: trade.Symbol}
	}
	return err
}

func (r *gatedTradeRepo) GetTrades(context.Context, int64) ([]domain.Trade, error) {
	return nil, nil
}

func (r *gatedTradeRepo) UpdateTrade(context.Context, int64, *domain.Trade) error { return nil }

func (r *gatedTradeRepo) DeleteTrade(context.Context, int64, string) error { return nil }

func (r *gatedTradeRepo) snapshot() (saved []savedTrade, ctxErrs []error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]savedTrade(nil), r.saved...), append([]error(nil), r.ctxErrs...)
}

func (r *gatedTradeRepo) savedSymbols() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.saved))
	for _, s := range r.saved {
		out = append(out, s.symbol)
	}
	return out
}

// ctxBlockingTradeRepo models an I/O call that only unwinds when its context
// is cancelled, i.e. the stuck Notion request the drain budget must abort.
type ctxBlockingTradeRepo struct {
	started  chan struct{}
	stopped  chan struct{}
	startOne sync.Once
	stopOnce sync.Once
}

func (r *ctxBlockingTradeRepo) SaveTrade(ctx context.Context, _ int64, _ *domain.Trade) error {
	r.startOne.Do(func() { close(r.started) })
	<-ctx.Done()
	r.stopOnce.Do(func() { close(r.stopped) })
	return ctx.Err()
}

func (r *ctxBlockingTradeRepo) GetTrades(context.Context, int64) ([]domain.Trade, error) {
	return nil, nil
}

func (r *ctxBlockingTradeRepo) UpdateTrade(context.Context, int64, *domain.Trade) error { return nil }

func (r *ctxBlockingTradeRepo) DeleteTrade(context.Context, int64, string) error { return nil }

// panickingTradeRepo panics from SaveTrade, modelling a handler crash inside a
// non-idempotent action.
type panickingTradeRepo struct{}

func (r *panickingTradeRepo) SaveTrade(context.Context, int64, *domain.Trade) error {
	panic("notion client exploded")
}

func (r *panickingTradeRepo) GetTrades(context.Context, int64) ([]domain.Trade, error) {
	return nil, nil
}

func (r *panickingTradeRepo) UpdateTrade(context.Context, int64, *domain.Trade) error { return nil }

func (r *panickingTradeRepo) DeleteTrade(context.Context, int64, string) error { return nil }

// stubbornTradeRepo ignores context cancellation until release is closed,
// modelling a handler that keeps touching the store after shutdown began.
type stubbornTradeRepo struct {
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
	once     sync.Once
}

func (r *stubbornTradeRepo) SaveTrade(context.Context, int64, *domain.Trade) error {
	r.once.Do(func() { close(r.started) })
	<-r.release
	close(r.finished)
	return nil
}

func (r *stubbornTradeRepo) GetTrades(context.Context, int64) ([]domain.Trade, error) {
	return nil, nil
}

func (r *stubbornTradeRepo) UpdateTrade(context.Context, int64, *domain.Trade) error { return nil }

func (r *stubbornTradeRepo) DeleteTrade(context.Context, int64, string) error { return nil }

// TestBot_StartOrdersPerUserAndDrainsOnShutdown drives the real polling loop
// against a fake Bot API: three updates arrive in one batch (two for one user,
// one for another). The two same-user journals must be recorded in arrival
// order, the other user must be processed concurrently, and Start must return
// context.Canceled after draining in-flight work.
func TestBot_StartOrdersPerUserAndDrainsOnShutdown(t *testing.T) {
	const updates = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":11,"username":"a"},"chat":{"id":11,"type":"private"},"text":"#journal\nPair: EURUSD\nType: BUY\nRR: +2"}},
		{"update_id":2,"message":{"message_id":2,"from":{"id":11,"username":"a"},"chat":{"id":11,"type":"private"},"text":"#journal\nPair: XAUUSD\nType: BUY\nRR: +1"}},
		{"update_id":3,"message":{"message_id":3,"from":{"id":22,"username":"b"},"chat":{"id":22,"type":"private"},"text":"#journal\nPair: GBPUSD\nType: SELL\nRR: -1"}}
	]}`

	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			if r.URL.Query().Get("timeout") == "0" {
				// Shutdown confirmation: answer immediately.
				_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
				return
			}
			if atomic.AddInt32(&polls, 1) == 1 {
				_, _ = w.Write([]byte(updates))
				return
			}
			// Hold the long poll open until shutdown cancels the request.
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()

	tradeRepo := newOrderingTradeRepo()
	memberRepo := &mockResettableMemberRepo{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	sender := NewSender(testBotToken)
	sender.baseAPI = srv.URL + "/bot"
	h := NewHandler(sender, Services{Journal: service.NewJournalService(tradeRepo, memberRepo, nil)},
		nil, tradeRepo, memberRepo, NewRateLimiter(100, time.Minute), logger, 0, 0)
	defer h.Close()

	b := NewBot("test-token", h, logger)
	b.baseAPI = srv.URL + "/bot"

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	for i := range 3 {
		select {
		case <-tradeRepo.saves:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 3 updates were processed", i)
		}
	}

	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancellation (shutdown drain stuck)")
	}

	saved, maxInFlight := tradeRepo.snapshot()
	if len(saved) != 3 {
		t.Fatalf("saved %d trades, want 3: %+v", len(saved), saved)
	}
	var userA []string
	for _, s := range saved {
		if s.memberID == 11 {
			userA = append(userA, s.symbol)
		}
	}
	if len(userA) != 2 || userA[0] != "EURUSD" || userA[1] != "XAUUSD" {
		t.Fatalf("same-user updates were not processed in arrival order: %v", userA)
	}
	if maxInFlight < 2 {
		t.Fatalf("different users were serialised (max in-flight saves = %d)", maxInFlight)
	}
}

// replayTestBot wires a journal bot with a durable ledger and a fixed drain
// budget, returning the pieces a replay test asserts on.
func replayTestBot(t *testing.T, batch string, repo service.TradeRepository, store *memReplayStore) (*Bot, *pollRecorder, chan error, context.CancelFunc) {
	t.Helper()
	return replayTestBotOpts(t, batch, repo, store, journalBotOpts{})
}

// replayTestBotOpts is replayTestBot with an explicit bot configuration, so a
// test can run with an owner and therefore exercise the reset barrier.
func replayTestBotOpts(t *testing.T, batch string, repo service.TradeRepository, store *memReplayStore, opts journalBotOpts) (*Bot, *pollRecorder, chan error, context.CancelFunc) {
	t.Helper()
	srv, rec := recordingPollServer(t, batch)
	b := newJournalTestBotOpts(t, srv, repo, opts)
	b.SetReplayGuard(store)
	b.drainTimeout = 5 * time.Second
	// Keep the retry loop out of the way: durability is asserted directly.
	b.noticeEvery = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()
	return b, rec, errCh, cancel
}

// TestBot_StartAdmitsWhileHandlerBlocked pins the whole point of the change: a
// blocked handler must not stop fetching. The batch is admitted durably, the
// offset advances immediately, and a second poll happens while the first
// update is still in Notion.
func TestBot_StartAdmitsWhileHandlerBlocked(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":71,"username":"a"},"chat":{"id":71,"type":"private"},"text":"#journal\nPair: EURUSD\nType: BUY\nRR: +2"}}
	]}`
	repo := &gatedTradeRepo{started: make(chan struct{}), release: make(chan struct{}), savedCh: make(chan savedTrade, 4)}
	store := newMemReplayStore()

	_, rec, errCh, cancel := replayTestBot(t, batch, repo, store)

	select {
	case <-repo.started:
	case <-time.After(5 * time.Second):
		t.Fatal("journal never started")
	}
	// The handler is blocked, yet the batch is already admitted and Telegram
	// has been told to forget it: this is the stall fix.
	if got := store.cursorOffset(); got != 2 {
		t.Fatalf("cursor = %d while handler blocked, want 2", got)
	}
	select {
	case <-rec.acked:
	case <-time.After(5 * time.Second):
		t.Fatal("offset did not advance while a handler was blocked")
	}

	close(repo.release)
	select {
	case <-repo.savedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("journal was never saved")
	}
	cancel()
	if err := <-errCh; err != context.Canceled {
		t.Fatalf("Start returned %v, want context.Canceled", err)
	}
	if state, ok := store.claimState(1); !ok || state != stateDone {
		t.Fatalf("state = %q (found %v), want done", state, ok)
	}
}

// TestBot_StartMarksRunningBeforeHandler pins ordering: the row is running while
// the handler works, so a crash at that instant is detectable.
func TestBot_StartMarksRunningBeforeHandler(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":72,"username":"a"},"chat":{"id":72,"type":"private"},"text":"#journal\nPair: EURUSD\nType: BUY\nRR: +2"}}
	]}`
	repo := &gatedTradeRepo{started: make(chan struct{}), release: make(chan struct{}), savedCh: make(chan savedTrade, 4)}
	store := newMemReplayStore()

	_, _, errCh, cancel := replayTestBot(t, batch, repo, store)

	select {
	case <-repo.started:
	case <-time.After(5 * time.Second):
		t.Fatal("journal never started")
	}
	if state, ok := store.claimState(1); !ok || state != stateRunning {
		t.Fatalf("state while handler runs = %q (found %v), want running", state, ok)
	}
	close(repo.release)
	select {
	case <-repo.savedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("journal was never saved")
	}
	cancel()
	<-errCh
	if state, ok := store.claimState(1); !ok || state != stateDone {
		t.Fatalf("state = %q (found %v), want done", state, ok)
	}
}

// TestBot_StartReplaysAdmittedOnStartup pins restart recovery: a row that was
// admitted but never started is executed on the next start, in update order,
// without Telegram redelivering anything.
func TestBot_StartReplaysAdmittedOnStartup(t *testing.T) {
	srv, rec := recordingPollServer(t, `{"ok":true,"result":[]}`)
	repo := &gatedTradeRepo{started: make(chan struct{}), release: make(chan struct{}), savedCh: make(chan savedTrade, 8)}
	close(repo.release)
	store := newMemReplayStore()
	// Seed two admitted rows as a previous process would have left them.
	payload1, _ := json.Marshal(Update{UpdateID: 1, Message: &Message{
		MessageID: 1, From: &User{ID: 73, Username: "a"}, Chat: &Chat{ID: 73, Type: "private"},
		Text: "#journal\nPair: EURUSD\nType: BUY\nRR: +2",
	}})
	payload2, _ := json.Marshal(Update{UpdateID: 2, Message: &Message{
		MessageID: 2, From: &User{ID: 73, Username: "a"}, Chat: &Chat{ID: 73, Type: "private"},
		Text: "#journal\nPair: XAUUSD\nType: BUY\nRR: +1",
	}})
	store.seedRecord(updateRecord{UpdateID: 1, State: stateAdmitted, Payload: payload1, ChatID: 73})
	store.seedRecord(updateRecord{UpdateID: 2, State: stateAdmitted, Payload: payload2, ChatID: 73})
	_ = store.SetWithTTL(testGuard.cursorKey(), fetchCursor{Offset: 3, UpdatedAt: time.Now()}, 0)

	b := newJournalTestBot(t, srv, repo)
	b.SetReplayGuard(store)
	b.drainTimeout = 5 * time.Second
	b.noticeEvery = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	for i := range 2 {
		select {
		case <-repo.savedCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 2 admitted journals were replayed", i)
		}
	}
	cancel()
	if err := <-errCh; err != context.Canceled {
		t.Fatalf("Start returned %v, want context.Canceled", err)
	}
	saved, _ := repo.snapshot()
	if len(saved) != 2 || saved[0].symbol != "EURUSD" || saved[1].symbol != "XAUUSD" {
		t.Fatalf("replay order = %+v, want EURUSD then XAUUSD", saved)
	}
	// The first poll must carry the recovered cursor, not 0.
	if got := rec.offsetsSnapshot(); len(got) == 0 || got[0] != "3" {
		t.Fatalf("first poll offsets = %v, want offset 3", got)
	}
}

// TestBot_StartDoesNotReplayRunningAndNotifies pins the manual-verification
// policy: a row left running by a crash is never re-executed, the user is told,
// and the row is closed only after the notice is durable.
func TestBot_StartDoesNotReplayRunningAndNotifies(t *testing.T) {
	srv, rec := recordingPollServer(t, `{"ok":true,"result":[]}`)
	repo := &gatedTradeRepo{started: make(chan struct{}), release: make(chan struct{}), savedCh: make(chan savedTrade, 4)}
	close(repo.release)
	store := newMemReplayStore()
	payload, _ := json.Marshal(privateUpdate(74, 1))
	store.seedRecord(updateRecord{UpdateID: 1, State: stateRunning, Payload: payload, ChatID: 74})
	_ = store.SetWithTTL(testGuard.cursorKey(), fetchCursor{Offset: 2, UpdatedAt: time.Now()}, 0)

	b := newJournalTestBot(t, srv, repo)
	b.SetReplayGuard(store)
	b.drainTimeout = 5 * time.Second
	b.noticeEvery = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	deadline := time.After(5 * time.Second)
	for {
		if state, ok := store.claimState(1); ok && state == stateDone {
			break
		}
		select {
		case <-deadline:
			t.Fatal("interrupted row was never resolved")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-errCh

	if got := repo.savedSymbols(); len(got) != 0 {
		t.Fatalf("interrupted update was replayed: %v", got)
	}
	if store.noticeCount() == 0 && !sentContains(rec.sendsSnapshot(), "terputus") {
		t.Fatal("interrupted update has neither a pending notice nor a delivered warning")
	}
}

// TestBot_StartRefusesWhenInterruptedNoticeCannotBeStored pins fail-closed
// startup: when the verification notice for a row left running cannot be stored,
// the bot must not start. Continuing would poll and replay admitted rows while
// the user's only signal is still unwritten, and a later crash would lose it —
// the interrupted work would then look like work that never happened.
func TestBot_StartRefusesWhenInterruptedNoticeCannotBeStored(t *testing.T) {
	repo := &gatedTradeRepo{started: make(chan struct{}), release: make(chan struct{}), savedCh: make(chan savedTrade, 4)}
	close(repo.release)
	store := newMemReplayStore()

	interrupted := privateUpdate(88, 1)
	payload, err := json.Marshal(interrupted)
	if err != nil {
		t.Fatalf("marshal interrupted update: %v", err)
	}
	store.seedRecord(updateRecord{UpdateID: 1, State: stateRunning, Payload: payload, ChatID: 88})
	// Only the notice write fails: the row itself is readable, so the failure is
	// exactly the one startup must not paper over.
	store.failSet = func(key string) error {
		if strings.Contains(key, ":notice:") {
			return errors.New("badger write failed")
		}
		return nil
	}

	srv, rec := recordingPollServer(t, `{"ok":true,"result":[]}`)
	b := newJournalTestBot(t, srv, repo)
	b.SetReplayGuard(store)
	b.drainTimeout = time.Second
	b.noticeEvery = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	select {
	case err := <-errCh:
		if err == nil || err == context.Canceled {
			t.Fatalf("Start returned %v, want a fatal notice error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start kept running although the interrupted row's notice could not be stored")
	}
	if got := repo.savedSymbols(); len(got) != 0 {
		t.Fatalf("handler ran despite the refused startup: %v", got)
	}
	if got := store.noticeCount(); got != 0 {
		t.Fatalf("notices = %d, want none stored", got)
	}
	// The row stays running, so the next start retries the notice instead of
	// silently dropping the user's only signal.
	if state, ok := store.claimState(1); !ok || state != stateRunning {
		t.Fatalf("interrupted row = %q (found %v), want running for the next start", state, ok)
	}
	// Nothing was fetched: refusing startup must happen before polling.
	if offsets := rec.offsetsSnapshot(); len(offsets) != 0 {
		t.Fatalf("Start polled (%v) although startup was refused", offsets)
	}
}

// TestBot_StartAdmitFailureDoesNotAdvanceOffset pins fail-closed admission: a
// failed write leaves the offset untouched so Telegram redelivers the batch.
func TestBot_StartAdmitFailureDoesNotAdvanceOffset(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":75,"username":"a"},"chat":{"id":75,"type":"private"},"text":"#journal\nPair: EURUSD\nType: BUY\nRR: +2"}}
	]}`
	repo := &gatedTradeRepo{started: make(chan struct{}), release: make(chan struct{}), savedCh: make(chan savedTrade, 4)}
	close(repo.release)
	store := newMemReplayStore()
	store.failPut = errors.New("badger unavailable")

	_, rec, errCh, cancel := replayTestBot(t, batch, repo, store)
	t.Cleanup(cancel)

	// Give Start time to attempt and fail at least twice (2s backoff between).
	time.Sleep(200 * time.Millisecond)
	if got := store.cursorOffset(); got != 0 {
		t.Fatalf("cursor = %d after failed admission, want 0", got)
	}
	if got := repo.savedSymbols(); len(got) != 0 {
		t.Fatalf("handler ran without admission: %v", got)
	}
	for _, offset := range rec.offsetsSnapshot() {
		if offset != "0" {
			t.Fatalf("failed admission advanced the offset: %v", rec.offsetsSnapshot())
		}
	}
	_ = errCh
}

// TestBot_StartDuplicateDeliveryNotEnqueuedTwice pins dedupe: a batch that
// repeats an already-known update id inserts nothing and runs nothing.
func TestBot_StartDuplicateDeliveryNotEnqueuedTwice(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":76,"username":"a"},"chat":{"id":76,"type":"private"},"text":"#journal\nPair: EURUSD\nType: BUY\nRR: +2"}}
	]}`
	repo := &gatedTradeRepo{started: make(chan struct{}), release: make(chan struct{}), savedCh: make(chan savedTrade, 4)}
	close(repo.release)
	store := newMemReplayStore()
	store.seedRecord(updateRecord{UpdateID: 1, State: stateDone})

	_, rec, errCh, cancel := replayTestBot(t, batch, repo, store)

	select {
	case <-rec.acked: // offset advanced past the duplicate
	case <-time.After(5 * time.Second):
		t.Fatal("duplicate delivery stalled the offset")
	}
	cancel()
	<-errCh
	if got := repo.savedSymbols(); len(got) != 0 {
		t.Fatalf("duplicate update was executed: %v", got)
	}
}

// TestBot_StartAbortedHandlerStaysRunning pins shutdown semantics: an aborted
// handler leaves the row running (never done, never replayed), so the next start
// asks the user to verify.
func TestBot_StartAbortedHandlerStaysRunning(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":77,"username":"a"},"chat":{"id":77,"type":"private"},"text":"#journal\nPair: EURUSD\nType: BUY\nRR: +2"}}
	]}`
	repo := &ctxBlockingTradeRepo{started: make(chan struct{}), stopped: make(chan struct{})}
	store := newMemReplayStore()
	// Short drain so the handler is cancelled promptly instead of after 5s.
	srv, _ := recordingPollServer(t, batch)
	b := newJournalTestBot(t, srv, repo)
	b.SetReplayGuard(store)
	b.drainTimeout = 20 * time.Millisecond
	b.noticeEvery = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	select {
	case <-repo.started:
	case <-time.After(5 * time.Second):
		t.Fatal("journal never started")
	}
	cancel()
	if err := <-errCh; err != context.Canceled {
		t.Fatalf("Start returned %v, want context.Canceled", err)
	}
	if state, ok := store.claimState(1); !ok || state != stateRunning {
		t.Fatalf("aborted row = %q (found %v), want running", state, ok)
	}
}

// TestBot_StartStopsWhenMarkRunningFails pins fail-stop: if the running state
// cannot be written, the handler must not run and the bot must stop rather than
// let this user's later updates overtake the unrecorded one.
//
// The row is seeded as admitted (Telegram was already acknowledged, so only the
// ledger can drive the replay) and the write is broken before Start reads it:
// breaking it after admission would race the worker that marks the row running.
func TestBot_StartStopsWhenMarkRunningFails(t *testing.T) {
	repo := &gatedTradeRepo{started: make(chan struct{}), release: make(chan struct{}), savedCh: make(chan savedTrade, 4)}
	close(repo.release)
	store := newMemReplayStore()

	replayed := Update{UpdateID: 1, Message: &Message{
		MessageID: 1,
		From:      &User{ID: 81, Username: "a"},
		Chat:      &Chat{ID: 81, Type: "private"},
		Text:      "#journal\nPair: EURUSD\nType: BUY\nRR: +2",
	}}
	payload, err := json.Marshal(replayed)
	if err != nil {
		t.Fatalf("marshal admitted update: %v", err)
	}
	store.seedRecord(updateRecord{UpdateID: 1, State: stateAdmitted, Payload: payload})
	store.failSet = func(key string) error {
		if strings.Contains(key, ":upd:") {
			return errors.New("badger write failed")
		}
		return nil
	}

	srv, _ := recordingPollServer(t, `{"ok":true,"result":[]}`)
	b := newJournalTestBot(t, srv, repo)
	b.SetReplayGuard(store)
	b.drainTimeout = 5 * time.Second
	b.noticeEvery = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	select {
	case err := <-errCh:
		if err == nil || err == context.Canceled {
			t.Fatalf("Start returned %v, want a fatal ledger error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start kept polling after the running state could not be recorded")
	}
	if got := repo.savedSymbols(); len(got) != 0 {
		t.Fatalf("handler ran without a durable running state: %v", got)
	}
	if state, ok := store.claimState(1); !ok || state != stateAdmitted {
		t.Fatalf("state = %q (found %v), want admitted for replay", state, ok)
	}
}

// TestBot_StartDefersResetUntilOlderUpdatesFinish pins the reset barrier: a
// destructive owner reset must not run while an older update is still unfinished,
// or that update would write after the wipe and resurrect pre-reset state. The
// row stays admitted, so a crash before the retry still replays it.
func TestBot_StartDefersResetUntilOlderUpdatesFinish(t *testing.T) {
	const ownerID = 99
	const batch = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":82,"username":"a"},"chat":{"id":82,"type":"private"},"text":"#journal\nPair: EURUSD\nType: BUY\nRR: +2"}},
		{"update_id":2,"message":{"message_id":2,"from":{"id":99,"username":"owner"},"chat":{"id":99,"type":"private"},"text":"/reset confirm"}}
	]}`
	repo := &gatedTradeRepo{started: make(chan struct{}), release: make(chan struct{}), savedCh: make(chan savedTrade, 4)}
	store := newMemReplayStore()

	_, _, errCh, cancel := replayTestBotOpts(t, batch, repo, store, journalBotOpts{ownerID: ownerID})

	select {
	case <-repo.started:
	case <-time.After(5 * time.Second):
		t.Fatal("journal never started")
	}
	// The journal (id 1) is running, so the reset (id 2) must stay admitted and
	// must not have started.
	if state, ok := store.claimState(2); !ok || state != stateAdmitted {
		t.Fatalf("reset state = %q (found %v), want admitted while older work runs", state, ok)
	}

	close(repo.release)
	select {
	case <-repo.savedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("journal was never saved")
	}
	// The retry tick is a minute away, so the reset is still parked. What matters
	// is that it never ran beside the journal: no wipe may happen until every
	// older update is terminal.
	if state, ok := store.claimState(2); !ok || state != stateAdmitted {
		t.Fatalf("reset state = %q (found %v), want still admitted (not started)", state, ok)
	}
	// The row becomes done only after the handler returns, which is after
	// SaveTrade reported its save: poll instead of racing the completion hook.
	deadline := time.After(5 * time.Second)
	for {
		state, ok := store.claimState(1)
		if ok && state == stateDone {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("journal state = %q (found %v), want done", state, ok)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not stop after cancellation")
	}
}

// TestUpdateDispatcher_StartHookPanicReturnsError pins that a panicking start
// hook is reported as an error: returning nil would run the handler without a
// durable running record.
func TestUpdateDispatcher_StartHookPanicReturnsError(t *testing.T) {
	var (
		mu      sync.Mutex
		ran     bool
		outcome updateOutcome
	)
	d := newUpdateDispatcher(context.Background(), 1, func(context.Context, Update) {
		mu.Lock()
		ran = true
		mu.Unlock()
	}, func(Update, any) {})
	d.setOnStart(func(Update) error { panic("hook exploded") })
	d.setOnDone(func(_ Update, o updateOutcome) {
		mu.Lock()
		outcome = o
		mu.Unlock()
	})

	if err := d.Enqueue(context.Background(), privateUpdate(1, 1)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if !d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if ran {
		t.Fatal("handler ran although the start hook panicked")
	}
	if outcome.Started {
		t.Fatal("a panicked start hook must not report the update as started")
	}
	select {
	case <-d.Fatal():
		if d.FatalErr() == nil {
			t.Fatal("fatal signal must retain its error")
		}
	default:
		t.Fatal("a panicking start hook must be fatal, not silently skipped")
	}
}

// TestUpdateDispatcher_FatalFencesLaterUpdates pins that a fatal condition
// immediately stops later updates from starting, even before the poll loop
// notices: otherwise the next update could overtake the failed one.
func TestUpdateDispatcher_FatalFencesLaterUpdates(t *testing.T) {
	var (
		mu  sync.Mutex
		ran []int
	)
	d := newUpdateDispatcher(context.Background(), 1, func(_ context.Context, u Update) {
		mu.Lock()
		ran = append(ran, u.UpdateID)
		mu.Unlock()
	}, nil)
	d.setOnStart(func(u Update) error {
		if u.UpdateID == 1 {
			return errors.New("ledger write failed")
		}
		return nil
	})

	if err := d.Enqueue(context.Background(), privateUpdate(1, 1)); err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	// Wait until the fatal condition is recorded, then try a later update.
	deadline := time.After(2 * time.Second)
	for !d.Stopped() {
		select {
		case <-deadline:
			t.Fatal("dispatcher was not fenced after the fatal error")
		case <-time.After(time.Millisecond):
		}
	}
	if err := d.Enqueue(context.Background(), privateUpdate(1, 2)); !errors.Is(err, errDispatcherStopped) {
		t.Fatalf("enqueue after fatal = %v, want errDispatcherStopped", err)
	}
	d.Wait(time.Second)
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 0 {
		t.Fatalf("handlers ran despite the fatal fence: %v", ran)
	}
}

// TestUpdateDispatcher_ResetGateHoldsThroughCompletion pins the global barrier
// end to end: while a reset is waiting and while it runs, no other update may
// start — including another user's — and the barrier is released only after the
// reset's completion has been recorded.
func TestUpdateDispatcher_ResetGateHoldsThroughCompletion(t *testing.T) {
	var (
		mu      sync.Mutex
		ran     []int
		release = make(chan struct{})
	)
	d := newUpdateDispatcher(context.Background(), 4, func(_ context.Context, u Update) {
		mu.Lock()
		ran = append(ran, u.UpdateID)
		mu.Unlock()
		if u.UpdateID == 1 {
			<-release // the reset is still working
		}
	}, nil)
	// First attempt is deferred because older work is pending.
	var block int32 = 1
	d.setOnStart(func(Update) error {
		if atomic.LoadInt32(&block) == 1 {
			return errResetBlocked
		}
		return nil
	})
	// Deferral is signalled directly: while the reset is parked inside the
	// handler, Wait cannot report a drain because the reset is accepted work.
	deferrals := make(chan int, 8)
	d.setOnDefer(func(u Update) { deferrals <- u.UpdateID })

	if err := d.Enqueue(context.Background(), resetUpdate(1)); err != nil {
		t.Fatalf("enqueue reset: %v", err)
	}
	select {
	case id := <-deferrals:
		if id != 1 {
			t.Fatalf("deferred update = %d, want the reset (1)", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reset was never deferred")
	}
	if got := d.gateUpdate(); got != 1 {
		t.Fatalf("gate = %d, want 1 (reset waiting)", got)
	}

	// Another user's update arrives while the barrier is up: it must not run.
	if err := d.Enqueue(context.Background(), privateUpdate(2, 2)); err != nil {
		t.Fatalf("enqueue other user: %v", err)
	}
	select {
	case id := <-deferrals:
		if id != 2 {
			t.Fatalf("deferred update = %d, want the other user's update (2)", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the other user's update was not deferred behind the barrier")
	}
	mu.Lock()
	if len(ran) != 0 {
		mu.Unlock()
		t.Fatalf("update ran while the reset barrier was up: %v", ran)
	}
	mu.Unlock()

	// The reset may now start; hold it inside the handler and prove the barrier
	// stays up while it executes.
	atomic.StoreInt32(&block, 0)
	if err := d.Enqueue(context.Background(), resetUpdate(1)); err != nil {
		t.Fatalf("re-enqueue reset: %v", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		running := len(ran) == 1 && ran[0] == 1
		mu.Unlock()
		if running {
			break
		}
		select {
		case <-deadline:
			t.Fatal("reset never started")
		case <-time.After(time.Millisecond):
		}
	}
	if err := d.Enqueue(context.Background(), privateUpdate(2, 2)); err != nil {
		t.Fatalf("enqueue other user during reset: %v", err)
	}
	select {
	case id := <-deferrals:
		if id != 2 {
			t.Fatalf("deferred update = %d, want the update admitted during the reset (2)", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the update admitted during the reset was not deferred")
	}
	mu.Lock()
	for _, id := range ran {
		if id == 2 {
			mu.Unlock()
			t.Fatal("another user's update ran while the reset was still executing")
		}
	}
	mu.Unlock()

	// Finish the reset, then release the barrier the way Start does (after the
	// row is durably terminal).
	close(release)
	if !d.Wait(5 * time.Second) {
		t.Fatal("dispatcher did not finish the reset")
	}
	d.clearGateFor(1)
	if err := d.Enqueue(context.Background(), privateUpdate(2, 2)); err != nil {
		t.Fatalf("re-enqueue other user: %v", err)
	}
	if !d.Wait(5 * time.Second) {
		t.Fatal("dispatcher did not drain after the barrier lifted")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 2 || ran[0] != 1 || ran[1] != 2 {
		t.Fatalf("ran = %v, want [1 2]", ran)
	}
}

// TestUpdateDispatcher_ResetDoesNotDeadlockOnOlderQueuedUpdate pins the
// regression where the barrier blocked everything: an older update still queued
// behind a running one must be allowed through, because the reset waits on it
// (olderPending). Blocking it deferred the reset forever.
func TestUpdateDispatcher_ResetDoesNotDeadlockOnOlderQueuedUpdate(t *testing.T) {
	var (
		mu      sync.Mutex
		ran     []int
		release = make(chan struct{})
	)
	d := newUpdateDispatcher(context.Background(), 4, func(_ context.Context, u Update) {
		mu.Lock()
		ran = append(ran, u.UpdateID)
		mu.Unlock()
		if u.UpdateID == 1 {
			<-release // the first update is still executing
		}
	}, nil)

	ctx := context.Background()
	// User 1: update 1 runs, update 2 is queued behind it (per-user FIFO).
	if err := d.Enqueue(ctx, privateUpdate(1, 1)); err != nil {
		t.Fatalf("enqueue update 1: %v", err)
	}
	if err := d.Enqueue(ctx, privateUpdate(1, 2)); err != nil {
		t.Fatalf("enqueue update 2: %v", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		running := len(ran) == 1 && ran[0] == 1
		mu.Unlock()
		if running {
			break
		}
		select {
		case <-deadline:
			t.Fatal("update 1 never started")
		case <-time.After(time.Millisecond):
		}
	}

	// The owner's reset arrives while update 1 executes: it must raise the
	// barrier without blocking update 2, which it depends on.
	if err := d.Enqueue(ctx, resetUpdate(3)); err != nil {
		t.Fatalf("enqueue reset: %v", err)
	}
	<-time.After(50 * time.Millisecond)
	if got := d.gateUpdate(); got != 3 {
		t.Fatalf("gate = %d, want 3 (reset waiting for in-flight work)", got)
	}

	close(release)
	// Update 2 (id 2 < gate 3) must run once update 1 finishes; the reset must
	// not run before it.
	deadline = time.After(5 * time.Second)
	for {
		mu.Lock()
		got := append([]int(nil), ran...)
		mu.Unlock()
		if len(got) >= 2 {
			if got[0] != 1 || got[1] != 2 {
				t.Fatalf("ran = %v, want [1 2 ...] (older update must pass the barrier)", got)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("older queued update was blocked by the reset barrier (ran = %v)", got)
		case <-time.After(time.Millisecond):
		}
	}

	// Nothing else ran: the reset is still waiting because it is deferred and
	// only the retry loop re-enqueues it.
	mu.Lock()
	if len(ran) != 2 {
		mu.Unlock()
		t.Fatalf("ran = %v, want exactly [1 2]", ran)
	}
	mu.Unlock()
	if got := d.gateUpdate(); got != 3 {
		t.Fatalf("gate = %d, want 3 until the reset completes", got)
	}

	// Mirror retryDeferred: the reset is admitted once nothing is running.
	if err := d.Enqueue(ctx, resetUpdate(3)); err != nil {
		t.Fatalf("re-enqueue reset: %v", err)
	}
	if !d.Wait(5 * time.Second) {
		t.Fatal("dispatcher did not drain: the reset deadlocked behind the barrier")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 3 || ran[2] != 3 {
		t.Fatalf("ran = %v, want [1 2 3]", ran)
	}
}

// TestRetryDeferredRequeuesOlderEntriesBehindReset pins the retry rule: while a
// reset holds the barrier, entries OLDER than it must be retried (the reset
// depends on them), while newer ones keep waiting.
func TestRetryDeferredRequeuesOlderEntriesBehindReset(t *testing.T) {
	var (
		mu   sync.Mutex
		ran  []int
		gate = make(chan struct{})
	)
	d := newUpdateDispatcher(context.Background(), 4, func(_ context.Context, u Update) {
		mu.Lock()
		ran = append(ran, u.UpdateID)
		mu.Unlock()
	}, nil)
	d.setOnStart(func(u Update) error {
		if u.UpdateID == 3 {
			return errResetBlocked // reset waiting for older work
		}
		return nil
	})
	d.setOnDefer(func(u Update) {
		if u.UpdateID == 3 {
			select {
			case <-gate:
			default:
				close(gate)
			}
		}
	})

	store := newMemReplayStore()
	replay := newTestGuard(store)

	ctx := context.Background()
	if err := d.Enqueue(ctx, resetUpdate(3)); err != nil {
		t.Fatalf("enqueue reset: %v", err)
	}
	select {
	case <-gate:
	case <-time.After(2 * time.Second):
		t.Fatal("reset was never deferred")
	}
	if got := d.gateUpdate(); got != 3 {
		t.Fatalf("gate = %d, want 3", got)
	}

	deferred := make(chan Update, 8)
	d.pending <- struct{}{}
	d.pending <- struct{}{}
	deferred <- privateUpdate(1, 2) // older than the barrier: must be retried
	deferred <- privateUpdate(1, 4) // newer than the barrier: must keep waiting

	b := &Bot{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), replay: replay}
	if !b.retryDeferredOnce(ctx, deferred, d, replay) {
		t.Fatal("retryDeferredOnce reported a fatal condition")
	}
	if !d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not drain")
	}
	mu.Lock()
	if len(ran) != 1 || ran[0] != 2 {
		mu.Unlock()
		t.Fatalf("ran = %v, want only the older update [2]", ran)
	}
	mu.Unlock()

	// The newer entry is still parked, not dropped.
	select {
	case u := <-deferred:
		if u.UpdateID != 4 {
			t.Fatalf("deferred entry = %d, want 4", u.UpdateID)
		}
	default:
		t.Fatal("newer deferred entry was dropped instead of re-queued")
	}
}

// TestRetryDeferredDifferentUsersRetryIndependently pins that ordinary deferred
// work is retried per user: one user's still-blocked update must not hold back
// another user's, or a single stalled row would serialise the whole community —
// exactly the stall the per-user queues exist to prevent. Only a destructive
// reset waits for every user, and that global check must survive.
func TestRetryDeferredDifferentUsersRetryIndependently(t *testing.T) {
	store := newMemReplayStore()
	replay := newTestGuard(store)
	stalled := privateUpdate(1, 5)
	ready := privateUpdate(2, 6)
	seedAdmitted(t, store, stalled)
	seedAdmitted(t, store, ready)

	var (
		mu  sync.Mutex
		ran []int
	)
	deferred := make(chan Update, 8)
	d := newUpdateDispatcher(context.Background(), 4, func(_ context.Context, u Update) {
		mu.Lock()
		ran = append(ran, u.UpdateID)
		mu.Unlock()
	}, nil)
	d.setOnStart(func(u Update) error {
		if u.UpdateID == 5 {
			return errResetBlocked // user 1 is still stalled
		}
		return replay.markRunning(u.UpdateID)
	})
	d.setOnDefer(func(u Update) {
		if !requeue(deferred, u) {
			d.fail(fmt.Errorf("deferred update %d could not be queued", u.UpdateID))
		}
	})
	d.setOnDone(func(u Update, outcome updateOutcome) {
		if !outcome.Started || !outcome.Completed || outcome.Aborted {
			return
		}
		if err := replay.markDone(u.UpdateID); err != nil {
			d.fail(fmt.Errorf("could not complete update %d: %w", u.UpdateID, err))
		}
	})

	// Both were deferred earlier; the stalled one is first in the buffer.
	d.pending <- struct{}{}
	d.pending <- struct{}{}
	deferred <- stalled
	deferred <- ready

	ctx := context.Background()
	b := &Bot{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), replay: replay}
	if !b.retryDeferredOnce(ctx, deferred, d, replay) {
		t.Fatal("retryDeferredOnce reported a fatal condition")
	}
	if !d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not settle")
	}
	mu.Lock()
	got := append([]int(nil), ran...)
	mu.Unlock()
	if len(got) != 1 || got[0] != 6 {
		t.Fatalf("ran = %v, want only the other user's update [6]", got)
	}
	if state, ok := store.claimState(6); !ok || state != stateDone {
		t.Fatalf("ready row = %q (found %v), want done", state, ok)
	}
	if state, ok := store.claimState(5); !ok || state != stateAdmitted {
		t.Fatalf("stalled row = %q (found %v), want admitted for a later retry", state, ok)
	}
	// The stalled entry is parked, not dropped.
	select {
	case u := <-deferred:
		if u.UpdateID != 5 {
			t.Fatalf("deferred entry = %d, want the stalled update 5", u.UpdateID)
		}
	default:
		t.Fatal("stalled deferred entry was dropped instead of re-queued")
	}

	// The reset barrier must stay global: it waits for every user's unfinished
	// work, so user 1's admitted row still counts for a reset.
	older, err := replay.olderPending(7)
	if err != nil {
		t.Fatalf("olderPending: %v", err)
	}
	if older != 1 {
		t.Fatalf("olderPending(7) = %d, want 1: a reset must wait for other users' work", older)
	}
}

// TestUpdateDispatcher_DeferredResetLetsOlderAdmittedWorkRun pins the barrier's
// liveness contract: a reset deferred because an older row is still admitted
// must not freeze the barrier. The older row still runs — the reset is waiting
// on exactly that row — and once it is terminal the retried reset completes and
// releases the barrier.
func TestUpdateDispatcher_DeferredResetLetsOlderAdmittedWorkRun(t *testing.T) {
	store := newMemReplayStore()
	replay := newTestGuard(store)

	older := privateUpdate(1, 2)
	older.Message.Text = "#journal"
	reset := resetUpdate(3)
	seedAdmitted(t, store, older)
	seedAdmitted(t, store, reset)

	var (
		mu  sync.Mutex
		ran []int
	)
	h := newLedgerHarness(replay, func(_ context.Context, u Update) {
		mu.Lock()
		ran = append(ran, u.UpdateID)
		mu.Unlock()
	})
	ctx := context.Background()

	// The reset is processed while the older row is still admitted (e.g. it was
	// deferred behind an earlier barrier): it must be parked, not started.
	if err := h.d.Enqueue(ctx, reset); err != nil {
		t.Fatalf("enqueue reset: %v", err)
	}
	select {
	case id := <-h.deferrals:
		if id != 3 {
			t.Fatalf("deferred update = %d, want the reset (3)", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reset was never deferred")
	}
	// The older admitted row must still start: deferring it too would leave the
	// reset waiting on work that can never run.
	if err := h.d.Enqueue(ctx, older); err != nil {
		t.Fatalf("enqueue older update: %v", err)
	}
	if !h.d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not finish the older update")
	}
	mu.Lock()
	got := append([]int(nil), ran...)
	mu.Unlock()
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("ran = %v, want only the older update [2]", got)
	}
	if state, ok := store.claimState(2); !ok || state != stateDone {
		t.Fatalf("older row = %q (found %v), want done", state, ok)
	}
	if state, ok := store.claimState(3); !ok || state != stateAdmitted {
		t.Fatalf("reset row = %q (found %v), want admitted while deferred", state, ok)
	}
	if gate := h.d.gateUpdate(); gate != 3 {
		t.Fatalf("gate = %d, want 3 until the reset completes", gate)
	}

	// Retry: nothing older than the reset is unfinished now, so it runs and
	// releases the barrier only after its row is durably terminal.
	b := &Bot{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), replay: replay}
	if !b.retryDeferredOnce(ctx, h.deferred, h.d, replay) {
		t.Fatal("retryDeferredOnce reported a fatal condition")
	}
	if !h.d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not finish the reset")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 2 || ran[0] != 2 || ran[1] != 3 {
		t.Fatalf("ran = %v, want [2 3]", ran)
	}
	if state, ok := store.claimState(3); !ok || state != stateDone {
		t.Fatalf("reset row = %q (found %v), want done", state, ok)
	}
	if gate := h.d.gateUpdate(); gate != 0 {
		t.Fatalf("gate = %d, want 0 after the reset completed", gate)
	}
}

// TestRetryDeferredOlderInputNotOvertakenByFreshInput pins arrival order across
// deferral: even when the deferred buffer holds a fresh update ahead of an older
// one, the retry loop must start the older one first, because the ledger — not
// the buffer order — decides what may run. Both updates belong to one user, so a
// reordered start would also invert that user's FIFO queue.
func TestRetryDeferredOlderInputNotOvertakenByFreshInput(t *testing.T) {
	store := newMemReplayStore()
	replay := newTestGuard(store)

	older := privateUpdate(1, 2)
	fresh := privateUpdate(1, 4)
	seedAdmitted(t, store, older)
	seedAdmitted(t, store, fresh)

	var (
		mu  sync.Mutex
		ran []int
	)
	h := newLedgerHarness(replay, func(_ context.Context, u Update) {
		mu.Lock()
		ran = append(ran, u.UpdateID)
		mu.Unlock()
	})
	// Adversarial order: the fresh update was re-queued ahead of the older one.
	h.d.pending <- struct{}{}
	h.d.pending <- struct{}{}
	h.deferred <- fresh
	h.deferred <- older

	ctx := context.Background()
	b := &Bot{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), replay: replay}
	if !b.retryDeferredOnce(ctx, h.deferred, h.d, replay) {
		t.Fatal("retryDeferredOnce reported a fatal condition")
	}
	if !h.d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not drain")
	}
	mu.Lock()
	got := append([]int(nil), ran...)
	mu.Unlock()
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("ran = %v, want only the older update [2] first", got)
	}
	// The fresh update is parked, not dropped, until the older one is terminal.
	// Peek without consuming: the retry loop owns the queue.
	select {
	case u := <-h.deferred:
		if u.UpdateID != 4 {
			t.Fatalf("deferred entry = %d, want the fresh update 4", u.UpdateID)
		}
		h.deferred <- u
	default:
		t.Fatal("fresh deferred update was dropped instead of re-queued")
	}
	if !b.retryDeferredOnce(ctx, h.deferred, h.d, replay) {
		t.Fatal("retryDeferredOnce reported a fatal condition")
	}
	if !h.d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not finish the fresh update")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 2 || ran[0] != 2 || ran[1] != 4 {
		t.Fatalf("ran = %v, want [2 4] (arrival order preserved)", ran)
	}
}

// TestUpdateDispatcher_DeferredUserUpdateHoldsBackFreshInput pins the per-user
// hold: once an update of a user is deferred, a fresh update of the SAME user
// must not start ahead of it, even though the barrier that deferred it is gone.
// The retry tick can be a minute away, so without the hold the fresh update would
// overtake the older one and invert that user's FIFO queue (guided answers,
// callbacks and edits all depend on arrival order).
func TestUpdateDispatcher_DeferredUserUpdateHoldsBackFreshInput(t *testing.T) {
	store := newMemReplayStore()
	replay := newTestGuard(store)
	older := privateUpdate(1, 3)
	fresh := privateUpdate(1, 4)
	seedAdmitted(t, store, older)
	seedAdmitted(t, store, fresh)

	var (
		mu      sync.Mutex
		ran     []int
		blocked atomic.Bool
	)
	blocked.Store(true)
	releaseOlder := make(chan struct{})
	d := newUpdateDispatcher(context.Background(), 4, func(_ context.Context, u Update) {
		mu.Lock()
		ran = append(ran, u.UpdateID)
		mu.Unlock()
		if u.UpdateID == 3 {
			<-releaseOlder // keep the older row non-terminal during the first pass
		}
	}, nil)
	deferred := make(chan Update, 8)
	d.setOnStart(func(u Update) error {
		if blocked.Load() {
			return errResetBlocked // deferred, exactly like a reset waiting for older work
		}
		return replay.markRunning(u.UpdateID)
	})
	d.setOnDefer(func(u Update) {
		if !requeue(deferred, u) {
			d.fail(fmt.Errorf("deferred update %d could not be queued", u.UpdateID))
		}
	})
	d.setOnDone(func(u Update, outcome updateOutcome) {
		if !outcome.Started || !outcome.Completed || outcome.Aborted {
			return
		}
		if err := replay.markDone(u.UpdateID); err != nil {
			d.fail(fmt.Errorf("could not complete update %d: %w", u.UpdateID, err))
		}
	})

	ctx := context.Background()
	if err := d.Enqueue(ctx, older); err != nil {
		t.Fatalf("enqueue older update: %v", err)
	}
	if !d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not defer the older update")
	}
	if got := len(deferred); got != 1 {
		t.Fatalf("deferred queue length = %d, want the older update parked", got)
	}

	// The fresh update arrives while the older one is still waiting for its
	// retry: it must be parked behind it, not run.
	if err := d.Enqueue(ctx, fresh); err != nil {
		t.Fatalf("enqueue fresh update: %v", err)
	}
	if !d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not settle the fresh update")
	}
	mu.Lock()
	got := append([]int(nil), ran...)
	mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("ran = %v, want nothing: the fresh update overtook the deferred older one", got)
	}
	if got := len(deferred); got != 2 {
		t.Fatalf("deferred queue length = %d, want both updates parked", got)
	}

	// Release the block and retry: the older update starts first and stays in
	// flight, so the fresh one must keep waiting behind it rather than be
	// admitted in the same pass.
	blocked.Store(false)
	b := &Bot{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), replay: replay}
	if !b.retryDeferredOnce(ctx, deferred, d, replay) {
		t.Fatal("retryDeferredOnce reported a fatal condition")
	}
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		started := len(ran) == 1 && ran[0] == 3
		mu.Unlock()
		if started {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the older update never started")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case u := <-deferred:
		if u.UpdateID != 4 {
			t.Fatalf("deferred entry = %d, want the fresh update 4", u.UpdateID)
		}
		// Peek without consuming: the retry loop owns the queue.
		deferred <- u
	default:
		t.Fatal("the fresh update was not parked behind the in-flight older one")
	}

	close(releaseOlder)
	if !d.Wait(5 * time.Second) {
		t.Fatal("dispatcher did not finish the older update")
	}
	if !b.retryDeferredOnce(ctx, deferred, d, replay) {
		t.Fatal("retryDeferredOnce reported a fatal condition")
	}
	if !d.Wait(5 * time.Second) {
		t.Fatal("dispatcher did not finish the fresh update")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 2 || ran[0] != 3 || ran[1] != 4 {
		t.Fatalf("ran = %v, want [3 4] (deferred update must go first)", ran)
	}
}

// TestUpdateDispatcher_ResetWaitsForInFlightUpdate pins that a reset admitted
// after an update is already executing does not start concurrently with it: a
// write landing after the wipe would survive the reset. The deferred reset is
// retried (as Start's retry loop does) once the older work is terminal.
func TestUpdateDispatcher_ResetWaitsForInFlightUpdate(t *testing.T) {
	var (
		mu          sync.Mutex
		ran         []int
		inFlight    int
		maxInFlight int
	)
	release := make(chan struct{})
	deferred := make(chan int, 1)
	d := newUpdateDispatcher(context.Background(), 4, func(_ context.Context, u Update) {
		mu.Lock()
		ran = append(ran, u.UpdateID)
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		if u.UpdateID == 1 {
			<-release // the older update is slow
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
	}, nil)
	d.resetCheck = isDestructiveReset
	d.setOnDefer(func(u Update) { deferred <- u.UpdateID })

	ctx := context.Background()
	if err := d.Enqueue(ctx, privateUpdate(1, 1)); err != nil {
		t.Fatalf("enqueue older update: %v", err)
	}
	// Wait until the older update is really executing, then admit the reset.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		started := len(ran) == 1
		mu.Unlock()
		if started {
			break
		}
		select {
		case <-deadline:
			t.Fatal("older update never started")
		case <-time.After(time.Millisecond):
		}
	}
	if err := d.Enqueue(ctx, resetUpdate(2)); err != nil {
		t.Fatalf("enqueue reset: %v", err)
	}
	select {
	case id := <-deferred:
		if id != 2 {
			t.Fatalf("deferred update = %d, want the reset (2)", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reset was not deferred while an update was in flight")
	}
	if got := d.gateUpdate(); got != 2 {
		t.Fatalf("gate = %d, want 2 while the older update runs", got)
	}
	if d.Wait(50 * time.Millisecond) {
		t.Fatal("dispatcher drained while the older update was still in flight")
	}

	close(release)
	if !d.Wait(5 * time.Second) {
		t.Fatal("dispatcher did not finish the older update")
	}
	// Start's retry loop re-enqueues the deferred reset once nothing is running.
	if err := d.Enqueue(ctx, resetUpdate(2)); err != nil {
		t.Fatalf("re-enqueue reset: %v", err)
	}
	if !d.Wait(5 * time.Second) {
		t.Fatal("dispatcher did not drain the reset")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 2 || ran[0] != 1 || ran[1] != 2 {
		t.Fatalf("ran = %v, want [1 2]", ran)
	}
	if maxInFlight != 1 {
		t.Fatalf("max concurrent handlers = %d, want 1 (reset must not overlap)", maxInFlight)
	}
}

// TestUpdateDispatcher_GateKeepsLowestReset pins that the barrier tracks the
// lowest pending reset id, so an earlier reset is never overtaken by a later one.
func TestUpdateDispatcher_GateKeepsLowestReset(t *testing.T) {
	d := newUpdateDispatcher(context.Background(), 1, func(context.Context, Update) {}, nil)
	d.addGate(5)
	d.addGate(9)
	if got := d.gateUpdate(); got != 5 {
		t.Fatalf("gate = %d, want 5 (lowest pending reset)", got)
	}
	// A later update is deferred; the gated reset itself is admitted.
	if got := d.beginRun(Update{UpdateID: 7}); got != runDefer {
		t.Fatalf("later update decision = %v, want runDefer", got)
	}
	// An OLDER update still runs while the reset is only waiting...
	if got := d.beginRun(Update{UpdateID: 4}); got != runProceed {
		t.Fatalf("older update decision = %v, want runProceed (reset waits on it)", got)
	}
	d.endRun()
	// ...but once the reset itself executes, nothing else may start beside it.
	if got := d.beginRun(resetUpdate(5)); got != runProceed {
		t.Fatalf("gated reset decision = %v, want runProceed", got)
	}
	if got := d.beginRun(Update{UpdateID: 4}); got != runDefer {
		t.Fatalf("older update during reset = %v, want runDefer", got)
	}
	d.endRun()
	// Finishing the lowest reset moves the barrier to the next pending one
	// instead of dropping it.
	d.clearGateFor(5)
	if got := d.gateUpdate(); got != 9 {
		t.Fatalf("gate = %d after the first reset, want 9 (next pending reset)", got)
	}
	if got := d.beginRun(Update{UpdateID: 7}); got != runProceed {
		t.Fatalf("update older than the remaining reset = %v, want runProceed", got)
	}
	d.endRun()
	if got := d.beginRun(Update{UpdateID: 10}); got != runDefer {
		t.Fatalf("update newer than the remaining reset = %v, want runDefer", got)
	}
	d.clearGateFor(9)
	if got := d.beginRun(Update{UpdateID: 10}); got != runProceed {
		t.Fatalf("update after all resets cleared = %v, want runProceed", got)
	}
	d.endRun()
}

// TestBot_StartResolvesPanicInProcess pins that a panicking handler is resolved
// immediately: a durable notice plus a terminal row, instead of a running row
// that blocks resets and tells nobody until the next restart.
func TestBot_StartResolvesPanicInProcess(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":83,"username":"a"},"chat":{"id":83,"type":"private"},"text":"#journal\nPair: EURUSD\nType: BUY\nRR: +2"}}
	]}`
	store := newMemReplayStore()
	srv, rec := recordingPollServer(t, batch)
	b := newJournalTestBot(t, srv, &panickingTradeRepo{})
	b.SetReplayGuard(store)
	b.drainTimeout = 5 * time.Second
	b.noticeEvery = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	deadline := time.After(5 * time.Second)
	for {
		if state, ok := store.claimState(1); ok && state == stateDone {
			break
		}
		select {
		case <-deadline:
			t.Fatal("panicked update was left running instead of resolved")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-errCh
	if store.noticeCount() == 0 && !sentContains(rec.sendsSnapshot(), "terputus") {
		t.Fatal("panicked update has neither a pending notice nor a delivered warning")
	}
}

// TestBot_StartFailsClosedWhenCompletionCannotBeRecorded pins that a failed
// completion write is fatal: leaving the row running would block the reset
// barrier and never notify anyone.
func TestBot_StartFailsClosedWhenCompletionCannotBeRecorded(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":84,"username":"a"},"chat":{"id":84,"type":"private"},"text":"#journal\nPair: EURUSD\nType: BUY\nRR: +2"}}
	]}`
	repo := &gatedTradeRepo{started: make(chan struct{}), release: make(chan struct{}), savedCh: make(chan savedTrade, 4)}
	close(repo.release)
	store := newMemReplayStore()

	_, _, errCh, cancel := replayTestBot(t, batch, repo, store)
	t.Cleanup(cancel)

	// Let the handler finish, then break only the completion write.
	select {
	case <-repo.savedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("journal never saved")
	}
	store.failSet = func(key string) error { return errors.New("badger write failed") }

	select {
	case err := <-errCh:
		if err == nil || err == context.Canceled {
			t.Fatalf("Start returned %v, want a fatal completion error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start kept running after a failed completion write")
	}
}

// TestBot_StartJoinsDeferredRetryBeforeShutdown pins that the deferred-retry
// goroutine is joined before the dispatcher is waited on and the store closes.
func TestBot_StartJoinsDeferredRetryBeforeShutdown(t *testing.T) {
	const batch = `{"ok":true,"result":[]}`
	store := newMemReplayStore()
	srv, _ := recordingPollServer(t, batch)
	b := newJournalTestBot(t, srv, &gatedTradeRepo{
		started: make(chan struct{}), release: make(chan struct{}), savedCh: make(chan savedTrade, 4),
	})
	b.SetReplayGuard(store)
	b.noticeEvery = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return (deferred retry goroutine not joined)")
	}
}

// TestUpdateDispatcher_FatalIsBroadcastAndRetained pins the fail-stop signal: it
// must be a broadcast with a retained error. A single-value channel would be
// drained by one observer, leaving the others (and the poll loop) waiting on a
// cancelled context forever.
func TestUpdateDispatcher_FatalIsBroadcastAndRetained(t *testing.T) {
	d := newUpdateDispatcher(context.Background(), 1, func(context.Context, Update) {}, nil)
	want := errors.New("ledger write failed")
	d.fail(want)

	// Two independent observers both see it.
	for i := range 2 {
		select {
		case <-d.Fatal():
		default:
			t.Fatalf("observer %d did not see the fatal broadcast", i+1)
		}
	}
	if got := d.FatalErr(); !errors.Is(got, want) {
		t.Fatalf("FatalErr = %v, want %v", got, want)
	}
	// The error survives repeated reads.
	if got := d.FatalErr(); !errors.Is(got, want) {
		t.Fatalf("FatalErr lost the error on the second read: %v", got)
	}
	// A second failure must not replace the first cause.
	d.fail(errors.New("second failure"))
	if got := d.FatalErr(); !errors.Is(got, want) {
		t.Fatalf("FatalErr = %v, want the first error retained", got)
	}
}

// TestRetryDeferredOnceTerminatesOnBlockedEntries pins that a pass is bounded:
// entries that are re-queued (still blocked) must not be read again in the same
// pass, or the retry loop spins forever and ignores shutdown.
func TestRetryDeferredOnceTerminatesOnBlockedEntries(t *testing.T) {
	store := newMemReplayStore()
	replay := newTestGuard(store)
	d := newUpdateDispatcher(context.Background(), 1, func(context.Context, Update) {}, nil)
	// A reset is pending, so both entries are newer and must stay parked.
	d.addGate(1)

	deferred := make(chan Update, 8)
	deferred <- privateUpdate(1, 5)
	deferred <- privateUpdate(1, 6)

	b := &Bot{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), replay: replay}
	done := make(chan bool, 1)
	go func() { done <- b.retryDeferredOnce(context.Background(), deferred, d, replay) }()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("retryDeferredOnce reported a fatal condition")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retryDeferredOnce did not terminate: it re-read the entries it re-queued")
	}
	if got := len(deferred); got != 2 {
		t.Fatalf("deferred queue length = %d, want both entries parked", got)
	}
}

// TestRetryDeferredOnceStopsOnCancel pins that a pass aborts promptly once the
// context is cancelled.
func TestRetryDeferredOnceStopsOnCancel(t *testing.T) {
	store := newMemReplayStore()
	replay := newTestGuard(store)
	d := newUpdateDispatcher(context.Background(), 1, func(context.Context, Update) {}, nil)
	d.addGate(1)

	deferred := make(chan Update, 8)
	deferred <- privateUpdate(1, 5)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	b := &Bot{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), replay: replay}
	if b.retryDeferredOnce(ctx, deferred, d, replay) {
		t.Fatal("retryDeferredOnce must report shutdown")
	}
}

// TestUpdateDispatcher_BarrierRaisedAtAdmission pins the ordering fix: the
// barrier exists as soon as the reset is enqueued, so a newer update enqueued
// afterwards can never start — and finish — before the reset wipes its work.
func TestUpdateDispatcher_BarrierRaisedAtAdmission(t *testing.T) {
	var (
		mu  sync.Mutex
		ran []int
	)
	d := newUpdateDispatcher(context.Background(), 4, func(_ context.Context, u Update) {
		mu.Lock()
		ran = append(ran, u.UpdateID)
		mu.Unlock()
	}, nil)
	d.resetCheck = func(u Update) bool { return u.UpdateID == 10 }

	// Enqueue the reset first (admission order), then a newer user's update.
	if err := d.Enqueue(context.Background(), resetUpdate(10)); err != nil {
		t.Fatalf("enqueue reset: %v", err)
	}
	if got := d.gateUpdate(); got != 10 {
		t.Fatalf("gate = %d right after admission, want 10", got)
	}
	if err := d.Enqueue(context.Background(), privateUpdate(2, 11)); err != nil {
		t.Fatalf("enqueue newer update: %v", err)
	}
	if !d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not settle")
	}
	mu.Lock()
	for _, id := range ran {
		if id == 11 {
			mu.Unlock()
			t.Fatal("a newer update ran while the reset was pending")
		}
	}
	mu.Unlock()

	// The reset runs once nothing else is in flight, and only then may the
	// newer update proceed.
	if got := d.gateUpdate(); got != 10 {
		t.Fatalf("gate = %d while the reset is unfinished, want 10", got)
	}
	d.clearGateFor(10)
	if err := d.Enqueue(context.Background(), privateUpdate(2, 11)); err != nil {
		t.Fatalf("re-enqueue newer update: %v", err)
	}
	if !d.Wait(2 * time.Second) {
		t.Fatal("dispatcher did not drain")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 2 || ran[0] != 10 || ran[1] != 11 {
		t.Fatalf("ran = %v, want [10 11]", ran)
	}
}

// TestIsResetUpdate pins the safety gate's recognition of reset invocations.
func TestIsResetUpdate(t *testing.T) {
	msg := func(text string) Update {
		return Update{UpdateID: 1, Message: &Message{Text: text, Chat: &Chat{ID: 1, Type: "private"}}}
	}
	// Only a confirmed reset destroys state; the prompt only renders a keyboard,
	// so raising the barrier for it would stall every user for a no-op.
	if isDestructiveReset(msg("/reset")) {
		t.Fatal("the /reset prompt must not hold the barrier")
	}
	if !isDestructiveReset(msg("/reset confirm")) {
		t.Fatal("/reset confirm not recognised")
	}
	if !isDestructiveReset(msg("/RESET CONFIRM")) {
		t.Fatal("the handler routes /RESET CONFIRM, so it must be recognised")
	}
	if !isDestructiveReset(msg("/reset@ArkVaultBot CONFIRM")) {
		t.Fatal("bot-suffixed confirmed reset not recognised")
	}
	if isDestructiveReset(msg("/reset maybe")) {
		t.Fatal("a non-confirming argument must not hold the barrier")
	}
	if isDestructiveReset(msg("/report confirm")) {
		t.Fatal("/report misidentified as reset")
	}
	if !isDestructiveReset(Update{UpdateID: 2, CallbackQuery: &CallbackQuery{Data: "reset:confirm"}}) {
		t.Fatal("reset confirm callback not recognised")
	}
	if isDestructiveReset(Update{UpdateID: 3, CallbackQuery: &CallbackQuery{Data: "reset:cancel"}}) {
		t.Fatal("cancel callback must not hold the barrier")
	}
	if isDestructiveReset(Update{UpdateID: 4, CallbackQuery: &CallbackQuery{Data: "hist:del:x"}}) {
		t.Fatal("unrelated callback misidentified as reset")
	}
}

// TestBot_ResetBarrierRequiresOwner pins that a non-owner's /reset confirm does
// not stall the bot: the handler ignores it, so the barrier must too.
func TestBot_ResetBarrierRequiresOwner(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(NewSender(testBotToken), Services{}, nil, nil, nil, NewRateLimiter(100, time.Minute), logger, 0, 99)
	defer h.Close()
	b := &Bot{handler: h, logger: logger}

	owner := Update{UpdateID: 1, Message: &Message{
		From: &User{ID: 99}, Chat: &Chat{ID: 99, Type: "private"}, Text: "/reset confirm",
	}}
	if !b.resetBarrier(owner) {
		t.Fatal("owner's confirmed reset must hold the barrier")
	}
	stranger := Update{UpdateID: 2, Message: &Message{
		From: &User{ID: 7}, Chat: &Chat{ID: 7, Type: "private"}, Text: "/reset confirm",
	}}
	if b.resetBarrier(stranger) {
		t.Fatal("a non-owner's /reset must not hold the barrier")
	}
}

// TestBot_ResetBarrierMatchesHandlerCommandParsing pins that the barrier uses
// the same command parsing as the handler: case-insensitive and tolerant of the
// "/reset@BotName" suffix Telegram adds in group chats. A mismatch either lets a
// real wipe run without the barrier or stalls every user for a no-op.
func TestBot_ResetBarrierMatchesHandlerCommandParsing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(NewSender(testBotToken), Services{}, nil, nil, nil, NewRateLimiter(100, time.Minute), logger, 0, 99)
	defer h.Close()
	b := &Bot{handler: h, logger: logger}

	ownerMsg := func(text string) Update {
		return Update{UpdateID: 1, Message: &Message{
			From: &User{ID: 99}, Chat: &Chat{ID: 99, Type: "private"}, Text: text,
		}}
	}
	for _, text := range []string{
		"/reset confirm",
		"/RESET CONFIRM",
		"/reset@ArkVaultBot confirm",
		"/Reset@ArkVaultBot CONFIRM",
	} {
		// The handler only wipes when its own router sees /reset, so the barrier
		// must agree on every form the router accepts.
		if name, ok := parseCommandName(text); !ok || name != "reset" {
			t.Fatalf("%q is not routed to /reset by the handler parser", text)
		}
		if !b.resetBarrier(ownerMsg(text)) {
			t.Fatalf("%q must hold the reset barrier", text)
		}
	}
	for _, text := range []string{
		"/reset",                     // prompt only renders the keyboard
		"/reset cancel",              // no-op
		"/reset maybe",               // not a confirmation
		"/report confirm",            // different command
		"/resetconfirmation confirm", // not the reset command
	} {
		if b.resetBarrier(ownerMsg(text)) {
			t.Fatalf("%q must not hold the reset barrier", text)
		}
	}
	if !b.resetBarrier(Update{UpdateID: 2, CallbackQuery: &CallbackQuery{From: &User{ID: 99}, Data: "reset:confirm"}}) {
		t.Fatal("the confirm button must hold the barrier")
	}
	if b.resetBarrier(Update{UpdateID: 3, CallbackQuery: &CallbackQuery{From: &User{ID: 99}, Data: "reset:cancel"}}) {
		t.Fatal("the cancel button must not hold the barrier")
	}
	stranger := Update{UpdateID: 4, Message: &Message{
		From: &User{ID: 7}, Chat: &Chat{ID: 7, Type: "private"}, Text: "/RESET@ArkVaultBot CONFIRM",
	}}
	if b.resetBarrier(stranger) {
		t.Fatal("a non-owner's confirmed reset must not stall the bot")
	}
}

// TestBot_ResetBarrierMatchesHandlerForPhotoCaption pins the media case: a photo
// whose caption is "/reset confirm" IS a command to the handler, because
// HandleUpdate falls back to Caption when Text is empty, so it really runs the
// wipe. The barrier must recognise the same update, or the owner's wipe executes
// with no barrier and updates from other users can start and finish around it.
func TestBot_ResetBarrierMatchesHandlerForPhotoCaption(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, sent := capturingSendServer(t)
	sender := NewSender(testBotToken)
	sender.baseAPI = srv.URL + "/bot"
	h := NewHandler(sender, Services{}, nil, nil, nil, NewRateLimiter(100, time.Minute), logger, 0, 99)
	defer h.Close()
	b := &Bot{handler: h, logger: logger}

	photoReset := func(caption string, from int64) Update {
		return Update{UpdateID: 7, Message: &Message{
			MessageID: 7,
			From:      &User{ID: from, Username: "owner"},
			Chat:      &Chat{ID: from, Type: "private"},
			Photo:     []PhotoSize{{FileID: "photo-1", Width: 90, Height: 90}},
			Caption:   caption,
		}}
	}

	// First prove the handler really wipes for a caption-only reset: this is what
	// makes the barrier mandatory, rather than a nicety.
	h.HandleUpdate(context.Background(), photoReset("/reset confirm", 99))
	if !sentContains(sent(), "Reset selesai") {
		t.Fatalf("handler did not run the reset for a photo caption, sent = %v", sent())
	}
	if !b.resetBarrier(photoReset("/reset confirm", 99)) {
		t.Fatal("owner photo caption /reset confirm must hold the reset barrier")
	}
	// Case and the group-chat bot suffix are accepted by the same parser.
	if !b.resetBarrier(photoReset("/RESET@ArkVaultBot CONFIRM", 99)) {
		t.Fatal("bot-suffixed uppercase caption reset must hold the barrier")
	}
	// The prompt caption only renders the keyboard, and a non-owner is ignored.
	if b.resetBarrier(photoReset("/reset", 99)) {
		t.Fatal("the prompt caption must not hold the barrier")
	}
	if b.resetBarrier(photoReset("/reset confirm", 7)) {
		t.Fatal("a non-owner's caption reset must not hold the barrier")
	}
}

// TestLedger_OlderPendingCountsOnlyUnfinishedRows pins the barrier query.
func TestLedger_OlderPendingCountsOnlyUnfinishedRows(t *testing.T) {
	store := newMemReplayStore()
	g := newTestGuard(store)
	store.seedRecord(updateRecord{UpdateID: 1, State: stateAdmitted})
	store.seedRecord(updateRecord{UpdateID: 2, State: stateRunning})
	store.seedRecord(updateRecord{UpdateID: 3, State: stateDone})
	store.seedRecord(updateRecord{UpdateID: 4, State: stateVerify})

	older, err := g.olderPending(10)
	if err != nil {
		t.Fatalf("olderPending: %v", err)
	}
	if older != 3 {
		t.Fatalf("older pending = %d, want 3 (admitted, running, verify)", older)
	}
	older, err = g.olderPending(2)
	if err != nil {
		t.Fatalf("olderPending: %v", err)
	}
	if older != 1 {
		t.Fatalf("older pending = %d, want 1", older)
	}
}

// TestBot_StartWaitsForStuckHandlerBeforeReturn pins that Start never returns
// while a worker can still reach the store, even when the handler ignores
// cancellation (the container stop grace is the outer bound).
func TestBot_StartWaitsForStuckHandlerBeforeReturn(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":78,"username":"c"},"chat":{"id":78,"type":"private"},"text":"#journal\nPair: GBPUSD\nType: BUY\nRR: +1"}}
	]}`
	repo := &stubbornTradeRepo{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}
	store := newMemReplayStore()

	_, _, errCh, cancel := replayTestBot(t, batch, repo, store)

	select {
	case <-repo.started:
	case <-time.After(5 * time.Second):
		t.Fatal("journal never started")
	}
	cancel()

	select {
	case err := <-errCh:
		t.Fatalf("Start returned (%v) while a handler could still touch the store", err)
	case <-time.After(300 * time.Millisecond):
	}
	// Still inside the handler: the row must not be terminal, or a crash at
	// this instant would be indistinguishable from completed work.
	if state, ok := store.claimState(1); !ok || state != stateRunning {
		t.Fatalf("row while the handler is stuck = %q (found %v), want running", state, ok)
	}

	close(repo.release)
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the stuck handler exited")
	}
	// The handler finished within the drain budget, so its effects are known to
	// be complete and the row must be closed rather than replayed next start.
	if state, ok := store.claimState(1); !ok || state != stateDone {
		t.Fatalf("finished row = %q (found %v), want done", state, ok)
	}
}

// TestHandler_GuidedFlowDoesNotSwallowJournal pins the regression where a
// pending guided flow consumed the next #journal message as the answer to the
// current step (e.g. storing the whole journal text as the symbol) instead of
// recording the trade.
func TestHandler_GuidedFlowDoesNotSwallowJournal(t *testing.T) {
	var (
		mu   sync.Mutex
		sent []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		if text, ok := payload["text"].(string); ok {
			mu.Lock()
			sent = append(sent, text)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":13}}`))
	}))
	defer srv.Close()

	sender := NewSender(testBotToken)
	sender.baseAPI = srv.URL + "/bot"
	tradeRepo := newMockFeatureTradeRepo()
	memberRepo := &mockResettableMemberRepo{}
	journalSvc := service.NewJournalService(tradeRepo, memberRepo, nil)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(sender, Services{Journal: journalSvc}, nil, tradeRepo, memberRepo, NewRateLimiter(100, time.Minute), logger, 0, 0)
	defer h.Close()

	ctx := context.Background()
	uid := int64(91)
	session := h.guided.Start(uid, uid, 0)
	session.Step = StepSymbol

	msg := func(text string) Update {
		return Update{Message: &Message{
			From: &User{ID: uid, Username: "trader"},
			Chat: &Chat{ID: uid, Type: "private"},
			Text: text,
		}}
	}

	h.HandleUpdate(ctx, msg("#journal\nPair: EURUSD\nType: BUY\nRR: +2"))

	saved := tradeRepo.trades[uid]
	if len(saved) != 1 || saved[0].Symbol != "EURUSD" || saved[0].Status != domain.StatusWin {
		t.Fatalf("#journal was not recorded independently of the guided flow, got %+v", saved)
	}
	if h.guided.Get(uid) == nil {
		t.Fatal("guided session should survive a #journal message")
	}
	if got := session.Trade.Symbol; got != "" {
		t.Fatalf("journal text leaked into the guided symbol step: %q", got)
	}

	mu.Lock()
	last := ""
	if len(sent) > 0 {
		last = sent[len(sent)-1]
	}
	mu.Unlock()
	if !strings.Contains(last, "Trade berhasil dicatat") {
		t.Fatalf("#journal should be confirmed to the user, got %q", last)
	}

	// Free text still answers the pending step.
	h.HandleUpdate(ctx, msg("xauusd"))
	if got := session.Trade.Symbol; got != "XAUUSD" {
		t.Fatalf("guided symbol step stopped consuming free text, got %q", got)
	}
}
