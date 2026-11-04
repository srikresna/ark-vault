# Web CRM

Foldkit SPA for ARK Vault. Reads and writes the same Postgres store the Telegram
bot uses, through the Go API in `internal/api`.

## Run

The API must be running on `:8080` (the bot binary serves it). Vite proxies
`/api` to it, so the session cookie stays first-party in development.

```bash
npm install
VITE_BOT_USERNAME=your_bot_username npm run dev
```

`VITE_BOT_USERNAME` is the bot's `@username` without the `@`. The Telegram Login
Widget needs it; it is public information.

## Build

```bash
VITE_BOT_USERNAME=your_bot_username npm run build
```

Output goes to `dist/`. Nginx Proxy Manager serves that directory and proxies
`/api` to the API container.

## Checks

```bash
npx tsc --noEmit
npm run build
```

## Layout

```
src/entry.ts              Runtime.makeApplication + routing
src/main.ts               Flags and init
src/model.ts              Model schema, AsyncData fields, form state
src/message.ts            Message union
src/route.ts              Route union, path(), nav items
src/command.ts            HTTP commands (Effect + HttpClient)
src/update.ts             Pure update
src/view.ts               Pure view
src/view/primitives.ts    Shared formatters and table helpers
src/view/telegramLogin.ts Mount that embeds the Telegram Login Widget
src/domain/api.ts         Wire schemas mirroring the Go responses
DESIGN.md                 Visual direction and dials
```

This project is a Foldkit app: state transitions live in `update`, side effects
in `command`, rendering in `view`. Do not add component-local state.
