import { Match, Option } from 'effect'
import { AsyncData } from 'foldkit'
import { Dialog } from '@foldkit/ui'
import type { Document, Html, HtmlBuilder } from 'foldkit/html'

import type { LeaderboardEntry, Member } from './domain/api'
import { Message } from './message'
import type { Model, Notice, TradeForm } from './model'
import { AppRoute, navItems, path } from './route'
import { TelegramLogin } from './view/telegramLogin'
import {
  countOpen,
  emptyState,
  errorState,
  eyebrow,
  formatPercent,
  formatRR,
  loadingState,
  memberName,
  noticeLine,
  panel,
  statsSummary,
  statusLabel,
  summary,
  tradeRows,
  tradeTableHead,
} from './view/primitives'

type H = HtmlBuilder<Message>

/** DOM id of the element the Telegram widget is injected into. */
const TELEGRAM_HOST_ID = 'telegram-login-host'

// --- shell ------------------------------------------------------------------

const navBar = (model: Model, h: H): Html => {
  const role = Option.match(model.session, {
    onNone: () => 'member',
    onSome: member => member.role,
  })
  const canManage = role === 'admin' || role === 'owner'
  const current = path(model.route)

  return h.header(
    [h.Class('topbar')],
    [
      h.div([h.Class('wordmark')], ['ARK Vault']),
      h.nav(
        [h.Class('nav'), h.AriaLabel('Navigasi utama')],
        [
          ...navItems
            .filter(item => item.adminOnly !== true || canManage)
            .map(item => {
              const href = path(item.route)
              const isCurrent = href === current
              return h.a(
                [
                  h.Href(href),
                  h.OnClick(Message.ClickedLink({ request: internalRequest(href) })),
                  ...(isCurrent ? [h.AriaCurrent('page')] : []),
                ],
                [item.label],
              )
            }),
          Option.match(model.session, {
            onNone: () => h.empty,
            onSome: member =>
              h.button(
                [
                  h.Class('btn-quiet'),
                  h.Type('button'),
                  h.OnClick(Message.ClickedSignOut()),
                ],
                [`Keluar (${memberName(member)})`],
              ),
          }),
        ],
      ),
    ],
  )
}

/** internalRequest builds the navigation request for an in-app link. The anchor
 *  keeps a real href so the link works without JavaScript and is reachable by
 *  keyboard. */
const internalRequest = (href: string) => ({
  _tag: 'Internal' as const,
  url: {
    protocol: 'https:',
    host: '',
    port: Option.none(),
    pathname: href,
    search: Option.none(),
    hash: Option.none(),
  },
})

const noticeBanner = (value: Option.Option<Notice>, h: H): Html =>
  Option.match(value, {
    onNone: () => h.empty,
    onSome: item =>
      noticeLine(
        item.text,
        h.button(
          [
            h.Class('btn-quiet'),
            h.Type('button'),
            h.OnClick(Message.DismissedNotice()),
            h.AriaLabel('Tutup pemberitahuan'),
          ],
          ['Tutup'],
        ),
        h,
      ),
  })

// --- login ------------------------------------------------------------------

const loginPage = (model: Model, h: H): Html =>
  h.main(
    [h.Class('shell'), h.Style({ maxWidth: '460px' })],
    [
      eyebrow('Masuk', h),
      h.h1([], ['ARK Vault']),
      h.p(
        [h.Class('lede')],
        [
          'Jurnal trade komunitas. Masuk dengan Discord, atau dengan akun Telegram yang sudah terhubung ke bot.',
        ],
      ),
      panel(
        'Masuk',
        [
          h.div(
            [h.Class('panel-body')],
            [
              h.a(
                [
                  h.Class('btn'),
                  h.Href('/api/auth/discord/start'),
                  h.Style({ display: 'inline-block', textDecoration: 'none' }),
                ],
                ['Masuk dengan Discord'],
              ),
              h.div(
                [h.Style({ marginTop: '20px' })],
                [
                  eyebrow('Atau dengan Telegram', h),
                  // The widget is a script, so it needs a host element; the
                  // Mount owns everything inside it.
                  h.div(
                    [h.Id(TELEGRAM_HOST_ID), h.OnMount(TelegramLogin({
                      botUsername: model.botUsername,
                      discordId: Option.getOrElse(model.pendingDiscordId, () => ''),
                    }))],
                    [],
                  ),
                ],
              ),
              noticeBanner(model.notice, h),
            ],
          ),
        ],
        h,
      ),
    ],
  )

