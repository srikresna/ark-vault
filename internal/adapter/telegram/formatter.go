package telegram

import (
	"fmt"
	"strings"
	"time"

	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/internal/service"
)

// FormatStats formats member stats as an HTML Telegram message.
func FormatStats(username string, s *domain.Stats) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📊 <b>Stats — @%s</b>\n\n", username))
	sb.WriteString(fmt.Sprintf("Total trades: <b>%d</b>\n", s.TotalTrades))
	sb.WriteString(fmt.Sprintf("Win/Loss/BE: <b>%d</b>/%d/%d\n", s.Wins, s.Losses, s.BreakEvens))
	if s.OpenTrades > 0 {
		sb.WriteString(fmt.Sprintf("Open: %d\n", s.OpenTrades))
	}
	sb.WriteString(fmt.Sprintf("Win Rate: <b>%.1f%%</b>\n", s.WinRate))
	sb.WriteString(fmt.Sprintf("Avg RR: <b>%.2f</b>\n", s.AvgRR))
	sb.WriteString(fmt.Sprintf("Total RR: <b>%+.1fR</b>\n", s.TotalRR))
	sb.WriteString(fmt.Sprintf("Best: <b>%+.1fR</b>\n", s.BestRR))
	sb.WriteString(fmt.Sprintf("Worst: <b>%+.1fR</b>\n", s.WorstRR))

	if s.CurStreak > 0 {
		sb.WriteString(fmt.Sprintf("Current streak: 🔥 <b>%d</b> wins\n", s.CurStreak))
	} else if s.CurStreak < 0 {
		sb.WriteString(fmt.Sprintf("Current streak: ❄️ <b>%d</b> losses\n", -s.CurStreak))
	}
	if s.MaxWinStrk > 0 {
		sb.WriteString(fmt.Sprintf("Longest win streak: <b>%d</b>\n", s.MaxWinStrk))
	}

	// Per-asset breakdown
	if len(s.ByAsset) > 1 {
		sb.WriteString("\n<b>Per Asset:</b>\n")
		for at, as := range s.ByAsset {
			sb.WriteString(fmt.Sprintf("  %s: %d trades, %.1f%% WR, %+.1fR\n",
				at.String(), as.Total, as.WinRate, as.TotalRR))
		}
	}

	return sb.String()
}

// FormatLeaderboard formats the leaderboard as an HTML Telegram message.
func FormatLeaderboard(entries []service.LeaderboardEntry, metric string) string {
	var sb strings.Builder
	sb.WriteString("🏆 <b>Leaderboard</b>")
	if metric == "rr" {
		sb.WriteString(" (by RR)")
	} else {
		sb.WriteString(" (by Win Rate)")
	}
	sb.WriteString("\n\n")

	medals := []string{"🥇", "🥈", "🥉"}
	for i, e := range entries {
		rank := fmt.Sprintf("%d.", i+1)
		if i < 3 {
			rank = medals[i]
		}
		name := e.Username
		if name == "" {
			name = e.FirstName
		}
		if metric == "rr" {
			sb.WriteString(fmt.Sprintf("%s <b>@%s</b> — %+.1fR (%d trades)\n",
				rank, name, e.TotalRR, e.TotalTrades))
		} else {
			sb.WriteString(fmt.Sprintf("%s <b>@%s</b> — %.1f%% WR (%d trades)\n",
				rank, name, e.WinRate, e.TotalTrades))
		}
	}

	if len(entries) == 0 {
		sb.WriteString("Belum ada member yang memenuhi syarat.")
	}

	return sb.String()
}

// FormatTradeConfirmation formats a saved trade confirmation message.
func FormatTradeConfirmation(t *domain.Trade) string {
	var sb strings.Builder
	sb.WriteString("✅ <b>Trade berhasil dicatat!</b>\n\n")
	sb.WriteString(fmt.Sprintf("Symbol: <b>%s</b> (%s)\n", t.Symbol, t.AssetType.String()))
	sb.WriteString(fmt.Sprintf("Direction: <b>%s</b>\n", t.Direction))
	sb.WriteString(fmt.Sprintf("Status: <b>%s</b>", t.Status))
	if t.ResultRR != 0 {
		sb.WriteString(fmt.Sprintf(" (%+.1fR)", t.ResultRR))
	}
	sb.WriteString("\n")
	if t.TimeWindow != "" {
		sb.WriteString(fmt.Sprintf("Session: <b>%s</b>\n", t.TimeWindow))
	}
	if t.Confluence != "" {
		sb.WriteString(fmt.Sprintf("Confluence: %s\n", t.Confluence))
	}
	return sb.String()
}

