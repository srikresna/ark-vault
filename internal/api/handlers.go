package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// memberView is the wire shape of a member. The Telegram and Discord ids are
// included because the UI shows which identities are linked, but no token or
// session material ever appears here.
type memberView struct {
	ID         string `json:"id"`
	TelegramID int64  `json:"telegram_id"`
	DiscordID  string `json:"discord_id,omitempty"`
	Username   string `json:"username"`
	FirstName  string `json:"first_name"`
	Role       string `json:"role"`
	JoinedAt   string `json:"joined_at"`
}

func toMemberView(m *domain.Member) memberView {
	return memberView{
		ID:         m.ID,
		TelegramID: m.TelegramID,
		DiscordID:  m.DiscordID,
		Username:   m.Username,
		FirstName:  m.FirstName,
		Role:       string(m.Role),
		JoinedAt:   m.JoinDate.Format(time.RFC3339),
	}
}

// tradeView is the wire shape of a trade.
type tradeView struct {
	ID            string  `json:"id"`
	Symbol        string  `json:"symbol"`
	Direction     string  `json:"direction"`
	AssetType     string  `json:"asset_type"`
	Status        string  `json:"status"`
	Date          string  `json:"date"`
	ResultRR      float64 `json:"result_rr"`
	TimeWindow    string  `json:"time_window"`
	Confluence    string  `json:"confluence"`
	Notes         string  `json:"notes"`
	HasScreenshot bool    `json:"has_screenshot"`
	ScreenshotURL string  `json:"screenshot_url,omitempty"`
	CreatedAt     string  `json:"created_at"`
	OwnerID       string  `json:"owner_id,omitempty"`
	OwnerUsername string  `json:"owner_username,omitempty"`
}

func toTradeView(t domain.Trade) tradeView {
	v := tradeView{
		ID:            t.ID,
		Symbol:        t.Symbol,
		Direction:     string(t.Direction),
		AssetType:     t.AssetType.String(),
		Status:        string(t.Status),
		Date:          t.Date.Format("2006-01-02"),
		ResultRR:      t.ResultRR,
		TimeWindow:    string(t.TimeWindow),
		Confluence:    t.Confluence,
		Notes:         t.Notes,
		HasScreenshot: t.ScreenshotURL != "",
		CreatedAt:     t.CreatedAt.Format(time.RFC3339),
	}
	if t.ScreenshotURL != "" {
		v.ScreenshotURL = "/api/trades/" + t.ID + "/screenshot"
	}
	return v
}

func (s *Server) handleMe(w http.ResponseWriter, _ *http.Request, member *domain.Member) {
	writeJSON(w, http.StatusOK, toMemberView(member))
}

// targetMember resolves which member a request acts on.
//
// The client may name a member, but only an admin may name somebody else; a
// member naming anyone but themselves is rejected rather than silently scoped,
// so a UI bug surfaces as an error instead of writing to the wrong journal.
func (s *Server) targetMember(r *http.Request, actor *domain.Member) (*domain.Member, error) {
	requested := strings.TrimSpace(r.URL.Query().Get("member"))
	if requested == "" || requested == actor.ID {
		return actor, nil
	}
	target, err := s.deps.Members.GetMemberByID(r.Context(), requested)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, errNotFound
	}
	if !canActOn(actor, target) {
		return nil, errForbidden
	}
	return target, nil
}

var (
	errNotFound  = errors.New("not found")
	errForbidden = errors.New("forbidden")
)

func (s *Server) handleListTrades(w http.ResponseWriter, r *http.Request, member *domain.Member) {
	target, err := s.targetMember(r, member)
	if err != nil {
		writeTargetError(w, err)
		return
	}
	trades, err := s.deps.Trades.GetTrades(r.Context(), target.TelegramID)
	if err != nil {
		s.deps.Logger.Error("list trades failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not load trades")
		return
	}
	out := make([]tradeView, 0, len(trades))
	for _, t := range trades {
		v := toTradeView(t)
		v.OwnerID = target.ID
		v.OwnerUsername = target.Username
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"trades": out})
}

