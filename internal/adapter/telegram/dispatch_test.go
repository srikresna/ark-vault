package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/internal/service"
)

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
// never started after cancellation and is reported as unfinished, so Telegram
// redelivers it. Only the handler that was already running executes.
func TestUpdateDispatcher_ShutdownFlushesQueuedUpdates(t *testing.T) {
	workCtx, cancelWork := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	var (
		mu    sync.Mutex
		ran   []int
		acked []bool
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
	d.setOnDone(func(u Update, ackable bool) {
		mu.Lock()
		acked = append(acked, ackable)
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
	if len(acked) != 3 {
		t.Fatalf("expected one completion report per accepted update, got %d", len(acked))
	}
	for i, a := range acked {
		if a {
			t.Fatalf("update %d was reported ackable during shutdown", i)
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
// panic: the panicking update is reported, its execution slot is freed, the
// same user's remaining updates still run in order, and every update is
// acknowledged so a poison update cannot wedge the Telegram offset.
func TestUpdateDispatcher_PanicDoesNotStrandQueue(t *testing.T) {
	var (
		mu     sync.Mutex
		got    []int
		panics []int
		acked  []bool
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
	d.setOnDone(func(_ Update, ackable bool) {
		mu.Lock()
		acked = append(acked, ackable)
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
	if len(acked) != 3 {
		t.Fatalf("expected one completion report per update, got %v", acked)
	}
	for i, a := range acked {
		if !a {
			t.Fatalf("update %d was left unacknowledged; a poison update would block the offset", i+1)
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

// TestUpdateAcker_AcknowledgesOnlyContiguousProcessedPrefix pins the
// acknowledgement rule directly: the offset may only cover updates that
// finished, and never skip past one that did not.
func TestUpdateAcker_AcknowledgesOnlyContiguousProcessedPrefix(t *testing.T) {
	a := newUpdateAcker(10)
	for _, id := range []int{11, 12, 13} {
		a.accept(id)
	}
	a.seal()

	a.complete(Update{UpdateID: 12}, true) // out of order: 11 is still pending
	if got := a.offset(); got != 10 {
		t.Fatalf("offset advanced past an unfinished update: %d", got)
	}

	a.complete(Update{UpdateID: 11}, true)
	if got := a.offset(); got != 13 {
		t.Fatalf("offset = %d, want 13 (11 and 12 are contiguous and processed)", got)
	}
	select {
	case <-a.done():
		t.Fatal("done closed before every update reported")
	default:
	}

	a.complete(Update{UpdateID: 13}, false) // aborted during shutdown
	if got := a.offset(); got != 13 {
		t.Fatalf("aborted update was acknowledged: offset = %d, want 13", got)
	}
	select {
	case <-a.done():
	case <-time.After(time.Second):
		t.Fatal("done was not closed after every update reported")
	}
}

// TestUpdateAcker_FirstCompletionBeforeLaterAdmission pins the early-done
// race: a batch's first update can finish before the remaining ids are
// admitted. An unsealed acker would close done at that point, letting Start
// poll again and re-deliver the rest of the batch (duplicate journals). With
// admission sealed after the whole batch is registered, done stays open until
// every registered update has reported.
func TestUpdateAcker_FirstCompletionBeforeLaterAdmission(t *testing.T) {
	a := newUpdateAcker(0)
	a.accept(1)

	a.complete(Update{UpdateID: 1}, true) // first worker wins the race
	select {
	case <-a.done():
		t.Fatal("done closed before the rest of the batch was admitted")
	default:
	}

	a.accept(2)
	a.accept(3)
	a.seal()
	select {
	case <-a.done():
		t.Fatal("done closed while admitted updates were still outstanding")
	default:
	}
	if got := a.offset(); got != 2 {
		t.Fatalf("offset = %d, want 2 (only update 1 is processed)", got)
	}

	a.complete(Update{UpdateID: 3}, true) // out of order, 2 still pending
	select {
	case <-a.done():
		t.Fatal("done closed with a gap at update 2")
	default:
	}

	a.complete(Update{UpdateID: 2}, true)
	select {
	case <-a.done():
	case <-time.After(time.Second):
		t.Fatal("done did not close after the sealed batch drained")
	}
	if got := a.offset(); got != 4 {
		t.Fatalf("offset = %d, want 4", got)
	}
}

// TestUpdateAcker_IgnoresUnmatchedCompletions pins that a stale or duplicate
// report cannot consume another update's outstanding count and close done
// early.
func TestUpdateAcker_IgnoresUnmatchedCompletions(t *testing.T) {
	a := newUpdateAcker(0)
	a.accept(1)
	a.accept(2)
	a.seal()

	a.complete(Update{UpdateID: 99}, true) // never accepted
	a.complete(Update{UpdateID: 1}, true)
	a.complete(Update{UpdateID: 1}, true) // duplicate report
	select {
	case <-a.done():
		t.Fatal("an unmatched report closed done while update 2 was outstanding")
	default:
	}

	a.complete(Update{UpdateID: 2}, true)
	select {
	case <-a.done():
	case <-time.After(time.Second):
		t.Fatal("done did not close after the real batch drained")
	}
}

// TestUpdateAcker_GapKeepsLaterSuccessesUnacknowledged pins the one duplicate
// the offset cannot avoid: when update N is aborted and N+1 already succeeded,
// the watermark can only advance to N. N+1 is therefore redelivered even though
// its work completed, which is why Start avoids cancelling accepted updates and
// why a durable update-id idempotency key is still required for exactly-once
// journaling.
func TestUpdateAcker_GapKeepsLaterSuccessesUnacknowledged(t *testing.T) {
	a := newUpdateAcker(0)
	for _, id := range []int{1, 2, 3} {
		a.accept(id)
	}
	a.seal()

	a.complete(Update{UpdateID: 2}, true) // succeeded out of order
	a.complete(Update{UpdateID: 3}, true)
	a.complete(Update{UpdateID: 1}, false) // aborted by shutdown
	if got := a.offset(); got != 0 {
		t.Fatalf("offset = %d, want 0: the aborted update blocks the watermark", got)
	}
	select {
	case <-a.done():
	case <-time.After(time.Second):
		t.Fatal("done did not close after the batch reported")
	}
}

// TestUpdateKey pins the per-user ordering scope.
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

// pollRecorder captures what the fake Bot API observed: every getUpdates
// offset in order, and the offsets of the immediate (timeout=0) calls Start
// makes to confirm processed updates during shutdown.
type pollRecorder struct {
	mu       sync.Mutex
	offsets  []string
	confirms []string
	acked    chan struct{}
	once     sync.Once
}

func (p *pollRecorder) offsetsSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.offsets...)
}

func (p *pollRecorder) confirmsSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.confirms...)
}

// recordingPollServer serves batch on the first getUpdates call, then holds
// every long poll open until the request is cancelled, mirroring the real
// endpoint. Immediate calls (timeout=0) — the shutdown confirmation — are
// recorded and answered at once. rec.acked closes on the first long poll that
// acknowledges anything.
func recordingPollServer(t *testing.T, batch string) (*httptest.Server, *pollRecorder) {
	t.Helper()
	rec := &pollRecorder{acked: make(chan struct{})}
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/getUpdates") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
			return
		}
		query := r.URL.Query()
		offset := query.Get("offset")
		confirm := query.Get("timeout") == "0"
		rec.mu.Lock()
		rec.offsets = append(rec.offsets, offset)
		if confirm {
			rec.confirms = append(rec.confirms, offset)
		}
		rec.mu.Unlock()

		if atomic.AddInt32(&polls, 1) == 1 {
			_, _ = w.Write([]byte(batch))
			return
		}
		if offset != "0" {
			rec.once.Do(func() { close(rec.acked) })
		}
		if confirm {
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func newJournalTestBot(t *testing.T, srv *httptest.Server, tradeRepo service.TradeRepository) *Bot {
	t.Helper()
	memberRepo := &mockResettableMemberRepo{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sender := NewSender("test-token")
	sender.baseAPI = srv.URL + "/bot"
	h := NewHandler(sender, Services{Journal: service.NewJournalService(tradeRepo, memberRepo, nil)},
		nil, tradeRepo, memberRepo, NewRateLimiter(100, time.Minute), logger, 0, 0)
	t.Cleanup(h.Close)

	b := NewBot("test-token", h, logger)
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

// partialTradeRepo saves the first journal immediately and blocks the second
// one until the work context is cancelled, modelling a batch whose head
// completes while its tail is still in flight when the drain budget expires.
type partialTradeRepo struct {
	mu            sync.Mutex
	calls         int
	saved         []savedTrade
	savedCh       chan savedTrade
	secondStarted chan struct{}
	secondStopped chan struct{}
}

func (r *partialTradeRepo) SaveTrade(ctx context.Context, memberID int64, trade *domain.Trade) error {
	r.mu.Lock()
	r.calls++
	n := r.calls
	if n == 1 {
		r.saved = append(r.saved, savedTrade{memberID: memberID, symbol: trade.Symbol})
	}
	r.mu.Unlock()

	if n == 1 {
		r.savedCh <- savedTrade{memberID: memberID, symbol: trade.Symbol}
		return nil
	}
	close(r.secondStarted)
	<-ctx.Done()
	close(r.secondStopped)
	return ctx.Err()
}

func (r *partialTradeRepo) GetTrades(context.Context, int64) ([]domain.Trade, error) {
	return nil, nil
}

func (r *partialTradeRepo) UpdateTrade(context.Context, int64, *domain.Trade) error { return nil }

func (r *partialTradeRepo) DeleteTrade(context.Context, int64, string) error { return nil }

func (r *partialTradeRepo) savedSymbols() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.saved))
	for _, s := range r.saved {
		out = append(out, s.symbol)
	}
	return out
}

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

	sender := NewSender("test-token")
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

// TestBot_StartAcknowledgesOnlyProcessedUpdates pins that the Telegram offset
// (the only acknowledgement the API offers) advances only after the batch's
// updates actually finished: while the first journal is still blocked in
// Notion nothing is acknowledged, and the follow-up poll carries the offset
// that covers both updates.
func TestBot_StartAcknowledgesOnlyProcessedUpdates(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":31,"username":"a"},"chat":{"id":31,"type":"private"},"text":"#journal\nPair: EURUSD\nType: BUY\nRR: +2"}},
		{"update_id":2,"message":{"message_id":2,"from":{"id":31,"username":"a"},"chat":{"id":31,"type":"private"},"text":"#journal\nPair: XAUUSD\nType: BUY\nRR: +1"}}
	]}`
	srv, rec := recordingPollServer(t, batch)
	repo := &gatedTradeRepo{
		started: make(chan struct{}),
		release: make(chan struct{}),
		savedCh: make(chan savedTrade, 4),
	}
	b := newJournalTestBot(t, srv, repo)
	b.drainTimeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	select {
	case <-repo.started:
	case <-time.After(5 * time.Second):
		t.Fatal("journal update never reached persistence")
	}
	if got := rec.offsetsSnapshot(); len(got) != 1 || got[0] != "0" {
		t.Fatalf("work in flight was acknowledged early: offsets = %v", got)
	}

	close(repo.release)
	for i := range 2 {
		select {
		case <-repo.savedCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 2 journals were saved", i)
		}
	}
	select {
	case <-rec.acked:
	case <-time.After(5 * time.Second):
		t.Fatal("processed batch was never acknowledged to Telegram")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancellation")
	}
	if got := rec.offsetsSnapshot(); len(got) < 2 || got[1] != "3" {
		t.Fatalf("acknowledged offset = %v, want a poll with offset 3", got)
	}
	if got := rec.confirmsSnapshot(); len(got) != 1 || got[0] != "3" {
		t.Fatalf("shutdown confirmation = %v, want one call with offset 3", got)
	}
}

// TestBot_StartConfirmsProcessedBatchOnShutdown pins the duplicate-journal
// fix: Telegram forgets an update only when getUpdates is called with a higher
// offset. A batch that finished just before shutdown would otherwise never be
// confirmed and would be redelivered (journaled twice) on the next start, so
// Start sends one final immediate getUpdates carrying the processed offset.
func TestBot_StartConfirmsProcessedBatchOnShutdown(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":41,"username":"a"},"chat":{"id":41,"type":"private"},"text":"#journal\nPair: EURUSD\nType: BUY\nRR: +2"}}
	]}`
	srv, rec := recordingPollServer(t, batch)
	repo := &gatedTradeRepo{
		started: make(chan struct{}),
		release: make(chan struct{}),
		savedCh: make(chan savedTrade, 4),
	}
	close(repo.release) // nothing to gate: the journal saves immediately
	b := newJournalTestBot(t, srv, repo)
	b.drainTimeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	select {
	case <-repo.savedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("journal was never saved")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancellation")
	}

	confirms := rec.confirmsSnapshot()
	if len(confirms) != 1 || confirms[0] != "2" {
		t.Fatalf("shutdown confirmation = %v, want exactly one call with offset 2", confirms)
	}
}

// TestBot_StartCompletesSlowBatchInsteadOfCancelling pins the duplicate fix at
// the policy level: a batch that outlives the soft drain budget must still be
// allowed to finish. Cancelling it would leave a gap at the aborted update, and
// since Telegram's offset is a watermark every later update in the batch would
// be redelivered — journaling trades that were already written a second time.
func TestBot_StartCompletesSlowBatchInsteadOfCancelling(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":51,"username":"a"},"chat":{"id":51,"type":"private"},"text":"#journal\nPair: EURUSD\nType: BUY\nRR: +2"}},
		{"update_id":2,"message":{"message_id":2,"from":{"id":51,"username":"a"},"chat":{"id":51,"type":"private"},"text":"#journal\nPair: XAUUSD\nType: BUY\nRR: +1"}}
	]}`
	srv, rec := recordingPollServer(t, batch)
	// Same user: the two journals are serialised, each a 40ms round trip, so
	// the batch takes ~80ms — well past the 10ms soft budget.
	tradeRepo := newOrderingTradeRepo()
	b := newJournalTestBot(t, srv, tradeRepo)
	b.drainTimeout = 10 * time.Millisecond
	b.hardTimeout = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	select {
	case <-tradeRepo.saves: // first journal done, second is queued
	case <-time.After(5 * time.Second):
		t.Fatal("first journal was never saved")
	}
	cancel()

	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return")
	}

	saved, _ := tradeRepo.snapshot()
	if len(saved) != 2 || saved[0].symbol != "EURUSD" || saved[1].symbol != "XAUUSD" {
		t.Fatalf("slow batch was cut short: %+v", saved)
	}
	// Both updates were acknowledged, so neither is redelivered.
	if got := rec.confirmsSnapshot(); len(got) != 1 || got[0] != "3" {
		t.Fatalf("shutdown confirmation = %v, want one call with offset 3", got)
	}
}

// TestBot_StartSkipsConfirmationWhenNothingProcessed pins that an idle bot does
// not send a pointless confirmation call for offset 0.
func TestBot_StartSkipsConfirmationWhenNothingProcessed(t *testing.T) {
	srv, rec := recordingPollServer(t, `{"ok":true,"result":[]}`)
	repo := &gatedTradeRepo{started: make(chan struct{}), release: make(chan struct{})}
	close(repo.release)
	b := newJournalTestBot(t, srv, repo)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	// Wait until Start is parked in its long poll, then shut down.
	deadline := time.After(5 * time.Second)
	for len(rec.offsetsSnapshot()) < 2 {
		select {
		case <-deadline:
			t.Fatal("Start never issued a second poll")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancellation")
	}
	if got := rec.confirmsSnapshot(); len(got) != 0 {
		t.Fatalf("confirmed with nothing processed: %v", got)
	}
}

// TestBot_StartConfirmsOnlyProcessedPrefixOnShutdown pins the two halves of
// the acknowledgement rule at once: the journal that finished is confirmed (so
// it is not journaled again after a restart), while the one aborted by the
// drain deadline stays above the confirmed offset (so Telegram redelivers it).
func TestBot_StartConfirmsOnlyProcessedPrefixOnShutdown(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":32,"username":"a"},"chat":{"id":32,"type":"private"},"text":"#journal\nPair: EURUSD\nType: BUY\nRR: +2"}},
		{"update_id":2,"message":{"message_id":2,"from":{"id":32,"username":"a"},"chat":{"id":32,"type":"private"},"text":"#journal\nPair: XAUUSD\nType: BUY\nRR: +1"}}
	]}`
	srv, rec := recordingPollServer(t, batch)
	repo := &partialTradeRepo{
		savedCh:       make(chan savedTrade, 4),
		secondStarted: make(chan struct{}),
		secondStopped: make(chan struct{}),
	}
	b := newJournalTestBot(t, srv, repo)
	b.drainTimeout = 10 * time.Millisecond
	b.hardTimeout = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	select {
	case <-repo.savedCh: // first journal persisted, second is in flight
	case <-time.After(5 * time.Second):
		t.Fatal("first journal was never saved")
	}
	select {
	case <-repo.secondStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("second journal never started")
	}
	cancel()

	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancellation")
	}

	if got := repo.savedSymbols(); len(got) != 1 || got[0] != "EURUSD" {
		t.Fatalf("saved %v, want only the finished journal", got)
	}
	confirms := rec.confirmsSnapshot()
	if len(confirms) != 1 || confirms[0] != "2" {
		t.Fatalf("shutdown confirmation = %v, want exactly one call with offset 2", confirms)
	}
}

// TestBot_StartStopsPromptlyWhenDrainTimesOut pins the bounded drain: when a
// handler overruns drainTimeout, Start cancels it, waits for it to unwind, and
// only then returns, so main can close BadgerDB without a live worker touching
// it. The aborted update is never acknowledged, so Telegram redelivers it.
func TestBot_StartStopsPromptlyWhenDrainTimesOut(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":33,"username":"b"},"chat":{"id":33,"type":"private"},"text":"#journal\nPair: XAUUSD\nType: SELL\nRR: -1"}}
	]}`
	srv, rec := recordingPollServer(t, batch)
	repo := &ctxBlockingTradeRepo{started: make(chan struct{}), stopped: make(chan struct{})}
	b := newJournalTestBot(t, srv, repo)
	b.drainTimeout = 10 * time.Millisecond
	b.hardTimeout = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	select {
	case <-repo.started:
	case <-time.After(5 * time.Second):
		t.Fatal("journal update never reached persistence")
	}
	cancel()

	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("Start returned %v, want context.Canceled", err)
		}
		// Start must not return while a handler can still write.
		select {
		case <-repo.stopped:
		default:
			t.Fatal("Start returned while a cancelled handler was still running")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start ignored the drain timeout")
	}

	for _, offset := range rec.offsetsSnapshot() {
		if offset != "0" {
			t.Fatalf("aborted update was acknowledged (offsets = %v)", rec.offsetsSnapshot())
		}
	}
	if got := rec.confirmsSnapshot(); len(got) != 0 {
		t.Fatalf("aborted update was confirmed: %v", got)
	}
}

// TestBot_StartWaitsForStuckHandlerBeforeReturn pins that Start never returns
// while a worker can still reach the store: a handler that ignores
// cancellation blocks shutdown until it exits (the container stop grace is the
// outer bound and SIGKILLs the process without the store ever being closed
// underneath it).
func TestBot_StartWaitsForStuckHandlerBeforeReturn(t *testing.T) {
	const batch = `{"ok":true,"result":[
		{"update_id":1,"message":{"message_id":1,"from":{"id":34,"username":"c"},"chat":{"id":34,"type":"private"},"text":"#journal\nPair: GBPUSD\nType: BUY\nRR: +1"}}
	]}`
	srv, rec := recordingPollServer(t, batch)
	repo := &stubbornTradeRepo{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}
	b := newJournalTestBot(t, srv, repo)
	b.drainTimeout = 10 * time.Millisecond
	b.hardTimeout = 30 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- b.Start(ctx) }()

	select {
	case <-repo.started:
	case <-time.After(5 * time.Second):
		t.Fatal("journal update never reached persistence")
	}
	cancel()

	select {
	case err := <-errCh:
		t.Fatalf("Start returned (%v) while a handler could still touch the store", err)
	case <-time.After(300 * time.Millisecond):
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
	// The handler was aborted, so nothing may be confirmed.
	if got := rec.confirmsSnapshot(); len(got) != 0 {
		t.Fatalf("aborted work was confirmed: %v", got)
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

	sender := NewSender("test-token")
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
