import { Effect, Option, Schema } from 'effect'
import { HttpClient, HttpClientRequest } from 'effect/http'
import { Command, Http } from 'foldkit'

import {
  ApiError,
  DashboardPayload,
  LeaderboardPayload,
  Member,
  MembersPayload,
  ProfilePayload,
  StatsPayload,
  Trade,
  TradesPayload,
} from './domain/api'
import { Message } from './message'

/** Base path of the Go API. Same origin in every environment: in dev Vite
 *  proxies /api, in production Nginx routes it to the bot container. Keeping it
 *  relative is what makes the session cookie first-party. */
const API = '/api'

type JsonResponse = Readonly<{
  status: number
  json: Effect.Effect<unknown, unknown, never>
}>

/** readError turns a non-2xx response into the sentence the server wrote, so the
 *  UI shows "symbol is required" instead of "HTTP 400". */
const readError = (response: JsonResponse): Effect.Effect<string> =>
  Schema.decodeUnknownEffect(ApiError)(
    response.json.pipe(
      Effect.orElseSucceed(() => ({ error: 'Permintaan gagal' })),
    ),
  ).pipe(
    Effect.map(decoded => decoded.error),
    Effect.orElseSucceed(() => 'Permintaan gagal'),
  )

/** describeFailure extracts a readable sentence from a thrown Effect failure
 *  without trusting its shape: the runtime can fail with a string, an Error, or
 *  a decoded API error, and only the last one carries a server message. */
const describeFailure = (error: unknown, fallback: string): string => {
  if (typeof error === 'string' && error.length > 0) {
    return error
  }
  if (error instanceof Error && error.message.length > 0) {
    return error.message
  }
  if (typeof error === 'object' && error !== null && 'error' in error) {
    return String(error.error)
  }
  return fallback
}

/** call builds a request and rewrites any transport failure into a Message, so
 *  a dropped connection reaches the user instead of crashing the runtime.
 *
 *  `Http.layer` is provided per Command: that is the seam Foldkit exposes for
 *  the HTTP client, and it keeps the layer inside the Effect instead of relying
 *  on a global. */
const call = <Ok extends Message, Err extends Message>(
  build: (client: HttpClient.HttpClient) => Effect.Effect<Ok, unknown>,
  onFailure: (message: string) => Err,
  fallback: string,
): Effect.Effect<Ok | Err> =>
  Effect.gen(function* () {
    const client = yield* HttpClient.HttpClient
    return yield* build(client)
  }).pipe(
    Effect.catch(error =>
      Effect.succeed(onFailure(describeFailure(error, fallback))),
    ),
    Effect.provide(Http.layer),
  )

const GET = (path: string) => HttpClientRequest.make('GET')(`${API}${path}`)
const POST = (path: string) => HttpClientRequest.make('POST')(`${API}${path}`)
const PATCH = (path: string) => HttpClientRequest.make('PATCH')(`${API}${path}`)
const DELETE = (path: string) => HttpClientRequest.make('DELETE')(`${API}${path}`)

// --- session ----------------------------------------------------------------

/** LoadSession asks the server who the current cookie belongs to.
 *
 *  The session cookie is HttpOnly, so the client cannot inspect it: this first
 *  request is what turns "signed out" into "signed in". A 401 is a normal
 *  answer, not an error, and produces no notice. */
export const LoadSession = Command.define('LoadSession', {
  messages: [Message.SucceededLoadSession, Message.FailedLoadSession],
  execute: call(
    client =>
      Effect.gen(function* () {
        const response = yield* client.execute(GET('/me'))
        if (response.status !== 200) {
          return Message.FailedLoadSession()
        }
        const member = yield* Schema.decodeUnknownEffect(Member)(
          yield* response.json,
        )
        return Message.SucceededLoadSession({ member })
      }),
    () => Message.FailedLoadSession(),
    'Tidak dapat memeriksa sesi',
  ),
})

/** SignInWithTelegram posts the Login Widget payload.
 *
 *  The signature is verified server-side: the bot token that signs it must never
 *  reach the browser, so the client's only job is to forward the payload it
 *  received and let the API decide whether it is genuine. */