// createTradeRequest is the client payload for recording a trade.
type createTradeRequest struct {
	Symbol     string  `json:"symbol"`
	Direction  string  `json:"direction"`
	AssetType  string  `json:"asset_type"`
	Status     string  `json:"status"`
	Date       string  `json:"date"`
	ResultRR   float64 `json:"result_rr"`
	TimeWindow string  `json:"time_window"`
	Confluence string  `json:"confluence"`
	Notes      string  `json:"notes"`
}

func (s *Server) handleCreateTrade(w http.ResponseWriter, r *http.Request, member *domain.Member) {
	var req createTradeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	trade, err := tradeFromRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The web app writes through the same service the bot uses, so validation,
	// WIB date stamping and asset detection cannot drift between the two.
	if err := s.deps.Journal.RecordTrade(r.Context(), member.TelegramID,
		member.Username, member.FirstName, trade); err != nil {
		s.deps.Logger.Error("record trade failed", "error", err)
		writeError(w, http.StatusBadRequest, userMessage(err))
		return
	}
	// Rewards run after the trade is durable, exactly like the bot path, so a
	// gamification failure never loses the journal entry.
	if s.deps.PostTrade != nil {
		if result, err := s.deps.PostTrade.ApplyTradeRewards(r.Context(), member.TelegramID, trade); err != nil {
			s.deps.Logger.Error("trade rewards failed", "error", err)
		} else if result != nil {
			rewards := s.deps.PostTrade.EvaluateRewards(r.Context(), member.TelegramID, trade)
			writeJSON(w, http.StatusCreated, map[string]any{
				"trade":  toTradeView(*trade),
				"xp":     result,
				"badges": rewards.Badges,
				"goal":   rewards.GoalAchieved,
			})
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"trade": toTradeView(*trade)})
}

func (s *Server) handleGetTrade(w http.ResponseWriter, r *http.Request, member *domain.Member) {
	trade, err := s.deps.Trades.GetTrade(r.Context(), member.TelegramID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load trade")
		return
	}
	if trade == nil {
		// A trade owned by somebody else is reported as missing, not forbidden:
		// the id space is not a directory to probe.
		writeError(w, http.StatusNotFound, "trade not found")
		return
	}
	writeJSON(w, http.StatusOK, toTradeView(*trade))
}

func (s *Server) handleUpdateTrade(w http.ResponseWriter, r *http.Request, member *domain.Member) {
	existing, err := s.deps.Trades.GetTrade(r.Context(), member.TelegramID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load trade")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "trade not found")
		return
	}
	var req createTradeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	updated, err := tradeFromRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated.ID = existing.ID
	updated.MemberID = existing.MemberID
	if updated.Date.IsZero() {
		updated.Date = existing.Date
	}
	updated.AutoDetectAsset()
	if err := updated.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.deps.Trades.UpdateTrade(r.Context(), member.TelegramID, updated); err != nil {
		s.deps.Logger.Error("update trade failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not update trade")
		return
	}
	// XP reconciliation is best-effort here: the trade is already correct, and
	// a reward failure is reported rather than rolled back.
	if s.deps.GamService != nil {
		if _, _, err := s.deps.GamService.AdjustXPForTradeChange(r.Context(),
			member.TelegramID, existing, updated); err != nil {
			s.deps.Logger.Error("xp adjustment failed", "error", err)
		}
	}
	writeJSON(w, http.StatusOK, toTradeView(*updated))
}

