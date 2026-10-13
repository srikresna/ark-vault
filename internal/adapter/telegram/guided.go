package telegram

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// GuidedStep tracks which step a user is on in the guided journal flow.
type GuidedStep int

const (
	StepNone GuidedStep = iota
	StepAssetType
	StepSymbol
	StepDirection
	StepResult
	StepRRAmount
	StepTimeWindow
	StepConfluence
	StepScreenshot
	StepConfirm
)

// GuidedSession holds the in-progress state for one user's guided flow.
type GuidedSession struct {
	mu          sync.Mutex
	Step        GuidedStep
	Trade       domain.Trade
	ChatID      int64
	ThreadID    int
	PhotoURL    string
	PhotoFileID string
	Submitting  bool // prevents double submit
	ExpiresAt   time.Time
}

// GuidedFlow manages guided journal sessions per user.
type GuidedFlow struct {
	mu       sync.RWMutex
	sessions map[int64]*GuidedSession // keyed by Telegram user ID
	ttl      time.Duration
	done     chan struct{}
}

// NewGuidedFlow creates a new GuidedFlow.
func NewGuidedFlow() *GuidedFlow {
	gf := &GuidedFlow{
		sessions: make(map[int64]*GuidedSession),
		ttl:      15 * time.Minute,
		done:     make(chan struct{}),
	}
	go gf.cleanupLoop()
	return gf
}

// Stop terminates the cleanup goroutine.
func (gf *GuidedFlow) Stop() {
	close(gf.done)
}

// Start creates a new session for the user.
func (gf *GuidedFlow) Start(userID int64, chatID int64, threadID int) *GuidedSession {
	gf.mu.Lock()
	defer gf.mu.Unlock()
	s := &GuidedSession{
		Step:      StepAssetType,
		ChatID:    chatID,
		ThreadID:  threadID,
		ExpiresAt: time.Now().Add(gf.ttl),
	}
	gf.sessions[userID] = s
	return s
}

// Get returns the active session for a user, or nil.
func (gf *GuidedFlow) Get(userID int64) *GuidedSession {
	gf.mu.RLock()
	defer gf.mu.RUnlock()
	s, ok := gf.sessions[userID]
	if !ok || time.Now().After(s.ExpiresAt) {
		return nil
	}
	return s
}

// Remove deletes a user's session.
func (gf *GuidedFlow) Remove(userID int64) {
	gf.mu.Lock()
	defer gf.mu.Unlock()
	delete(gf.sessions, userID)
}

// StepPrompt returns the text and optional keyboard rows for the current step.
func StepPrompt(s *GuidedSession) (string, [][]InlineBtn) {
	switch s.Step {
	case StepAssetType:
		return "📊 <b>Step 1/9</b> — Pilih asset type:", [][]InlineBtn{
			{
				{Text: "Forex", Data: "asset:FOREX"},
				{Text: "Gold", Data: "asset:GOLD"},
			},
			{
				{Text: "Indices", Data: "asset:INDICES"},
				{Text: "Crypto", Data: "asset:CRYPTO"},
			},
		}
	case StepSymbol:
		return fmt.Sprintf("📊 <b>Step 2/9</b> — Masukkan pair/symbol:\n(Asset: %s)", s.Trade.AssetType.String()), nil
	case StepDirection:
		return "📊 <b>Step 3/9</b> — Direction:", [][]InlineBtn{
			{
				{Text: "🟢 BUY", Data: "dir:BUY"},
				{Text: "🔴 SELL", Data: "dir:SELL"},
			},
		}
	case StepResult:
		return "📊 <b>Step 4/9</b> — Result:", [][]InlineBtn{
			{
				{Text: "✅ WIN", Data: "result:WIN"},
				{Text: "❌ LOSS", Data: "result:LOSS"},
			},
			{
				{Text: "➖ BE", Data: "result:BE"},
				{Text: "⏳ OPEN", Data: "result:OPEN"},
			},
		}
	case StepRRAmount:
		return "📊 <b>Step 5/9</b> — Masukkan jumlah RR (contoh: 2, 1.5):", nil
	case StepTimeWindow:
		return "📊 <b>Step 6/9</b> — Time Window:", [][]InlineBtn{
			{
				{Text: "Asia", Data: "session:asia"},
				{Text: "London", Data: "session:london"},
			},
			{
				{Text: "NY AM", Data: "session:nyam"},
				{Text: "NY PM", Data: "session:nypm"},
			},
			{
				{Text: "⏭ Skip", Data: "session:skip"},
			},
		}
	case StepConfluence:
		return "📊 <b>Step 7/9</b> — Masukkan confluence (alasan entry, atau ketik 'skip'):", nil
	case StepScreenshot:
		return "📊 <b>Step 8/9</b> — Kirim screenshot trade (opsional):", [][]InlineBtn{
			{{Text: "⏭ Skip", Data: "screenshot:skip"}},
		}
	case StepConfirm:
		return buildConfirmText(s), [][]InlineBtn{
			{
				{Text: "✅ Submit", Data: "confirm:yes"},
				{Text: "❌ Cancel", Data: "confirm:no"},
			},
		}
	default:
		return "", nil
	}
}

// InlineBtn is a shorthand for guided flow buttons (not the port type).
type InlineBtn struct {
	Text string
	Data string
}

func buildConfirmText(s *GuidedSession) string {
	t := &s.Trade
	var sb strings.Builder
	sb.WriteString("📋 <b>Konfirmasi Trade:</b>\n\n")
	fmt.Fprintf(&sb, "Asset: <b>%s</b>\n", t.AssetType.String())
	fmt.Fprintf(&sb, "Symbol: <b>%s</b>\n", escapeHTML(t.Symbol))
	fmt.Fprintf(&sb, "Direction: <b>%s</b>\n", t.Direction)
	fmt.Fprintf(&sb, "Status: <b>%s</b>\n", t.Status)
	if t.Status == domain.StatusWin || t.Status == domain.StatusLoss {
		fmt.Fprintf(&sb, "Result RR: <b>%+.1fR</b>\n", t.ResultRR)
	}
	if t.TimeWindow != "" {
		fmt.Fprintf(&sb, "Session: <b>%s</b>\n", t.TimeWindow)
	}
	if t.Confluence != "" {
		fmt.Fprintf(&sb, "Confluence: <b>%s</b>\n", escapeHTML(t.Confluence))
	}
	if s.PhotoURL != "" || s.PhotoFileID != "" {
		sb.WriteString("Screenshot: ✅ attached\n")
	}
	sb.WriteString("\nKirim submit untuk menyimpan.")
	return sb.String()
}

func (gf *GuidedFlow) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-gf.done:
			return
		case <-ticker.C:
			gf.mu.Lock()
			now := time.Now()
			for uid, s := range gf.sessions {
				if now.After(s.ExpiresAt) {
					delete(gf.sessions, uid)
				}
			}
			gf.mu.Unlock()
		}
	}
}
