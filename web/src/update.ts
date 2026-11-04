import { Effect, Match, Option, Schema } from 'effect'
import { AsyncData, Command, Update } from 'foldkit'
import { Dialog } from '@foldkit/ui'
import { UrlRequest, load, pushUrl } from 'foldkit/navigation'
import { modifyFields } from 'foldkit/struct'
import { toString as urlToString } from 'foldkit/url'

import {
  ChangeRole,
  CreateTrade,
  DeleteTrade,
  LoadDashboard,
  LoadLeaderboard,
  LoadMembers,
  LoadProfile,
  LoadSession,
  LoadStats,
  LoadTrades,
  SignInWithTelegram,
  SignOut,
} from './command'
import { Member } from './domain/api'
import { Message } from './message'
import { Model, emptyTradeForm, todayISO } from './model'
import { AppRoute, isPublicRoute, path, queryParam, urlToAppRoute } from './route'

const NavigateInternal = Command.define('NavigateInternal', {
  args: { url: Schema.String },
  messages: [Message.CompletedNavigateInternal],
  execute: ({ url }) =>
    pushUrl(url).pipe(Effect.as(Message.CompletedNavigateInternal())),
})

const LoadExternal = Command.define('LoadExternal', {
  args: { href: Schema.String },
  messages: [Message.CompletedLoadExternal],
  execute: ({ href }) =>
    load(href).pipe(Effect.as(Message.CompletedLoadExternal())),
})

type UpdateReturn = Update.Return<Model, Message>
const withUpdateReturn = Match.withReturnType<UpdateReturn>()

/** foldConfirmDialog routes the delete-confirmation dialog through the child
 *  fold, so the dialog keeps ownership of its own state (open flag, focus
 *  restoration, Escape handling) and the parent only reacts to its OutMessages.
 *
 *  Closing the dialog without confirming clears the pending trade id: a later
 *  confirm must never delete a trade the member had already dismissed. */
const foldConfirmDialog = Update.foldChild({
  update: Dialog.update,
  read: (model: Model) => Option.some(model.confirmDialog),
  write: (model, next) => modifyFields(model, { confirmDialog: () => next }),
  toParentMessage: message => Message.GotConfirmDialogMessage({ message }),
  foldOutMessage: Dialog.OutMessage.match<Update.Step<Model, Message>>({
    Opened: () => model => ({ model }),
    Closed: () => model => ({
      model: modifyFields(model, { pendingDelete: () => Option.none() }),
    }),
  }),
})

/** Opening and closing the dialog are Commands, not inline child updates: the
 *  child's commands belong to the child's Message space, and routing them
 *  through a parent Message is what keeps that boundary intact. */
const OpenConfirmDialog = Command.define('OpenConfirmDialog', {
  messages: [Message.GotConfirmDialogMessage],
  execute: Effect.succeed(
    Message.GotConfirmDialogMessage({ message: Dialog.Message.RequestedOpen() }),
  ),
})

const CloseConfirmDialog = Command.define('CloseConfirmDialog', {
  messages: [Message.GotConfirmDialogMessage],
  execute: Effect.succeed(
    Message.GotConfirmDialogMessage({
      message: Dialog.Message.RequestedClose(),
    }),
  ),
})

/** commandsFor loads the data a page needs. Called after sign-in and on every
 *  navigation, so a page never renders another page's stale payload. */
const commandsFor = (
  route: AppRoute,
  period: string,
): ReadonlyArray<Command.Command<Message>> =>
  Match.value(route).pipe(
    Match.tagsExhaustive({
      Dashboard: () => [LoadDashboard()],
      Journal: () => [LoadTrades()],
      Stats: () => [LoadStats({ period })],
      Profile: () => [LoadProfile()],
      Leaderboard: () => [LoadLeaderboard()],
      Admin: () => [LoadMembers()],
      Login: () => [],
      Link: () => [],
      NotFound: () => [],
    }),
  )

/** validateForm returns the first blocking problem, so the member fixes one
 *  thing at a time instead of reading a wall of messages. */
const validateForm = (form: Model['form']): Option.Option<string> => {
  if (form.symbol.trim().length === 0) {
    return Option.some('Pair wajib diisi.')
  }
  if (form.date.trim().length === 0) {
    return Option.some('Tanggal wajib diisi.')
  }
  if (
    form.resultRR.trim().length > 0 &&
    !Number.isFinite(Number(form.resultRR))
  ) {
    return Option.some('RR harus berupa angka, misalnya 2 atau -1.5.')
  }
  return Option.none()
}

const notice = (text: string) => Option.some({ text, at: Date.now() })