func (s *Server) handleDeleteTrade(w http.ResponseWriter, r *http.Request, member *domain.Member) {
	existing, err := s.deps.Trades.GetTrade(r.Context(), member.TelegramID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load trade")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "trade not found")
		return
	}
	if err := s.deps.Trades.DeleteTrade(r.Context(), member.TelegramID, existing.ID); err != nil {
		s.deps.Logger.Error("delete trade failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not delete trade")
		return
	}
	if s.deps.GamService != nil {
		if _, _, err := s.deps.GamService.AdjustXPForTradeChange(r.Context(),
			member.TelegramID, existing, nil); err != nil {
			s.deps.Logger.Error("xp adjustment after delete failed", "error", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, member *domain.Member) {
	target, err := s.targetMember(r, member)
	if err != nil {
		writeTargetError(w, err)
		return
	}
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "all"
	}
	stats, err := s.deps.Journal.GetMemberStatsWithFilter(r.Context(), target.TelegramID,
		domain.StatsFilter{
			Period:  period,
			Symbol:  r.URL.Query().Get("symbol"),
			Session: domain.TimeWindow(r.URL.Query().Get("session")),
		})
	if err != nil {
		s.deps.Logger.Error("stats failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not load stats")
		return
	}
	if stats == nil {
		writeJSON(w, http.StatusOK, map[string]any{"stats": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": stats})
}

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request, member *domain.Member) {
	target, err := s.targetMember(r, member)
	if err != nil {
		writeTargetError(w, err)
		return
	}
	ctx := r.Context()
	profile, err := s.deps.Gam.GetProfile(ctx, target.TelegramID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load profile")
		return
	}
	streak, err := s.deps.Gam.GetStreak(ctx, target.TelegramID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load streak")
		return
	}
	badges, err := s.deps.Badges.GetBadges(ctx, target.TelegramID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load badges")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"member":  toMemberView(target),
		"profile": profile,
		"streak":  streak,
		"badges":  badges,
	})
}

func (s *Server) handleLeaderboard(w http.ResponseWriter, r *http.Request, _ *domain.Member) {
	metric := r.URL.Query().Get("metric")
	if metric == "" {
		metric = "rr"
	}
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "month"
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	entries, err := s.deps.Leaderboard.GetLeaderboardByPeriod(r.Context(), metric, period, limit, 1)
	if err != nil {
		s.deps.Logger.Error("leaderboard failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not load leaderboard")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request, member *domain.Member) {
	if !member.Role.CanManageOthers() {
		writeError(w, http.StatusForbidden, "admin role required")
		return
	}
	members, err := s.deps.Members.ListMembers(r.Context())
	if err != nil {
		s.deps.Logger.Error("list members failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not load members")
		return
	}
	out := make([]memberView, 0, len(members))
	for i := range members {
		out = append(out, toMemberView(&members[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}

// handleSetRole lets an owner promote or demote another member.
//
// Only the owner may do this: an admin promoting themselves to owner would be a
// privilege escalation, and an admin demoting the owner would lock the community
// out of its own tool.
func (s *Server) handleSetRole(w http.ResponseWriter, r *http.Request, member *domain.Member) {
	if member.Role != domain.RoleOwner {
		writeError(w, http.StatusForbidden, "owner role required")
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	role := domain.Role(req.Role)
	if !role.Valid() {
		writeError(w, http.StatusBadRequest, "unknown role")
		return
	}
	targetID := r.PathValue("id")
	if targetID == member.ID && role != domain.RoleOwner {
		writeError(w, http.StatusBadRequest, "cannot demote yourself")
		return
	}
	if err := s.deps.Members.SetRole(r.Context(), targetID, role); err != nil {
		writeError(w, http.StatusBadRequest, userMessage(err))
		return
	}
	_ = s.deps.Sessions.Audit(r.Context(), &member.ID, "set_role", map[string]string{
		"target": targetID, "role": string(role),
	})
	writeJSON(w, http.StatusOK, map[string]string{"id": targetID, "role": string(role)})
}

// handleUploadScreenshot stores an image for a trade the caller owns.
func (s *Server) handleUploadScreenshot(w http.ResponseWriter, r *http.Request, member *domain.Member) {
	trade, err := s.deps.Trades.GetTrade(r.Context(), member.TelegramID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load trade")
		return
	}
	if trade == nil {
		writeError(w, http.StatusNotFound, "trade not found")
		return
	}
	// 5 MiB cap: a screenshot is a phone photo, and an unbounded body would let
	// one request exhaust the container's memory limit.
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20)
	if err := r.ParseMultipartForm(5 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "image is too large or malformed")
		return
	}
	file, header, err := r.FormFile("image")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing image field")
		return
	}
	defer func() { _ = file.Close() }()
	data := make([]byte, 0, header.Size)
	buf := make([]byte, 32<<10)
	for {
		n, readErr := file.Read(buf)
		data = append(data, buf[:n]...)
		if readErr != nil {
			break
		}
	}
	key, err := s.deps.Images.Upload(r.Context(), trade.ID, header.Filename, data)
	if err != nil {
		s.deps.Logger.Error("screenshot upload failed", "error", err)
		writeError(w, http.StatusBadRequest, "could not store image")
		return
	}
	if err := s.deps.Trades.SetScreenshot(r.Context(), member.TelegramID, trade.ID, key); err != nil {
		s.deps.Logger.Error("screenshot attach failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not attach image")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"screenshot_url": "/api/trades/" + trade.ID + "/screenshot",
	})
}

func (s *Server) handleGetScreenshot(w http.ResponseWriter, r *http.Request, member *domain.Member) {
	trade, err := s.deps.Trades.GetTrade(r.Context(), member.TelegramID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load trade")
		return
	}
	if trade == nil || trade.ScreenshotURL == "" {
		writeError(w, http.StatusNotFound, "screenshot not found")
		return
	}
	f, size, err := s.deps.Images.Open(trade.ScreenshotURL)
	if err != nil {
		writeError(w, http.StatusNotFound, "screenshot not found")
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, trade.ScreenshotURL, time.Time{}, f)
	_ = size
}

// --- request helpers --------------------------------------------------------

// tradeFromRequest converts a client payload into a domain trade.
//
// The conversion is strict on purpose: an unknown direction or status is an
// error rather than a default, so a UI bug cannot silently record a WIN as an
// OPEN trade.
func tradeFromRequest(req createTradeRequest) (*domain.Trade, error) {
	symbol := strings.ToUpper(strings.TrimSpace(req.Symbol))
	if symbol == "" {
		return nil, errors.New("symbol is required")
	}
	direction := domain.Direction(strings.ToUpper(strings.TrimSpace(req.Direction)))
	if direction != domain.DirBuy && direction != domain.DirSell {
		return nil, errors.New("direction must be BUY or SELL")
	}
	status := domain.TradeStatus(strings.ToUpper(strings.TrimSpace(req.Status)))
	if status == "" {
		status = domain.StatusOpen
	}
	switch status {
	case domain.StatusOpen, domain.StatusWin, domain.StatusLoss, domain.StatusBE:
	default:
		return nil, errors.New("status must be OPEN, WIN, LOSS or BE")
	}
	var date time.Time
	if req.Date != "" {
		parsed, err := time.Parse("2006-01-02", req.Date)
		if err != nil {
			return nil, errors.New("date must be YYYY-MM-DD")
		}
		date = parsed
	}
	trade := &domain.Trade{
		Symbol:     symbol,
		Direction:  direction,
		Status:     status,
		Date:       date,
		ResultRR:   req.ResultRR,
		TimeWindow: domain.TimeWindow(req.TimeWindow),
		Confluence: strings.TrimSpace(req.Confluence),
		Notes:      strings.TrimSpace(req.Notes),
	}
	if at, ok := domain.ParseAssetType(req.AssetType); ok {
		trade.AssetType = at
	}
	return trade, nil
}

// userMessage strips the internal wrapper so a validation error reaches the UI
// as a sentence instead of "validation: symbol is required".
func userMessage(err error) string {
	msg := err.Error()
	for _, prefix := range []string{"validation: ", "save trade: ", "update trade: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	return msg
}

func writeTargetError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errForbidden):
		writeError(w, http.StatusForbidden, "you may only access your own journal")
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "member not found")
	default:
		writeError(w, http.StatusInternalServerError, "request failed")
	}
}
