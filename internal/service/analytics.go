package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
)

// AnalyticsService provides AI-powered trade analytics with caching.
type AnalyticsService struct {
	cache  AnalyticsCacheStore
	ai     AIAnalyzer
	trades TradeRepository
}

// NewAnalyticsService creates a new AnalyticsService.
func NewAnalyticsService(cache AnalyticsCacheStore, ai AIAnalyzer, trades TradeRepository) *AnalyticsService {
	return &AnalyticsService{
		cache:  cache,
		ai:     ai,
		trades: trades,
	}
}

// GetAnalytics returns AI-powered analytics for a member's trades.
// Results are cached for 24 hours.
func (s *AnalyticsService) GetAnalytics(ctx context.Context, telegramID int64) (*domain.TradeAnalytics, error) {
	// Check cache first
	cached, err := s.cache.GetAnalyticsCache(ctx, telegramID)
	if err == nil && cached != nil && time.Now().Before(cached.ExpiresAt) {
		return cached, nil
	}

	// Fetch trades
	trades, err := s.trades.GetTrades(ctx, telegramID)
	if err != nil {
		return nil, fmt.Errorf("failed to get trades: %w", err)
	}
	if len(trades) == 0 {
		return nil, fmt.Errorf("belum ada trade untuk dianalisis")
	}

	// Calculate basic stats
	var wins, losses, breakEvens int
	var totalRR float64
	for _, t := range trades {
		switch t.Status {
		case domain.StatusWin:
			wins++
		case domain.StatusLoss:
			losses++
		case domain.StatusBE:
			breakEvens++
		}
		totalRR += t.ResultRR
	}

	total := len(trades)
	winRate := 0.0
	closed := wins + losses + breakEvens
	if closed > 0 {
		winRate = float64(wins) / float64(closed) * 100
	}
	avgRR := 0.0
	if total > 0 {
		avgRR = totalRR / float64(total)
	}

	// Build prompt for AI
	prompt := buildAnalyticsPrompt(trades, total, wins, losses, breakEvens, winRate, totalRR, avgRR)

	// Call AI
	response, err := s.ai.AnalyzeTrades(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("AI analysis failed: %w", err)
	}

	// Parse AI response into sections
	analytics := parseAIResponse(response)
	now := time.Now()
	analytics.TelegramID = telegramID
	analytics.GeneratedAt = now
	analytics.ExpiresAt = now.Add(24 * time.Hour)
	analytics.TotalTrades = total
	analytics.WinRate = winRate
	analytics.TotalRR = totalRR
	analytics.AvgRR = avgRR

	// Cache the result
	_ = s.cache.SaveAnalyticsCache(ctx, analytics)

	return analytics, nil
}

// buildAnalyticsPrompt creates a prompt summarising the trader's data.
func buildAnalyticsPrompt(trades []domain.Trade, total, wins, losses, breakEvens int, winRate, totalRR, avgRR float64) string {
	var sb strings.Builder
	sb.WriteString("Kamu adalah trading coach profesional. Analisis data trading berikut dalam Bahasa Indonesia.\n\n")
	fmt.Fprintf(&sb, "Total trades: %d\n", total)
	fmt.Fprintf(&sb, "Win: %d, Loss: %d, BE: %d\n", wins, losses, breakEvens)
	fmt.Fprintf(&sb, "Win Rate: %.1f%%\n", winRate)
	fmt.Fprintf(&sb, "Total RR: %+.1f, Avg RR: %+.2f\n\n", totalRR, avgRR)

	// Include recent trades (max 50 for token limit)
	limit := len(trades)
	if limit > 50 {
		limit = 50
	}

	sb.WriteString("Data trade terbaru:\n")
	for i := 0; i < limit; i++ {
		t := trades[i]
		fmt.Fprintf(&sb, "- %s %s %s %s RR:%+.1f Session:%s\n",
			t.Date.Format("2006-01-02"),
			t.Symbol,
			t.Direction,
			t.Status,
			t.ResultRR,
			t.TimeWindow,
		)
	}

	sb.WriteString("\nBerikan analisis dalam format berikut (tanpa markdown, plain text saja):\n")
	sb.WriteString("[KEKUATAN]\n(analisis kekuatan trader)\n")
	sb.WriteString("[KELEMAHAN]\n(analisis kelemahan)\n")
	sb.WriteString("[POLA]\n(pola trading yang terdeteksi)\n")
	sb.WriteString("[REKOMENDASI]\n(saran actionable)\n")
	sb.WriteString("[PENILAIAN]\n(penilaian keseluruhan singkat)\n")

	return sb.String()
}

// parseAIResponse extracts sections from the AI response text.
func parseAIResponse(response string) *domain.TradeAnalytics {
	a := &domain.TradeAnalytics{}

	sections := map[string]*string{
		"[KEKUATAN]":    &a.StrengthAnalysis,
		"[KELEMAHAN]":   &a.WeaknessAnalysis,
		"[POLA]":        &a.PatternInsights,
		"[REKOMENDASI]": &a.Recommendations,
		"[PENILAIAN]":   &a.OverallAssessment,
	}

	// Order matters for parsing
	tags := []string{"[KEKUATAN]", "[KELEMAHAN]", "[POLA]", "[REKOMENDASI]", "[PENILAIAN]"}

	for i, tag := range tags {
		start := strings.Index(response, tag)
		if start == -1 {
			continue
		}
		start += len(tag)

		end := len(response)
		// Find the next tag
		for j := i + 1; j < len(tags); j++ {
			nextStart := strings.Index(response[start:], tags[j])
			if nextStart != -1 {
				end = start + nextStart
				break
			}
		}

		text := strings.TrimSpace(response[start:end])
		*sections[tag] = text
	}

	// If no sections were found, put the whole response as overall assessment
	if a.StrengthAnalysis == "" && a.WeaknessAnalysis == "" && a.OverallAssessment == "" {
		a.OverallAssessment = strings.TrimSpace(response)
	}

	return a
}