// FormatHelp returns the public /help message.
func FormatHelp() string {
	return FormatHelpForUser(false)
}

// FormatHelpForUser returns the /help message, showing owner-only commands only when isOwner is true.
func FormatHelpForUser(isOwner bool) string {
	help := `🔐 <b>ARK Vault — Help</b>

<b>Mencatat Trade:</b>
• /journal — Guided flow (step-by-step dengan tombol)
• Kirim pesan dengan format:
<pre>#journal
Pair: XAUUSD
Type: BUY
RR: +2
Session: London
Confluence: FVG + OB mitigation on 15m</pre>
  Bisa attach screenshot bersamaan.

<b>Statistik &amp; Ranking:</b>
• /stats — Statistik personal
• /stats @user — Statistik member lain
• /leaderboard — Top 10 (win rate)
• /leaderboard rr — Top 10 (total RR)

<b>Export &amp; Report:</b>
• /export — Export journal (CSV)
• /export pdf — Export journal (PDF)
• /report — Summary report minggu ini

<b>Lainnya:</b>
• /profile — Profil &amp; level gamifikasi
• /badges — Koleksi badge
• /challenge — Weekly challenge &amp; standings
• /reminder — Atur daily reminder
• /goal — Monthly goal &amp; progress
• /analyze — AI trade analytics
• /reportcard — Monthly report card
• /help — Tampilkan pesan ini

<b>Tips:</b>
• RR bisa: +2, -1, 0 (BE), atau WIN 2RR, LOSS 1RR, BE
• Kalau RR kosong, trade dicatat sebagai OPEN
• Session: Asia, London, NY AM, NY PM`
	if isOwner {
		help += "\n\n<b>Owner Only:</b>\n• /reset — Reset semua data BadgerDB &amp; Notion"
	}
	return help
}

// FormatWeeklySummary formats a weekly community report as HTML.
func FormatWeeklySummary(s *service.WeeklySummary) string {
	var sb strings.Builder
	sb.WriteString("📈 <b>ARK Vault — Weekly Report</b>\n")
	sb.WriteString(fmt.Sprintf("📅 %s — %s\n\n",
		s.PeriodStart.Format("02 Jan"),
		s.PeriodEnd.Format("02 Jan 2006")))

	sb.WriteString(fmt.Sprintf("Total trades: <b>%d</b>\n", s.TotalTrades))
	sb.WriteString(fmt.Sprintf("Active members: <b>%d</b>\n", s.TotalMembers))
	sb.WriteString(fmt.Sprintf("Community win rate: <b>%.1f%%</b>\n", s.CommunityWR))
	sb.WriteString(fmt.Sprintf("Total RR: <b>%+.1fR</b>\n", s.TotalRR))
	if s.MostTraded != "" {
		sb.WriteString(fmt.Sprintf("Most traded: <b>%s</b>\n", s.MostTraded))
	}

	if len(s.TopPerformers) > 0 {
		sb.WriteString("\n🏆 <b>Top Performers (by RR):</b>\n")
		medals := []string{"🥇", "🥈", "🥉"}
		for i, e := range s.TopPerformers {
			medal := fmt.Sprintf("%d.", i+1)
			if i < len(medals) {
				medal = medals[i]
			}
			name := e.Username
			if name == "" {
				name = e.FirstName
			}
			sb.WriteString(fmt.Sprintf("%s @%s — %+.1fR (%.1f%% WR)\n",
				medal, name, e.TotalRR, e.WinRate))
		}
	}

	if s.TotalTrades == 0 {
		sb.WriteString("\n📭 Belum ada trade minggu ini.")
	}

	return sb.String()
}

