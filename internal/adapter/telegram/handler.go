package telegram

import (
	"context"
	"fmt"
	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/internal/service"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Exporter generates downloadable files from trade data.
type Exporter interface {
	ExportCSV(ctx context.Context, trades []domain.Trade) ([]byte, error)
	ExportPDF(ctx context.Context, username string, trades []domain.Trade, stats *domain.Stats) ([]byte, error)
}

// Services bundles all application services for the handler.
type Services struct {
	Journal      *service.JournalService
	Leaderboard  *service.LeaderboardService
	Report       *service.ReportService
	Gamification *service.GamificationService
	Badge        *service.BadgeService
	Challenge    *service.ChallengeService
	Reminder     *service.ReminderService
	Goal         *service.GoalService
	Analytics    *service.AnalyticsService
	ReportCard   *service.ReportCardService
}

// Resetter resets all BadgerDB state.
type Resetter interface {
	Reset() error
}

// NotionResetter archives all Notion member pages and clears the member cache.
type NotionResetter interface {
	ResetAll(ctx context.Context) (int, error)
}

// TradeEditField identifies which trade attribute is awaiting free-text input.
type TradeEditField string

const (
	EditFieldSymbol     TradeEditField = "symbol"
	EditFieldResultRR   TradeEditField = "result_rr"
	EditFieldConfluence TradeEditField = "confluence"
)

// TradeEditSession holds pending state when a user is editing or closing an existing trade.
type TradeEditSession struct {
	TradeID   string
	OldTrade  domain.Trade
	NewTrade  domain.Trade
	Field     TradeEditField
	ChatID    int64
	ThreadID  int
	ExpiresAt time.Time
}

// Handler routes incoming Telegram updates to the appropriate logic.
type Handler struct {
	sender                 *Sender
	bot                    *Bot
	svc                    Services
	exporter               Exporter
	trades                 service.TradeRepository
	members                service.MemberRepository
	resetter               Resetter
	guided                 *GuidedFlow
	editMu                 sync.RWMutex
	editSessions           map[int64]*TradeEditSession
	limiter                *RateLimiter
	logger                 *slog.Logger
	communityGroupID       int64
	ownerID                int64
	unauthMu               sync.Mutex
	unauthNotified         map[int64]time.Time // per-user last owner-notification time
	metricUpdatesTotal     uint64
	metricCommandsTotal    uint64
	metricCallbacksTotal   uint64
	metricRateLimitedTotal uint64
	metricPanicsTotal      uint64
}

// NewHandler creates a Handler.
func NewHandler(
	sender *Sender,
	svc Services,
	exporter Exporter,
	trades service.TradeRepository,
	members service.MemberRepository,
	limiter *RateLimiter,
	logger *slog.Logger,
	communityGroupID int64,
	ownerID int64,
) *Handler {
	return &Handler{
		sender:           sender,
		svc:              svc,
		exporter:         exporter,
		trades:           trades,
		members:          members,
		guided:           NewGuidedFlow(),
		editSessions:     make(map[int64]*TradeEditSession),
		limiter:          limiter,
		logger:           logger,
		communityGroupID: communityGroupID,
		ownerID:          ownerID,
		unauthNotified:   make(map[int64]time.Time),
	}
}

// SetBot sets the Bot reference (needed for file downloads).
func (h *Handler) SetBot(b *Bot) {
	h.bot = b
}

// SetResetter sets the BadgerDB store resetter for owner-only /reset.
func (h *Handler) SetResetter(r Resetter) {
	h.resetter = r
}

// Close stops background goroutines owned by the handler.
func (h *Handler) Close() {
	if h.guided != nil {
		h.guided.Stop()
	}
	if h.limiter != nil {
		h.limiter.Stop()
	}
}

// HandlerMetrics captures operational counters exposed via Prometheus /metrics.
type HandlerMetrics struct {
	UpdatesTotal     uint64
	CommandsTotal    uint64
	CallbacksTotal   uint64
	RateLimitedTotal uint64
	PanicsTotal      uint64
}

// Metrics returns a snapshot of handler operational counters.
func (h *Handler) Metrics() HandlerMetrics {
	return HandlerMetrics{
		UpdatesTotal:     atomic.LoadUint64(&h.metricUpdatesTotal),
		CommandsTotal:    atomic.LoadUint64(&h.metricCommandsTotal),
		CallbacksTotal:   atomic.LoadUint64(&h.metricCallbacksTotal),
		RateLimitedTotal: atomic.LoadUint64(&h.metricRateLimitedTotal),
		PanicsTotal:      atomic.LoadUint64(&h.metricPanicsTotal),
	}
}

// isAuthorized checks if the user is a member of the community group.
// If communityGroupID is 0, everyone is authorized.
// In group chats, non-members are silently ignored (no spam in group).
// In private chats, non-members get a friendly denial with owner contact.
func (h *Handler) isAuthorized(ctx context.Context, userID int64, chatID int64, threadID int, chatType string) bool {
	if h.communityGroupID == 0 || h.bot == nil {
		return true // no community gate configured
	}
	isMember, err := h.bot.CheckChatMember(ctx, h.communityGroupID, userID)
	if err != nil {
		h.logger.Error("check membership failed", "error", err, "user_id", userID)
		return true // fail open on API errors
	}
	if !isMember {
		// Only send denial message in private chat — don't spam group chats
		if chatType == "private" {
			msg := "🔒 Fitur ini hanya tersedia untuk member komunitas."
			if h.ownerID > 0 {
				msg += fmt.Sprintf("\n\nUntuk bergabung, hubungi owner:\n➡ <a href=\"tg://user?id=%d\">Contact Owner</a>", h.ownerID)
			} else {
				msg += "\n\nHubungi admin komunitas untuk bergabung."
			}
			_, _ = h.sender.SendHTML(ctx, chatID, msg, threadID)
		}
		// Notify owner about unauthorized access attempt (at most once per user per hour)
		if h.ownerID > 0 {
			now := time.Now()
			h.unauthMu.Lock()
			for id, ts := range h.unauthNotified {
				if now.Sub(ts) > time.Hour {
					delete(h.unauthNotified, id)
				}
			}
			last, ok := h.unauthNotified[userID]
			shouldNotify := !ok || now.Sub(last) > time.Hour
			if shouldNotify {
				h.unauthNotified[userID] = now
			}
			h.unauthMu.Unlock()
			if shouldNotify {
				notif := fmt.Sprintf("⚠️ <b>Unauthorized access attempt</b>\n\nUser: <a href=\"tg://user?id=%d\">%d</a>\nChat: <code>%d</code>\nType: %s",
					userID, userID, chatID, chatType)
				_, _ = h.sender.SendHTML(ctx, h.ownerID, notif)
			}
		}
		return false
	}
	return true
}

// isJournalMessage reports whether the text is a passive #journal entry.
// Case-insensitive to match the parser's `(?i)^#journal` tag.
func isJournalMessage(text string) bool {
	return strings.HasPrefix(strings.ToLower(text), "#journal")
}

// HandleUpdate processes a single Telegram update.
func (h *Handler) HandleUpdate(ctx context.Context, u Update) {
	atomic.AddUint64(&h.metricUpdatesTotal, 1)
	defer func() {
		if r := recover(); r != nil {
			atomic.AddUint64(&h.metricPanicsTotal, 1)
			if h.logger != nil {
				h.logger.Error("recovered panic in HandleUpdate", "update_id", u.UpdateID, "panic", r)
			}
		}
	}()
	if u.CallbackQuery != nil {
		h.handleCallback(ctx, u.CallbackQuery)
		return
	}
	if u.Message == nil || u.Message.Chat == nil {
		return
	}

	msg := u.Message
	text := strings.TrimSpace(msg.Text)
	if text == "" && msg.Caption != "" {
		text = strings.TrimSpace(msg.Caption)
	}

	// Rate limiting for commands and #journal submissions
	if msg.From != nil && h.limiter != nil && (strings.HasPrefix(text, "/") || isJournalMessage(text)) {
		if !h.limiter.Allow(msg.From.ID) {
			atomic.AddUint64(&h.metricRateLimitedTotal, 1)
			_, _ = h.sender.SendText(ctx, msg.Chat.ID, "⏳ Terlalu banyak request. Coba lagi nanti.", msg.MessageThreadID)
			return
		}
	}
	// Community membership gate
	if msg.From != nil && !h.isAuthorized(ctx, msg.From.ID, msg.Chat.ID, msg.MessageThreadID, msg.Chat.Type) {
		return
	}

	// Check if user is in a guided session and this is a free-text input step.
	// Slash commands always escape to normal routing (so /cancel and /help
	// work mid-flow), and #journal entries must never be swallowed by a
	// pending prompt — previously the whole journal text became the answer.
	if msg.From != nil {
		if editSess := h.getEditSession(msg.From.ID); editSess != nil && !strings.HasPrefix(text, "/") && !isJournalMessage(text) {
			h.handleTradeEditInput(ctx, msg, editSess)
			return
		}
		if session := h.guided.Get(msg.From.ID); session != nil && !strings.HasPrefix(text, "/") {
			h.handleGuidedInput(ctx, msg, session)
			return
		}
	}

	// Command routing (catalog-driven, see commands.go).
	if h.dispatchCommand(ctx, msg, text) {
		return
	}
	if isJournalMessage(text) {
		h.handleTextJournal(ctx, msg, text)
	}
}

// handleTextJournal processes a #journal text-format trade entry.
func (h *Handler) handleTextJournal(ctx context.Context, msg *Message, text string) {
	if msg.From == nil {
		return
	}

	result := ParseJournalMessage(text)
	if result.Err != "" {
		_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, "❌ "+result.Err, msg.MessageThreadID)
		return
	}

	trade := result.Trade
	username := msg.From.Username
	firstName := msg.From.FirstName

	if err := h.svc.Journal.RecordTrade(ctx, msg.From.ID, username, firstName, trade); err != nil {
		h.logger.Error("record trade failed", "error", err)
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Gagal menyimpan trade. Silakan coba lagi.", msg.MessageThreadID)
		return
	}

	// Handle attached photo. The reference must land on trade.ScreenshotURL
	// BEFORE the gamification call below, or the screenshot XP bonus never
	// sees it.
	if len(msg.Photo) > 0 && trade.ID != "" && h.bot != nil {
		if ref := h.uploadPhoto(ctx, msg.Photo, trade.ID); ref != "" {
			trade.ScreenshotURL = ref
		}
	}

	h.notifyTradeRecorded(ctx, msg.From.ID, trade, msg.Chat.ID, msg.MessageThreadID)
}

