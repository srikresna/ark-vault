package importer

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// importState writes the gamification rows for members that exist after the
// Notion pass.
//
// A record whose member is unknown is counted and dropped: the old store was
// keyed by Telegram ID, so this only happens for someone who left before the
// migration, and inventing a member row would put a journal-less id into the
// roster.
func importState(ctx context.Context, tx pgx.Tx, state *State, memberIDs map[int64]string, tradeIDs map[string]string, report *Report) error {
	for _, p := range state.Profiles {
		mid, ok := memberIDs[p.TelegramID]
		if !ok {
			report.UnknownMember++
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO profiles (member_id, total_xp, level, title, updated_at)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (member_id) DO UPDATE
			   SET total_xp = EXCLUDED.total_xp, level = EXCLUDED.level,
			       title = EXCLUDED.title, updated_at = EXCLUDED.updated_at`,
			mid, p.TotalXP, p.Level, p.Title, p.UpdatedAt); err != nil {
			return fmt.Errorf("insert profile %d: %w", p.TelegramID, err)
		}
		report.Profiles++
	}

	for _, s := range state.Streaks {
		mid, ok := memberIDs[s.TelegramID]
		if !ok {
			report.UnknownMember++
			continue
		}
		var last any
		if s.LastLogDate != "" {
			last = s.LastLogDate
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO streaks (member_id, current_streak, longest_streak,
			                     last_log_date, updated_at)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (member_id) DO UPDATE
			   SET current_streak = EXCLUDED.current_streak,
			       longest_streak = EXCLUDED.longest_streak,
			       last_log_date = EXCLUDED.last_log_date,
			       updated_at = EXCLUDED.updated_at`,
			mid, s.CurrentStreak, s.LongestStreak, last, s.UpdatedAt); err != nil {
			return fmt.Errorf("insert streak %d: %w", s.TelegramID, err)
		}
		report.Streaks++
	}

	for _, e := range state.XPEvents {
		mid, ok := memberIDs[e.TelegramID]
		if !ok {
			report.UnknownMember++
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO xp_events (member_id, amount, reason, created_at)
			VALUES ($1,$2,$3,$4)`,
			mid, e.Amount, e.Reason, e.Timestamp); err != nil {
			return fmt.Errorf("insert xp event for %d: %w", e.TelegramID, err)
		}
		report.XPEvents++
	}

	for _, g := range state.Grants {
		mid, ok := memberIDs[g.TelegramID]
		if !ok {
			report.UnknownMember++
			continue
		}
		// The grant is keyed by the old Notion trade id. A trade deleted in
		// Notion before the migration has no row to point at, so the grant is
		// counted rather than inserted: a dangling grant would break the foreign
		// key and hide the omission.
		newTradeID, ok := tradeIDs[g.TradeID]
		if !ok {
			report.GrantsDropped++
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO trade_xp_grants (trade_id, member_id, base, screenshot, granted_at)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (trade_id) DO UPDATE
			   SET base = EXCLUDED.base, screenshot = EXCLUDED.screenshot,
			       granted_at = EXCLUDED.granted_at`,
			newTradeID, mid, g.Base, g.Screenshot, g.GrantedAt); err != nil {
			return fmt.Errorf("insert grant for trade %s: %w", g.TradeID, err)
		}
		report.Grants++
	}

	for _, b := range state.Badges {
		mid, ok := memberIDs[b.TelegramID]
		if !ok {
			report.UnknownMember++
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO badges (member_id, badge_id, awarded_at)
			VALUES ($1,$2,$3)
			ON CONFLICT (member_id, badge_id) DO NOTHING`,
			mid, string(b.BadgeID), b.AwardedAt); err != nil {
			return fmt.Errorf("insert badge %d/%s: %w", b.TelegramID, b.BadgeID, err)
		}
		report.Badges++
	}

	for _, g := range state.Goals {
		mid, ok := memberIDs[g.TelegramID]
		if !ok {
			report.UnknownMember++
			continue
		}
		var achievedAt any
		if !g.AchievedAt.IsZero() {
			achievedAt = g.AchievedAt
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO goals (member_id, year_month, goal_type, target_value,
			                   achieved, achieved_at, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (member_id, year_month) DO UPDATE
			   SET goal_type = EXCLUDED.goal_type,
			       target_value = EXCLUDED.target_value,
			       achieved = EXCLUDED.achieved,
			       achieved_at = EXCLUDED.achieved_at`,
			mid, g.YearMonth, string(g.GoalType), g.TargetValue,
			g.Achieved, achievedAt, g.CreatedAt); err != nil {
			return fmt.Errorf("insert goal %d/%s: %w", g.TelegramID, g.YearMonth, err)
		}
		report.Goals++
	}

	for _, r := range state.Reminders {
		mid, ok := memberIDs[r.TelegramID]
		if !ok {
			report.UnknownMember++
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO reminder_prefs (member_id, enabled, hour, chat_id,
			                            thread_id, is_community, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6, now())
			ON CONFLICT (member_id) DO UPDATE
			   SET enabled = EXCLUDED.enabled, hour = EXCLUDED.hour,
			       chat_id = EXCLUDED.chat_id, thread_id = EXCLUDED.thread_id,
			       is_community = EXCLUDED.is_community, updated_at = now()`,
			mid, r.Enabled, r.Hour, r.ChatID, r.ThreadID, r.IsCommunity); err != nil {
			return fmt.Errorf("insert reminder for %d: %w", r.TelegramID, err)
		}
		report.Reminders++
	}

	for _, c := range state.Challenges {
		if _, err := tx.Exec(ctx, `
			INSERT INTO challenges (year_week, kind, title, description,
			                        start_date, end_date, finalized)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (year_week) DO UPDATE
			   SET kind = EXCLUDED.kind, title = EXCLUDED.title,
			       description = EXCLUDED.description,
			       start_date = EXCLUDED.start_date, end_date = EXCLUDED.end_date,
			       finalized = EXCLUDED.finalized`,
			c.YearWeek, string(c.Type), c.Title, c.Description,
			c.StartDate, c.EndDate, c.Finalized); err != nil {
			return fmt.Errorf("insert challenge %s: %w", c.YearWeek, err)
		}
		report.Challenges++
	}

	return nil
}