// FormatTradeConfirmationWithXP formats a saved trade confirmation with gamification XP info.
func FormatTradeConfirmationWithXP(t *domain.Trade, xpGained int, totalXP int, level int, title string, leveledUp bool, streak int) string {
	base := FormatTradeConfirmation(t)
	var sb strings.Builder
	sb.WriteString(base)
	sb.WriteString(fmt.Sprintf("\n⚡ +%d XP (total: %d)\n", xpGained, totalXP))
	sb.WriteString(fmt.Sprintf("📊 Level %d — %s\n", level, title))
	if streak > 0 {
		sb.WriteString(fmt.Sprintf("🔥 Streak: %d hari\n", streak))
	}
	if leveledUp {
		sb.WriteString(fmt.Sprintf("\n🎉 <b>LEVEL UP!</b> Level %d — %s\n", level, title))
	}
	return sb.String()
}

// FormatProfile formats a user's gamification profile as HTML.
func FormatProfile(profile *domain.GamificationProfile, streak *domain.StreakData) string {
	var sb strings.Builder
	sb.WriteString("🏆 <b>Profil Trader</b>\n\n")

	lvl := 1
	title := "Retail"
	totalXP := 0
	if profile != nil {
		lvl = profile.Level
		title = profile.Title
		totalXP = profile.TotalXP
		if lvl == 0 {
			lvl = 1
			title = "Retail"
		}
	}

	nextXP := domain.XPForNextLevel(totalXP)
	nextTitle := ""
	if nextXP > 0 {
		_, nextTitle = domain.LevelForXP(nextXP)
		sb.WriteString(fmt.Sprintf("Level: <b>%d</b> — %s\n", lvl, title))
		sb.WriteString(fmt.Sprintf("XP: <b>%d</b> / %d (next: %s)\n", totalXP, nextXP, nextTitle))

		// Progress bar
		var prevXP int
		for _, lt := range domain.LevelTable {
			if lt.Level == lvl {
				prevXP = lt.XP
				break
			}
		}
		progress := 0
		span := nextXP - prevXP
		if span > 0 {
			progress = (totalXP - prevXP) * 100 / span
		}
		filled := progress * 16 / 100
		if filled < 0 {
			filled = 0
		}
		if filled > 16 {
			filled = 16
		}
		bar := strings.Repeat("█", filled) + strings.Repeat("░", 16-filled)
		sb.WriteString(fmt.Sprintf("[%s] %d%%\n", bar, progress))
	} else {
		sb.WriteString(fmt.Sprintf("Level: <b>%d</b> — %s\n", lvl, title))
		sb.WriteString(fmt.Sprintf("XP: <b>%d</b> (MAX LEVEL)\n", totalXP))
		sb.WriteString("[████████████████] 100%\n")
	}

	curStreak := 0
	longestStreak := 0
	if streak != nil {
		curStreak = streak.CurrentStreak
		longestStreak = streak.LongestStreak
	}

	sb.WriteString(fmt.Sprintf("\n🔥 Streak: <b>%d</b> hari berturut-turut\n", curStreak))
	sb.WriteString(fmt.Sprintf("📊 Streak terpanjang: <b>%d</b> hari\n", longestStreak))

	return sb.String()
}

// FormatBadgeList formats the user's badge collection.
func FormatBadgeList(badges []domain.BadgeAward) string {
	var sb strings.Builder
	sb.WriteString("🏅 <b>Badge Collection</b>\n\n")

	if len(badges) == 0 {
		sb.WriteString("Belum ada badge. Terus trading untuk mendapatkan badge pertamamu!")
		return sb.String()
	}

	earnedMap := make(map[domain.BadgeID]time.Time)
	for _, b := range badges {
		earnedMap[b.BadgeID] = b.AwardedAt
	}

	for _, def := range domain.BadgeRegistry {
		if t, ok := earnedMap[def.ID]; ok {
			sb.WriteString(fmt.Sprintf("%s <b>%s</b> — %s\n   <i>Earned %s</i>\n", def.Emoji, def.Name, def.Description, t.Format("02 Jan 2006")))
		} else {
			sb.WriteString(fmt.Sprintf("🔒 <s>%s</s> — %s\n", def.Name, def.Description))
		}
	}

	sb.WriteString(fmt.Sprintf("\n📊 %d/%d badges earned", len(badges), len(domain.BadgeRegistry)))
	return sb.String()
}

