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
	return FormatStatsWithPeriod(username, "all", s)
}

func periodLabel(period string) string {
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "week", "weekly":
		return "Minggu Ini"
	case "month", "monthly":
		return "Bulan Ini"
	default:
		return "All-Time"
	}
}

// FormatStatsWithPeriod formats member stats for a specific period ("all", "week", "month").
func FormatStatsWithPeriod(username, period string, s *domain.Stats) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "📊 <b>Stats — @%s (%s)</b>\n\n", username, periodLabel(period))
	fmt.Fprintf(&sb, "Total trades: <b>%d</b>\n", s.TotalTrades)
	fmt.Fprintf(&sb, "Win/Loss/BE: <b>%d</b>/%d/%d\n", s.Wins, s.Losses, s.BreakEvens)
	if s.OpenTrades > 0 {
		fmt.Fprintf(&sb, "Open: %d\n", s.OpenTrades)
	}
	fmt.Fprintf(&sb, "Win Rate: <b>%.1f%%</b>\n", s.WinRate)
	fmt.Fprintf(&sb, "Avg RR: <b>%.2f</b>\n", s.AvgRR)
	fmt.Fprintf(&sb, "Expectancy: <b>%+.2fR / trade</b>\n", s.Expectancy)
	if s.ProfitFactor > 0 {
		fmt.Fprintf(&sb, "Profit Factor: <b>%.2f</b>\n", s.ProfitFactor)
	}
	fmt.Fprintf(&sb, "Total RR: <b>%+.1fR</b>\n", s.TotalRR)
	fmt.Fprintf(&sb, "Best: <b>%+.1fR</b> | Worst: <b>%+.1fR</b>\n", s.BestRR, s.WorstRR)

	if s.CurStreak > 0 {
		fmt.Fprintf(&sb, "Current streak: 🔥 <b>%d</b> wins\n", s.CurStreak)
	} else if s.CurStreak < 0 {
		fmt.Fprintf(&sb, "Current streak: ❄️ <b>%d</b> losses\n", -s.CurStreak)
	}
	if s.MaxWinStrk > 0 {
		fmt.Fprintf(&sb, "Longest win streak: <b>%d</b>\n", s.MaxWinStrk)
	}

	// Per-session breakdown
	if len(s.BySession) > 0 {
		sb.WriteString("\n<b>Per Session:</b>\n")
		sessions := []domain.TimeWindow{domain.SessionAsia, domain.SessionLondon, domain.SessionNYAM, domain.SessionNYPM}
		seen := make(map[domain.TimeWindow]bool)
		for _, tw := range sessions {
			if ss, ok := s.BySession[tw]; ok && ss.Total > 0 {
				fmt.Fprintf(&sb, "  %s: %d trades, %.1f%% WR, %+.1fR\n", tw, ss.Total, ss.WinRate, ss.TotalRR)
				seen[tw] = true
			}
		}
		for tw, ss := range s.BySession {
			if !seen[tw] && ss.Total > 0 {
				fmt.Fprintf(&sb, "  %s: %d trades, %.1f%% WR, %+.1fR\n", tw, ss.Total, ss.WinRate, ss.TotalRR)
			}
		}
	}

	// Top symbols breakdown
	if len(s.TopSymbols) > 0 {
		sb.WriteString("\n<b>Top Pairs:</b>\n")
		for _, sym := range s.TopSymbols {
			fmt.Fprintf(&sb, "  %s: %d trades, %.1f%% WR, %+.1fR\n", sym.Symbol, sym.Total, sym.WinRate, sym.TotalRR)
		}
	}

	// Per-asset breakdown
	if len(s.ByAsset) > 1 {
		sb.WriteString("\n<b>Per Asset:</b>\n")
		for at, as := range s.ByAsset {
			fmt.Fprintf(&sb, "  %s: %d trades, %.1f%% WR, %+.1fR\n",
				at.String(), as.Total, as.WinRate, as.TotalRR)
		}
	}

	return sb.String()
}

// FormatLeaderboard formats the leaderboard as an HTML Telegram message.
func FormatLeaderboard(entries []service.LeaderboardEntry, metric string) string {
	return FormatLeaderboardWithPeriod(entries, metric, "all")
}