// notifyTradeRecorded sends the trade confirmation (with XP/level/streak if
// gamification is enabled) and evaluates badge and monthly goal rewards.
func (h *Handler) notifyTradeRecorded(ctx context.Context, userID int64, trade *domain.Trade, chatID int64, threadID int) {
	revengeWarn := h.checkRevengeWarning(ctx, userID, trade)

	if h.svc.Gamification != nil {
		gamResult, err := h.svc.Gamification.OnTradeRecorded(ctx, userID, trade)
		if err != nil {
			h.logger.Error("gamification failed", "error", err)
		} else if gamResult != nil {
			confirmText := FormatTradeConfirmationWithXP(trade, gamResult.XPGained, gamResult.TotalXP, gamResult.Level, gamResult.Title, gamResult.LeveledUp, gamResult.Streak) + revengeWarn
			_, _ = h.sender.SendHTML(ctx, chatID, confirmText, threadID)
			h.evaluatePostTradeRewards(ctx, userID, trade, chatID, threadID)
			return
		}
	}

	_, _ = h.sender.SendHTML(ctx, chatID, FormatTradeConfirmation(trade)+revengeWarn, threadID)
}

// evaluatePostTradeRewards checks badge unlocks and monthly goal completion
// after a trade is recorded or updated.
func (h *Handler) evaluatePostTradeRewards(ctx context.Context, userID int64, trade *domain.Trade, chatID int64, threadID int) {
	if h.svc.Gamification == nil {
		return
	}
	if h.svc.Badge != nil {
		streak, _ := h.svc.Gamification.GetStreak(ctx, userID)
		newBadges, _ := h.svc.Badge.CheckAndAwardBadges(ctx, userID, trade, streak)
		if len(newBadges) > 0 {
			_, _ = h.sender.SendHTML(ctx, chatID, FormatBadgeUnlock(newBadges), threadID)
		}
	}
	if h.svc.Goal != nil {
		if achieved, _ := h.svc.Goal.CheckGoalCompletion(ctx, userID); achieved {
			_, _ = h.sender.SendHTML(ctx, chatID, "🎯 <b>Monthly Goal Achieved!</b>\n\nSelamat! Kamu telah mencapai target bulananmu. +75 XP!", threadID)
		}
	}
}

// cmdJournal starts the guided flow.
func (h *Handler) cmdJournal(ctx context.Context, msg *Message) {
	if msg.From == nil {
		return
	}
	session := h.guided.Start(msg.From.ID, msg.Chat.ID, msg.MessageThreadID)
	text, btns := StepPrompt(session)
	rows := convertButtons(btns)
	if _, err := h.sender.SendWithKeyboard(ctx, msg.Chat.ID, text, rows, msg.MessageThreadID); err != nil {
		h.logger.Error("send guided prompt", "error", err)
		h.guided.Remove(msg.From.ID)
	}
}

