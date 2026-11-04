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
    // The URL is trusted here: the session cookie is HttpOnly, so whether the
    // visitor is signed in is not known yet. The view renders Login for a
    // private route until LoadSession answers, and ChangedUrl pins a signed-out
    // visitor to the public pages. Forcing Login in init would throw away a
    // deep link the member followed on purpose.
    route: route._tag === 'NotFound' ? AppRoute.Login() : route,
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
