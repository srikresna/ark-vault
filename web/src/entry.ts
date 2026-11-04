import { Effect } from 'effect'
import { Runtime } from 'foldkit'

import { LoadSession } from './command'
import { Flags, init } from './main'
import { Message } from './message'
import { Model } from './model'
import { update } from './update'
import { view } from './view'

/** The session cookie is HttpOnly, so the first thing the app does after boot is
 *  ask the server who it is. That one request replaces any client-side token. */
const application = Runtime.makeApplication({
  Model,
  Flags,
  init: (flags, url) => ({
    model: init(flags, url),
    commands: [LoadSession()],
  }),
  update,
  view,
  container: document.getElementById('root'),
  routing: {
    onUrlRequest: request => Message.ClickedLink({ request }),
    onUrlChange: url => Message.ChangedUrl({ url }),
  },
  devTools: {
    Message,
  },
})

/** The bot username is public (it appears in every bot link), so it is injected
 *  at build time from an environment variable rather than fetched. */
const flags: typeof Flags.Type = {
  botUsername: import.meta.env.VITE_BOT_USERNAME ?? '',
}

/** Runtime.run takes flags as an Effect: the value has to be produced inside the
 *  runtime so a future async flag source (a fetch, storage) needs no call-site
 *  change. */
Runtime.run(application, { flags: Effect.succeed(flags) })