export const update = (model: Model, message: Message): UpdateReturn =>
  Message.match<UpdateReturn>(message, {
    ClickedLink: ({ request }) =>
      UrlRequest.match<UpdateReturn>(request, {
        Internal: ({ url }) => ({
          model,
          commands: [NavigateInternal({ url: urlToString(url) })],
        }),
        External: ({ href }) => ({
          model,
          commands: [LoadExternal({ href })],
        }),
      }),

    ChangedUrl: ({ url }) => {
      const route = urlToAppRoute(url)
      const discordId = queryParam(url.search, 'discord')
      // A signed-out visitor is pinned to the public pages: private pages have
      // no data and their requests would only produce 401s.
      const nextRoute = Option.isSome(model.session) ? route : (
        isPublicRoute(route) ? route : AppRoute.Login()
      )
      return {
        model: modifyFields(model, {
          route: () => nextRoute,
          pendingDiscordId: () => discordId,
          notice: () => Option.none(),
        }),
        commands: commandsFor(nextRoute, model.statsPeriod),
      }
    },

    ReceivedTelegramAuth: ({ payload }) => ({
      model,
      commands: [SignInWithTelegram({ payload })],
    }),

    CompletedNavigateInternal: () => ({ model }),
    CompletedLoadExternal: () => ({ model }),

    LoadedSession: () => ({ model, commands: [LoadSession()] }),
    SucceededLoadSession: ({ member }) => ({
      model: modifyFields(model, { session: () => Option.some(member) }),
      // Load whatever the deep link asked for. The route is already the URL's
      // route, so signing in does not throw away where the member was going.
      commands: commandsFor(model.route, model.statsPeriod),
    }),
    FailedLoadSession: () => ({ model }),

    SucceededSignIn: ({ member }) => {
      const route =
        model.route._tag === 'Login' ? AppRoute.Dashboard() : model.route
      return {
        model: modifyFields(model, {
          session: () => Option.some(member),
          route: () => route,
          notice: () => Option.none(),
        }),
        commands: commandsFor(route, model.statsPeriod),
      }
    },
    FailedSignIn: ({ error }) => ({
      model: modifyFields(model, { notice: () => notice(error) }),
    }),

    ClickedSignOut: () => ({ model, commands: [SignOut()] }),
    SucceededSignOut: () => ({
      model: modifyFields(model, {
        session: () => Option.none(),
        route: () => AppRoute.Login(),
        dashboard: () => AsyncData.Idle(),
        trades: () => AsyncData.Idle(),
        stats: () => AsyncData.Idle(),
        profile: () => AsyncData.Idle(),
        leaderboard: () => AsyncData.Idle(),
        members: () => AsyncData.Idle(),
        notice: () => Option.none(),
      }),
    }),
    FailedSignOut: ({ error }) => ({
      model: modifyFields(model, { notice: () => notice(error) }),
    }),

    LoadedDashboard: () => ({
      model: modifyFields(model, { dashboard: () => AsyncData.Loading() }),
      commands: [LoadDashboard()],
    }),
    SucceededLoadDashboard: ({ payload }) => ({
      model: modifyFields(model, {
        dashboard: () => AsyncData.Success({ data: payload }),
      }),
    }),
    FailedLoadDashboard: ({ error }) => ({
      model: modifyFields(model, {
        dashboard: () => AsyncData.Failure({ error }),
      }),
    }),

    LoadedTrades: () => ({
      model: modifyFields(model, { trades: () => AsyncData.Loading() }),
      commands: [LoadTrades()],
    }),
    SucceededLoadTrades: ({ trades }) => ({
      model: modifyFields(model, {
        trades: () => AsyncData.Success({ data: trades }),
      }),
    }),
    FailedLoadTrades: ({ error }) => ({
      model: modifyFields(model, {
        trades: () => AsyncData.Failure({ error }),
      }),
    }),

    UpdatedSymbol: ({ value }) => ({
      model: modifyFields(model, {
        form: form => ({ ...form, symbol: value }),
      }),
    }),
    UpdatedDirection: ({ value }) => ({
      model: modifyFields(model, {
        form: form => ({ ...form, direction: value }),
      }),
    }),
    UpdatedStatus: ({ value }) => ({
      model: modifyFields(model, {
        form: form => ({ ...form, status: value }),
      }),
    }),
    UpdatedDate: ({ value }) => ({
      model: modifyFields(model, {
        form: form => ({ ...form, date: value }),
      }),
    }),
    UpdatedResultRR: ({ value }) => ({
      model: modifyFields(model, {
        form: form => ({ ...form, resultRR: value }),
      }),
    }),
    UpdatedTimeWindow: ({ value }) => ({
      model: modifyFields(model, {
        form: form => ({ ...form, timeWindow: value }),
      }),
    }),
    UpdatedConfluence: ({ value }) => ({
      model: modifyFields(model, {
        form: form => ({ ...form, confluence: value }),
      }),
    }),
    UpdatedNotes: ({ value }) => ({
      model: modifyFields(model, {
        form: form => ({ ...form, notes: value }),
      }),
    }),

    ClickedSubmitTrade: () => {
      const problem = validateForm(model.form)
      if (Option.isSome(problem)) {
        return {
          model: modifyFields(model, {
            formError: () => problem,
            notice: () => Option.none(),
          }),
        }
      }
      return {
        model: modifyFields(model, {
          isSubmitting: () => true,
          formError: () => Option.none(),
          notice: () => Option.none(),
        }),
        commands: [
          CreateTrade({
            symbol: model.form.symbol,
            direction: model.form.direction,
            status: model.form.status,
            date: model.form.date,
            resultRR: model.form.resultRR,
            timeWindow: model.form.timeWindow,
            confluence: model.form.confluence,
            notes: model.form.notes,
          }),
        ],
      }
    },
    SucceededCreateTrade: ({ trade }) => ({
      model: modifyFields(model, {
        isSubmitting: () => false,
        form: () => emptyTradeForm(todayISO()),
        notice: () => notice(`Trade ${trade.symbol} tersimpan.`),
      }),
      // Reload from the server so the row shows the stored copy, including the
      // status the API resolved.
      commands: [LoadTrades(), LoadDashboard()],
    }),
    FailedCreateTrade: ({ error }) => ({
      model: modifyFields(model, {
        isSubmitting: () => false,
        notice: () => notice(error),
      }),
    }),

    ClickedDeleteTrade: ({ id }) => ({
      model: modifyFields(model, { pendingDelete: () => Option.some(id) }),
      commands: [OpenConfirmDialog()],
    }),
    ClickedConfirmDelete: () =>
      Option.match(model.pendingDelete, {
        onNone: () => ({ model }),
        onSome: id => ({
          model: modifyFields(model, { pendingDelete: () => Option.none() }),
          commands: [CloseConfirmDialog(), DeleteTrade({ id })],
        }),
      }),
    SucceededDeleteTrade: () => ({
      model: modifyFields(model, { notice: () => notice('Trade dihapus.') }),
      // Reload instead of splicing locally: deleting a trade reconciles its XP
      // grant on the server, so the local copy would be stale.
      commands: [LoadTrades(), LoadDashboard()],
    }),
    FailedDeleteTrade: ({ error }) => ({
      model: modifyFields(model, { notice: () => notice(error) }),
    }),

    LoadedStats: () => ({
      model: modifyFields(model, { stats: () => AsyncData.Loading() }),
      commands: [LoadStats({ period: model.statsPeriod })],
    }),
    SucceededLoadStats: ({ payload }) => ({
      model: modifyFields(model, {
        stats: () => AsyncData.Success({ data: payload }),
      }),
    }),
    FailedLoadStats: ({ error }) => ({
      model: modifyFields(model, {
        stats: () => AsyncData.Failure({ error }),
      }),
    }),
    UpdatedStatsPeriod: ({ value }) => ({
      model: modifyFields(model, {
        statsPeriod: () => value,
        stats: () => AsyncData.Loading(),
      }),
      commands: [LoadStats({ period: value })],
    }),

    LoadedProfile: () => ({
      model: modifyFields(model, { profile: () => AsyncData.Loading() }),
      commands: [LoadProfile()],
    }),
    SucceededLoadProfile: ({ payload }) => ({
      model: modifyFields(model, {
        profile: () => AsyncData.Success({ data: payload }),
      }),
    }),
    FailedLoadProfile: ({ error }) => ({
      model: modifyFields(model, {
        profile: () => AsyncData.Failure({ error }),
      }),
    }),

    LoadedLeaderboard: () => ({
      model: modifyFields(model, { leaderboard: () => AsyncData.Loading() }),
      commands: [LoadLeaderboard()],
    }),
    SucceededLoadLeaderboard: ({ entries }) => ({
      model: modifyFields(model, {
        leaderboard: () => AsyncData.Success({ data: entries }),
      }),
    }),
    FailedLoadLeaderboard: ({ error }) => ({
      model: modifyFields(model, {
        leaderboard: () => AsyncData.Failure({ error }),
      }),
    }),

    LoadedMembers: () => ({
      model: modifyFields(model, { members: () => AsyncData.Loading() }),
      commands: [LoadMembers()],
    }),
    SucceededLoadMembers: ({ members }) => ({
      model: modifyFields(model, {
        members: () => AsyncData.Success({ data: members }),
      }),
    }),
    FailedLoadMembers: ({ error }) => ({
      model: modifyFields(model, {
        members: () => AsyncData.Failure({ error }),
      }),
    }),
    ClickedChangeRole: ({ id, role }) => ({
      model,
      commands: [ChangeRole({ id, role })],
    }),
    SucceededChangeRole: ({ id, role }) => ({
      model: modifyFields(model, {
        notice: () => notice(`Role diperbarui menjadi ${role}.`),
        members: () =>
          AsyncData.map(model.members, members =>
            members.map(member =>
              member.id === id ? { ...member, role } : member,
            ),
          ),
      }),
    }),
    FailedChangeRole: ({ error }) => ({
      model: modifyFields(model, { notice: () => notice(error) }),
    }),

    DismissedNotice: () => ({
      model: modifyFields(model, { notice: () => Option.none() }),
    }),

    GotConfirmDialogMessage: ({ message }) => foldConfirmDialog(model, message),
  })

/** activePath is used by the nav to mark the current item. */
export const activePath = (route: AppRoute): string => path(route)

export type { Member }
export { withUpdateReturn }
