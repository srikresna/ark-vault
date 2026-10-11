package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
)

// ReminderService manages daily reminder preferences and due-reminder queries.
type ReminderService struct {
	store   ReminderStore
	streaks GamificationStore
	logger  *slog.Logger
}

// NewReminderService creates a new ReminderService.
func NewReminderService(store ReminderStore, streaks GamificationStore, logger *slog.Logger) *ReminderService {
	return &ReminderService{
		store:   store,
		streaks: streaks,
		logger:  logger,
	}
}

// SetReminder enables or disables a daily reminder for a user.
func (s *ReminderService) SetReminder(ctx context.Context, telegramID int64, chatID int64, threadID int, enabled bool, hour int) error {
	if hour < 0 || hour > 23 {
		return fmt.Errorf("hour must be between 0 and 23, got %d", hour)
	}
	pref := &domain.ReminderPreference{
		TelegramID:  telegramID,
		Enabled:     enabled,
		Hour:        hour,
		ChatID:      chatID,
		ThreadID:    threadID,
		IsCommunity: chatID < 0 || chatID != telegramID,
	}
	return s.store.SaveReminderPref(ctx, pref)
}

// GetReminder returns the reminder preference for a user.
func (s *ReminderService) GetReminder(ctx context.Context, telegramID int64) (*domain.ReminderPreference, error) {
	return s.store.GetReminderPref(ctx, telegramID)
}

// GetDueReminders returns users who should receive a reminder at the current WIB hour
// and who have not yet logged a trade today.
func (s *ReminderService) GetDueReminders(ctx context.Context) ([]domain.ReminderPreference, error) {
	enabled, err := s.store.ListEnabledReminders(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now().In(timeutil.WIB)
	currentHour := now.Hour()
	today := now.Format("2006-01-02")

	var due []domain.ReminderPreference
	for _, pref := range enabled {
		if pref.Hour != currentHour {
			continue
		}
		if pref.IsCommunity || pref.ChatID < 0 {
			pref.IsCommunity = true
			due = append(due, pref)
			continue
		}

		// Personal DM reminder: check if the user already logged a trade today.
		streak, err := s.streaks.GetStreak(ctx, pref.TelegramID)
		if err != nil {
			s.logger.Warn("failed to get streak for reminder check",
				slog.Int64("telegram_id", pref.TelegramID),
				slog.String("error", err.Error()),
			)
			continue
		}
		if streak != nil && streak.LastLogDate == today {
			// Already logged today, no reminder needed.
			continue
		}

		due = append(due, pref)
	}
	return due, nil
}
