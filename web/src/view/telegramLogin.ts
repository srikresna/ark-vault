import { Effect, Schema } from 'effect'
import { Mount } from 'foldkit'

import { Message } from '../message'

/** Payload the Telegram Login Widget hands to its callback. Every field is
 *  attacker-controllable until the server verifies the HMAC signature, so the
 *  app forwards it verbatim and never trusts it locally. */
export type TelegramAuthPayload = Readonly<{
  id: number
  first_name: string
  last_name?: string
  username?: string
  photo_url?: string
  auth_date: number
  hash: string
}>

/** The global the widget calls. Telegram hardcodes this name, so it cannot be
 *  namespaced or made per-instance. */
const CALLBACK_NAME = 'onTelegramAuth'

/** TelegramLogin embeds the Login Widget.
 *
 *  The widget is a script that reads data-* attributes off its own tag and calls
 *  a global callback, so it cannot be expressed as declarative markup. Mount is
 *  the framework's seam for exactly this: the imperative part lives here, and its
 *  result leaves as a Message.
 *
 *  The payload is forwarded as JSON and verified server-side, because the bot
 *  token that signs it must never reach the browser. */
export const TelegramLogin = Mount.define('TelegramLogin', {
  args: {
    botUsername: Schema.String,
    /** Discord account to link, when the visitor arrived from the linking page. */
    discordId: Schema.String,
  },
  messages: [Message.ReceivedTelegramAuth],
  execute: ({ element, botUsername, discordId }) =>
    Effect.callback<string>((resume, _signal) => {
      const container = element as HTMLElement
      // The widget replaces its own script tag, so a re-render must not stack
      // widgets: clear first, then add exactly one.
      container.replaceChildren()

      const handler = (payload: TelegramAuthPayload) => {
        resume(
          Effect.succeed(JSON.stringify({ ...payload, discord_id: discordId })),
        )
      }
      ;(window as unknown as Record<string, unknown>)[CALLBACK_NAME] = handler

      const script = document.createElement('script')
      script.async = true
      script.src = 'https://telegram.org/js/telegram-widget.js?22'
      script.setAttribute('data-telegram-login', botUsername)
      script.setAttribute('data-size', 'medium')
      script.setAttribute('data-request-access', 'write')
      script.setAttribute('data-onauth', `${CALLBACK_NAME}(user)`)
      container.appendChild(script)

      return Effect.sync(() => {
        delete (window as unknown as Record<string, unknown>)[CALLBACK_NAME]
        container.replaceChildren()
      })
    }).pipe(
      Effect.map(payload => Message.ReceivedTelegramAuth({ payload })),
    ),
})
