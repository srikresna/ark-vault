import { Schema } from 'effect'
import { AsyncData } from 'foldkit'
import { Dialog } from '@foldkit/ui'

import {
  DashboardPayload,
  LeaderboardEntry,
  Member,
  ProfilePayload,
  StatsPayload,
  Trade,
} from './domain/api'
import { AppRoute } from './route'

/** Per-page remote data. Each page owns its own AsyncData so a slow journal
 *  request never blanks the dashboard. */
export const DashboardAsync = AsyncData.Schema(DashboardPayload, Schema.String)
export const TradesAsync = AsyncData.Schema(Schema.Array(Trade), Schema.String)
export const StatsAsync = AsyncData.Schema(StatsPayload, Schema.String)
export const ProfileAsync = AsyncData.Schema(ProfilePayload, Schema.String)
export const LeaderboardAsync = AsyncData.Schema(
  Schema.Array(LeaderboardEntry),
  Schema.String,
)
export const MembersAsync = AsyncData.Schema(Schema.Array(Member), Schema.String)

/** Journal form draft. Kept as strings because every field is typed by the user
 *  and converted only when the request is built; that keeps an in-progress
 *  value like "-" from being coerced to a number mid-typing. */
export const TradeForm = Schema.Struct({
  symbol: Schema.String,
  direction: Schema.String,
  status: Schema.String,
  date: Schema.String,
  resultRR: Schema.String,
  timeWindow: Schema.String,
  confluence: Schema.String,
  notes: Schema.String,
})
export type TradeForm = typeof TradeForm.Type

/** todayISO returns the member's local calendar day, which is what a trade date
 *  field means to them. */
export const todayISO = (): string => new Date().toISOString().slice(0, 10)

export const emptyTradeForm = (today: string): TradeForm => ({
  symbol: '',
  direction: 'BUY',
  status: 'OPEN',
  date: today,
  resultRR: '',
  timeWindow: '',
  confluence: '',
  notes: '',
})

/** Notice is one line of feedback with the moment it appeared, so the view can
 *  dismiss a stale message instead of showing it forever. */
export const Notice = Schema.Struct({
  text: Schema.String,
  at: Schema.Number,
})
export type Notice = typeof Notice.Type

export const Model = Schema.Struct({
  route: AppRoute,
  /** Bot username the Telegram Login Widget needs. It is public (it appears in
   *  every bot link), so it is safe to embed. */
  botUsername: Schema.String,
  session: Schema.Option(Member),
  dashboard: DashboardAsync.schema,
  trades: TradesAsync.schema,
  stats: StatsAsync.schema,
  profile: ProfileAsync.schema,
  leaderboard: LeaderboardAsync.schema,
  members: MembersAsync.schema,
  form: TradeForm,
  /** Selected stats period: "week", "month" or "all". Kept in the model so a
   *  refresh reuses the member's choice instead of resetting to the default. */
  statsPeriod: Schema.String,
  /** Trade id awaiting delete confirmation. Delete only happens through the
   *  dialog, so a stray click can never destroy a journal entry. */
  pendingDelete: Schema.Option(Schema.String),
  /** Discord account waiting to be linked, taken from /link?discord=… . Sent
   *  with the Telegram login payload so the server attaches both identities to
   *  one member. */
  pendingDiscordId: Schema.Option(Schema.String),
  confirmDialog: Dialog.Model,
  isSubmitting: Schema.Boolean,
  notice: Schema.Option(Notice),
  /** Validation message for the journal form, produced before any request. */
  formError: Schema.Option(Schema.String),
})

export type Model = typeof Model.Type
