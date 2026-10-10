package service

import (
	"context"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

type TradeRepository interface {
	SaveTrade(ctx context.Context, memberID int64, trade *domain.Trade) error
	GetTrades(ctx context.Context, memberID int64) ([]domain.Trade, error)
	UpdateTrade(ctx context.Context, memberID int64, trade *domain.Trade) error
	DeleteTrade(ctx context.Context, memberID int64, tradeID string) error
}

// MemberRepository defines persistence operations for members consumed by services.
type MemberRepository interface {
	EnsureMember(ctx context.Context, m *domain.Member) (*domain.Member, error)
	GetMember(ctx context.Context, telegramID int64) (*domain.Member, error)
	ListMembers(ctx context.Context) ([]domain.Member, error)
}

// ImageStore handles uploading trade screenshots.
type ImageStore interface {
	Upload(ctx context.Context, pageID string, filename string, data []byte) (string, error)
}

// AIAnalyzer provides AI-powered trade analysis.
type AIAnalyzer interface {
	AnalyzeTrades(ctx context.Context, prompt string) (string, error)
}

// GamificationStore provides storage for gamification data.
type GamificationStore interface {
	GetProfile(ctx context.Context, telegramID int64) (*domain.GamificationProfile, error)
	SaveProfile(ctx context.Context, profile *domain.GamificationProfile) error
	GetStreak(ctx context.Context, telegramID int64) (*domain.StreakData, error)
	SaveStreak(ctx context.Context, streak *domain.StreakData) error
	AppendXPEvent(ctx context.Context, event *domain.XPEvent) error
	GetXPEvents(ctx context.Context, telegramID int64, since time.Time) ([]domain.XPEvent, error)
}

// BadgeStore provides storage for badge awards.
type BadgeStore interface {
	GetBadges(ctx context.Context, telegramID int64) ([]domain.BadgeAward, error)
	HasBadge(ctx context.Context, telegramID int64, badgeID domain.BadgeID) (bool, error)
	AwardBadge(ctx context.Context, award *domain.BadgeAward) error
}

// ChallengeStore provides storage for weekly challenges.
type ChallengeStore interface {
	GetChallenge(ctx context.Context, yearWeek string) (*domain.WeeklyChallenge, error)
	SaveChallenge(ctx context.Context, challenge *domain.WeeklyChallenge) error
	GetChallengeEntry(ctx context.Context, yearWeek string, telegramID int64) (*domain.ChallengeEntry, error)
	SaveChallengeEntry(ctx context.Context, entry *domain.ChallengeEntry) error
	GetChallengeEntries(ctx context.Context, yearWeek string) ([]domain.ChallengeEntry, error)
}

// ReminderStore provides storage for reminder preferences.
type ReminderStore interface {
	GetReminderPref(ctx context.Context, telegramID int64) (*domain.ReminderPreference, error)
	SaveReminderPref(ctx context.Context, pref *domain.ReminderPreference) error
	ListEnabledReminders(ctx context.Context) ([]domain.ReminderPreference, error)
}

// GoalStore provides storage for monthly goals.
type GoalStore interface {
	GetGoal(ctx context.Context, telegramID int64, yearMonth string) (*domain.MonthlyGoal, error)
	SaveGoal(ctx context.Context, goal *domain.MonthlyGoal) error
	ListActiveGoals(ctx context.Context, yearMonth string) ([]domain.MonthlyGoal, error)
}

// AnalyticsCacheStore caches AI analytics results.
type AnalyticsCacheStore interface {
	GetAnalyticsCache(ctx context.Context, telegramID int64) (*domain.TradeAnalytics, error)
	SaveAnalyticsCache(ctx context.Context, analytics *domain.TradeAnalytics) error
}

// ReportCardStore stores monthly report cards.
type ReportCardStore interface {
	GetReportCard(ctx context.Context, telegramID int64, yearMonth string) (*domain.MonthlyReportCard, error)
	SaveReportCard(ctx context.Context, report *domain.MonthlyReportCard) error
}
