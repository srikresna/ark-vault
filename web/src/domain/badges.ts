/** Display names for the badge ids the API returns.
 *
 *  The registry lives in the Go domain (`internal/domain/badge.go`). The web app
 *  only shows what it receives, so this is a presentation map, not a second
 *  source of truth: an unknown id falls back to the raw id instead of
 *  disappearing. */
const badgeLabels: Readonly<Record<string, string>> = {
  first_trade: 'Trade pertama',
  '10_trades': '10 trade',
  '50_trades': '50 trade',
  '100_trades': 'Centurion',
  win_streak_3: '3 win beruntun',
  win_streak_5: '5 win beruntun',
  streak_7: 'Disiplin 7 hari',
  streak_30: 'Disiplin 30 hari',
  first_green_month: 'Bulan hijau pertama',
  '10r_month': 'Bulan 10R',
  winrate_60: 'Sharp shooter',
  challenge_winner: 'Juara mingguan',
  goal_achiever: 'Goal crusher',
}

export const badgeLabel = (id: string): string => badgeLabels[id] ?? id