// --- dashboard --------------------------------------------------------------

const dashboardPage = (model: Model, h: H): Html =>
  h.main(
    [h.Class('shell')],
    [
      eyebrow('Ringkasan bulan ini', h),
      h.h1([], ['Dashboard']),
      AsyncData.match(model.dashboard, {
        onIdle: () => loadingState(h),
        onLoading: () => loadingState(h),
        onRefreshing: () => loadingState(h),
        onFailure: error =>
          errorState(
            error,
            h.button(
              [
                h.Class('btn-secondary'),
                h.Type('button'),
                h.OnClick(Message.LoadedDashboard()),
              ],
              ['Coba lagi'],
            ),
            h,
          ),
        onStale: () => loadingState(h),
        onSuccess: data => {
          const recent = data.trades.slice(0, 5)
          return h.div(
            [],
            [
              summary(statsSummary(data.stats), h),
              panel(
                'Trade terakhir',
                [
                  recent.length === 0
                    ? emptyState(
                        'Belum ada trade bulan ini.',
                        h.a([h.Class('btn'), h.Href(path(AppRoute.Journal()))], [
                          'Catat trade pertama',
                        ]),
                        h,
                      )
                    : h.table(
                        [],
                        [
                          tradeTableHead(h),
                          tradeRows(recent, () => h.empty, h),
                        ],
                      ),
                ],
                h,
              ),
              summary(
                [
                  { label: 'Terbuka', value: String(countOpen(data.trades)) },
                  { label: 'Total trade', value: String(data.trades.length) },
                ],
                h,
              ),
            ],
          )
        },
      }),
    ],
  )

// --- journal ----------------------------------------------------------------

const field = (
  id: string,
  label: string,
  control: Html,
  h: H,
): Html =>
  h.div([h.Class('field')], [h.label([h.For(id)], [label]), control])

const textField = (
  id: string,
  label: string,
  value: string,
  onChange: (payload: Readonly<{ value: string }>) => Message,
  h: H,
  options?: Readonly<{ type?: string; placeholder?: string }>,
): Html =>
  field(
    id,
    label,
    h.input([
      h.Id(id),
      h.Name(id),
      h.Type(options?.type ?? 'text'),
      h.Value(value),
      h.OnInput((next: string) => onChange({ value: next })),
      ...(options?.placeholder === undefined
        ? []
        : [h.Placeholder(options.placeholder)]),
    ]),
    h,
  )

const selectField = (
  id: string,
  label: string,
  value: string,
  choices: ReadonlyArray<Readonly<{ value: string; label: string }>>,
  onChange: (payload: Readonly<{ value: string }>) => Message,
  h: H,
): Html =>
  field(
    id,
    label,
    h.select(
      [h.Id(id), h.Name(id), h.OnInput((next: string) => onChange({ value: next }))],
      choices.map(choice =>
        h.option(
          [
            h.Value(choice.value),
            ...(choice.value === value ? [h.Selected(true)] : []),
          ],
          [choice.label],
        ),
      ),
    ),
    h,
  )