// FormatBadgeUnlock formats notification for newly earned badges.
func FormatBadgeUnlock(badges []domain.BadgeAward) string {
	var sb strings.Builder
	for _, b := range badges {
		def := domain.GetBadgeDefinition(b.BadgeID)
		if def != nil {
			sb.WriteString(fmt.Sprintf("\n🎖 <b>Badge Unlocked!</b> %s %s\n<i>%s</i>\n", def.Emoji, def.Name, def.Description))
		}
	}
	return sb.String()
}

// FormatChallenge formats the weekly challenge and standings.
func FormatChallenge(c *domain.WeeklyChallenge, standings []domain.ChallengeResult) string {
	var sb strings.Builder
	sb.WriteString("⚔️ <b>Weekly Challenge</b>\n")
	sb.WriteString(fmt.Sprintf("📅 %s\n\n", c.YearWeek))
	sb.WriteString(fmt.Sprintf("<b>%s</b>\n", c.Title))
	sb.WriteString(fmt.Sprintf("%s\n\n", c.Description))

	if c.Finalized {
		sb.WriteString("🏁 <b>Challenge selesai!</b>\n\n")
	}

	if len(standings) == 0 {
		sb.WriteString("Belum ada peserta minggu ini.")
		return sb.String()
	}

	medals := []string{"🥇", "🥈", "🥉"}
	for i, s := range standings {
		rank := fmt.Sprintf("%d.", i+1)
		if i < 3 {
			rank = medals[i]
		}
		name := s.Username
		if name == "" {
			name = fmt.Sprintf("user_%d", s.TelegramID)
		}

		var valueStr string
		switch c.Type {
		case domain.ChallengeMostTrades:
			valueStr = fmt.Sprintf("%.0f trades", s.Value)
		case domain.ChallengeBestRR, domain.ChallengeMostRR:
			valueStr = fmt.Sprintf("%+.1fR", s.Value)
		case domain.ChallengeHighestWR:
			valueStr = fmt.Sprintf("%.1f%%", s.Value)
		}

		sb.WriteString(fmt.Sprintf("%s @%s — %s\n", rank, name, valueStr))
	}

	return sb.String()
}

// FormatChallengeResults formats the final results of a completed challenge.
func FormatChallengeResults(c *domain.WeeklyChallenge, results []domain.ChallengeResult) string {
	var sb strings.Builder
	sb.WriteString("🏆 <b>Weekly Challenge Results!</b>\n")
	sb.WriteString(fmt.Sprintf("📅 %s — <b>%s</b>\n\n", c.YearWeek, c.Title))

	if len(results) == 0 {
		sb.WriteString("Tidak ada peserta minggu ini.")
		return sb.String()
	}

	medals := []string{"🥇", "🥈", "🥉"}
	for i, r := range results {
		if i >= 5 {
			break
		}
		rank := fmt.Sprintf("%d.", i+1)
		if i < 3 {
			rank = medals[i]
		}
		name := r.Username
		if name == "" {
			name = fmt.Sprintf("user_%d", r.TelegramID)
		}
		sb.WriteString(fmt.Sprintf("%s @%s — %.1f\n", rank, name, r.Value))
	}

	sb.WriteString("\nSelamat kepada para pemenang! 🎉")
	return sb.String()
}

// FormatReminderHelp shows reminder usage.
func FormatReminderHelp() string {
	return `🔔 <b>Daily Reminder</b>

Gunakan:
• <code>/reminder on</code> — Aktifkan (default jam 20:00 WIB)
• <code>/reminder on 19</code> — Aktifkan jam 19:00 WIB
• <code>/reminder off</code> — Nonaktifkan
• <code>/reminder</code> — Cek status`
}

