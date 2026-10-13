package service

import (
	"context"
	"fmt"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
)

// JournalService orchestrates trade recording.
type JournalService struct {
	trades  TradeRepository
	members MemberRepository
	images  ImageStore
}

// NewJournalService creates a new JournalService.
func NewJournalService(tr TradeRepository, mr MemberRepository, is ImageStore) *JournalService {
	return &JournalService{trades: tr, members: mr, images: is}
}

// RecordTrade validates, enriches, and persists a new trade.
func (s *JournalService) RecordTrade(ctx context.Context, memberTgID int64, username, firstName string, trade *domain.Trade) error {
	trade.AutoDetectAsset()
	if err := trade.Validate(); err != nil {
		return fmt.Errorf("validation: %w", err)
	}
	if trade.Date.IsZero() {
		// Stamp the WIB calendar day: every downstream boundary (daily XP cap,
		// streaks, reminders, week/month filters) is evaluated in WIB, so a UTC
		// stamp would file trades recorded between 00:00 and 07:00 WIB under the
		// previous day in Notion.
		trade.Date = time.Now().In(timeutil.WIB)
	}
	if trade.Status == "" {
		trade.Status = domain.StatusOpen
	}
	trade.MemberID = memberTgID

	// Ensure member exists in Notion
	member := &domain.Member{
		TelegramID: memberTgID,
		Username:   username,
		FirstName:  firstName,
		JoinDate:   time.Now().In(timeutil.WIB),
	}
	if _, err := s.members.EnsureMember(ctx, member); err != nil {
		return fmt.Errorf("ensure member: %w", err)
	}

	// Save trade
	if err := s.trades.SaveTrade(ctx, memberTgID, trade); err != nil {
		return fmt.Errorf("save trade: %w", err)
	}

	return nil
}

// UploadScreenshot attaches a screenshot to an existing trade entry and
// returns the Notion file reference. The caller stores it on the trade so the
// gamification screenshot bonus (and later XP reconciliation) can see it.
func (s *JournalService) UploadScreenshot(ctx context.Context, tradeID string, filename string, data []byte) (string, error) {
	if s.images == nil {
		return "", nil // image store not configured
	}
	ref, err := s.images.Upload(ctx, tradeID, filename, data)
	return ref, err
}

// GetMemberStats retrieves all trades for a member and calculates stats.
func (s *JournalService) GetMemberStats(ctx context.Context, memberTgID int64) (*domain.Stats, error) {
	return s.GetMemberStatsByPeriod(ctx, memberTgID, "all")
}

// GetMemberStatsByPeriod retrieves trades for a member filtered by period ("week", "month", "all") and calculates stats.
func (s *JournalService) GetMemberStatsByPeriod(ctx context.Context, memberTgID int64, period string) (*domain.Stats, error) {
	return s.GetMemberStatsWithFilter(ctx, memberTgID, domain.StatsFilter{Period: period})
}

// GetMemberStatsWithFilter retrieves trades for a member filtered by period, symbol, and session and calculates stats.
func (s *JournalService) GetMemberStatsWithFilter(ctx context.Context, memberTgID int64, filter domain.StatsFilter) (*domain.Stats, error) {
	trades, err := s.trades.GetTrades(ctx, memberTgID)
	if err != nil {
		return nil, fmt.Errorf("get trades: %w", err)
	}
	filtered := domain.FilterTrades(trades, filter, time.Now().In(timeutil.WIB))
	if len(filtered) == 0 {
		return nil, nil
	}
	stats := domain.CalculateStats(filtered)
	return &stats, nil
}

// GetOpenTrades returns all trades currently in OPEN status for a member.
func (s *JournalService) GetOpenTrades(ctx context.Context, memberTgID int64) ([]domain.Trade, error) {
	trades, err := s.trades.GetTrades(ctx, memberTgID)
	if err != nil {
		return nil, fmt.Errorf("get trades: %w", err)
	}
	var open []domain.Trade
	for _, t := range trades {
		if t.Status == domain.StatusOpen {
			open = append(open, t)
		}
	}
	return open, nil
}

// UpdateTrade validates and updates an existing trade entry.
func (s *JournalService) UpdateTrade(ctx context.Context, memberTgID int64, trade *domain.Trade) error {
	trade.AutoDetectAsset()
	if err := trade.Validate(); err != nil {
		return fmt.Errorf("validation: %w", err)
	}
	return s.trades.UpdateTrade(ctx, memberTgID, trade)
}

// DeleteTrade archives/deletes a trade entry by ID.
func (s *JournalService) DeleteTrade(ctx context.Context, memberTgID int64, tradeID string) error {
	return s.trades.DeleteTrade(ctx, memberTgID, tradeID)
}
