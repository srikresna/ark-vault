// Package api serves the web CRM that fronts the same data the Telegram bot
// writes: members, trades, gamification and stats.
//
// Authentication is deliberately two-door. Discord OAuth is the primary login,
// and the Telegram Login Widget is the second door so a member who never joined
// the Discord server can still reach their own journal. Both doors end at the
// same session, and a session always resolves to a member row, which is where
// the role lives.
package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/arkcode369/ark-vault/internal/adapter/postgres"
	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/internal/service"
)

// Config is everything the API needs that the bot does not already own.
type Config struct {
	// TelegramToken verifies the Telegram Login Widget signature. The same bot
	// token the bot polls with; it is never sent to the browser.
	TelegramToken string
	// DiscordClientID/Secret/RedirectURL configure the OAuth code exchange.
	// Empty values disable the Discord door.
	DiscordClientID     string
	DiscordClientSecret string
	DiscordRedirectURL  string
	// PublicBaseURL is the origin used to build absolute redirects.
	PublicBaseURL string
	// CookieSecure must be true in production: session cookies are not sent
	// over plain HTTP.
	CookieSecure bool
	// SessionCookieName is the cookie carrying the session id.
	SessionCookieName string
}

// Deps are the collaborators the handlers use.
type Deps struct {
	Members  *postgres.MemberRepo
	Trades   *postgres.TradeRepo
	Gam      *postgres.GamificationRepo
	Badges   *postgres.BadgeRepo
	Sessions *postgres.SessionRepo
	Images   *postgres.ImageStore

	Journal     *service.JournalService
	Leaderboard *service.LeaderboardService
	GamService  *service.GamificationService
	PostTrade   *service.PostTradeService
	BadgeSvc    *service.BadgeService

	Logger *slog.Logger
}

// Server is the HTTP handler tree.
type Server struct {
	cfg  Config
	deps Deps
	mux  *http.ServeMux
}

// New builds the API server and its routes.
func New(cfg Config, deps Deps) *Server {
	if cfg.SessionCookieName == "" {
		cfg.SessionCookieName = "ark_session"
	}
	s := &Server{cfg: cfg, deps: deps, mux: http.NewServeMux()}
	s.routes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A single origin serves the SPA and the API, so no CORS preflight is
	// needed. Security headers still apply to every response.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	m := s.mux

	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// Auth
	m.HandleFunc("GET /api/auth/discord/start", s.handleDiscordStart)
	m.HandleFunc("GET /api/auth/discord/callback", s.handleDiscordCallback)
	m.HandleFunc("POST /api/auth/telegram", s.handleTelegramLogin)
	m.HandleFunc("POST /api/auth/logout", s.handleLogout)
	m.HandleFunc("GET /api/me", s.withAuth(s.handleMe))

	// Journal
	m.HandleFunc("GET /api/trades", s.withAuth(s.handleListTrades))
	m.HandleFunc("POST /api/trades", s.withAuth(s.handleCreateTrade))
	m.HandleFunc("GET /api/trades/{id}", s.withAuth(s.handleGetTrade))
	m.HandleFunc("PATCH /api/trades/{id}", s.withAuth(s.handleUpdateTrade))
	m.HandleFunc("DELETE /api/trades/{id}", s.withAuth(s.handleDeleteTrade))
	m.HandleFunc("POST /api/trades/{id}/screenshot", s.withAuth(s.handleUploadScreenshot))
	m.HandleFunc("GET /api/trades/{id}/screenshot", s.withAuth(s.handleGetScreenshot))

	// Analytics
	m.HandleFunc("GET /api/stats", s.withAuth(s.handleStats))
	m.HandleFunc("GET /api/profile", s.withAuth(s.handleProfile))
	m.HandleFunc("GET /api/leaderboard", s.withAuth(s.handleLeaderboard))

	// Administration
	m.HandleFunc("GET /api/members", s.withAuth(s.handleListMembers))
	m.HandleFunc("PATCH /api/members/{id}/role", s.withAuth(s.handleSetRole))
}

// --- session middleware -----------------------------------------------------

// withAuth resolves the session cookie to a member and rejects anonymous
// requests. The member is the only identity the handlers trust; they never read
// a member id from the request body.
func (s *Server) withAuth(next func(http.ResponseWriter, *http.Request, *domain.Member)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(s.cfg.SessionCookieName)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		member, err := s.deps.Sessions.GetSessionMember(r.Context(), cookie.Value)
		if err != nil {
			s.deps.Logger.Error("resolve session failed", "error", err)
			writeError(w, http.StatusInternalServerError, "session lookup failed")
			return
		}
		if member == nil {
			// Expired or forged: clear it so the browser stops sending it.
			s.clearSessionCookie(w)
			writeError(w, http.StatusUnauthorized, "session expired")
			return
		}
		next(w, r, member)
	}
}