// cmdStats shows personal or another member's stats with optional period, symbol, and session filters.
func (h *Handler) cmdStats(ctx context.Context, msg *Message, text string) {
	if msg.From == nil {
		return
	}
	targetID := msg.From.ID
	targetUsername := msg.From.Username
	filter := domain.StatsFilter{Period: "all"}

	// Supports: /stats [week|month|all] [@username] [symbol] [asia|london|nyam|nypm]
	parts := strings.Fields(text)
	for _, p := range parts[1:] {
		if strings.HasPrefix(p, "@") {
			lookupUsername := strings.TrimPrefix(p, "@")
			member, err := h.findMemberByUsername(ctx, lookupUsername)
			if err != nil || member == nil {
				_, _ = h.sender.SendText(ctx, msg.Chat.ID, fmt.Sprintf("📭 Member @%s belum pernah mencatat trade.", lookupUsername), msg.MessageThreadID)
				return
			}
			targetID = member.TelegramID
			targetUsername = member.Username
			continue
		}
		switch strings.ToLower(p) {
		case "week", "weekly":
			filter.Period = "week"
		case "month", "monthly":
			filter.Period = "month"
		case "all":
			filter.Period = "all"
		case "asia":
			filter.Session = domain.SessionAsia
		case "london":
			filter.Session = domain.SessionLondon
		case "nyam":
			filter.Session = domain.SessionNYAM
		case "nypm":
			filter.Session = domain.SessionNYPM
		default:
			filter.Symbol = strings.ToUpper(strings.ReplaceAll(p, "/", ""))
		}
	}

	stats, err := h.svc.Journal.GetMemberStatsWithFilter(ctx, targetID, filter)
	if err != nil || stats == nil {
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "📭 Belum ada trade yang tercatat untuk filter ini.", msg.MessageThreadID)
		return
	}

	_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, FormatStatsWithFilter(targetUsername, filter, stats), msg.MessageThreadID)
}

// findMemberByUsername searches cached members by username.
func (h *Handler) findMemberByUsername(ctx context.Context, username string) (*domain.Member, error) {
	members, err := h.members.ListMembers(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		if strings.EqualFold(m.Username, username) {
			return &m, nil
		}
	}
	return nil, nil
}

// cmdLeaderboard shows the leaderboard.
func (h *Handler) cmdLeaderboard(ctx context.Context, msg *Message, text string) {
	metric := "winrate"
	period := "all"
	minTrades := 5

	for _, p := range strings.Fields(strings.ToLower(text))[1:] {
		switch p {
		case "rr":
			metric = "rr"
		case "week", "weekly":
			period = "week"
			minTrades = 2
		case "month", "monthly":
			period = "month"
			minTrades = 3
		}
	}

	entries, err := h.svc.Leaderboard.GetLeaderboardByPeriod(ctx, metric, period, 10, minTrades)
	if err != nil {
		h.logger.Error("leaderboard failed", "error", err)
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Gagal memuat leaderboard.", msg.MessageThreadID)
		return
	}

	_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, FormatLeaderboardWithPeriod(entries, metric, period), msg.MessageThreadID)
}

// cmdExport exports the member's trades as CSV or PDF with optional period filtering.
func (h *Handler) cmdExport(ctx context.Context, msg *Message, text string) {
	if msg.From == nil {
		return
	}
	if h.exporter == nil {
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "⚠️ Export belum tersedia.", msg.MessageThreadID)
		return
	}

	format := "csv"
	period := "all"
	for _, p := range strings.Fields(strings.ToLower(text))[1:] {
		switch p {
		case "pdf":
			format = "pdf"
		case "csv":
			format = "csv"
		case "week", "weekly":
			period = "week"
		case "month", "monthly":
			period = "month"
		case "all":
			period = "all"
		}
	}

	trades, err := h.trades.GetTrades(ctx, msg.From.ID)
	if err != nil {
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "📭 Belum ada trade untuk di-export.", msg.MessageThreadID)
		return
	}
	trades = domain.FilterTradesByPeriod(trades, period, time.Now().In(timeutil.WIB))
	if len(trades) == 0 {
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "📭 Belum ada trade untuk di-export pada periode ini.", msg.MessageThreadID)
		return
	}

	username := msg.From.Username
	if username == "" {
		username = msg.From.FirstName
	}
	suffix := ""
	if period != "all" {
		suffix = "_" + period
	}

	switch format {
	case "pdf":
		computed := domain.CalculateStats(trades)
		data, err := h.exporter.ExportPDF(ctx, username, trades, &computed)
		if err != nil {
			h.logger.Error("export pdf failed", "error", err)
			_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Export PDF gagal.", msg.MessageThreadID)
			return
		}
		filename := fmt.Sprintf("journal_%s%s.pdf", username, suffix)
		_ = h.sender.SendDocument(ctx, msg.Chat.ID, filename, data, "📊 Export trade journal (PDF)", msg.MessageThreadID)
	default:
		data, err := h.exporter.ExportCSV(ctx, trades)
		if err != nil {
			h.logger.Error("export csv failed", "error", err)
			_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Export CSV gagal.", msg.MessageThreadID)
			return
		}
		filename := fmt.Sprintf("journal_%s%s.csv", username, suffix)
		_ = h.sender.SendDocument(ctx, msg.Chat.ID, filename, data, "📊 Export trade journal (CSV)", msg.MessageThreadID)
	}
}

// cmdReport generates and sends a weekly community summary.
func (h *Handler) cmdReport(ctx context.Context, msg *Message) {
	weekStart := timeutil.StartOfWeek(time.Now())
	weekEnd := weekStart.AddDate(0, 0, 7)

	summary, err := h.svc.Report.GenerateWeeklySummary(ctx, weekStart, weekEnd)
	if err != nil {
		h.logger.Error("weekly report failed", "error", err)
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Gagal memuat laporan mingguan.", msg.MessageThreadID)
		return
	}

	_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, FormatWeeklySummary(summary), msg.MessageThreadID)
}

// SendScheduledReport sends the weekly report to a specific chat and optional topic thread.
// Called by the scheduler.
func (h *Handler) SendScheduledReport(ctx context.Context, chatID int64, threadID int) error {
	weekStart := timeutil.StartOfWeek(time.Now())
	weekEnd := weekStart.AddDate(0, 0, 7)

	summary, err := h.svc.Report.GenerateWeeklySummary(ctx, weekStart, weekEnd)
	if err != nil {
		return err
	}

	_, err = h.sender.SendHTMLToThread(ctx, chatID, threadID, FormatWeeklySummary(summary))
	return err
}

// cmdProfile shows the user's gamification profile.
func (h *Handler) cmdProfile(ctx context.Context, msg *Message) {
	if msg.From == nil || h.svc.Gamification == nil {
		return
	}
	profile, err := h.svc.Gamification.GetProfile(ctx, msg.From.ID)
	if err != nil {
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Gagal memuat profil.", msg.MessageThreadID)
		return
	}
	streak, _ := h.svc.Gamification.GetStreak(ctx, msg.From.ID)
	_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, FormatProfile(profile, streak), msg.MessageThreadID)
}