export const SignInWithTelegram = Command.define('SignInWithTelegram', {
  args: { payload: Schema.String },
  messages: [Message.SucceededSignIn, Message.FailedSignIn],
  execute: ({ payload }) =>
    call(
      client =>
        Effect.gen(function* () {
          const body = yield* Schema.decodeUnknownEffect(
            Schema.Record(Schema.String, Schema.Unknown),
          )(JSON.parse(payload)).pipe(
            Effect.orElseSucceed(() => ({}) as Record<string, unknown>),
          )
          const response = yield* client.execute(
            HttpClientRequest.bodyJsonUnsafe(POST('/auth/telegram'), body),
          )
          if (response.status !== 200) {
            return Message.FailedSignIn({ error: yield* readError(response) })
          }
          const member = yield* Schema.decodeUnknownEffect(Member)(
            yield* response.json,
          )
          return Message.SucceededSignIn({ member })
        }),
      error => Message.FailedSignIn({ error }),
      'Login Telegram gagal',
    ),
})

export const SignOut = Command.define('SignOut', {
  messages: [Message.SucceededSignOut, Message.FailedSignOut],
  execute: call(
    client =>
      client
        .execute(POST('/auth/logout'))
        .pipe(Effect.as(Message.SucceededSignOut())),
    error => Message.FailedSignOut({ error }),
    'Gagal keluar',
  ),
})

// --- dashboard --------------------------------------------------------------

export const LoadDashboard = Command.define('LoadDashboard', {
  messages: [Message.SucceededLoadDashboard, Message.FailedLoadDashboard],
  execute: call(
    client =>
      Effect.gen(function* () {
        const tradesResponse = yield* client.execute(GET('/trades'))
        if (tradesResponse.status !== 200) {
          return Message.FailedLoadDashboard({
            error: yield* readError(tradesResponse),
          })
        }
        const trades = yield* Schema.decodeUnknownEffect(TradesPayload)(
          yield* tradesResponse.json,
        )
        const statsResponse = yield* client.execute(
          HttpClientRequest.setUrlParams(GET('/stats'), { period: 'month' }),
        )
        // Stats is optional on the server: a member with no trades yet gets
        // `null`, which is a valid empty state rather than a failure.
        const stats =
          statsResponse.status === 200
            ? yield* Schema.decodeUnknownEffect(StatsPayload)(
                yield* statsResponse.json,
              ).pipe(Effect.orElseSucceed(() => ({ stats: Option.none() })))
            : { stats: Option.none() }
        const payload: DashboardPayload = {
          trades: trades.trades,
          stats: stats.stats,
        }
        return Message.SucceededLoadDashboard({ payload })
      }),
    error => Message.FailedLoadDashboard({ error }),
    'Tidak dapat memuat dashboard',
  ),
})

// --- journal ----------------------------------------------------------------

export const LoadTrades = Command.define('LoadTrades', {
  messages: [Message.SucceededLoadTrades, Message.FailedLoadTrades],
  execute: call(
    client =>
      Effect.gen(function* () {
        const response = yield* client.execute(GET('/trades'))
        if (response.status !== 200) {
          return Message.FailedLoadTrades({ error: yield* readError(response) })
        }
        const body = yield* Schema.decodeUnknownEffect(TradesPayload)(
          yield* response.json,
        )
        return Message.SucceededLoadTrades({ trades: body.trades })
      }),
    error => Message.FailedLoadTrades({ error }),
    'Tidak dapat memuat jurnal',
  ),
})

export const CreateTrade = Command.define('CreateTrade', {
  args: {
    symbol: Schema.String,
    direction: Schema.String,
    status: Schema.String,
    date: Schema.String,
    resultRR: Schema.String,
    timeWindow: Schema.String,
    confluence: Schema.String,
    notes: Schema.String,
  },
  messages: [Message.SucceededCreateTrade, Message.FailedCreateTrade],
  execute: args =>
    call(
      client =>
        Effect.gen(function* () {
          const parsedRR = Number.parseFloat(args.resultRR)
          const response = yield* client.execute(
            HttpClientRequest.bodyJsonUnsafe(POST('/trades'), {
              symbol: args.symbol,
              direction: args.direction,
              status: args.status,
              date: args.date,
              result_rr: Number.isFinite(parsedRR) ? parsedRR : 0,
              time_window: args.timeWindow,
              confluence: args.confluence,
              notes: args.notes,
            }),
          )
          if (response.status !== 201) {
            return Message.FailedCreateTrade({
              error: yield* readError(response),
            })
          }
          const decoded = yield* Schema.decodeUnknownEffect(
            Schema.Struct({ trade: Trade }),
          )(yield* response.json)
          return Message.SucceededCreateTrade({ trade: decoded.trade })
        }),
      error => Message.FailedCreateTrade({ error }),
      'Tidak dapat menyimpan trade',
    ),
})