const journalForm = (form: TradeForm, isSubmitting: boolean, h: H): Html =>
  h.form(
    [h.Class('panel-body'), h.OnSubmit(Message.ClickedSubmitTrade())],
    [
      h.div(
        [h.Class('form-grid')],
        [
          textField('symbol', 'Pair', form.symbol, Message.UpdatedSymbol, h, {
            placeholder: 'XAUUSD',
          }),
          selectField(
            'direction',
            'Arah',
            form.direction,
            [
              { value: 'BUY', label: 'Buy' },
              { value: 'SELL', label: 'Sell' },
            ],
            Message.UpdatedDirection,
            h,
          ),
          selectField(
            'status',
            'Status',
            form.status,
            [
              { value: 'OPEN', label: 'Terbuka' },
              { value: 'WIN', label: 'Win' },
              { value: 'LOSS', label: 'Loss' },
              { value: 'BE', label: 'Break even' },
            ],
            Message.UpdatedStatus,
            h,
          ),
          textField('date', 'Tanggal', form.date, Message.UpdatedDate, h, {
            type: 'date',
          }),
          textField(
            'result-rr',
            'Hasil (R)',
            form.resultRR,
            Message.UpdatedResultRR,
            h,
            { type: 'number', placeholder: '2 atau -1.5' },
          ),
          selectField(
            'time-window',
            'Sesi',
            form.timeWindow,
            [
              { value: '', label: 'Tidak diisi' },
              { value: 'Asia', label: 'Asia' },
              { value: 'London', label: 'London' },
              { value: 'NY AM', label: 'NY AM' },
              { value: 'NY PM', label: 'NY PM' },
            ],
            Message.UpdatedTimeWindow,
            h,
          ),
        ],
      ),
      textField(
        'confluence',
        'Confluence',
        form.confluence,
        Message.UpdatedConfluence,
        h,
        { placeholder: 'FVG, OB mitigation' },
      ),
      field(
        'notes',
        'Catatan',
        h.textarea([
          h.Id('notes'),
          h.Name('notes'),
          h.Value(form.notes),
          h.OnInput((next: string) => Message.UpdatedNotes({ value: next })),
        ]),
        h,
      ),
      h.div(
        [h.Class('actions')],
        [
          h.button(
            [
              h.Class('btn'),
              h.Type('submit'),
              ...(isSubmitting ? [h.Disabled(true)] : []),
            ],
            [isSubmitting ? 'Menyimpan' : 'Catat trade'],
          ),
        ],
      ),
    ],
  )

const journalPage = (model: Model, h: H): Html =>
  h.main(
    [h.Class('shell')],
    [
      eyebrow('Jurnal', h),
      h.h1([], ['Jurnal trade']),
      noticeBanner(model.notice, h),
      Option.match(model.formError, {
        onNone: () => h.empty,
        onSome: message =>
          errorState(message, null, h),
      }),
      panel('Catat trade', [journalForm(model.form, model.isSubmitting, h)], h),
      panel(
        'Riwayat',
        [
          AsyncData.match(model.trades, {
            onIdle: () => loadingState(h),
            onLoading: () => loadingState(h),
            onRefreshing: () => loadingState(h),
            onFailure: error =>
              errorState(
                error,
                h.button(
                  [
                    h.Class('btn-secondary'),
                    h.Type('button'),
                    h.OnClick(Message.LoadedTrades()),
                  ],
                  ['Coba lagi'],
                ),
                h,
              ),
            onStale: () => loadingState(h),
            onSuccess: trades =>
              trades.length === 0
                ? emptyState(
                    'Jurnalmu masih kosong. Trade yang kamu catat akan muncul di sini.',
                    null,
                    h,
                  )
                : h.table(
                    [],
                    [
                      tradeTableHead(h),
                      tradeRows(
                        trades,
                        id =>
                          h.button(
                            [
                              h.Class('btn-quiet'),
                              h.Type('button'),
                              h.OnClick(Message.ClickedDeleteTrade({ id })),
                            ],
                            ['Hapus'],
                          ),
                        h,
                      ),
                    ],
                  ),
          }),
        ],
        h,
      ),
    ],
  )

// --- stats ------------------------------------------------------------------