// FormatLeaderboardWithPeriod formats the leaderboard for a given period ("all", "week", "month").
func FormatLeaderboardWithPeriod(entries []service.LeaderboardEntry, metric, period string) string {
	var sb strings.Builder
	sb.WriteString("🏆 <b>Leaderboard</b>")
	if metric == "rr" {
		sb.WriteString(" (by RR")
	} else {
		sb.WriteString(" (by Win Rate")
	}
	fmt.Fprintf(&sb, " — %s)\n\n", periodLabel(period))

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
			fmt.Fprintf(&sb, "%s <b>@%s</b> — %+.1fR (%d trades)\n",
				rank, name, e.TotalRR, e.TotalTrades)
		} else {
			fmt.Fprintf(&sb, "%s <b>@%s</b> — %.1f%% WR (%d trades)\n",
				rank, name, e.WinRate, e.TotalTrades)
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
	fmt.Fprintf(&sb, "Symbol: <b>%s</b> (%s)\n", t.Symbol, t.AssetType.String())
	fmt.Fprintf(&sb, "Direction: <b>%s</b>\n", t.Direction)
	fmt.Fprintf(&sb, "Status: <b>%s</b>", t.Status)
	if t.ResultRR != 0 {
		fmt.Fprintf(&sb, " (%+.1fR)", t.ResultRR)
	}
	sb.WriteString("\n")
	if t.TimeWindow != "" {
		fmt.Fprintf(&sb, "Session: <b>%s</b>\n", t.TimeWindow)
	}
	if t.Confluence != "" {
		fmt.Fprintf(&sb, "Confluence: %s\n", t.Confluence)
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

<b>Mencatat &amp; Kelola Trade:</b>
• /journal — Guided flow (step-by-step dengan tombol)
• /open — Lihat &amp; tutup trade yang masih OPEN
• /history — Riwayat trade (bisa Edit &amp; Hapus trade mana saja)
• /undo — Hapus 1 trade terakhir yang baru diinput
• Kirim pesan dengan format:
<pre>#journal
Pair: XAUUSD
Type: BUY
RR: +2
Session: London
Confluence: FVG + OB mitigation on 15m</pre>
  Bisa attach screenshot bersamaan.

<b>Statistik &amp; Ranking:</b>
• /stats [week|month] — Statistik personal
• /stats @user [week|month] — Statistik member lain
• /leaderboard [week|month] — Top 10 (win rate)
• /leaderboard [week|month] rr — Top 10 (total RR)
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
	fmt.Fprintf(&sb, "📅 %s — %s\n\n",
		s.PeriodStart.Format("02 Jan"),
		s.PeriodEnd.Format("02 Jan 2006"))

	fmt.Fprintf(&sb, "Total trades: <b>%d</b>\n", s.TotalTrades)
	fmt.Fprintf(&sb, "Active members: <b>%d</b>\n", s.TotalMembers)
	fmt.Fprintf(&sb, "Community win rate: <b>%.1f%%</b>\n", s.CommunityWR)
	fmt.Fprintf(&sb, "Total RR: <b>%+.1fR</b>\n", s.TotalRR)
	if s.MostTraded != "" {
		fmt.Fprintf(&sb, "Most traded: <b>%s</b>\n", s.MostTraded)
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
			fmt.Fprintf(&sb, "%s @%s — %+.1fR (%.1f%% WR)\n",
				medal, name, e.TotalRR, e.WinRate)
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
	fmt.Fprintf(&sb, "\n⚡ +%d XP (total: %d)\n", xpGained, totalXP)
	fmt.Fprintf(&sb, "📊 Level %d — %s\n", level, title)
	if streak > 0 {
		fmt.Fprintf(&sb, "🔥 Streak: %d hari\n", streak)
	}
	if leveledUp {
		fmt.Fprintf(&sb, "\n🎉 <b>LEVEL UP!</b> Level %d — %s\n", level, title)
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
		fmt.Fprintf(&sb, "Level: <b>%d</b> — %s\n", lvl, title)
		fmt.Fprintf(&sb, "XP: <b>%d</b> / %d (next: %s)\n", totalXP, nextXP, nextTitle)

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
		fmt.Fprintf(&sb, "[%s] %d%%\n", bar, progress)
	} else {
		fmt.Fprintf(&sb, "Level: <b>%d</b> — %s\n", lvl, title)
		fmt.Fprintf(&sb, "XP: <b>%d</b> (MAX LEVEL)\n", totalXP)
		sb.WriteString("[████████████████] 100%\n")
	}

	curStreak := 0
	longestStreak := 0
	if streak != nil {
		curStreak = streak.CurrentStreak
		longestStreak = streak.LongestStreak
	}

	fmt.Fprintf(&sb, "\n🔥 Streak: <b>%d</b> hari berturut-turut\n", curStreak)
	fmt.Fprintf(&sb, "📊 Streak terpanjang: <b>%d</b> hari\n", longestStreak)

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
			fmt.Fprintf(&sb, "%s <b>%s</b> — %s\n   <i>Earned %s</i>\n", def.Emoji, def.Name, def.Description, t.Format("02 Jan 2006"))
		} else {
			fmt.Fprintf(&sb, "🔒 <s>%s</s> — %s\n", def.Name, def.Description)
		}
	}

	fmt.Fprintf(&sb, "\n📊 %d/%d badges earned", len(badges), len(domain.BadgeRegistry))
	return sb.String()
}

