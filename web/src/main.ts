import { Option, Schema } from 'effect'
import { AsyncData } from 'foldkit'
import { Dialog } from '@foldkit/ui'
import type { Url } from 'foldkit/url'

import { emptyTradeForm, todayISO } from './model'
import type { Model } from './model'
import { AppRoute, urlToAppRoute } from './route'

/** Flags carry nothing from the server: the session lives in an HttpOnly cookie
 *  the browser sends automatically, so the first `GET /api/me` decides whether
 *  the visitor is signed in. That keeps no token in the HTML or in JavaScript. */
export const Flags = Schema.Struct({
  botUsername: Schema.String,
})
export type Flags = typeof Flags.Type

export const init = (flags: Flags, url: Url): Model => {
  const route = urlToAppRoute(url)
  return {
    // Every visitor starts signed out. The session cookie is HttpOnly, so the
    // client cannot read it; a Command asks the server instead.
    route: route._tag === 'Login' ? route : AppRoute.Login(),
    botUsername: flags.botUsername,
    session: Option.none(),
    dashboard: AsyncData.Idle(),
    trades: AsyncData.Idle(),
    stats: AsyncData.Idle(),
    profile: AsyncData.Idle(),
    leaderboard: AsyncData.Idle(),
    members: AsyncData.Idle(),
    form: emptyTradeForm(todayISO()),
    statsPeriod: 'month',
    pendingDelete: Option.none(),
    pendingDiscordId: Option.none(),
    confirmDialog: Dialog.init({ id: 'confirm-dialog' }),
    isSubmitting: false,
    notice: Option.none(),
    formError: Option.none(),
  }
}