export const DeleteTrade = Command.define('DeleteTrade', {
  args: { id: Schema.String },
  messages: [Message.SucceededDeleteTrade, Message.FailedDeleteTrade],
  execute: ({ id }) =>
    call(
      client =>
        Effect.gen(function* () {
          const response = yield* client.execute(
            DELETE(`/trades/${encodeURIComponent(id)}`),
          )
          if (response.status !== 204) {
            return Message.FailedDeleteTrade({
              error: yield* readError(response),
            })
          }
          return Message.SucceededDeleteTrade({ id })
        }),
      error => Message.FailedDeleteTrade({ error }),
      'Tidak dapat menghapus trade',
    ),
})

// --- stats ------------------------------------------------------------------

export const LoadStats = Command.define('LoadStats', {
  args: { period: Schema.String },
  messages: [Message.SucceededLoadStats, Message.FailedLoadStats],
  execute: ({ period }) =>
    call(
      client =>
        Effect.gen(function* () {
          const response = yield* client.execute(
            HttpClientRequest.setUrlParams(GET('/stats'), { period }),
          )
          if (response.status !== 200) {
            return Message.FailedLoadStats({
              error: yield* readError(response),
            })
          }
          const body = yield* Schema.decodeUnknownEffect(StatsPayload)(
            yield* response.json,
          )
          return Message.SucceededLoadStats({ payload: body })
        }),
      error => Message.FailedLoadStats({ error }),
      'Tidak dapat memuat statistik',
    ),
})

// --- profile ----------------------------------------------------------------

export const LoadProfile = Command.define('LoadProfile', {
  messages: [Message.SucceededLoadProfile, Message.FailedLoadProfile],
  execute: call(
    client =>
      Effect.gen(function* () {
        const response = yield* client.execute(GET('/profile'))
        if (response.status !== 200) {
          return Message.FailedLoadProfile({
            error: yield* readError(response),
          })
        }
        const body = yield* Schema.decodeUnknownEffect(ProfilePayload)(
          yield* response.json,
        )
        return Message.SucceededLoadProfile({ payload: body })
      }),
    error => Message.FailedLoadProfile({ error }),
    'Tidak dapat memuat profil',
  ),
})

// --- leaderboard ------------------------------------------------------------

export const LoadLeaderboard = Command.define('LoadLeaderboard', {
  messages: [Message.SucceededLoadLeaderboard, Message.FailedLoadLeaderboard],
  execute: call(
    client =>
      Effect.gen(function* () {
        const response = yield* client.execute(
          HttpClientRequest.setUrlParams(GET('/leaderboard'), {
            period: 'month',
            metric: 'rr',
          }),
        )
        if (response.status !== 200) {
          return Message.FailedLoadLeaderboard({
            error: yield* readError(response),
          })
        }
        const body = yield* Schema.decodeUnknownEffect(LeaderboardPayload)(
          yield* response.json,
        )
        return Message.SucceededLoadLeaderboard({ entries: body.entries })
      }),
    error => Message.FailedLoadLeaderboard({ error }),
    'Tidak dapat memuat leaderboard',
  ),
})

// --- admin ------------------------------------------------------------------

export const LoadMembers = Command.define('LoadMembers', {
  messages: [Message.SucceededLoadMembers, Message.FailedLoadMembers],
  execute: call(
    client =>
      Effect.gen(function* () {
        const response = yield* client.execute(GET('/members'))
        if (response.status !== 200) {
          return Message.FailedLoadMembers({
            error: yield* readError(response),
          })
        }
        const body = yield* Schema.decodeUnknownEffect(MembersPayload)(
          yield* response.json,
        )
        return Message.SucceededLoadMembers({ members: body.members })
      }),
    error => Message.FailedLoadMembers({ error }),
    'Tidak dapat memuat daftar member',
  ),
})

export const ChangeRole = Command.define('ChangeRole', {
  args: { id: Schema.String, role: Schema.String },
  messages: [Message.SucceededChangeRole, Message.FailedChangeRole],
  execute: ({ id, role }) =>
    call(
      client =>
        Effect.gen(function* () {
          const response = yield* client.execute(
            HttpClientRequest.bodyJsonUnsafe(
              PATCH(`/members/${encodeURIComponent(id)}/role`),
              { role },
            ),
          )
          if (response.status !== 200) {
            return Message.FailedChangeRole({
              error: yield* readError(response),
            })
          }
          return Message.SucceededChangeRole({ id, role })
        }),
      error => Message.FailedChangeRole({ error }),
      'Tidak dapat mengubah role',
    ),
})
