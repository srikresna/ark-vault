package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
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
	// queued) so a burst cannot exhaust memory. It must exceed Telegram's
	// batch size (100) or a single batch would block the poll loop on
	// backpressure.
	maxPendingUpdates = 256
	// shutdownDrainTimeout is when Start warns that accepted updates are still
	// running. It deliberately does NOT cancel them: cancelling one update
	// makes it unacknowledgeable, and because Telegram's offset is a watermark,
	// every later update in the batch — including ones that already finished
	// their Notion write — is then redelivered and journaled a second time.
	shutdownDrainTimeout = 8 * time.Second
	// shutdownHardTimeout is the last resort: accepted updates that are still
	// running are cancelled here so shutdown stays bounded. Together with
	// shutdownConfirmTimeout it stays inside the container stop grace period
	// (docker-compose.yml sets 30s), which SIGKILLs the process and therefore
	// never lets main close BadgerDB underneath a live worker.
	shutdownHardTimeout = 20 * time.Second
	// shutdownConfirmTimeout bounds the final getUpdates that confirms the
	// processed prefix.
	shutdownConfirmTimeout = 3 * time.Second
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
	hardTimeout  time.Duration // overrides shutdownHardTimeout in tests
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
		hardTimeout:  shutdownHardTimeout,
	}
}