// handleCallback processes inline button presses.
func (h *Handler) handleCallback(ctx context.Context, cb *CallbackQuery) {
	if cb == nil || cb.From == nil {
		return
	}
	atomic.AddUint64(&h.metricCallbacksTotal, 1)
	if h.limiter != nil && !h.limiter.Allow(cb.From.ID) {
		atomic.AddUint64(&h.metricRateLimitedTotal, 1)
		_ = h.sender.AnswerCallback(ctx, cb.ID, "⏳ Terlalu banyak request. Coba lagi nanti.")
		return
	}
	_ = h.sender.AnswerCallback(ctx, cb.ID, "")
	data := cb.Data
	if data == "reset:confirm" || data == "reset:cancel" {
		if h.ownerID == 0 || cb.From.ID != h.ownerID || cb.Message == nil || cb.Message.Chat == nil {
			return
		}
		if data == "reset:cancel" {
			_ = h.sender.EditMessage(ctx, cb.Message.Chat.ID, cb.Message.MessageID, "❌ Reset dibatalkan.")
			return
		}
		_ = h.sender.EditMessage(ctx, cb.Message.Chat.ID, cb.Message.MessageID, "⏳ Mereset seluruh data BadgerDB &amp; Notion...")
		h.executeReset(ctx, cb.Message.Chat.ID, cb.Message.MessageThreadID)
		return
	}

	// Community gate
	if cb.Message != nil && cb.Message.Chat != nil && !h.isAuthorized(ctx, cb.From.ID, cb.Message.Chat.ID, cb.Message.MessageThreadID, cb.Message.Chat.Type) {
		return
	}
	// Handle /open and /history callbacks before guided flow
	if h.handleTradeActionCallback(ctx, cb) {
		return
	}

	session := h.guided.Get(cb.From.ID)
	if session == nil {
		return
	}
	// Handle confirm actions WITHOUT holding session.mu to avoid deadlock
	// (submitGuidedTrade acquires session.mu internally)
	switch data {
	case "confirm:yes":
		h.submitGuidedTrade(ctx, cb.From, session)
		return
	case "confirm:no":
		h.guided.Remove(cb.From.ID)
		if cb.Message != nil {
			_ = h.sender.EditMessage(ctx, cb.Message.Chat.ID, cb.Message.MessageID, "❌ Trade dibatalkan.")
		}
		return
	}

	// For all other callbacks, lock the session to mutate step/trade fields
	session.mu.Lock()
	defer session.mu.Unlock()

	switch {
	case strings.HasPrefix(data, "asset:"):
		assetStr := strings.TrimPrefix(data, "asset:")
		session.Trade.AssetType = domain.AssetType(assetStr)
		session.Step = StepSymbol

	case strings.HasPrefix(data, "dir:"):
		dirStr := strings.TrimPrefix(data, "dir:")
		session.Trade.Direction = domain.Direction(dirStr)
		session.Step = StepResult

	case strings.HasPrefix(data, "result:"):
		resultStr := strings.TrimPrefix(data, "result:")
		switch resultStr {
		case "WIN":
			session.Trade.Status = domain.StatusWin
			session.Step = StepRRAmount
		case "LOSS":
			session.Trade.Status = domain.StatusLoss
			session.Step = StepRRAmount
		case "BE":
			session.Trade.Status = domain.StatusBE
			session.Trade.ResultRR = 0
			session.Step = StepTimeWindow
		case "OPEN":
			session.Trade.Status = domain.StatusOpen
			session.Step = StepTimeWindow
		}

	case strings.HasPrefix(data, "session:"):
		sessionStr := strings.TrimPrefix(data, "session:")
		switch sessionStr {
		case "asia":
			session.Trade.TimeWindow = domain.SessionAsia
		case "london":
			session.Trade.TimeWindow = domain.SessionLondon
		case "nyam":
			session.Trade.TimeWindow = domain.SessionNYAM
		case "nypm":
			session.Trade.TimeWindow = domain.SessionNYPM
		case "skip":
			// leave empty
		}
		session.Step = StepConfluence

	case data == "screenshot:skip":
		session.Step = StepConfirm
	}

	// Update the prompt message
	text, btns := StepPrompt(session)
	rows := convertButtons(btns)
	if cb.Message != nil {
		if len(rows) > 0 {
			_ = h.sender.EditWithKeyboard(ctx, cb.Message.Chat.ID, cb.Message.MessageID, text, rows)
		} else {
			_ = h.sender.EditMessage(ctx, cb.Message.Chat.ID, cb.Message.MessageID, text)
		}
	}
}

// handleGuidedInput processes free-text input during a guided flow.
func (h *Handler) handleGuidedInput(ctx context.Context, msg *Message, session *GuidedSession) {
	session.mu.Lock()
	defer session.mu.Unlock()

	text := strings.TrimSpace(msg.Text)

	switch session.Step {
	case StepSymbol:
		session.Trade.Symbol = strings.ToUpper(strings.ReplaceAll(truncateUTF8(text, maxSymbolLen), "/", ""))
		session.Step = StepDirection

	case StepRRAmount:
		v, err := strconv.ParseFloat(text, 64)
		if err != nil {
			_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Masukkan angka yang valid untuk RR (contoh: 2, 1.5).", session.ThreadID)
			return
		}
		switch session.Trade.Status {
		case domain.StatusWin:
			if v < 0 {
				v = -v
			}
			session.Trade.ResultRR = v
		case domain.StatusLoss:
			if v > 0 {
				v = -v
			}
			session.Trade.ResultRR = v
		}
		session.Step = StepTimeWindow

	case StepConfluence:
		if strings.ToLower(text) != "skip" {
			session.Trade.Confluence = truncateUTF8(text, maxTextFieldLen)
		}
		session.Step = StepScreenshot

	case StepScreenshot:
		// Check if it's a photo message
		if len(msg.Photo) > 0 {
			// Get largest photo
			photo := msg.Photo[len(msg.Photo)-1]
			session.PhotoFileID = photo.FileID
			session.Step = StepConfirm
		} else {
			_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Kirim foto, atau tekan Skip.", session.ThreadID)
			return
		}

	default:
		return
	}

	// Send next step prompt
	promptText, btns := StepPrompt(session)
	rows := convertButtons(btns)
	if len(rows) > 0 {
		_, _ = h.sender.SendWithKeyboard(ctx, msg.Chat.ID, promptText, rows, session.ThreadID)
	} else {
		_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, promptText, session.ThreadID)
	}
}

// submitGuidedTrade finalises the guided flow and saves the trade.
func (h *Handler) submitGuidedTrade(ctx context.Context, from *User, session *GuidedSession) {
	session.mu.Lock()
	defer session.mu.Unlock()

	if session.Submitting {
		return // already processing
	}
	session.Submitting = true

	trade := &session.Trade
	if trade.Status == "" {
		trade.Status = domain.StatusOpen
	}

	err := h.svc.Journal.RecordTrade(ctx, from.ID, from.Username, from.FirstName, trade)
	if err != nil {
		h.logger.Error("guided submit failed", "error", err)
		_, _ = h.sender.SendText(ctx, session.ChatID, "❌ Gagal menyimpan trade. Silakan coba lagi.", session.ThreadID)
		h.guided.Remove(from.ID)
		return
	}

	// Upload screenshot if provided. The reference is stored on the trade
	// before gamification runs so the screenshot XP bonus is granted and the
	// trade-XP grant reflects it.
	if session.PhotoFileID != "" && trade.ID != "" && h.bot != nil {
		if ref := h.uploadPhoto(ctx, []PhotoSize{{FileID: session.PhotoFileID}}, trade.ID); ref != "" {
			trade.ScreenshotURL = ref
		}
	}

	h.notifyTradeRecorded(ctx, from.ID, trade, session.ChatID, session.ThreadID)
	h.guided.Remove(from.ID)
}