// FormatBadgeUnlock formats notification for newly earned badges.
func FormatBadgeUnlock(badges []domain.BadgeAward) string {
	var sb strings.Builder
	for _, b := range badges {
		def := domain.GetBadgeDefinition(b.BadgeID)
		if def != nil {
			fmt.Fprintf(&sb, "\n🎖 <b>Badge Unlocked!</b> %s %s\n<i>%s</i>\n", def.Emoji, def.Name, def.Description)
		}
	}
	return sb.String()
}

// FormatChallenge formats the weekly challenge and standings.
func FormatChallenge(c *domain.WeeklyChallenge, standings []domain.ChallengeResult) string {
	var sb strings.Builder
	sb.WriteString("⚔️ <b>Weekly Challenge</b>\n")
	fmt.Fprintf(&sb, "📅 %s\n\n", c.YearWeek)
	fmt.Fprintf(&sb, "<b>%s</b>\n", c.Title)
	fmt.Fprintf(&sb, "%s\n\n", c.Description)

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

		fmt.Fprintf(&sb, "%s @%s — %s\n", rank, name, valueStr)
	}

	return sb.String()
}

// FormatChallengeResults formats the final results of a completed challenge.
func FormatChallengeResults(c *domain.WeeklyChallenge, results []domain.ChallengeResult) string {
	var sb strings.Builder
	sb.WriteString("🏆 <b>Weekly Challenge Results!</b>\n")
	fmt.Fprintf(&sb, "📅 %s — <b>%s</b>\n\n", c.YearWeek, c.Title)

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
		fmt.Fprintf(&sb, "%s @%s — %.1f\n", rank, name, r.Value)
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
		fmt.Fprintf(&sb, "🔥 Streak kamu: <b>%d hari</b>\n", streakDays)
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
	fmt.Fprintf(&sb, "🎯 <b>Monthly Goal — %s</b>\n\n", g.YearMonth)

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

	fmt.Fprintf(&sb, "<b>%s:</b> %s / %s\n", typeLabel, currentStr, targetStr)

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
	fmt.Fprintf(&sb, "[%s] %.0f%%\n", bar, p.Percentage)

	if g.Achieved {
		sb.WriteString("\n\u2705 <b>Goal tercapai!</b> \U0001f389")
	}

	return sb.String()
}

// FormatAnalytics formats AI trade analytics.
func FormatAnalytics(a *domain.TradeAnalytics) string {
	var sb strings.Builder
	sb.WriteString("\U0001f916 <b>AI Trade Analytics</b>\n\n")

	fmt.Fprintf(&sb, "\U0001f4ca Berdasarkan <b>%d trades</b> (WR: %.1f%%, RR: %+.1f)\n\n", a.TotalTrades, a.WinRate, a.TotalRR)

	if a.StrengthAnalysis != "" {
		fmt.Fprintf(&sb, "\U0001f4aa <b>Kekuatan:</b>\n%s\n\n", a.StrengthAnalysis)
	}
	if a.WeaknessAnalysis != "" {
		fmt.Fprintf(&sb, "\u26a0\ufe0f <b>Kelemahan:</b>\n%s\n\n", a.WeaknessAnalysis)
	}
	if a.PatternInsights != "" {
		fmt.Fprintf(&sb, "\U0001f50d <b>Pola Trading:</b>\n%s\n\n", a.PatternInsights)
	}
	if a.Recommendations != "" {
		fmt.Fprintf(&sb, "\U0001f4a1 <b>Rekomendasi:</b>\n%s\n\n", a.Recommendations)
	}
	if a.OverallAssessment != "" {
		fmt.Fprintf(&sb, "\U0001f4dd <b>Penilaian:</b>\n%s\n", a.OverallAssessment)
	}

	fmt.Fprintf(&sb, "\n<i>Generated: %s (cache 24h)</i>", a.GeneratedAt.Format("02 Jan 15:04"))
	return sb.String()
}

