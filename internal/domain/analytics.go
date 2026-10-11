package domain

import "time"

// TradeAnalytics holds AI-generated insights about a member's trades.
type TradeAnalytics struct {
	TelegramID int64     `json:"telegram_id"`
	GeneratedAt time.Time `json:"generated_at"`
	ExpiresAt   time.Time `json:"expires_at"` // 24h cache
	SourceHash  string    `json:"source_hash,omitempty"`

	// Statistical summary
	TotalTrades int     `json:"total_trades"`
	WinRate     float64 `json:"win_rate"`
	TotalRR     float64 `json:"total_rr"`
	AvgRR       float64 `json:"avg_rr"`

	// AI-generated insights (Bahasa Indonesia)
	StrengthAnalysis  string `json:"strength_analysis"`  // What the trader does well
	WeaknessAnalysis  string `json:"weakness_analysis"`  // Areas to improve
	PatternInsights   string `json:"pattern_insights"`   // Patterns detected (time, pairs, etc)
	Recommendations   string `json:"recommendations"`    // Actionable advice
	OverallAssessment string `json:"overall_assessment"` // Summary paragraph
}

// MonthlyReportCard holds a member's monthly performance report.
type MonthlyReportCard struct {
	TelegramID  int64     `json:"telegram_id"`
	YearMonth   string    `json:"year_month"` // "2026-03"
	GeneratedAt time.Time `json:"generated_at"`
	SourceHash  string    `json:"source_hash,omitempty"`

	// Stats
	TotalTrades int     `json:"total_trades"`
	Wins        int     `json:"wins"`
	Losses      int     `json:"losses"`
	BreakEvens  int     `json:"break_evens"`
	WinRate     float64 `json:"win_rate"`
	TotalRR     float64 `json:"total_rr"`
	BestTrade   float64 `json:"best_trade"`
	WorstTrade  float64 `json:"worst_trade"`

	// Gamification
	XPEarned      int    `json:"xp_earned"`
	BadgesEarned  int    `json:"badges_earned"`
	LongestStreak int    `json:"longest_streak"`
	Level         int    `json:"level"`
	Title         string `json:"title"`

	// AI Summary (Bahasa Indonesia)
	AISummary string `json:"ai_summary"`

	// Per-asset breakdown (reuses domain.AssetStats from stats.go)
	AssetBreakdown map[string]AssetStats `json:"asset_breakdown,omitempty"`
}
