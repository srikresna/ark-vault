import { Option, Schema, pipe } from 'effect'
import { Route } from 'foldkit'
import { defineRouteUnion, literal } from 'foldkit/route'

/** Routes of the app. Signed-out visitors are pinned to Login or Link; the rest
 *  require a session. */
export const AppRoute = defineRouteUnion({
  Login: {},
  Link: {},
  Dashboard: {},
  Journal: {},
  Stats: {},
  Profile: {},
  Leaderboard: {},
  Admin: {},
  NotFound: { path: Schema.String },
})

export type AppRoute = typeof AppRoute.Type

export const loginRouter = pipe(literal('login'), Route.mapTo(AppRoute.Login))
export const linkRouter = pipe(literal('link'), Route.mapTo(AppRoute.Link))
export const dashboardRouter = pipe(
  literal('dashboard'),
  Route.mapTo(AppRoute.Dashboard),
)
export const journalRouter = pipe(
  literal('journal'),
  Route.mapTo(AppRoute.Journal),
)
export const statsRouter = pipe(literal('stats'), Route.mapTo(AppRoute.Stats))
export const profileRouter = pipe(
  literal('profile'),
  Route.mapTo(AppRoute.Profile),
)
export const leaderboardRouter = pipe(
  literal('leaderboard'),
  Route.mapTo(AppRoute.Leaderboard),
)
export const adminRouter = pipe(literal('admin'), Route.mapTo(AppRoute.Admin))
export const rootRouter = pipe(Route.root, Route.mapTo(AppRoute.Dashboard))

const routeParser = Route.oneOf(
  loginRouter,
  linkRouter,
  dashboardRouter,
  journalRouter,
  statsRouter,
  profileRouter,
  leaderboardRouter,
  adminRouter,
  rootRouter,
)

export const urlToAppRoute = Route.parseUrlWithFallback(
  routeParser,
  AppRoute.NotFound,
)

/** path is the single place that maps a route back to a URL, so a link and a
 *  router entry can never disagree. */
export const path = (route: AppRoute): string => {
  switch (route._tag) {
    case 'Login':
      return '/login'
    case 'Link':
      return '/link'
    case 'Dashboard':
      return '/dashboard'
    case 'Journal':
      return '/journal'
    case 'Stats':
      return '/stats'
    case 'Profile':
      return '/profile'
    case 'Leaderboard':
      return '/leaderboard'
    case 'Admin':
      return '/admin'
    case 'NotFound':
      return '/not-found'
  }
}

/** routesWithoutSession are the only pages a signed-out visitor may see. */
export const isPublicRoute = (route: AppRoute): boolean =>
  route._tag === 'Login' || route._tag === 'Link' || route._tag === 'NotFound'

/** Navigation items. Admin is filtered by role at render time, not here, so one
 *  list serves every member. */
export const navItems: ReadonlyArray<{
  route: AppRoute
  label: string
  adminOnly?: boolean
}> = [
  { route: AppRoute.Dashboard(), label: 'Dashboard' },
  { route: AppRoute.Journal(), label: 'Jurnal' },
  { route: AppRoute.Stats(), label: 'Statistik' },
  { route: AppRoute.Profile(), label: 'Profil' },
  { route: AppRoute.Leaderboard(), label: 'Leaderboard' },
  { route: AppRoute.Admin(), label: 'Admin', adminOnly: true },
]

/** queryParam reads a value from a URL's search string. The Url type carries the
 *  raw query, so this is the one place that parses it. */
export const queryParam = (
  search: Option.Option<string>,
  name: string,
): Option.Option<string> =>
  Option.flatMap(search, raw => {
    const params = new URLSearchParams(raw)
    const value = params.get(name)
    return value === null || value.length === 0 ? Option.none() : Option.some(value)
  })