const statsPage = (model: Model, h: H): Html =>
  h.main(
    [h.Class('shell')],
    [
      eyebrow('Statistik', h),
      h.h1([], ['Performa']),
      h.div(
        [h.Class('actions')],
        [
          h.label([h.For('period')], ['Periode']),
          h.select(
            [
              h.Id('period'),
              h.Name('period'),
              h.OnInput((next: string) =>
                Message.UpdatedStatsPeriod({ value: next }),
              ),
            ],
            [
              { value: 'week', label: 'Minggu ini' },
              { value: 'month', label: 'Bulan ini' },
              { value: 'all', label: 'Semua' },
            ].map(choice =>
              h.option(
                [
                  h.Value(choice.value),
                  ...(choice.value === model.statsPeriod
                    ? [h.Selected(true)]
                    : []),
                ],
                [choice.label],
              ),
            ),
          ),
        ],
      ),
      AsyncData.match(model.stats, {
        onIdle: () => loadingState(h),
        onLoading: () => loadingState(h),
        onRefreshing: () => loadingState(h),
        onFailure: error =>
          errorState(
            error,
            h.button(
              [
                h.Class('btn-secondary'),
                h.Type('button'),
                h.OnClick(Message.LoadedStats()),
              ],
              ['Coba lagi'],
            ),
            h,
          ),
        onStale: () => loadingState(h),
        onSuccess: payload =>
          Option.match(payload.stats, {
            onNone: () =>
              emptyState(
                'Belum ada trade pada periode ini, jadi belum ada angka untuk ditampilkan.',
                null,
                h,
              ),
            onSome: stats =>
              h.div(
                [],
                [
                  summary(statsSummary(Option.some(stats)), h),
                  panel(
                    'Per pair',
                    [
                      h.table(
                        [],
                        [
                          h.thead(
                            [],
                            [
                              h.tr(
                                [],
                                [
                                  h.th([], ['Pair']),
                                  h.th([h.Class('right')], ['Trade']),
                                  h.th([h.Class('right')], ['Win rate']),
                                  h.th([h.Class('right')], ['Total R']),
                                ],
                              ),
                            ],
                          ),
                          h.tbody(
                            [],
                            Object.entries(stats.ByAsset).map(([asset, a]) =>
                              h.tr(
                                [h.Key(asset)],
                                [
                                  h.td([h.DataAttribute('label', 'Pair')], [asset]),
                                  h.td(
                                    [
                                      h.Class('right'),
                                      h.DataAttribute('label', 'Trade'),
                                    ],
                                    [String(a.Total)],
                                  ),
                                  h.td(
                                    [
                                      h.Class('right'),
                                      h.DataAttribute('label', 'Win rate'),
                                    ],
                                    [formatPercent(a.WinRate)],
                                  ),
                                  h.td(
                                    [
                                      h.Class('right'),
                                      h.DataAttribute('label', 'Total R'),
                                    ],
                                    [formatRR(a.TotalRR)],
                                  ),
                                ],
                              ),
                            ),
                          ),
                        ],
                      ),
                    ],
                    h,
                  ),
                ],
              ),
          }),
      }),
    ],
  )

// --- profile ----------------------------------------------------------------

const profilePage = (model: Model, h: H): Html =>
  h.main(
    [h.Class('shell')],
    [
      eyebrow('Profil', h),
      h.h1([], ['Gamifikasi']),
      AsyncData.match(model.profile, {
        onIdle: () => loadingState(h),
        onLoading: () => loadingState(h),
        onRefreshing: () => loadingState(h),
        onFailure: error =>
          errorState(
            error,
            h.button(
              [
                h.Class('btn-secondary'),
                h.Type('button'),
                h.OnClick(Message.LoadedProfile()),
              ],
              ['Coba lagi'],
            ),
            h,
          ),
        onStale: () => loadingState(h),
        onSuccess: payload =>
          h.div(
            [],
            [
              summary(
                [
                  {
                    label: 'XP',
                    value: Option.match(payload.profile, {
                      onNone: () => '0',
                      onSome: p => String(p.total_xp),
                    }),
                  },
                  {
                    label: 'Level',
                    value: Option.match(payload.profile, {
                      onNone: () => '1',
                      onSome: p => `${p.level} ${p.title}`,
                    }),
                  },
                  {
                    label: 'Streak',
                    value: Option.match(payload.streak, {
                      onNone: () => '0 hari',
                      onSome: s => `${s.current_streak} hari`,
                    }),
                  },
                  {
                    label: 'Streak terpanjang',
                    value: Option.match(payload.streak, {
                      onNone: () => '—',
                      onSome: s => `${s.longest_streak} hari`,
                    }),
                  },
                ],
                h,
              ),
              panel(
                'Badge',
                [
                  h.div(
                    [h.Class('panel-body')],
                    [
                      payload.badges.length === 0
                        ? emptyState(
                            'Belum ada badge. Badge terbuka saat kamu mencapai target tertentu.',
                            null,
                            h,
                          )
                        : h.div(
                            [h.Class('badge-row')],
                            payload.badges.map(badge =>
                              h.span(
                                [h.Class('badge'), h.Key(badge.badge_id)],
                                [badge.badge_id],
                              ),
                            ),
                          ),
                    ],
                  ),
                ],
                h,
              ),
            ],
          ),
      }),
    ],
  )

