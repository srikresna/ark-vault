package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// ReportCardService generates monthly report cards.
type ReportCardService struct {
	store  ReportCardStore
	trades TradeRepository
	gam    GamificationStore
	badges BadgeStore
	ai     AIAnalyzer
}

// NewReportCardService creates a new ReportCardService.
func NewReportCardService(
	store ReportCardStore,
	trades TradeRepository,
	gam GamificationStore,
	badges BadgeStore,
	ai AIAnalyzer,
) *ReportCardService {
	return &ReportCardService{
		store:  store,
		trades: trades,
		gam:    gam,
		badges: badges,
		ai:     ai,
	}
}

// GenerateMonthlyReport generates (or returns cached) a monthly report card.
func (s *ReportCardService) GenerateMonthlyReport(ctx context.Context, telegramID int64, yearMonth string) (*domain.MonthlyReportCard, error) {
	// Check cache first
	cached, err := s.store.GetReportCard(ctx, telegramID, yearMonth)
	if err == nil && cached != nil {
		return cached, nil
	}

	// Parse yearMonth to determine the month boundaries
	t, err := time.ParseInLocation("2006-01", yearMonth, wib)
	if err != nil {
		return nil, fmt.Errorf("invalid year-month format: %w", err)
	}
	monthStart := t
	monthEnd := t.AddDate(0, 1, 0)

	// Fetch all trades
	allTrades, err := s.trades.GetTrades(ctx, telegramID)
	if err != nil {
		return nil, fmt.Errorf("failed to get trades: %w", err)
	}

	// Filter trades in the month
	var monthTrades []domain.Trade
	for _, tr := range allTrades {
		if !tr.Date.Before(monthStart) && tr.Date.Before(monthEnd) {
			monthTrades = append(monthTrades, tr)
		}
	}

	// Calculate stats
	var wins, losses, breakEvens int
	var totalRR, bestTrade, worstTrade float64
	assetMap := make(map[string]*reportAssetAccumulator)

	for i, tr := range monthTrades {
		switch tr.Status {
		case domain.StatusWin:
			wins++
		case domain.StatusLoss:
			losses++
		case domain.StatusBE:
			breakEvens++
		}
		totalRR += tr.ResultRR

		if i == 0 {
			bestTrade = tr.ResultRR
			worstTrade = tr.ResultRR
		} else {
			if tr.ResultRR > bestTrade {
				bestTrade = tr.ResultRR
			}
			if tr.ResultRR < worstTrade {
				worstTrade = tr.ResultRR
			}
		}

		// Per-asset breakdown
		assetKey := tr.Symbol
		if assetKey == "" {
			assetKey = "Unknown"
		}
		acc, ok := assetMap[assetKey]
		if !ok {
			acc = &reportAssetAccumulator{}
			assetMap[assetKey] = acc
		}
		acc.total++
		acc.totalRR += tr.ResultRR
		if tr.Status == domain.StatusWin {
			acc.wins++
		}
	}

	totalTrades := len(monthTrades)
	winRate := 0.0
	closed := wins + losses + breakEvens
	if closed > 0 {
		winRate = float64(wins) / float64(closed) * 100
	}

	// Gamification data
	profile, _ := s.gam.GetProfile(ctx, telegramID)
	streak, _ := s.gam.GetStreak(ctx, telegramID)
	badges, _ := s.badges.GetBadges(ctx, telegramID)

	// Count XP earned this month and badges earned this month
	xpEarned := 0
	events, _ := s.gam.GetXPEvents(ctx, telegramID, monthStart)
	for _, e := range events {
		if e.Timestamp.Before(monthEnd) {
			xpEarned += e.Amount
		}
	}

	badgesEarned := 0
	for _, b := range badges {
		if !b.AwardedAt.Before(monthStart) && b.AwardedAt.Before(monthEnd) {
			badgesEarned++
		}
	}

	level := 1
	title := "Retail"
	if profile != nil {
		level = profile.Level
		title = profile.Title
		if level == 0 {
			level = 1
			title = "Retail"
		}
	}

	longestStreak := 0
	if streak != nil {
		longestStreak = streak.LongestStreak
	}

	// Build asset breakdown
	assetBreakdown := make(map[string]domain.AssetStats)
	for key, acc := range assetMap {
		wr := 0.0
		if acc.total > 0 {
			wr = float64(acc.wins) / float64(acc.total) * 100
		}
		assetBreakdown[key] = domain.AssetStats{
			Total:   acc.total,
			WinRate: wr,
			TotalRR: acc.totalRR,
		}
	}

	report := &domain.MonthlyReportCard{
		TelegramID:     telegramID,
		YearMonth:      yearMonth,
		GeneratedAt:    time.Now(),
		TotalTrades:    totalTrades,
		Wins:           wins,
		Losses:         losses,
		BreakEvens:     breakEvens,
		WinRate:        winRate,
		TotalRR:        totalRR,
		BestTrade:      bestTrade,
		WorstTrade:     worstTrade,
		XPEarned:       xpEarned,
		BadgesEarned:   badgesEarned,
		LongestStreak:  longestStreak,
		Level:          level,
		Title:          title,
		AssetBreakdown: assetBreakdown,
	}

	// Generate AI summary if trades exist and AI is available
	if totalTrades > 0 && s.ai != nil {
		summary, err := s.generateAISummary(ctx, report)
		if err == nil {
			report.AISummary = summary
		}
	}

	// Cache the report
	_ = s.store.SaveReportCard(ctx, report)

	return report, nil
}

type reportAssetAccumulator struct {
	total   int
	wins    int
	totalRR float64
}

func (s *ReportCardService) generateAISummary(ctx context.Context, report *domain.MonthlyReportCard) (string, error) {
	var sb strings.Builder
	sb.WriteString("Kamu adalah trading coach profesional. Buatkan ringkasan singkat (2-3 kalimat) dalam Bahasa Indonesia untuk monthly report card trader ini.\n\n")
	fmt.Fprintf(&sb, "Bulan: %s\n", report.YearMonth)
	fmt.Fprintf(&sb, "Total trades: %d (Win: %d, Loss: %d, BE: %d)\n", report.TotalTrades, report.Wins, report.Losses, report.BreakEvens)
	fmt.Fprintf(&sb, "Win Rate: %.1f%%, Total RR: %+.1f\n", report.WinRate, report.TotalRR)
	fmt.Fprintf(&sb, "Best: %+.1fR, Worst: %+.1fR\n", report.BestTrade, report.WorstTrade)
	fmt.Fprintf(&sb, "Level: %d — %s, XP earned: %d\n", report.Level, report.Title, report.XPEarned)

	if len(report.AssetBreakdown) > 0 {
		sb.WriteString("\nPer asset:\n")
		for asset, stats := range report.AssetBreakdown {
			fmt.Fprintf(&sb, "- %s: %d trades, %.1f%% WR, %+.1fR\n", asset, stats.Total, stats.WinRate, stats.TotalRR)
		}
	}

	sb.WriteString("\nBerikan ringkasan singkat yang memotivasi, tanpa markdown formatting.")

	return s.ai.AnalyzeTrades(ctx, sb.String())
}