// FormatDailyReminder formats the daily reminder message sent to users.
func FormatDailyReminder(streakDays int) string {
	var sb strings.Builder
	sb.WriteString("📝 <b>Reminder: Jangan lupa jurnal hari ini!</b>\n\n")
	if streakDays > 0 {
		sb.WriteString(fmt.Sprintf("🔥 Streak kamu: <b>%d hari</b>\n", streakDays))
		sb.WriteString("Jangan sampai putus! Catat trade hari ini.\n")
	} else {
		sb.WriteString("Mulai streak baru dengan mencatat trade pertamamu hari ini.\n")
	}
	sb.WriteString("\nGunakan /journal untuk mulai.")
	return sb.String()
}

// FormatCommunityReminder formats the daily reminder message sent to a community group/thread.
func FormatCommunityReminder() string {
	return "📝 <b>Community Reminder: Jangan lupa jurnal hari ini!</b>\n\n" +
		"Sudahkah kalian mencatat setup &amp; hasil trade hari ini?\n" +
		"Gunakan /journal atau <code>#journal</code> untuk menjaga disiplin trading kita bersama! 💪"
}

// FormatGoalHelp shows goal usage.
func FormatGoalHelp() string {
	return `🎯 <b>Monthly Goal</b>

Belum ada goal bulan ini. Set goal:
• <code>/goal set total_trades 30</code> — Target 30 trade
• <code>/goal set total_rr 10</code> — Target +10R
• <code>/goal set win_rate 60</code> — Target 60% win rate
• <code>/goal set streak_days 20</code> — Target 20 hari streak

Cek progress: <code>/goal</code>`
}

// FormatGoalSet confirms a goal has been set.
func FormatGoalSet(goal *domain.MonthlyGoal) string {
	var typeLabel string
	switch goal.GoalType {
	case domain.GoalTotalTrades:
		typeLabel = fmt.Sprintf("%.0f trades", goal.TargetValue)
	case domain.GoalTotalRR:
		typeLabel = fmt.Sprintf("+%.1fR", goal.TargetValue)
	case domain.GoalWinRate:
		typeLabel = fmt.Sprintf("%.0f%% win rate", goal.TargetValue)
	case domain.GoalStreakDays:
		typeLabel = fmt.Sprintf("%.0f hari streak", goal.TargetValue)
	}
	return fmt.Sprintf("🎯 <b>Goal bulan %s:</b> %s\n\nGunakan /goal untuk cek progress.", goal.YearMonth, typeLabel)
}

