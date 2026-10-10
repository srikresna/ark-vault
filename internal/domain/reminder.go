package domain

// ReminderPreference stores a user's daily reminder settings.
type ReminderPreference struct {
	TelegramID  int64 `json:"telegram_id"`
	Enabled     bool  `json:"enabled"`
	Hour        int   `json:"hour"`                   // Hour in WIB (0-23) to send reminder
	ChatID      int64 `json:"chat_id"`                // Private chat ID or Group chat ID
	ThreadID    int   `json:"thread_id,omitempty"`    // Optional thread ID
	IsCommunity bool  `json:"is_community,omitempty"` // True when configured in a group/community thread
}
