package service

import (
	"context"
	"fmt"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
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
		trade.Date = time.Now().UTC()
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
		JoinDate:   time.Now().UTC(),
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

// UploadScreenshot attaches a screenshot to an existing trade entry.
func (s *JournalService) UploadScreenshot(ctx context.Context, tradeID string, filename string, data []byte) error {
	if s.images == nil {
		return nil // image store not configured
	}
	_, err := s.images.Upload(ctx, tradeID, filename, data)
	return err
}

// GetMemberStats retrieves all trades for a member and calculates stats.
func (s *JournalService) GetMemberStats(ctx context.Context, memberTgID int64) (*domain.Stats, error) {
	trades, err := s.trades.GetTrades(ctx, memberTgID)
	if err != nil {
		return nil, fmt.Errorf("get trades: %w", err)
	}
	if len(trades) == 0 {
		return nil, nil
	}
	stats := domain.CalculateStats(trades)
	return &stats, nil
}