// FormatMonthlyReportCard formats a monthly performance report card.
func FormatMonthlyReportCard(r *domain.MonthlyReportCard) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "\U0001f4cb <b>Monthly Report Card \u2014 %s</b>\n\n", r.YearMonth)

	sb.WriteString("<b>\U0001f4ca Statistik:</b>\n")
	fmt.Fprintf(&sb, "Total trades: <b>%d</b>\n", r.TotalTrades)
	fmt.Fprintf(&sb, "Win/Loss/BE: <b>%d</b>/%d/%d\n", r.Wins, r.Losses, r.BreakEvens)
	fmt.Fprintf(&sb, "Win Rate: <b>%.1f%%</b>\n", r.WinRate)
	fmt.Fprintf(&sb, "Total RR: <b>%+.1fR</b>\n", r.TotalRR)
	fmt.Fprintf(&sb, "Best: <b>%+.1fR</b> | Worst: <b>%+.1fR</b>\n\n", r.BestTrade, r.WorstTrade)

	sb.WriteString("<b>\U0001f3c6 Gamifikasi:</b>\n")
	fmt.Fprintf(&sb, "Level: <b>%d</b> \u2014 %s\n", r.Level, r.Title)
	fmt.Fprintf(&sb, "XP earned: <b>+%d</b>\n", r.XPEarned)
	if r.BadgesEarned > 0 {
		fmt.Fprintf(&sb, "Badges earned: <b>%d</b>\n", r.BadgesEarned)
	}
	if r.LongestStreak > 0 {
		fmt.Fprintf(&sb, "Longest streak: <b>%d hari</b>\n", r.LongestStreak)
	}

	if len(r.AssetBreakdown) > 0 {
		sb.WriteString("\n<b>Per Asset:</b>\n")
		for asset, stats := range r.AssetBreakdown {
			fmt.Fprintf(&sb, "  %s: %d trades, %.1f%% WR, %+.1fR\n", asset, stats.Total, stats.WinRate, stats.TotalRR)
		}
	}

	if r.AISummary != "" {
		fmt.Fprintf(&sb, "\n\U0001f916 <b>AI Summary:</b>\n%s\n", r.AISummary)
	}

	if r.TotalTrades == 0 {
		sb.WriteString("\n\U0001f4ed Tidak ada trade bulan ini.")
	}

	return sb.String()
}

// FormatRevengeWarning returns a psychological cooldown warning if consecutive losses today >= 3.
func FormatRevengeWarning(consecutiveLosses int) string {
	if consecutiveLosses < 3 {
		return ""
	}
	return fmt.Sprintf("\n⚠️ <b>Cooldown Warning:</b> Kamu sudah mengalami <b>%dx LOSS berturut-turut</b> hari ini. Istirahat sejenak, evaluasi setup &amp; emosimu, dan hindari <i>revenge trading</i>! 🧘‍♂️", consecutiveLosses)
}

// FormatOpenTrades formats the list of open trades with action buttons to close them.
func FormatOpenTrades(trades []domain.Trade) (string, [][]InlineButton) {
	if len(trades) == 0 {
		return "📭 Tidak ada trade yang berstatus <b>OPEN</b> saat ini.", nil
	}
	var sb strings.Builder
	sb.WriteString("⏳ <b>Open Trades</b>\nPilih hasil untuk menutup trade:\n\n")

	limit := len(trades)
	if limit > 8 {
		limit = 8
	}
	var rows [][]InlineButton
	for i := range limit {
		t := trades[i]
		fmt.Fprintf(&sb, "<b>#%d</b> %s <b>%s</b> (%s)", i+1, t.Date.Format("02 Jan"), t.Symbol, t.Direction)
		if t.TimeWindow != "" {
			fmt.Fprintf(&sb, " • %s", t.TimeWindow)
		}
		sb.WriteString("\n")
		if t.ID != "" {
			rows = append(rows, []InlineButton{
				{Text: fmt.Sprintf("✅ #%d WIN", i+1), CallbackData: "open:win:" + t.ID},
				{Text: fmt.Sprintf("❌ #%d LOSS", i+1), CallbackData: "open:loss:" + t.ID},
				{Text: fmt.Sprintf("➖ #%d BE", i+1), CallbackData: "open:be:" + t.ID},
			})
		}
	}
	return sb.String(), rows
}

