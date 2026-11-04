import { Schema } from 'effect'

/** Wire schemas for the Go API. They mirror internal/api/handlers.go; a field
 *  added there without a matching field here fails decoding instead of being
 *  silently dropped. */

export const Member = Schema.Struct({
  id: Schema.String,
  telegram_id: Schema.Number,
  discord_id: Schema.OptionFromOptional(Schema.String),
  username: Schema.String,
  first_name: Schema.String,
  role: Schema.String,
  joined_at: Schema.String,
})
export type Member = typeof Member.Type

export const Trade = Schema.Struct({
  id: Schema.String,
  symbol: Schema.String,
  direction: Schema.String,
  asset_type: Schema.String,
  status: Schema.String,
  date: Schema.String,
  result_rr: Schema.Number,
  time_window: Schema.String,
  confluence: Schema.String,
  notes: Schema.String,
  has_screenshot: Schema.Boolean,
  screenshot_url: Schema.OptionFromOptional(Schema.String),
  created_at: Schema.String,
  owner_id: Schema.OptionFromOptional(Schema.String),
  owner_username: Schema.OptionFromOptional(Schema.String),
})
export type Trade = typeof Trade.Type

export const TradesPayload = Schema.Struct({ trades: Schema.Array(Trade) })
export type TradesPayload = typeof TradesPayload.Type

export const AssetStats = Schema.Struct({
  Total: Schema.Number,
  Wins: Schema.Number,
  Losses: Schema.Number,
  BreakEvens: Schema.Number,
  TotalRR: Schema.Number,
  WinRate: Schema.Number,
})
export type AssetStats = typeof AssetStats.Type

export const Stats = Schema.Struct({
  TotalTrades: Schema.Number,
  Wins: Schema.Number,
  Losses: Schema.Number,
  BreakEvens: Schema.Number,
  OpenTrades: Schema.Number,
  WinRate: Schema.Number,
  TotalRR: Schema.Number,
  AvgRR: Schema.Number,
  Expectancy: Schema.Number,
  ProfitFactor: Schema.Number,
  BestRR: Schema.Number,
  WorstRR: Schema.Number,
  CurStreak: Schema.Number,
  MaxWinStrk: Schema.Number,
  ByAsset: Schema.Record(Schema.String, AssetStats),
  BySession: Schema.Record(Schema.String, AssetStats),
  MaxDrawdownR: Schema.Number,
})
export type Stats = typeof Stats.Type

/** Stats may be absent when the member has no trades yet, which the UI shows as
 *  an empty state rather than as zeroes. */
export const MaybeStats = Schema.OptionFromNullOr(Stats)
export type MaybeStats = typeof MaybeStats.Type

export const StatsPayload = Schema.Struct({ stats: MaybeStats })
export type StatsPayload = typeof StatsPayload.Type

export const Profile = Schema.Struct({
  telegram_id: Schema.Number,
  total_xp: Schema.Number,
  level: Schema.Number,
  title: Schema.String,
  updated_at: Schema.String,
})
export type Profile = typeof Profile.Type

export const Streak = Schema.Struct({
  telegram_id: Schema.Number,
  current_streak: Schema.Number,
  longest_streak: Schema.Number,
  last_log_date: Schema.String,
  updated_at: Schema.String,
})
export type Streak = typeof Streak.Type

export const BadgeAward = Schema.Struct({
  telegram_id: Schema.Number,
  badge_id: Schema.String,
  awarded_at: Schema.String,
})
export type BadgeAward = typeof BadgeAward.Type

export const ProfilePayload = Schema.Struct({
  member: Member,
  profile: Schema.OptionFromNullOr(Profile),
  streak: Schema.OptionFromNullOr(Streak),
  badges: Schema.Array(BadgeAward),
})
export type ProfilePayload = typeof ProfilePayload.Type

export const LeaderboardEntry = Schema.Struct({
  Username: Schema.String,
  FirstName: Schema.String,
  TelegramID: Schema.Number,
  WinRate: Schema.Number,
  TotalRR: Schema.Number,
  TotalTrades: Schema.Number,
})
export type LeaderboardEntry = typeof LeaderboardEntry.Type

export const LeaderboardPayload = Schema.Struct({
  entries: Schema.Array(LeaderboardEntry),
})
export type LeaderboardPayload = typeof LeaderboardPayload.Type

export const MembersPayload = Schema.Struct({ members: Schema.Array(Member) })
export type MembersPayload = typeof MembersPayload.Type

/** Dashboard is composed from the two endpoints a member already has, so it
 *  needs no dedicated server route. */
export const DashboardPayload = Schema.Struct({
  trades: Schema.Array(Trade),
  stats: MaybeStats,
})
export type DashboardPayload = typeof DashboardPayload.Type

/** Error envelope shared by every endpoint. */
export const ApiError = Schema.Struct({ error: Schema.String })
export type ApiError = typeof ApiError.Type