// Start begins the long-polling loop. Blocks until ctx is cancelled.
func (b *Bot) Start(ctx context.Context) error {
	b.logger.Info("starting telegram bot polling")
	// Cancelling ctx means "stop fetching", not "abandon accepted work":
	// every update already admitted to the dispatcher must finish with a live
	// context, otherwise a queued #journal fails its Notion write after the
	// update was already accepted.
	// WithoutCancel keeps ctx values but drops its cancellation; the shutdown
	// path below cancels the remaining handlers only at the hard deadline.
	workCtx, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()
	dispatcher := newUpdateDispatcher(workCtx, maxConcurrentUpdates, b.handler.HandleUpdate, b.logUpdatePanic)
	stallWarning := b.drainTimeout
	if stallWarning <= 0 {
		stallWarning = shutdownDrainTimeout
	}
	hardTimeout := b.hardTimeout
	if hardTimeout <= 0 {
		hardTimeout = shutdownHardTimeout
	}
	if hardTimeout < stallWarning {
		hardTimeout = stallWarning
	}
	// acked is the offset already committed by a fully reported batch; active
	// is the batch currently in flight. Both are only touched by this
	// goroutine, and acker.offset() is mutex-guarded.
	acked := b.offset
	var active *updateAcker

	defer func() {
		// Let accepted updates finish with a live context. Cancelling early is
		// not free: an aborted update cannot be acknowledged, and the offset is
		// a watermark, so every later update in the batch would be redelivered
		// — duplicating the Notion write of any that already completed. So the
		// soft budget only warns; cancellation is the last resort at the hard
		// deadline.
		warned := time.AfterFunc(stallWarning, func() {
			b.logger.Warn("accepted updates still running; waiting for them so their work can be acknowledged",
				"drain_timeout", stallWarning)
		})
		finished := dispatcher.Wait(hardTimeout)
		warned.Stop()
		if !finished {
			cancelWork()
			b.logger.Warn("shutdown deadline reached; cancelling remaining updates", "hard_timeout", hardTimeout)
			// Never return while a worker can still reach the store: main closes
			// BadgerDB as soon as Start returns. A handler that ignores
			// cancellation is bounded from outside by the container stop grace,
			// which SIGKILLs the process before the store is closed.
			dispatcher.WaitAll()
		}
		// Confirm the processed prefix. Telegram only forgets an update when
		// getUpdates is called with a higher offset, so without this final
		// call a batch that finished just before shutdown would be redelivered
		// and its trades journaled twice. Workers are done at this point, so
		// the acker's offset is final.
		if active != nil {
			if got := active.offset(); got > acked {
				acked = got
			}
		}
		b.confirmOffset(ctx, acked)
	}()

	for {
		if ctx.Err() != nil {
			b.logger.Info("bot polling stopped")
			return ctx.Err()
		}
		updates, err := b.getUpdates(ctx)
		if err != nil {
			if ctx.Err() != nil {
				b.logger.Info("bot polling stopped")
				return ctx.Err()
			}
			b.logger.Error("get updates failed", "error", err)
			select {
			case <-ctx.Done():
				b.logger.Info("bot polling stopped")
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if len(updates) == 0 {
			continue
		}

		// The offset is Telegram's only acknowledgement: passing it on the
		// next getUpdates deletes everything below it. It is therefore
		// advanced only across updates that actually finished, and only after
		// the whole batch has been admitted. A crash or a drain timeout leaves
		// the tail unacknowledged, so Telegram redelivers it on the next start
		// instead of losing it.
		//
		// The next poll waits for this batch to finish: polling earlier with
		// the old offset would re-deliver the same updates and enqueue them
		// twice. Fetching therefore pauses while a slow handler runs, which is
		// the price of never acknowledging work that has not been processed;
		// Telegram keeps the backlog for 24h.
		//
		// Admission order matters: every id is registered before the first
		// worker can start, and the batch is sealed before any Enqueue. An
		// unsealed acker whose first update finished quickly would report the
		// batch complete while the rest were still being enqueued, letting the
		// poll loop advance and re-deliver them.
		acker := newUpdateAcker(b.offset)
		for _, u := range updates {
			acker.accept(u.UpdateID)
		}
		acker.seal()
		active = acker
		// Safe to install now: the previous batch's acker only reports done
		// once sealed and fully drained, and Start waits for that before
		// reaching this point, so no earlier worker can still call the hook.
		dispatcher.setOnDone(acker.complete)
		for _, u := range updates {
			if err := dispatcher.Enqueue(ctx, u); err != nil {
				// Only ctx cancellation aborts enqueueing. Nothing is
				// acknowledged here, so the unaccepted update and everything
				// after it is redelivered on the next start.
				b.logger.Info("bot polling stopped")
				return err
			}
		}
		select {
		case <-acker.done():
			b.offset = acker.offset()
			acked = b.offset
		case <-ctx.Done():
			b.logger.Info("bot polling stopped")
			return ctx.Err()
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
	wg      sync.WaitGroup
	mu      sync.Mutex
	queues  map[int64]*updateQueue
	hookMu  sync.Mutex
	onDone  func(Update, bool)
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
		queues:  make(map[int64]*updateQueue),
	}
}

// setOnDone installs the completion hook called once per accepted update. The
// hook reports whether the handler ran to completion (true) or was skipped or
// aborted during shutdown (false); only completed work may be acknowledged.
func (d *updateDispatcher) setOnDone(fn func(Update, bool)) {
	d.hookMu.Lock()
	d.onDone = fn
	d.hookMu.Unlock()
}

func (d *updateDispatcher) notifyDone(u Update, ackable bool) {
	d.hookMu.Lock()
	fn := d.onDone
	d.hookMu.Unlock()
	if fn != nil {
		fn(u, ackable)
	}
}

// Enqueue accepts an update for processing in arrival order. It blocks while
// maxPendingUpdates updates are already accepted, so a burst cannot exhaust
// memory. It returns ctx.Err() when the context ends first.
//
// ctx gates admission only. Handlers run under the dispatcher's workCtx, so
// cancelling the polling context stops new updates without aborting the ones
// already accepted.
func (d *updateDispatcher) Enqueue(ctx context.Context, u Update) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case d.pending <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}

	key, keyed := updateKey(u)
	if !keyed {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
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
		d.wg.Add(1)
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
	defer d.wg.Done()
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
// updates are reported as unfinished (so Telegram redelivers them) and their
// pending budget is released instead of running against a cancelled context.
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
		d.finish(rest[i], false)
	}
}

// execute runs one update under the execution bound. It reports false when
// shutdown was requested before the handler could start, in which case the
// update is reported as unfinished and never touches the store.
func (d *updateDispatcher) execute(u Update) bool {
	select {
	case d.exec <- struct{}{}:
	case <-d.workCtx.Done():
		d.finish(u, false)
		return false
	}
	// Cancellation may have landed between admission and the slot; do not
	// start work that would immediately abort.
	if d.workCtx.Err() != nil {
		<-d.exec
		d.finish(u, false)
		return false
	}
	d.run(d.workCtx, u)
	return true
}

// run executes one update and always frees its execution slot and pending
// budget, even when the handler panics: a panic must never strand the rest of
// that user's queue or permanently shrink the concurrency budget.
func (d *updateDispatcher) run(ctx context.Context, u Update) {
	defer func() {
		if r := recover(); r != nil && d.onPanic != nil {
			d.onPanic(u, r)
		}
		<-d.exec
		// Only work aborted by shutdown stays unacknowledged: Telegram must
		// redeliver it. A contained panic is acknowledged because retrying a
		// handler that always panics would wedge the offset forever (a poison
		// update), and HandleUpdate already recovers its own panics.
		d.finish(u, ctx.Err() == nil)
	}()
	d.handle(ctx, u)
}

// finish releases the update's pending budget and reports its outcome.
func (d *updateDispatcher) finish(u Update, ackable bool) {
	<-d.pending
	d.notifyDone(u, ackable)
}

// Wait blocks until every accepted update has finished processing or the
// timeout elapses. It reports whether the drain completed in time. It must be
// called after Enqueue has stopped being called (Start calls it once polling
// has ended), since sync.WaitGroup forbids Add concurrent with Wait.
func (d *updateDispatcher) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// WaitAll blocks until every accepted update has finished processing. Start
// uses it after cancelling the remaining handlers so the caller can never
// close its stores while a worker is still using them.
func (d *updateDispatcher) WaitAll() { d.wg.Wait() }

// updateAcker turns per-update completions into the Telegram offset, which is
// the only acknowledgement the Bot API offers: the next getUpdates call
// deletes every update below it. It advances across the contiguous processed
// prefix only, so a skipped or aborted update keeps the offset behind it and
// is redelivered after a restart instead of being lost.
//
// The resulting contract is at-least-once: work that was started but aborted
// by shutdown is redelivered. Because the offset is a watermark, an aborted
// update also forces redelivery of later updates that already succeeded, so
// processing MUST be idempotent for exactly-once journaling. Handler-level
// failures cannot be distinguished because Handler.HandleUpdate returns no
// error, so a failed Notion write is still acknowledged (no retry).
type updateAcker struct {
	mu          sync.Mutex
	pending     []ackedUpdate
	next        int
	outstanding int
	sealed      bool
	closed      bool
	doneCh      chan struct{}
}

type ackedUpdate struct {
	id      int
	done    bool
	ackable bool
}

func newUpdateAcker(baseOffset int) *updateAcker {
	return &updateAcker{next: baseOffset, doneCh: make(chan struct{})}
}

// accept records an update id that has been admitted, in delivery order. It
// must be called for the whole batch before seal: a worker started for an
// earlier update can finish while later ones are still being admitted, and an
// unsealed acker would then report the batch complete with updates missing.
func (a *updateAcker) accept(id int) {
	a.mu.Lock()
	a.pending = append(a.pending, ackedUpdate{id: id})
	a.outstanding++
	a.mu.Unlock()
}

// seal closes admission: the batch is now exactly the ids accepted so far, and
// done can close as soon as they have all reported. Without this, a batch whose
// first update finishes before the rest are accepted would signal completion
// early, letting Start poll again while the remainder is still pending.
func (a *updateAcker) seal() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sealed = true
	a.closeIfDrained()
}