// uploadPhoto downloads a photo from Telegram, attaches it to the trade's
// Notion page, and returns the Notion file reference ("" when the upload did
// not happen or failed — failures are logged, the trade itself stands).
func (h *Handler) uploadPhoto(ctx context.Context, photos []PhotoSize, tradeID string) string {
	if len(photos) == 0 || h.bot == nil {
		return ""
	}
	// Use the largest photo
	photo := photos[len(photos)-1]
	fileData, err := h.bot.GetFile(ctx, photo.FileID)
	if err != nil {
		h.logger.Error("download file", "error", err)
		return ""
	}
	// Pass the file content to Notion - the token is no longer exposed in URLs
	filename := fmt.Sprintf("screenshot_%s.jpg", tradeID)
	ref, err := h.svc.Journal.UploadScreenshot(ctx, tradeID, filename, fileData)
	if err != nil {
		h.logger.Error("upload screenshot", "error", err)
		return ""
	}
	return ref
}

// convertButtons converts guided button types to InlineButton rows.
func convertButtons(btns [][]InlineBtn) [][]InlineButton {
	if btns == nil {
		return nil
	}
	rows := make([][]InlineButton, len(btns))
	for i, row := range btns {
		rows[i] = make([]InlineButton, len(row))
		for j, btn := range row {
			rows[i][j] = InlineButton{
				Text:         btn.Text,
				CallbackData: btn.Data,
			}
		}
	}
	return rows
}

// cmdBadges shows the user's earned badges.
func (h *Handler) cmdBadges(ctx context.Context, msg *Message) {
	if msg.From == nil || h.svc.Badge == nil {
		return
	}
	badges, err := h.svc.Badge.GetBadges(ctx, msg.From.ID)
	if err != nil {
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Gagal memuat badges.", msg.MessageThreadID)
		return
	}
	_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, FormatBadgeList(badges), msg.MessageThreadID)
}

// cmdChallenge shows the current weekly challenge and standings.
func (h *Handler) cmdChallenge(ctx context.Context, msg *Message) {
	if msg.From == nil || h.svc.Challenge == nil {
		return
	}
	challenge, err := h.svc.Challenge.GetOrCreateChallenge(ctx, time.Now())
	if err != nil {
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Gagal memuat challenge.", msg.MessageThreadID)
		return
	}
	standings, err := h.svc.Challenge.GetCurrentStandings(ctx, challenge)
	if err != nil {
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Gagal memuat standings.", msg.MessageThreadID)
		return
	}
	_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, FormatChallenge(challenge, standings), msg.MessageThreadID)
}

// cmdReminder handles /reminder on, /reminder off, /reminder (show status)
func (h *Handler) cmdReminder(ctx context.Context, msg *Message, text string) {
	if msg.From == nil || h.svc.Reminder == nil {
		return
	}
	isCommunity := msg.Chat.Type != "private"
	targetID := msg.From.ID
	label := "Daily reminder"
	if isCommunity {
		targetID = msg.Chat.ID
		label = "Community reminder"
	}

	parts := strings.Fields(text)

	if len(parts) >= 2 {
		switch strings.ToLower(parts[1]) {
		case "on":
			hourVal := 20 // default 8 PM WIB
			if len(parts) >= 3 {
				parsed, err := strconv.Atoi(parts[2])
				if err == nil && parsed >= 0 && parsed <= 23 {
					hourVal = parsed
				}
			}
			err := h.svc.Reminder.SetReminder(ctx, targetID, msg.Chat.ID, msg.MessageThreadID, true, hourVal)
			if err != nil {
				_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Gagal mengatur reminder.", msg.MessageThreadID)
				return
			}
			_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, fmt.Sprintf("🔔 %s diaktifkan jam <b>%02d:00 WIB</b>", label, hourVal), msg.MessageThreadID)
		case "off":
			err := h.svc.Reminder.SetReminder(ctx, targetID, msg.Chat.ID, msg.MessageThreadID, false, 0)
			if err != nil {
				_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Gagal mengatur reminder.", msg.MessageThreadID)
				return
			}
			_, _ = h.sender.SendText(ctx, msg.Chat.ID, fmt.Sprintf("🔕 %s dinonaktifkan.", label), msg.MessageThreadID)
		default:
			_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, FormatReminderHelp(), msg.MessageThreadID)
		}
		return
	}

	// Show current status
	pref, err := h.svc.Reminder.GetReminder(ctx, targetID)
	if err != nil || pref == nil || !pref.Enabled {
		_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, fmt.Sprintf("🔕 %s belum aktif.\n\nGunakan <code>/reminder on</code> untuk mengaktifkan.\nGunakan <code>/reminder on 19</code> untuk jam 19:00 WIB.", label), msg.MessageThreadID)
		return
	}
	_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, fmt.Sprintf("🔔 %s aktif: <b>%02d:00 WIB</b>\n\nGunakan <code>/reminder off</code> untuk menonaktifkan.", label, pref.Hour), msg.MessageThreadID)
}

// cmdGoal handles /goal, /goal set <type> <target>
func (h *Handler) cmdGoal(ctx context.Context, msg *Message, text string) {
	if msg.From == nil || h.svc.Goal == nil {
		return
	}
	parts := strings.Fields(text)

	// /goal set trades 30
	if len(parts) >= 4 && strings.ToLower(parts[1]) == "set" {
		goalType := domain.GoalType(strings.ToLower(parts[2]))
		target, err := strconv.ParseFloat(parts[3], 64)
		if err != nil || target <= 0 {
			_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Target harus angka positif.", msg.MessageThreadID)
			return
		}

		// Validate goal type
		switch goalType {
		case domain.GoalTotalTrades, domain.GoalTotalRR, domain.GoalWinRate, domain.GoalStreakDays:
			// valid
		default:
			_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, FormatGoalHelp(), msg.MessageThreadID)
			return
		}

		goal, err := h.svc.Goal.SetGoal(ctx, msg.From.ID, goalType, target)
		if err != nil {
			_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Gagal menyimpan goal.", msg.MessageThreadID)
			return
		}
		_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, FormatGoalSet(goal), msg.MessageThreadID)
		return
	}

	// /goal — show progress
	progress, err := h.svc.Goal.GetProgress(ctx, msg.From.ID)
	if err != nil {
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Gagal memuat goal.", msg.MessageThreadID)
		return
	}
	if progress == nil {
		_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, FormatGoalHelp(), msg.MessageThreadID)
		return
	}
	_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, FormatGoalProgress(progress), msg.MessageThreadID)
}