// canActOn reports whether actor may read or change data owned by owner.
//
// This is the single authorization rule. Every handler that accepts a member
// id from the client routes it through here, so an added endpoint cannot
// accidentally skip the check by forgetting to look at the role.
func canActOn(actor, owner *domain.Member) bool {
	if actor == nil || owner == nil {
		return false
	}
	if actor.ID == owner.ID {
		return true
	}
	return actor.Role.CanManageOthers()
}

// --- Discord OAuth ----------------------------------------------------------

func (s *Server) handleDiscordStart(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DiscordClientID == "" || s.cfg.DiscordRedirectURL == "" {
		writeError(w, http.StatusServiceUnavailable, "discord login is not configured")
		return
	}
	// The redirect target is a relative path only: an absolute URL would let a
	// crafted link turn the login into an open redirect.
	redirectTo := sanitizeRedirect(r.URL.Query().Get("redirect"))
	state, err := s.deps.Sessions.CreateOAuthState(r.Context(), "discord", "", redirectTo)
	if err != nil {
		s.deps.Logger.Error("create oauth state failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not start login")
		return
	}
	q := url.Values{
		"client_id":     {s.cfg.DiscordClientID},
		"redirect_uri":  {s.cfg.DiscordRedirectURL},
		"response_type": {"code"},
		"scope":         {"identify"},
		"state":         {state},
	}
	http.Redirect(w, r, "https://discord.com/oauth2/authorize?"+q.Encode(), http.StatusFound)
}

// discordUser is the subset of the Discord profile the app needs.
type discordUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

func (s *Server) handleDiscordCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if errParam := q.Get("error"); errParam != "" {
		writeError(w, http.StatusBadRequest, "discord login was cancelled")
		return
	}
	code := q.Get("code")
	state := q.Get("state")
	if code == "" || state == "" {
		writeError(w, http.StatusBadRequest, "missing code or state")
		return
	}
	// Consuming the state deletes it, so a replayed callback cannot log in twice.
	pending, err := s.deps.Sessions.ConsumeOAuthState(r.Context(), state)
	if err != nil {
		writeError(w, http.StatusBadRequest, "login state is invalid or expired")
		return
	}

	profile, err := s.exchangeDiscordCode(r.Context(), code)
	if err != nil {
		s.deps.Logger.Error("discord token exchange failed", "error", err)
		writeError(w, http.StatusBadGateway, "discord login failed")
		return
	}

	// A linked Discord account is the normal path. An unlinked one means this
	// person must first prove the Telegram identity that owns the journal, so
	// they are sent to the linking screen instead of getting a session.
	member, err := s.deps.Members.GetMemberByDiscordID(r.Context(), profile.ID)
	if err != nil {
		s.deps.Logger.Error("discord member lookup failed", "error", err)
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}
	if member == nil {
		http.Redirect(w, r, s.cfg.PublicBaseURL+"/link?discord="+url.QueryEscape(profile.ID), http.StatusFound)
		return
	}

	if err := s.issueSession(r.Context(), w, member); err != nil {
		s.deps.Logger.Error("create session failed", "error", err)
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}
	target := sanitizeRedirect(pending.RedirectTo)
	if target == "" {
		target = "/"
	}
	http.Redirect(w, r, s.cfg.PublicBaseURL+target, http.StatusFound)
}

// exchangeDiscordCode swaps an authorization code for the user's profile.
func (s *Server) exchangeDiscordCode(ctx context.Context, code string) (*discordUser, error) {
	form := url.Values{
		"client_id":     {s.cfg.DiscordClientID},
		"client_secret": {s.cfg.DiscordClientSecret},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {s.cfg.DiscordRedirectURL},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://discord.com/api/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint status %d", resp.StatusCode)
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if token.AccessToken == "" {
		return nil, fmt.Errorf("token response had no access token")
	}

	meReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://discord.com/api/users/@me", nil)
	if err != nil {
		return nil, err
	}
	meReq.Header.Set("Authorization", "Bearer "+token.AccessToken)
	meResp, err := client.Do(meReq)
	if err != nil {
		return nil, fmt.Errorf("profile request: %w", err)
	}
	defer func() { _ = meResp.Body.Close() }()
	if meResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("profile endpoint status %d", meResp.StatusCode)
	}
	var user discordUser
	if err := json.NewDecoder(io.LimitReader(meResp.Body, 1<<16)).Decode(&user); err != nil {
		return nil, fmt.Errorf("decode profile: %w", err)
	}
	if user.ID == "" {
		return nil, fmt.Errorf("profile had no id")
	}
	return &user, nil
}

