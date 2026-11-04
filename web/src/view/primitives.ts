import { Array, Option, String as Str } from 'effect'
import type { Html, HtmlBuilder } from 'foldkit/html'

import type { Member, Stats, Trade } from '../domain/api'
import type { Message } from '../message'

/** Every helper takes the caller's builder, so the whole view tree shares one
 *  Message universe. Typing them `HtmlBuilder<never>` would make them unusable
 *  from a real view. */
type H = HtmlBuilder<Message>

/** formatRR renders a result as a signed R value. A result is never coloured:
 *  the sign carries the meaning, which keeps the interface readable without
 *  colour vision. */
export const formatRR = (value: number): string =>
  `${value > 0 ? '+' : value < 0 ? '−' : ''}${Math.abs(value).toFixed(1)}R`

export const formatPercent = (value: number): string => `${value.toFixed(0)}%`

/** statusLabel maps a stored status to the word a member reads. */
export const statusLabel = (status: string): string => {
  switch (status) {
    case 'WIN':
      return 'Win'
    case 'LOSS':
      return 'Loss'
    case 'BE':
      return 'Break even'
    case 'OPEN':
      return 'Terbuka'
    default:
      return status
  }
}

/** eyebrow is the small uppercase label that introduces a data block. It is the
 *  repeated identity motif from DESIGN.md. */
export const eyebrow = (text: string, h: H): Html =>
  h.p([h.Class('eyebrow')], [text])

/** summary renders the one wider block per page: a strip of label/value pairs
 *  separated by hairlines rather than cards with shadows. */
export const summary = (
  items: ReadonlyArray<Readonly<{ label: string; value: string }>>,
  h: H,
): Html =>
  h.div(
    [h.Class('summary')],
    items.map(item =>
      h.div(
        [],
        [
          h.span([h.Class('eyebrow')], [item.label]),
          h.span([h.Class('value')], [item.value]),
        ],
      ),
    ),
  )

/** panel wraps a titled block. */
export const panel = (
  title: string,
  body: ReadonlyArray<Html>,
  h: H,
): Html =>
  h.section(
    [h.Class('panel')],
    [h.div([h.Class('panel-head')], [h.h2([], [title])]), ...body],
  )

/** emptyState explains what is missing and what to do, instead of showing a
 *  blank table. */
export const emptyState = (
  message: string,
  action: Html | null,
  h: H,
): Html =>
  h.div(
    [h.Class('state')],
    [h.p([], [message]), ...(action === null ? [] : [action])],
  )

export const loadingState = (h: H): Html =>
  h.div([h.Class('loading-rule'), h.Role('status'), h.AriaLabel('Memuat')], [])

export const errorState = (
  message: string,
  onRetry: Html | null,
  h: H,
): Html =>
  h.div(
    [h.Class('state'), h.Role('alert')],
    [h.p([], [message]), ...(onRetry === null ? [] : [onRetry])],
  )

/** noticeLine renders the single line of feedback for the last action. It is a
 *  live region, so a screen reader announces a save or a failure without a
 *  reload. */
export const noticeLine = (text: string, onDismiss: Html, h: H): Html =>
  h.div(
    [h.Class('notice'), h.Role('status'), h.AriaLive('polite')],
    [h.span([], [text]), onDismiss],
  )

/** statsSummary turns a Stats payload into the dashboard strip. */
export const statsSummary = (
  stats: Option.Option<Stats>,
): ReadonlyArray<Readonly<{ label: string; value: string }>> =>
  Option.match(stats, {
    onNone: () => [
      { label: 'Trade', value: '0' },
      { label: 'Win rate', value: '-' },
      { label: 'Total R', value: '-' },
    ],
    onSome: s => [
      { label: 'Trade', value: String(s.TotalTrades) },
      { label: 'Win rate', value: formatPercent(s.WinRate) },
      { label: 'Total R', value: formatRR(s.TotalRR) },
      { label: 'Avg R', value: formatRR(s.AvgRR) },
      { label: 'Expectancy', value: formatRR(s.Expectancy) },
      { label: 'Profit factor', value: s.ProfitFactor.toFixed(2) },
    ],
  })

/** tradeTableHead renders the journal header. `withActions` must match the rows:
 *  a header cell with no matching body cell is a dead column. */
export const tradeTableHead = (withActions: boolean, h: H): Html =>
  h.thead(
    [],
    [
      h.tr(
        [],
        [
          h.th([], ['Pair']),
          h.th([], ['Arah']),
          h.th([], ['Status']),
          h.th([], ['Tanggal']),
          h.th([h.Class('right')], ['Hasil']),
          ...(withActions ? [h.th([], ['Aksi'])] : []),
        ],
      ),
    ],
  )

/** tradeRows renders a trade table body. Every cell carries data-label so the
 *  mobile layout can turn rows into labelled cards without a second markup.
 *  A nil onDelete omits the action column entirely, for read-only tables. */
export const tradeRows = (
  trades: ReadonlyArray<Trade>,
  onDelete: ((id: string) => Html) | null,
  h: H,
): Html =>
  h.tbody(
    [],
    trades.map(trade =>
      h.tr(
        [h.Key(trade.id)],
        [
          h.td([h.DataAttribute('label', 'Pair')], [trade.symbol]),
          h.td([h.DataAttribute('label', 'Arah')], [trade.direction]),
          h.td([h.DataAttribute('label', 'Status')], [statusLabel(trade.status)]),
          h.td([h.DataAttribute('label', 'Tanggal')], [trade.date]),
          h.td(
            [h.Class('right'), h.DataAttribute('label', 'Hasil')],
            [h.span([h.Class('num')], [formatRR(trade.result_rr)])],
          ),
          ...(onDelete === null
            ? []
            : [h.td([h.DataAttribute('label', 'Aksi')], [onDelete(trade.id)])]),
        ],
      ),
    ),
  )

/** memberName prefers the username, then the first name, then the numeric id:
 *  a roster entry is never blank. */
export const memberName = (member: Member): string =>
  Str.isNonEmpty(member.username)
    ? `@${member.username}`
    : Str.isNonEmpty(member.first_name)
      ? member.first_name
      : `id ${member.telegram_id}`

/** countOpen returns how many trades are still open, used by the dashboard. */
export const countOpen = (trades: ReadonlyArray<Trade>): number =>
  Array.filter(trades, trade => trade.status === 'OPEN').length