// cmdAnalyze shows AI-powered trade analytics.
func (h *Handler) cmdAnalyze(ctx context.Context, msg *Message) {
	if msg.From == nil || h.svc.Analytics == nil {
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "\u26a0\ufe0f Fitur analytics belum tersedia.", msg.MessageThreadID)
		return
	}
	// Send "analyzing" message first since AI call can take a few seconds
	waitMsgID, _ := h.sender.SendText(ctx, msg.Chat.ID, "\U0001f916 Menganalisis trading kamu...", msg.MessageThreadID)

	analytics, err := h.svc.Analytics.GetAnalytics(ctx, msg.From.ID)
	if err != nil {
		h.logger.Error("analytics failed", "error", err)
		if waitMsgID > 0 {
			errMsg := "❌ Gagal menganalisis trade. Silakan coba lagi nanti."
			if strings.Contains(err.Error(), "belum ada trade") {
				errMsg = "📭 Belum ada trade untuk dianalisis."
			}
			_ = h.sender.EditMessage(ctx, msg.Chat.ID, waitMsgID, errMsg)
		}
		return
	}

	// Delete waiting message and send result
	if waitMsgID > 0 {
		_ = h.sender.DeleteMessage(ctx, msg.Chat.ID, waitMsgID)
	}
	_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, FormatAnalytics(analytics), msg.MessageThreadID)
}

// cmdReportCard shows monthly report card.
func (h *Handler) cmdReportCard(ctx context.Context, msg *Message) {
	if msg.From == nil || h.svc.ReportCard == nil {
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "\u26a0\ufe0f Fitur report card belum tersedia.", msg.MessageThreadID)
		return
	}

	// Use previous month by default, or current if specified.
	// Evaluated in WIB so the month boundary matches the user's calendar,
	// not the server's clock.
	now := time.Now().In(timeutil.WIB)
	yearMonth := now.AddDate(0, -1, 0).Format("2006-01") // last month

	// Check if user wants current month: /reportcard current
	parts := strings.Fields(msg.Text)
	if len(parts) >= 2 && parts[1] == "current" {
		yearMonth = now.Format("2006-01")
	}

	waitMsgID, _ := h.sender.SendText(ctx, msg.Chat.ID, "\U0001f4ca Generating report card...", msg.MessageThreadID)

	report, err := h.svc.ReportCard.GenerateMonthlyReport(ctx, msg.From.ID, yearMonth)
	if err != nil {
		h.logger.Error("report card failed", "error", err)
		if waitMsgID > 0 {
			_ = h.sender.EditMessage(ctx, msg.Chat.ID, waitMsgID, "❌ Gagal membuat report card. Silakan coba lagi nanti.")
		}
		return
	}

	if waitMsgID > 0 {
		_ = h.sender.DeleteMessage(ctx, msg.Chat.ID, waitMsgID)
	}
	_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, FormatMonthlyReportCard(report), msg.MessageThreadID)
}

// cmdReset handles the owner-only /reset command. Non-owners are silently ignored.
func (h *Handler) cmdReset(ctx context.Context, msg *Message, text string) {
	if msg.From == nil || h.ownerID == 0 || msg.From.ID != h.ownerID {
		return // invisible to non-owners
	}
	parts := strings.Fields(text)
	if len(parts) >= 2 && strings.EqualFold(parts[1], "confirm") {
		h.executeReset(ctx, msg.Chat.ID, msg.MessageThreadID)
		return
	}
	rows := [][]InlineButton{
		{
			{Text: "✅ Ya, Reset Semua", CallbackData: "reset:confirm"},
			{Text: "❌ Batal", CallbackData: "reset:cancel"},
		},
	}
	prompt := "⚠️ <b>Konfirmasi Reset Total (Owner Only)</b>\n\n" +
		"Tindakan ini akan:\n" +
		"1. Menghapus seluruh data gamifikasi, XP, level, streak, badge, challenge, goal, reminder &amp; cache AI di <b>BadgerDB</b>.\n" +
		"2. Mengarsipkan seluruh halaman jurnal member &amp; database trade di <b>Notion</b>.\n\n" +
		"Apakah Anda yakin?"
	_, _ = h.sender.SendWithKeyboard(ctx, msg.Chat.ID, prompt, rows, msg.MessageThreadID)
}

func (h *Handler) executeReset(ctx context.Context, chatID int64, threadID int) {
	archivedCount := 0
	if nr, ok := h.members.(NotionResetter); ok && nr != nil {
		n, err := nr.ResetAll(ctx)
		archivedCount = n
		if err != nil {
			h.logger.Error("notion reset failed", "error", err)
			_, _ = h.sender.SendText(ctx, chatID, fmt.Sprintf("⚠️ Sebagian reset Notion gagal (%d halaman diarsipkan). Cek log server untuk detail.", archivedCount), threadID)
		}
	}
	if h.resetter != nil {
		if err := h.resetter.Reset(); err != nil {
			h.logger.Error("badger reset failed", "error", err)
			_, _ = h.sender.SendText(ctx, chatID, "❌ Gagal mereset BadgerDB.", threadID)
			return
		}
	}
	msg := fmt.Sprintf("✅ <b>Reset selesai!</b>\n\n• BadgerDB: seluruh data gamifikasi &amp; cache dihapus\n• Notion: <b>%d</b> halaman jurnal diarsipkan", archivedCount)
	_, _ = h.sender.SendHTML(ctx, chatID, msg, threadID)
}

func (h *Handler) checkRevengeWarning(ctx context.Context, userID int64, trade *domain.Trade) string {
	if trade == nil || trade.Status != domain.StatusLoss || h.trades == nil {
		return ""
	}
	trades, err := h.trades.GetTrades(ctx, userID)
	if err != nil {
		return ""
	}
	losses := domain.ConsecutiveLossesToday(trades, time.Now())
	return FormatRevengeWarning(losses)
}

func (h *Handler) setEditSession(userID int64, sess *TradeEditSession) {
	h.editMu.Lock()
	defer h.editMu.Unlock()
	now := time.Now()
	for id, s := range h.editSessions {
		if now.After(s.ExpiresAt) {
			delete(h.editSessions, id)
		}
	}
	sess.ExpiresAt = now.Add(15 * time.Minute)
	h.editSessions[userID] = sess
}

func (h *Handler) getEditSession(userID int64) *TradeEditSession {
	h.editMu.RLock()
	defer h.editMu.RUnlock()
	s, ok := h.editSessions[userID]
	if !ok || time.Now().After(s.ExpiresAt) {
		return nil
	}
	return s
}

func (h *Handler) removeEditSession(userID int64) {
	h.editMu.Lock()
	defer h.editMu.Unlock()
	delete(h.editSessions, userID)
}

func (h *Handler) findUserTrade(ctx context.Context, userID int64, tradeID string) (*domain.Trade, error) {
	if h.trades == nil {
		return nil, fmt.Errorf("trade repository not configured")
	}
	trades, err := h.trades.GetTrades(ctx, userID)
	if err != nil {
		return nil, err
	}
	for i := range trades {
		if trades[i].ID == tradeID {
			cp := trades[i]
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("trade tidak ditemukan")
}

// cmdOpen lists all trades with OPEN status and provides buttons to close them.
func (h *Handler) cmdOpen(ctx context.Context, msg *Message) {
	if msg.From == nil || h.svc.Journal == nil {
		return
	}
	openTrades, err := h.svc.Journal.GetOpenTrades(ctx, msg.From.ID)
	if err != nil {
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Gagal memuat open trades.", msg.MessageThreadID)
		return
	}
	text, rows := FormatOpenTrades(openTrades)
	if len(rows) > 0 {
		_, _ = h.sender.SendWithKeyboard(ctx, msg.Chat.ID, text, rows, msg.MessageThreadID)
	} else {
		_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, text, msg.MessageThreadID)
	}
}

// cmdHistory shows paginated trade history with Edit and Delete buttons.
func (h *Handler) cmdHistory(ctx context.Context, msg *Message) {
	if msg.From == nil || h.trades == nil {
		return
	}
	trades, err := h.trades.GetTrades(ctx, msg.From.ID)
	if err != nil || len(trades) == 0 {
		_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, "📭 Belum ada riwayat trade.", msg.MessageThreadID)
		return
	}
	text, rows := FormatHistoryPage(trades, 0, 5)
	if len(rows) > 0 {
		_, _ = h.sender.SendWithKeyboard(ctx, msg.Chat.ID, text, rows, msg.MessageThreadID)
	} else {
		_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, text, msg.MessageThreadID)
	}
}

