package domain

import (
	"fmt"
	"time"

	"github.com/arkcode369/ark-vault/pkg/timeutil"
)

// ChallengeType defines what's being competed on.
type ChallengeType string

const (
	ChallengeMostTrades ChallengeType = "most_trades"
	ChallengeBestRR     ChallengeType = "best_rr"
	ChallengeHighestWR  ChallengeType = "highest_wr"
	ChallengeMostRR     ChallengeType = "most_rr"
)

// WeeklyChallenge defines a weekly challenge.
type WeeklyChallenge struct {
	YearWeek    string        `json:"year_week"` // "2026-W12"
	Type        ChallengeType `json:"type"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	StartDate   time.Time     `json:"start_date"`
	EndDate     time.Time     `json:"end_date"`
	Finalized   bool          `json:"finalized"`
}
// ChallengeResult holds the final standings after a challenge is finalized.
type ChallengeResult struct {
	Rank       int     `json:"rank"`
	TelegramID int64   `json:"telegram_id"`
	Username   string  `json:"username"`
	Value      float64 `json:"value"`
}

// ChallengeTemplates defines the rotating set of weekly challenges.
var ChallengeTemplates = []struct {
	Type        ChallengeType
	Title       string
	Description string
}{
	{ChallengeMostTrades, "Trade Warrior", "Member dengan trade terbanyak minggu ini"},
	{ChallengeBestRR, "RR Hunter", "Member dengan single trade terbaik (RR tertinggi)"},
	{ChallengeHighestWR, "Sharp Shooter", "Member dengan win rate tertinggi (min 3 trades)"},
	{ChallengeMostRR, "Profit King", "Member dengan total RR tertinggi minggu ini"},
}

// YearWeekString returns the ISO year-week string for a time.
//
// t is normalised to WIB first: ISOWeek() uses t's own location, so a caller
// passing UTC would derive a different week key around the Sunday/Monday
// boundary and create or look up the wrong challenge.
func YearWeekString(t time.Time) string {
	year, week := t.In(timeutil.WIB).ISOWeek()
	return fmt.Sprintf("%d-W%02d", year, week)
}
