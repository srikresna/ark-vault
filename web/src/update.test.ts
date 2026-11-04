import { Option } from 'effect'
import { describe, expect, it } from 'vitest'
import type { Url } from 'foldkit/url'

import { Message } from './message'
import { init } from './main'
import type { Model } from './model'
import { AppRoute } from './route'
import { update } from './update'

const url = (pathname: string): Url => ({
  protocol: 'http:',
  host: 'localhost:4173',
  port: Option.none(),
  pathname,
  search: Option.none(),
  hash: Option.none(),
})

const fresh = (pathname = '/login'): Model =>
  init({ botUsername: 'test_bot' }, url(pathname))

const signedIn = (): Model => {
  const model = fresh('/dashboard')
  const result = update(model, Message.SucceededLoadSession({
    member: {
      id: 'm1',
      telegram_id: 1,
      discord_id: Option.none(),
      username: 'tester',
      first_name: 'Tester',
      role: 'member',
      joined_at: '2026-11-04T00:00:00Z',
    },
  }))
  return result.model
}

describe('journal form', () => {
  it('rejects a submit without a pair and issues no command', () => {
    const result = update(signedIn(), Message.ClickedSubmitTrade())

    expect(Option.isSome(result.model.formError)).toBe(true)
    expect(result.commands ?? []).toHaveLength(0)
    expect(result.model.isSubmitting).toBe(false)
  })

  it('rejects a non-numeric result', () => {
    let model = signedIn()
    model = update(model, Message.UpdatedSymbol({ value: 'XAUUSD' })).model
    model = update(model, Message.UpdatedResultRR({ value: 'dua' })).model

    const result = update(model, Message.ClickedSubmitTrade())

    expect(Option.isSome(result.model.formError)).toBe(true)
    expect(result.commands ?? []).toHaveLength(0)
  })

  it('sends the trade and enters the submitting state', () => {
    let model = signedIn()
    model = update(model, Message.UpdatedSymbol({ value: 'xauusd' })).model
    model = update(model, Message.UpdatedResultRR({ value: '2.5' })).model

    const result = update(model, Message.ClickedSubmitTrade())

    expect(result.model.isSubmitting).toBe(true)
    expect(result.commands ?? []).toHaveLength(1)
    expect((result.commands ?? [])[0]?.name).toBe('CreateTrade')
  })

  it('clears the draft and reports success when the trade is stored', () => {
    const submitting = { ...signedIn(), isSubmitting: true }

    const result = update(submitting, Message.SucceededCreateTrade({
      trade: {
        id: 't1',
        symbol: 'XAUUSD',
        direction: 'BUY',
        asset_type: 'GOLD',
        status: 'WIN',
        date: '2026-11-04',
        result_rr: 2.5,
        time_window: 'London',
        confluence: '',
        notes: '',
        has_screenshot: false,
        screenshot_url: Option.none(),
        created_at: '2026-11-04T00:00:00Z',
        owner_id: Option.none(),
        owner_username: Option.none(),
      },
    }))

    expect(result.model.isSubmitting).toBe(false)
    expect(result.model.form.symbol).toBe('')
    expect(Option.isSome(result.model.notice)).toBe(true)
  })
})

describe('routing', () => {
  it('keeps a deep link instead of forcing the dashboard', () => {
    const model = fresh('/stats')
    expect(model.route._tag).toBe('Stats')
  })

  it('pins a signed-out visitor to login when they open a private page', () => {
    const result = update(fresh('/journal'), Message.ChangedUrl({ url: url('/stats') }))
    expect(result.model.route._tag).toBe('Login')
    expect(result.commands ?? []).toHaveLength(0)
  })

  it('keeps the link page reachable while signed out', () => {
    const result = update(fresh('/login'), Message.ChangedUrl({
      url: { ...url('/link'), search: Option.some('?discord=42') },
    }))
    expect(result.model.route._tag).toBe('Link')
    expect(Option.getOrElse(result.model.pendingDiscordId, () => '')).toBe('42')
  })

  it('loads the requested page once the session resolves', () => {
    const result = update(fresh('/journal'), Message.SucceededLoadSession({
      member: {
        id: 'm1',
        telegram_id: 1,
        discord_id: Option.none(),
        username: 'tester',
        first_name: 'Tester',
        role: 'owner',
        joined_at: '2026-11-04T00:00:00Z',
      },
    }))

    expect(result.model.route._tag).toBe('Journal')
    expect((result.commands ?? []).map(c => c.name)).toContain('LoadTrades')
  })
})

describe('delete confirmation', () => {
  it('records the trade and opens the dialog when delete is clicked', () => {
    const result = update(signedIn(), Message.ClickedDeleteTrade({ id: 't1' }))

    expect(Option.getOrElse(result.model.pendingDelete, () => '')).toBe('t1')
    expect((result.commands ?? []).map(c => c.name)).toContain('OpenConfirmDialog')
  })

  it('clears the pending trade when the dialog closes without confirming', () => {
    let model = update(signedIn(), Message.ClickedDeleteTrade({ id: 't1' })).model
    model = update(model, Message.GotConfirmDialogMessage({
      message: { _tag: 'RequestedOpen' },
    })).model

    const closed = update(model, Message.GotConfirmDialogMessage({
      message: { _tag: 'RequestedClose' },
    }))

    expect(Option.isNone(closed.model.pendingDelete)).toBe(true)
    expect(closed.model.confirmDialog.isOpen).toBe(false)
  })

  it('deletes the trade and closes the dialog when confirmed', () => {
    const model = update(signedIn(), Message.ClickedDeleteTrade({ id: 't1' })).model

    const confirmed = update(model, Message.ClickedConfirmDelete())

    expect(Option.isNone(confirmed.model.pendingDelete)).toBe(true)
    expect((confirmed.commands ?? []).map(c => c.name)).toContain('DeleteTrade')
  })

  it('ignores a confirm with nothing pending', () => {
    const result = update(signedIn(), Message.ClickedConfirmDelete())

    expect(result.commands ?? []).toHaveLength(0)
  })
})

describe('sign out', () => {
  it('drops the session and returns to login', () => {
    const result = update(signedIn(), Message.SucceededSignOut())

    expect(Option.isNone(result.model.session)).toBe(true)
    expect(result.model.route._tag).toBe(AppRoute.Login()._tag)
  })
})
