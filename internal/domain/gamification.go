package domain

import "time"

// GamificationProfile stores a member's XP and level data.
type GamificationProfile struct {
	TelegramID int64     `json:"telegram_id"`
	TotalXP    int       `json:"total_xp"`
	Level      int       `json:"level"`
	Title      string    `json:"title"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// StreakData tracks consecutive days of journaling.
type StreakData struct {
	TelegramID    int64     `json:"telegram_id"`
	CurrentStreak int       `json:"current_streak"`
	LongestStreak int       `json:"longest_streak"`
	LastLogDate   string    `json:"last_log_date"` // "2006-01-02" in WIB
	UpdatedAt     time.Time `json:"updated_at"`
}

// XPEvent records a single XP gain event.
type XPEvent struct {
	TelegramID int64     `json:"telegram_id"`
	Amount     int       `json:"amount"`
	Reason     string    `json:"reason"`
	Timestamp  time.Time `json:"timestamp"`
}

// TradeXPGrant records the trade-intrinsic XP that was actually granted for a
// single trade, so a later edit or delete can reconcile exactly what was
// awarded. This matters because the daily trade-XP cap may have granted
// nothing at record time; subtracting the full base XP then would claw back
// XP the member never received. Grants are never pruned by age: a trade can be
// edited or deleted at any later point, so a grant lives until its trade is
// deleted.
type TradeXPGrant struct {
	TelegramID int64     `json:"telegram_id"`
	TradeID    string    `json:"trade_id"`
	Base       int       `json:"base"`       // XPTradeLogged + win bonus actually granted
	Screenshot int       `json:"screenshot"` // XPScreenshot bonus actually granted
	GrantedAt  time.Time `json:"granted_at"`
}
