import { Schema } from 'effect'
import { Dialog } from '@foldkit/ui'
import { defineMessageUnion } from 'foldkit/message'
import { UrlRequest } from 'foldkit/navigation'
import { Url } from 'foldkit/url'

import {
  DashboardPayload,
  LeaderboardEntry,
  Member,
  ProfilePayload,
  StatsPayload,
  Trade,
} from './domain/api'

/** Every transition the app can perform. Names describe what the user did or
 *  what completed, never the implementation: `UpdatedSymbol`, not
 *  `SetFormField`. */
export const Message = defineMessageUnion({
  // navigation
  ClickedLink: { request: UrlRequest },
  ChangedUrl: { url: Url },
  CompletedNavigateInternal: {},
  CompletedLoadExternal: {},

  // session
  LoadedSession: {},
  SucceededLoadSession: { member: Member },
  FailedLoadSession: {},
  ReceivedTelegramAuth: { payload: Schema.String },
  SucceededSignIn: { member: Member },
  FailedSignIn: { error: Schema.String },
  ClickedSignOut: {},
  SucceededSignOut: {},
  FailedSignOut: { error: Schema.String },

  // dashboard
  LoadedDashboard: {},
  SucceededLoadDashboard: { payload: DashboardPayload },
  FailedLoadDashboard: { error: Schema.String },

  // journal
  LoadedTrades: {},
  SucceededLoadTrades: { trades: Schema.Array(Trade) },
  FailedLoadTrades: { error: Schema.String },
  UpdatedSymbol: { value: Schema.String },
  UpdatedDirection: { value: Schema.String },
  UpdatedStatus: { value: Schema.String },
  UpdatedDate: { value: Schema.String },
  UpdatedResultRR: { value: Schema.String },
  UpdatedTimeWindow: { value: Schema.String },
  UpdatedConfluence: { value: Schema.String },
  UpdatedNotes: { value: Schema.String },
  ClickedSubmitTrade: {},
  SucceededCreateTrade: { trade: Trade },
  FailedCreateTrade: { error: Schema.String },
  ClickedDeleteTrade: { id: Schema.String },
  ClickedConfirmDelete: {},
  ClickedCancelDelete: {},
  SucceededDeleteTrade: { id: Schema.String },
  FailedDeleteTrade: { error: Schema.String },

  // stats
  LoadedStats: {},
  SucceededLoadStats: { payload: StatsPayload },
  FailedLoadStats: { error: Schema.String },
  UpdatedStatsPeriod: { value: Schema.String },

  // profile
  LoadedProfile: {},
  SucceededLoadProfile: { payload: ProfilePayload },
  FailedLoadProfile: { error: Schema.String },

  // leaderboard
  LoadedLeaderboard: {},
  SucceededLoadLeaderboard: { entries: Schema.Array(LeaderboardEntry) },
  FailedLoadLeaderboard: { error: Schema.String },

  // admin
  LoadedMembers: {},
  SucceededLoadMembers: { members: Schema.Array(Member) },
  FailedLoadMembers: { error: Schema.String },
  ClickedChangeRole: { id: Schema.String, role: Schema.String },
  SucceededChangeRole: { id: Schema.String, role: Schema.String },
  FailedChangeRole: { error: Schema.String },

  // delete confirmation dialog
  GotConfirmDialogMessage: { message: Dialog.Message },

  // shared feedback
  DismissedNotice: {},
})

export type Message = typeof Message.Type