// --- Telegram Login Widget --------------------------------------------------

// telegramAuthPayload is what the Login Widget posts back.
type telegramAuthPayload struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
	PhotoURL  string `json:"photo_url"`
	AuthDate  int64  `json:"auth_date"`
	Hash      string `json:"hash"`
	// DiscordID is set when the flow started from the linking screen: the
	// member proved their Telegram identity and wants Discord attached.
	DiscordID string `json:"discord_id"`
}

// verifyTelegramAuth checks the widget signature.
//
// Telegram signs a sorted, newline-joined "key=value" string with
// HMAC-SHA256 keyed by SHA256(bot token). The signature is compared in constant
// time, and auth_date is bounded so a captured payload cannot be replayed
// forever.
func verifyTelegramAuth(token string, p telegramAuthPayload, now time.Time, maxAge time.Duration) error {
	if p.ID == 0 || p.Hash == "" {
		return fmt.Errorf("telegram payload is incomplete")
	}
	if now.Sub(time.Unix(p.AuthDate, 0)) > maxAge {
		return fmt.Errorf("telegram login has expired")
	}
	pairs := map[string]string{
		"auth_date":  fmt.Sprintf("%d", p.AuthDate),
		"first_name": p.FirstName,
		"id":         fmt.Sprintf("%d", p.ID),
	}
	if p.LastName != "" {
		pairs["last_name"] = p.LastName
	}
	if p.Username != "" {
		pairs["username"] = p.Username
	}
	if p.PhotoURL != "" {
		pairs["photo_url"] = p.PhotoURL
	}
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(pairs[k])
	}

	secret := sha256.Sum256([]byte(token))
	mac := hmac.New(sha256.New, secret[:])
	mac.Write([]byte(sb.String()))
	expected := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(expected), []byte(strings.ToLower(p.Hash))) != 1 {
		return fmt.Errorf("telegram signature is invalid")
	}
	return nil
}

func (s *Server) handleTelegramLogin(w http.ResponseWriter, r *http.Request) {
	var payload telegramAuthPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := verifyTelegramAuth(s.cfg.TelegramToken, payload, time.Now(), 24*time.Hour); err != nil {
		s.deps.Logger.Warn("telegram login rejected", "error", err)
		writeError(w, http.StatusUnauthorized, "telegram login could not be verified")
		return
	}

	name := payload.FirstName
	if payload.LastName != "" {
		name = strings.TrimSpace(payload.FirstName + " " + payload.LastName)
	}
	member, err := s.deps.Members.EnsureMember(r.Context(), &domain.Member{
		TelegramID: payload.ID,
		Username:   payload.Username,
		FirstName:  name,
	})
	if err != nil {
		s.deps.Logger.Error("ensure member failed", "error", err)
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}

	// Linking mode: attach the Discord account that could not be matched.
	if payload.DiscordID != "" {
		if err := s.deps.Members.LinkDiscord(r.Context(), member.ID, payload.DiscordID); err != nil {
			s.deps.Logger.Warn("link discord failed", "error", err)
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		member.DiscordID = payload.DiscordID
	}

	if err := s.issueSession(r.Context(), w, member); err != nil {
		s.deps.Logger.Error("create session failed", "error", err)
		writeError(w, http.StatusInternalServerError, "login failed")
		return
	}
	writeJSON(w, http.StatusOK, toMemberView(member))
}

// --- session helpers --------------------------------------------------------

func (s *Server) issueSession(ctx context.Context, w http.ResponseWriter, member *domain.Member) error {
	id, err := s.deps.Sessions.CreateSession(ctx, member.ID)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.SessionCookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(postgres.SessionTTL),
	})
	return nil
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(s.cfg.SessionCookieName); err == nil && cookie.Value != "" {
		if err := s.deps.Sessions.DeleteSession(r.Context(), cookie.Value); err != nil {
			s.deps.Logger.Warn("delete session failed", "error", err)
		}
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// sanitizeRedirect keeps only a local absolute path, so the login flow cannot
// be turned into an open redirect.
func sanitizeRedirect(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return ""
	}
	if strings.ContainsAny(raw, "\\\r\n") {
		return ""
	}
	return raw
}

// --- small helpers ----------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeJSON decodes a request body with a size cap and rejects unknown fields,
// so a client typo surfaces as a 400 instead of being silently ignored.
func decodeJSON(r *http.Request, dest any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(dest)
}