// FormatHistoryPage formats a paginated list of trades with Edit and Delete buttons.
func FormatHistoryPage(trades []domain.Trade, page int, pageSize int) (string, [][]InlineButton) {
	if len(trades) == 0 {
		return "📭 Belum ada riwayat trade.", nil
	}
	if pageSize <= 0 {
		pageSize = 5
	}
	totalPages := (len(trades) + pageSize - 1) / pageSize
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}
	start := page * pageSize
	end := start + pageSize
	if end > len(trades) {
		end = len(trades)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "📜 <b>Riwayat Trade</b> (Hal %d/%d — Total %d)\n\n", page+1, totalPages, len(trades))

	var rows [][]InlineButton
	for i := start; i < end; i++ {
		t := trades[i]
		num := i + 1
		rrStr := ""
		if t.ResultRR != 0 || t.Status == domain.StatusBE {
			rrStr = fmt.Sprintf(" (%+.1fR)", t.ResultRR)
		}
		fmt.Fprintf(&sb, "<b>#%d</b> • %s • <b>%s</b> %s — <b>%s</b>%s\n",
			num, t.Date.Format("02 Jan"), t.Symbol, t.Direction, t.Status, rrStr)
		if t.TimeWindow != "" || t.Confluence != "" {
			fmt.Fprintf(&sb, "   <i>%s %s</i>\n", t.TimeWindow, t.Confluence)
		}
		if t.ID != "" {
			rows = append(rows, []InlineButton{
				{Text: fmt.Sprintf("✏️ Edit #%d", num), CallbackData: "hist:edit:" + t.ID},
				{Text: fmt.Sprintf("🗑 Hapus #%d", num), CallbackData: "hist:del:" + t.ID},
			})
		}
	}

	if totalPages > 1 {
		var nav []InlineButton
		if page > 0 {
			nav = append(nav, InlineButton{Text: "⬅️ Prev", CallbackData: fmt.Sprintf("hist:page:%d", page-1)})
		}
		if page+1 < totalPages {
			nav = append(nav, InlineButton{Text: "Next ➡️", CallbackData: fmt.Sprintf("hist:page:%d", page+1)})
		}
		if len(nav) > 0 {
			rows = append(rows, nav)
		}
	}

	return sb.String(), rows
}

// FormatEditTradeMenu builds the inline menu for editing a specific trade.
func FormatEditTradeMenu(t *domain.Trade) (string, [][]InlineButton) {
	var sb strings.Builder
	sb.WriteString("✏️ <b>Edit Trade</b>\n\n")
	fmt.Fprintf(&sb, "Symbol: <b>%s</b> (%s)\n", t.Symbol, t.AssetType.String())
	fmt.Fprintf(&sb, "Direction: <b>%s</b>\n", t.Direction)
	fmt.Fprintf(&sb, "Status: <b>%s</b> (%+.1fR)\n", t.Status, t.ResultRR)
	fmt.Fprintf(&sb, "Session: <b>%s</b>\n", t.TimeWindow)
	fmt.Fprintf(&sb, "Confluence: %s\n\n", t.Confluence)
	sb.WriteString("Pilih bagian yang ingin diubah:")

	rows := [][]InlineButton{
		{
			{Text: "🔤 Pair/Symbol", CallbackData: "editf:symbol:" + t.ID},
			{Text: "↕️ Direction", CallbackData: "editf:dir:" + t.ID},
		},
		{
			{Text: "🎯 Status & RR", CallbackData: "editf:status:" + t.ID},
			{Text: "🕒 Session", CallbackData: "editf:session:" + t.ID},
		},
		{
			{Text: "📝 Confluence", CallbackData: "editf:conf:" + t.ID},
			{Text: "🗑 Hapus Trade", CallbackData: "hist:del:" + t.ID},
		},
	}
	return sb.String(), rows
}