// FormatGoalProgress shows progress toward monthly goal.
func FormatGoalProgress(p *domain.GoalProgress) string {
	var sb strings.Builder
	g := p.Goal
	sb.WriteString(fmt.Sprintf("🎯 <b>Monthly Goal — %s</b>\n\n", g.YearMonth))

	var typeLabel string
	var currentStr, targetStr string
	switch g.GoalType {
	case domain.GoalTotalTrades:
		typeLabel = "Total Trades"
		currentStr = fmt.Sprintf("%.0f", p.CurrentValue)
		targetStr = fmt.Sprintf("%.0f", g.TargetValue)
	case domain.GoalTotalRR:
		typeLabel = "Total RR"
		currentStr = fmt.Sprintf("%+.1f", p.CurrentValue)
		targetStr = fmt.Sprintf("+%.1f", g.TargetValue)
	case domain.GoalWinRate:
		typeLabel = "Win Rate"
		currentStr = fmt.Sprintf("%.1f%%", p.CurrentValue)
		targetStr = fmt.Sprintf("%.0f%%", g.TargetValue)
	case domain.GoalStreakDays:
		typeLabel = "Streak Days"
		currentStr = fmt.Sprintf("%.0f", p.CurrentValue)
		targetStr = fmt.Sprintf("%.0f", g.TargetValue)
	}

	sb.WriteString(fmt.Sprintf("<b>%s:</b> %s / %s\n", typeLabel, currentStr, targetStr))

	// Progress bar
	pct := p.Percentage
	if pct > 100 {
		pct = 100
	}
	filled := int(pct) * 16 / 100
	if filled < 0 {
		filled = 0
	}
	if filled > 16 {
		filled = 16
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", 16-filled)
	sb.WriteString(fmt.Sprintf("[%s] %.0f%%\n", bar, p.Percentage))

	if g.Achieved {
		sb.WriteString("\n\u2705 <b>Goal tercapai!</b> \U0001f389")
	}

	return sb.String()
}

// FormatAnalytics formats AI trade analytics.
func FormatAnalytics(a *domain.TradeAnalytics) string {
	var sb strings.Builder
	sb.WriteString("\U0001f916 <b>AI Trade Analytics</b>\n\n")

	sb.WriteString(fmt.Sprintf("\U0001f4ca Berdasarkan <b>%d trades</b> (WR: %.1f%%, RR: %+.1f)\n\n", a.TotalTrades, a.WinRate, a.TotalRR))

	if a.StrengthAnalysis != "" {
		sb.WriteString(fmt.Sprintf("\U0001f4aa <b>Kekuatan:</b>\n%s\n\n", a.StrengthAnalysis))
	}
	if a.WeaknessAnalysis != "" {
		sb.WriteString(fmt.Sprintf("\u26a0\ufe0f <b>Kelemahan:</b>\n%s\n\n", a.WeaknessAnalysis))
	}
	if a.PatternInsights != "" {
		sb.WriteString(fmt.Sprintf("\U0001f50d <b>Pola Trading:</b>\n%s\n\n", a.PatternInsights))
	}
	if a.Recommendations != "" {
		sb.WriteString(fmt.Sprintf("\U0001f4a1 <b>Rekomendasi:</b>\n%s\n\n", a.Recommendations))
	}
	if a.OverallAssessment != "" {
		sb.WriteString(fmt.Sprintf("\U0001f4dd <b>Penilaian:</b>\n%s\n", a.OverallAssessment))
	}

	sb.WriteString(fmt.Sprintf("\n<i>Generated: %s (cache 24h)</i>", a.GeneratedAt.Format("02 Jan 15:04")))
	return sb.String()
}

// FormatMonthlyReportCard formats a monthly performance report card.
func FormatMonthlyReportCard(r *domain.MonthlyReportCard) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\U0001f4cb <b>Monthly Report Card \u2014 %s</b>\n\n", r.YearMonth))

	sb.WriteString("<b>\U0001f4ca Statistik:</b>\n")
	sb.WriteString(fmt.Sprintf("Total trades: <b>%d</b>\n", r.TotalTrades))
	sb.WriteString(fmt.Sprintf("Win/Loss/BE: <b>%d</b>/%d/%d\n", r.Wins, r.Losses, r.BreakEvens))
	sb.WriteString(fmt.Sprintf("Win Rate: <b>%.1f%%</b>\n", r.WinRate))
	sb.WriteString(fmt.Sprintf("Total RR: <b>%+.1fR</b>\n", r.TotalRR))
	sb.WriteString(fmt.Sprintf("Best: <b>%+.1fR</b> | Worst: <b>%+.1fR</b>\n\n", r.BestTrade, r.WorstTrade))

	sb.WriteString("<b>\U0001f3c6 Gamifikasi:</b>\n")
	sb.WriteString(fmt.Sprintf("Level: <b>%d</b> \u2014 %s\n", r.Level, r.Title))
	sb.WriteString(fmt.Sprintf("XP earned: <b>+%d</b>\n", r.XPEarned))
	if r.BadgesEarned > 0 {
		sb.WriteString(fmt.Sprintf("Badges earned: <b>%d</b>\n", r.BadgesEarned))
	}
	if r.LongestStreak > 0 {
		sb.WriteString(fmt.Sprintf("Longest streak: <b>%d hari</b>\n", r.LongestStreak))
	}

	if len(r.AssetBreakdown) > 0 {
		sb.WriteString("\n<b>Per Asset:</b>\n")
		for asset, stats := range r.AssetBreakdown {
			sb.WriteString(fmt.Sprintf("  %s: %d trades, %.1f%% WR, %+.1fR\n", asset, stats.Total, stats.WinRate, stats.TotalRR))
		}
	}

	if r.AISummary != "" {
		sb.WriteString(fmt.Sprintf("\n\U0001f916 <b>AI Summary:</b>\n%s\n", r.AISummary))
	}

	if r.TotalTrades == 0 {
		sb.WriteString("\n\U0001f4ed Tidak ada trade bulan ini.")
	}

	return sb.String()
}
