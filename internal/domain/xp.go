package domain

// XP amounts for various actions
const (
	XPTradeLogged       = 10
	XPWinBonus          = 5
	XPWinHighRR         = 10 // replaces XPWinBonus for 2R+
	XPDailyStreak       = 5
	XPStreak7           = 25
	XPStreak30          = 100
	XPBadgeEarned       = 20
	XPChallengeParticip = 10
	XPChallenge1st      = 50
	XPChallenge2nd      = 30
	XPChallenge3rd      = 20
	XPGoalAchieved      = 75
	XPScreenshot         = 3
	MaxDailyTradeXPCount = 3
)

// LevelThreshold defines a level and its XP requirement.
type LevelThreshold struct {
	Level int
	XP    int
	Title string
}

// LevelTable is the ordered list of level thresholds.
var LevelTable = []LevelThreshold{
	{1, 0, "Retail"},
	{2, 300, "Chartist"},
	{3, 1000, "Analyst"},
	{4, 2500, "Strategist"},
	{5, 5000, "Systematic"},
	{6, 9000, "Algorithmic"},
	{7, 15000, "Quantitative"},
	{8, 23000, "Portfolio Manager"},
	{9, 35000, "Fund Manager"},
	{10, 50000, "Market Maker"},
}

// LevelForXP returns the level and title for the given XP amount.
func LevelForXP(xp int) (int, string) {
	level := 1
	title := "Retail"
	for _, lt := range LevelTable {
		if xp >= lt.XP {
			level = lt.Level
			title = lt.Title
		} else {
			break
		}
	}
	return level, title
}

// XPForNextLevel returns the XP needed for the next level, or 0 if max level.
func XPForNextLevel(currentXP int) int {
	for _, lt := range LevelTable {
		if lt.XP > currentXP {
			return lt.XP
		}
	}
	return 0
}