// cmdUndo deletes the user's most recent trade entry.
func (h *Handler) cmdUndo(ctx context.Context, msg *Message) {
	if msg.From == nil || h.trades == nil || h.svc.Journal == nil {
		return
	}
	trades, err := h.trades.GetTrades(ctx, msg.From.ID)
	if err != nil || len(trades) == 0 {
		_, _ = h.sender.SendText(ctx, msg.Chat.ID, "📭 Belum ada trade untuk di-undo.", msg.MessageThreadID)
		return
	}
	// Most recent by journal date; Notion's created time breaks ties between
	// trades recorded on the same day (the date sort alone is arbitrary there).
	latest := trades[0]
	for _, t := range trades[1:] {
		switch {
		case t.Date.After(latest.Date):
			latest = t
		case t.Date.Equal(latest.Date) && t.CreatedAt.After(latest.CreatedAt):
			latest = t
		}
	}
	h.deleteTradeAndNotify(ctx, msg.From.ID, &latest, msg.Chat.ID, msg.MessageThreadID)
}

// cmdCancel aborts any active guided journal flow or pending edit prompt so a
// user is never trapped in a 15-minute session.
func (h *Handler) cmdCancel(ctx context.Context, msg *Message) {
	if msg.From == nil {
		return
	}
	cancelled := false
	if h.guided.Get(msg.From.ID) != nil {
		h.guided.Remove(msg.From.ID)
		cancelled = true
	}
	if h.getEditSession(msg.From.ID) != nil {
		h.removeEditSession(msg.From.ID)
		cancelled = true
	}
	if cancelled {
		_, _ = h.sender.SendHTML(ctx, msg.Chat.ID, "❌ Flow dibatalkan.", msg.MessageThreadID)
		return
	}
	_, _ = h.sender.SendText(ctx, msg.Chat.ID, "Tidak ada flow atau sesi edit yang aktif.", msg.MessageThreadID)
}

func (h *Handler) deleteTradeAndNotify(ctx context.Context, userID int64, trade *domain.Trade, chatID int64, threadID int) {
	if err := h.svc.Journal.DeleteTrade(ctx, userID, trade.ID); err != nil {
		h.logger.Error("delete trade failed", "error", err)
		_, _ = h.sender.SendText(ctx, chatID, "❌ Gagal menghapus trade.", threadID)
		return
	}
	xpNote := ""
	if h.svc.Gamification != nil {
		if prof, delta, err := h.svc.Gamification.AdjustXPForTradeChange(ctx, userID, trade, nil); err == nil && delta != 0 {
			xpNote = fmt.Sprintf("\n⚡ XP disesuaikan: <b>%+d XP</b> (Total: %d XP — Level %d %s)", delta, prof.TotalXP, prof.Level, prof.Title)
		}
	}
	msg := fmt.Sprintf("🗑 Trade <b>%s %s</b> (%s) berhasil dihapus dari jurnal &amp; Notion.%s",
		escapeHTML(trade.Symbol), trade.Direction, trade.Status, xpNote)
	_, _ = h.sender.SendHTML(ctx, chatID, msg, threadID)
}

func (h *Handler) applyTradeUpdate(ctx context.Context, userID int64, oldTrade, newTrade *domain.Trade, chatID int64, threadID int) {
	if err := h.svc.Journal.UpdateTrade(ctx, userID, newTrade); err != nil {
		h.logger.Error("update trade failed", "error", err)
		_, _ = h.sender.SendText(ctx, chatID, "❌ Gagal mengupdate trade.", threadID)
		return
	}
	xpNote := ""
	if h.svc.Gamification != nil {
		if prof, delta, err := h.svc.Gamification.AdjustXPForTradeChange(ctx, userID, oldTrade, newTrade); err == nil && delta != 0 {
			xpNote = fmt.Sprintf("\n⚡ XP disesuaikan: <b>%+d XP</b> (Total: %d XP — Level %d %s)", delta, prof.TotalXP, prof.Level, prof.Title)
		}
		h.evaluatePostTradeRewards(ctx, userID, newTrade, chatID, threadID)
	}
	revengeWarn := h.checkRevengeWarning(ctx, userID, newTrade)
	msg := fmt.Sprintf("✅ <b>Trade berhasil diperbarui!</b>\n\nSymbol: <b>%s</b> (%s)\nDirection: <b>%s</b>\nStatus: <b>%s</b> (%+.1fR)%s%s",
		escapeHTML(newTrade.Symbol), newTrade.AssetType.String(), newTrade.Direction, newTrade.Status, newTrade.ResultRR, xpNote, revengeWarn)
	_, _ = h.sender.SendHTML(ctx, chatID, msg, threadID)
}