// complete records one update's outcome and advances the acknowledged offset
// across the contiguous prefix that finished successfully. Reports for ids that
// were never accepted (or already reported) are ignored: they must not consume
// another update's outstanding count, which would close done early.
func (a *updateAcker) complete(u Update, ackable bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	matched := false
	for i := range a.pending {
		if a.pending[i].id == u.UpdateID && !a.pending[i].done {
			a.pending[i].done = true
			a.pending[i].ackable = ackable
			matched = true
			break
		}
	}
	if !matched {
		return
	}
	for len(a.pending) > 0 && a.pending[0].done {
		if !a.pending[0].ackable {
			break // gap: nothing after an unfinished update may be acknowledged
		}
		a.next = a.pending[0].id + 1
		a.pending[0] = ackedUpdate{}
		a.pending = a.pending[1:]
	}
	if a.outstanding > 0 {
		a.outstanding--
	}
	a.closeIfDrained()
}

// closeIfDrained closes done once admission is sealed and every accepted
// update has reported. It must be called with a.mu held and reports whether it
// closed the channel.
func (a *updateAcker) closeIfDrained() bool {
	if a.closed || !a.sealed || a.outstanding > 0 {
		return false
	}
	a.closed = true
	close(a.doneCh)
	return true
}

// offset returns the next getUpdates offset: everything below it is processed
// and may be acknowledged.
func (a *updateAcker) offset() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.next
}

// done is closed once every accepted update has reported its outcome.
func (a *updateAcker) done() <-chan struct{} { return a.doneCh }

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

// confirmOffset performs one final, immediate getUpdates carrying the
// acknowledged offset.
//
// Telegram confirms an update only when getUpdates is called with a higher
// offset; the acknowledgement is therefore not something the client can "send"
// on its own. Without this call a batch that finished just before shutdown
// would never be confirmed, and the next start would fetch it again and journal
// every trade a second time.
//
// The call may return updates that arrived meanwhile. They are deliberately
// discarded: Telegram only confirms updates below the offset, so the returned
// ones stay unconfirmed and the next start fetches them again. This confirms
// the processed prefix without accepting (or losing) anything new.
func (b *Bot) confirmOffset(ctx context.Context, offset int) {
	if offset <= 0 {
		return
	}
	// The polling context is already cancelled; the confirmation must still go
	// out, but only briefly.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownConfirmTimeout)
	defer cancel()
	if _, err := b.getUpdatesAt(ctx, offset, 0, 1); err != nil {
		b.logger.Warn("could not confirm processed updates; they will be redelivered",
			"offset", offset, "error", err)
		return
	}
	b.logger.Info("confirmed processed updates", "offset", offset)
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