// --- leaderboard ------------------------------------------------------------

const leaderboardPage = (model: Model, h: H): Html =>
  h.main(
    [h.Class('shell')],
    [
      eyebrow('Komunitas', h),
      h.h1([], ['Leaderboard']),
      AsyncData.match(model.leaderboard, {
        onIdle: () => loadingState(h),
        onLoading: () => loadingState(h),
        onRefreshing: () => loadingState(h),
        onFailure: error =>
          errorState(
            error,
            h.button(
              [
                h.Class('btn-secondary'),
                h.Type('button'),
                h.OnClick(Message.LoadedLeaderboard()),
              ],
              ['Coba lagi'],
            ),
            h,
          ),
        onStale: () => loadingState(h),
        onSuccess: entries =>
          entries.length === 0
            ? emptyState('Belum ada peringkat bulan ini.', null, h)
            : panel(
                'Peringkat bulan ini',
                [
                  h.table(
                    [],
                    [
                      h.thead(
                        [],
                        [
                          h.tr(
                            [],
                            [
                              h.th([], ['#']),
                              h.th([], ['Member']),
                              h.th([h.Class('right')], ['Trade']),
                              h.th([h.Class('right')], ['Win rate']),
                              h.th([h.Class('right')], ['Total R']),
                            ],
                          ),
                        ],
                      ),
                      h.tbody(
                        [],
                        entries.map((entry, index) =>
                          h.tr(
                            [h.Key(String(entry.TelegramID))],
                            [
                              h.td([h.DataAttribute('label', '#')], [
                                String(index + 1),
                              ]),
                              h.td([h.DataAttribute('label', 'Member')], [
                                entryLabel(entry),
                              ]),
                              h.td(
                                [
                                  h.Class('right'),
                                  h.DataAttribute('label', 'Trade'),
                                ],
                                [String(entry.TotalTrades)],
                              ),
                              h.td(
                                [
                                  h.Class('right'),
                                  h.DataAttribute('label', 'Win rate'),
                                ],
                                [formatPercent(entry.WinRate)],
                              ),
                              h.td(
                                [
                                  h.Class('right'),
                                  h.DataAttribute('label', 'Total R'),
                                ],
                                [h.span([h.Class('num')], [formatRR(entry.TotalRR)])],
                              ),
                            ],
                          ),
                        ),
                      ),
                    ],
                  ),
                ],
                h,
              ),
      }),
    ],
  )

const entryLabel = (entry: LeaderboardEntry): string =>
  entry.Username.length > 0 ? `@${entry.Username}` : entry.FirstName

// --- admin ------------------------------------------------------------------

const roleSelect = (
  member: Member,
  canEdit: boolean,
  h: H,
): Html =>
  h.select(
    [
      h.AriaLabel(`Role ${memberName(member)}`),
      ...(canEdit
        ? [
            h.OnInput((value: string) =>
              Message.ClickedChangeRole({ id: member.id, role: value }),
            ),
          ]
        : [h.Disabled(true)]),
    ],
    ['member', 'admin', 'owner'].map(role =>
      h.option(
        [h.Value(role), ...(role === member.role ? [h.Selected(true)] : [])],
        [role],
      ),
    ),
  )