func (h *Handler) handleTradeActionCallback(ctx context.Context, cb *CallbackQuery) bool {
	if cb.From == nil || cb.Message == nil {
		return false
	}
	data := cb.Data
	userID := cb.From.ID
	chatID := cb.Message.Chat.ID
	threadID := cb.Message.MessageThreadID

	switch {
	case strings.HasPrefix(data, "open:"):
		parts := strings.SplitN(data, ":", 3)
		if len(parts) != 3 {
			return true
		}
		action, tradeID := parts[1], parts[2]
		oldTrade, err := h.findUserTrade(ctx, userID, tradeID)
		if err != nil {
			_, _ = h.sender.SendText(ctx, chatID, "❌ Trade tidak ditemukan.", threadID)
			return true
		}
		newTrade := *oldTrade
		switch action {
		case "be":
			newTrade.Status = domain.StatusBE
			newTrade.ResultRR = 0
			h.applyTradeUpdate(ctx, userID, oldTrade, &newTrade, chatID, threadID)
		case "open":
			newTrade.Status = domain.StatusOpen
			newTrade.ResultRR = 0
			h.applyTradeUpdate(ctx, userID, oldTrade, &newTrade, chatID, threadID)
		case "win":
			newTrade.Status = domain.StatusWin
			h.setEditSession(userID, &TradeEditSession{
				TradeID:  tradeID,
				OldTrade: *oldTrade,
				NewTrade: newTrade,
				Field:    EditFieldResultRR,
				ChatID:   chatID,
				ThreadID: threadID,
			})
			_, _ = h.sender.SendHTML(ctx, chatID, fmt.Sprintf("📊 Masukkan jumlah RR untuk <b>%s</b> (WIN) (contoh: 2, 1.5):", escapeHTML(oldTrade.Symbol)), threadID)
		case "loss":
			newTrade.Status = domain.StatusLoss
			h.setEditSession(userID, &TradeEditSession{
				TradeID:  tradeID,
				OldTrade: *oldTrade,
				NewTrade: newTrade,
				Field:    EditFieldResultRR,
				ChatID:   chatID,
				ThreadID: threadID,
			})
			_, _ = h.sender.SendHTML(ctx, chatID, fmt.Sprintf("📊 Masukkan jumlah RR untuk <b>%s</b> (LOSS) (contoh: 1, 1.5):", escapeHTML(oldTrade.Symbol)), threadID)
		}
		return true

	case strings.HasPrefix(data, "hist:page:"):
		pageStr := strings.TrimPrefix(data, "hist:page:")
		page, _ := strconv.Atoi(pageStr)
		trades, err := h.trades.GetTrades(ctx, userID)
		if err != nil || len(trades) == 0 {
			return true
		}
		text, rows := FormatHistoryPage(trades, page, 5)
		_ = h.sender.EditWithKeyboard(ctx, chatID, cb.Message.MessageID, text, rows)
		return true

	case strings.HasPrefix(data, "hist:del:"):
		tradeID := strings.TrimPrefix(data, "hist:del:")
		trade, err := h.findUserTrade(ctx, userID, tradeID)
		if err != nil {
			_, _ = h.sender.SendText(ctx, chatID, "❌ Trade tidak ditemukan.", threadID)
			return true
		}
		h.deleteTradeAndNotify(ctx, userID, trade, chatID, threadID)
		return true

	case strings.HasPrefix(data, "hist:edit:"):
		tradeID := strings.TrimPrefix(data, "hist:edit:")
		trade, err := h.findUserTrade(ctx, userID, tradeID)
		if err != nil {
			_, _ = h.sender.SendText(ctx, chatID, "❌ Trade tidak ditemukan.", threadID)
			return true
		}
		text, rows := FormatEditTradeMenu(trade)
		_, _ = h.sender.SendWithKeyboard(ctx, chatID, text, rows, threadID)
		return true

	case strings.HasPrefix(data, "editf:"):
		parts := strings.SplitN(data, ":", 3)
		if len(parts) != 3 {
			return true
		}
		field, tradeID := parts[1], parts[2]
		trade, err := h.findUserTrade(ctx, userID, tradeID)
		if err != nil {
			_, _ = h.sender.SendText(ctx, chatID, "❌ Trade tidak ditemukan.", threadID)
			return true
		}
		switch field {
		case "symbol":
			h.setEditSession(userID, &TradeEditSession{
				TradeID:  tradeID,
				OldTrade: *trade,
				NewTrade: *trade,
				Field:    EditFieldSymbol,
				ChatID:   chatID,
				ThreadID: threadID,
			})
			_, _ = h.sender.SendHTML(ctx, chatID, "🔤 Masukkan pair/symbol baru (contoh: XAUUSD, EURUSD):", threadID)
		case "conf":
			h.setEditSession(userID, &TradeEditSession{
				TradeID:  tradeID,
				OldTrade: *trade,
				NewTrade: *trade,
				Field:    EditFieldConfluence,
				ChatID:   chatID,
				ThreadID: threadID,
			})
			_, _ = h.sender.SendHTML(ctx, chatID, "📝 Masukkan confluence baru:", threadID)
		case "dir":
			rows := [][]InlineButton{
				{
					{Text: "🟢 BUY", CallbackData: "editv:dir:BUY:" + tradeID},
					{Text: "🔴 SELL", CallbackData: "editv:dir:SELL:" + tradeID},
				},
			}
			_, _ = h.sender.SendWithKeyboard(ctx, chatID, "↕️ Pilih direction baru:", rows, threadID)
		case "status":
			rows := [][]InlineButton{
				{
					{Text: "✅ WIN", CallbackData: "open:win:" + tradeID},
					{Text: "❌ LOSS", CallbackData: "open:loss:" + tradeID},
				},
				{
					{Text: "➖ BE", CallbackData: "open:be:" + tradeID},
					{Text: "⏳ OPEN", CallbackData: "open:open:" + tradeID},
				},
			}
			_, _ = h.sender.SendWithKeyboard(ctx, chatID, "🎯 Pilih status baru:", rows, threadID)
		case "session":
			rows := [][]InlineButton{
				{
					{Text: "Asia", CallbackData: "editv:session:Asia:" + tradeID},
					{Text: "London", CallbackData: "editv:session:London:" + tradeID},
				},
				{
					{Text: "NY AM", CallbackData: "editv:session:NY AM:" + tradeID},
					{Text: "NY PM", CallbackData: "editv:session:NY PM:" + tradeID},
				},
			}
			_, _ = h.sender.SendWithKeyboard(ctx, chatID, "🕒 Pilih session baru:", rows, threadID)
		}
		return true

	case strings.HasPrefix(data, "editv:"):
		parts := strings.SplitN(data, ":", 4)
		if len(parts) != 4 {
			return true
		}
		field, val, tradeID := parts[1], parts[2], parts[3]
		oldTrade, err := h.findUserTrade(ctx, userID, tradeID)
		if err != nil {
			_, _ = h.sender.SendText(ctx, chatID, "❌ Trade tidak ditemukan.", threadID)
			return true
		}
		newTrade := *oldTrade
		switch field {
		case "dir":
			newTrade.Direction = domain.Direction(val)
		case "session":
			newTrade.TimeWindow = domain.TimeWindow(val)
		}
		h.applyTradeUpdate(ctx, userID, oldTrade, &newTrade, chatID, threadID)
		return true
	}

	return false
}

func (h *Handler) handleTradeEditInput(ctx context.Context, msg *Message, sess *TradeEditSession) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}
	newTrade := sess.NewTrade
	switch sess.Field {
	case EditFieldSymbol:
		newTrade.Symbol = strings.ToUpper(strings.ReplaceAll(truncateUTF8(text, maxSymbolLen), "/", ""))
		newTrade.AssetType = domain.DetectAssetType(newTrade.Symbol)
	case EditFieldConfluence:
		newTrade.Confluence = truncateUTF8(text, maxTextFieldLen)
	case EditFieldResultRR:
		v, err := strconv.ParseFloat(text, 64)
		if err != nil {
			_, _ = h.sender.SendText(ctx, msg.Chat.ID, "❌ Masukkan angka yang valid untuk RR (contoh: 2, 1.5).", sess.ThreadID)
			return
		}
		if newTrade.Status == domain.StatusWin && v < 0 {
			v = -v
		} else if newTrade.Status == domain.StatusLoss && v > 0 {
			v = -v
		}
		newTrade.ResultRR = v
	}
	h.removeEditSession(msg.From.ID)
	h.applyTradeUpdate(ctx, msg.From.ID, &sess.OldTrade, &newTrade, sess.ChatID, sess.ThreadID)
}