const adminPage = (model: Model, h: H): Html => {
  const isOwner = Option.match(model.session, {
    onNone: () => false,
    onSome: member => member.role === 'owner',
  })
  return h.main(
    [h.Class('shell')],
    [
      eyebrow('Administrasi', h),
      h.h1([], ['Member']),
      noticeBanner(model.notice, h),
      AsyncData.match(model.members, {
        onIdle: () => loadingState(h),
        onLoading: () => loadingState(h),
        onRefreshing: () => loadingState(h),
        onFailure: error =>
          errorState(
            error,
            h.button(
              [
                h.Class('btn-secondary'),
                h.Type('button'),
                h.OnClick(Message.LoadedMembers()),
              ],
              ['Coba lagi'],
            ),
            h,
          ),
        onStale: () => loadingState(h),
        onSuccess: members =>
          members.length === 0
            ? emptyState('Belum ada member terdaftar.', null, h)
            : panel(
                `${members.length} member`,
                [
                  h.table(
                    [],
                    [
                      h.thead(
                        [],
                        [
                          h.tr(
                            [],
                            [
                              h.th([], ['Member']),
                              h.th([], ['Telegram']),
                              h.th([], ['Discord']),
                              h.th([], ['Role']),
                            ],
                          ),
                        ],
                      ),
                      h.tbody(
                        [],
                        members.map(member =>
                          h.tr(
                            [h.Key(member.id)],
                            [
                              h.td([h.DataAttribute('label', 'Member')], [
                                memberName(member),
                              ]),
                              h.td([h.DataAttribute('label', 'Telegram')], [
                                String(member.telegram_id),
                              ]),
                              h.td([h.DataAttribute('label', 'Discord')], [
                                Option.getOrElse(member.discord_id, () => '—'),
                              ]),
                              h.td([h.DataAttribute('label', 'Role')], [
                                roleSelect(member, isOwner, h),
                              ]),
                            ],
                          ),
                        ),
                      ),
                    ],
                  ),
                ],
                h,
              ),
      }),
      h.p(
        [h.Class('muted')],
        [
          isOwner
            ? 'Hanya owner yang dapat mengubah role.'
            : 'Hanya owner yang dapat mengubah role.',
        ],
      ),
    ],
  )
}

// --- not found --------------------------------------------------------------

const notFoundPage = (h: H): Html =>
  h.main(
    [h.Class('shell')],
    [
      eyebrow('404', h),
      h.h1([], ['Halaman tidak ditemukan']),
      h.p([h.Class('lede')], ['Alamat ini tidak ada di ARK Vault.']),
      h.a([h.Class('btn'), h.Href(path(AppRoute.Dashboard()))], [
        'Kembali ke dashboard',
      ]),
    ],
  )

// --- delete confirmation dialog --------------------------------------------

const confirmDialog = (model: Model, h: H): Html =>
  h.submodel({
    slotId: 'confirm-dialog',
    model: model.confirmDialog,
    view: Dialog.view,
    viewInputs: {
      hasDescription: true,
      toView: render =>
        h.dialog(
          [...render.dialog],
          render.isVisible
            ? [
                h.div([...render.backdrop], []),
                h.div(
                  [...render.panel],
                  [
                    h.div(
                      [h.Class('panel-body')],
                      [
                        h.h2([...render.title], ['Hapus trade?']),
                        h.p(
                          [...render.description],
                          [
                            'Trade akan dihapus dari jurnal dan XP yang pernah diberikan akan disesuaikan. Tindakan ini tidak bisa dibatalkan.',
                          ],
                        ),
                        h.div(
                          [h.Class('actions')],
                          [
                            h.button(
                              [...render.initialFocus, h.Class('btn-danger')],
                              ['Hapus trade'],
                            ),
                            h.button(
                              [
                                ...render.closeButton,
                                h.Class('btn-secondary'),
                              ],
                              ['Batal'],
                            ),
                          ],
                        ),
                      ],
                    ),
                  ],
                ),
              ]
            : [],
        ),
    },
    toParentMessage: message => Message.GotConfirmDialogMessage({ message }),
  })

// --- root -------------------------------------------------------------------

const body = (model: Model, h: H): Html =>
  Match.value(model.route).pipe(
    Match.tagsExhaustive({
      Login: () => loginPage(model, h),
      Link: () => loginPage(model, h),
      Dashboard: () => dashboardPage(model, h),
      Journal: () => journalPage(model, h),
      Stats: () => statsPage(model, h),
      Profile: () => profilePage(model, h),
      Leaderboard: () => leaderboardPage(model, h),
      Admin: () => adminPage(model, h),
      NotFound: () => notFoundPage(h),
    }),
  )

export const view = (model: Model, h: HtmlBuilder<Message>): Document => ({
  title: model.route._tag === 'Login' ? 'Masuk · ARK Vault' : 'ARK Vault',
  body:
    model.route._tag === 'Login'
      ? h.div([], [body(model, h), confirmDialog(model, h)])
      : h.div([], [navBar(model, h), body(model, h), confirmDialog(model, h)]),
})

export { statusLabel, formatRR }
