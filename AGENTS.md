# Repository Guidelines

## Project Overview

**ARK Vault** (`github.com/arkcode369/ark-vault`) is a trade-journaling system for a trading community. It has two faces over one store:

1. a **Telegram bot** (Go) that records trades, tracks XP/levels/streaks/badges/challenges/goals, posts reminders and reports, and offers optional Gemini AI coaching;
2. a **web CRM** (Foldkit SPA + Go JSON API) where the same members read their journal and statistics, and where admins manage roles.

**Postgres is the single system of record.** It replaced the previous Notion journal plus BadgerDB gamification store; both adapters were deleted. There is no second database, no cache tier, and no external document store.

## Architecture & Data Flow

Hexagonal (Ports & Adapters) with consumer-side interfaces.

1. **Composition root (`cmd/bot/main.go`)**: loads config, opens the Postgres pool (`postgres.Open`, which applies pending migrations), wires the adapters into application services, registers the async image store and the API server, starts the scheduler and the Telegram long-poller.
2. **Primary adapters**:
   - `internal/adapter/telegram/`: `Bot.Start` long-polls `getUpdates` and admits each batch to a durable Postgres ledger before the offset advances, so polling never waits for handlers. `updateDispatcher` keeps per-user FIFO with separate limits for running and pending updates. `Handler.HandleUpdate` applies rate limiting, the community membership gate, the pending guided-flow/edit sessions, then command or `#journal` routing.
   - `internal/api/`: the web JSON API. Cookie sessions, Discord OAuth, Telegram Login Widget verification, and role-based access control.
3. **Application services (`internal/service/`)**: depend only on the interfaces in `contracts.go` (`TradeRepository`, `MemberRepository`, `ImageStore`, `AIAnalyzer`, `GamificationStore`, `BadgeStore`, `ChallengeStore`, `ReminderStore`, `GoalStore`, `AnalyticsCacheStore`, `ReportCardStore`). Both the Telegram adapter and the API call these services, so validation, XP, badges and goals cannot drift between the two faces.
4. **Secondary adapters (`internal/adapter/{postgres,gemini,exporter}`)**: `postgres` implements every repository over one pool; `gemini` is the optional AI analyzer; `exporter` renders CSV/PDF.

## Key Directories

- `cmd/bot/` — entrypoint, manual DI, signal handling, scheduler jobs, nightly `pg_dump` backup.
- `internal/domain/` — pure models and rules, zero I/O (`trade.go`, `stats.go`, `xp.go`, `asset.go`, `gamification.go`, `badge.go`, `challenge.go`, `goal.go`, `reminder.go`, `analytics.go`, `member.go`).
- `internal/service/` — use cases and the consumer-side ports (`contracts.go`).
- `internal/adapter/postgres/` — pool + embedded migrations (`migrations/*.sql`), repositories (`member.go`, `trade.go`, `gamification.go`, `badge.go`, `challenge_goal.go`, `reminder_cache.go`), transactional reset (`reset.go`), durable ledger key/value (`kv.go`), sessions and OAuth state (`session.go`), local image store (`image.go`).
- `internal/adapter/telegram/` — Bot API client, sender, command catalog, router, `#journal` parser, 9-step wizard, HTML formatters, rate limiter.
- `internal/api/` — HTTP handlers, auth middleware, Discord/Telegram login, view models.
- `web/` — Foldkit SPA. `web/DESIGN.md` holds the visual direction; `web/src/route.ts`, `model.ts`, `message.ts`, `update.ts`, `command.ts`, `view.ts` are the TEA core.
- `repos/foldkit/` — vendored Foldkit reference subtree (examples, framework source, quality bar). Documentation only: it is never compiled into the bot and `repos` is in `.dockerignore`.
- `pkg/timeutil/` — WIB (UTC+7) helpers.

## Development Commands

```bash
go build ./...                                   # compile everything
go test ./...                                    # full suite
go test -p=1 ./internal/adapter/postgres         # SQL adapter (skips without TEST_DATABASE_URL)
golangci-lint run ./...                          # must report 0 issues
CGO_ENABLED=0 go build -ldflags="-s -w" -o ark-vault ./cmd/bot
cd web && npm install && npm run build           # frontend bundle
cd web && npx tsc --noEmit                       # frontend typecheck
docker compose up -d --build
```

Postgres integration tests run against a real database and are skipped unless `TEST_DATABASE_URL` is set:

```bash
docker run -d --rm --name ark-pg -e POSTGRES_PASSWORD=t -e POSTGRES_USER=t -e POSTGRES_DB=t -p 55432:5432 postgres:17-alpine
TEST_DATABASE_URL="postgres://t:t@127.0.0.1:55432/t?sslmode=disable" go test ./internal/adapter/postgres
```

## Code Conventions

- **Consumer-side interfaces**: define ports where they are consumed; adapters return concrete structs (`*MemberRepo`, `*TradeRepo`, `*GamificationRepo`).
- **One store, one transaction**: any write set the service documents as atomic is one SQL transaction. Do not reintroduce multi-step writes with compensating logic; add a repository method that commits them together.
- **Surrogate identities**: `members.id` is a uuid; `telegram_id` stays the business key the bot speaks and is unique. Web sessions and relational rows reference the uuid only.
- **Timezone invariant (WIB / Asia/Jakarta)**: every user-facing date boundary (daily streak, `MaxDailyTradeXPCount = 3` cap, period filters, reminders, weekly challenges, monthly goals and report cards) is evaluated in WIB via `pkg/timeutil`.
- **Win rate**: `domain.WinRate`/`domain.CountResults` is the single definition (wins ÷ wins+losses+BE, OPEN excluded). Every service must call it instead of dividing locally.
- **Trade XP grants** are authoritative: zero means the trade earned nothing (daily cap or a failed reward commit); a missing grant means unknown (pre-grant data). Never assume a nominal base for a missing grant. Grants are never pruned by age.
- **Migrations**: append a numbered file to `internal/adapter/postgres/migrations/`; `Open` applies pending ones in filename order inside a transaction and records them in `schema_migrations`. Never edit an applied migration.
- **Postgres SQL**: qualify every column in a JOIN (`memberColumns("m")`); `id` exists on several tables and an unqualified projection fails at runtime with "column reference is ambiguous". Treat SQLSTATE `22P02` (malformed uuid) as "not found", not a 500.
- **Reset semantics**: `/reset` is one transaction (`postgres.Resetter`) that deletes journals and gamification state but keeps members and the delivery ledger. It reports how many trades it removed and never claims success on failure.
- **Security invariants**:
  - Never embed `TELEGRAM_BOT_TOKEN` in URLs or send it to the browser; the Telegram Login Widget payload is verified server-side with an HMAC over a sorted `key=value` string keyed by `SHA256(bot token)`.
  - Session and OAuth-state tokens are 256-bit random values in `HttpOnly`, `SameSite=Lax` cookies (`Secure` whenever `PUBLIC_BASE_URL` is https). OAuth state is single-use: consuming it deletes it.
  - Only an owner may change roles, and an owner cannot demote themselves; a member naming another member's data is rejected rather than silently scoped.
  - Screenshots are stored under `IMAGE_DIR` with keys derived from the trade id plus random bytes, never from the client filename; `Open` re-checks the joined path against the root.
  - CSV cells starting with `=`, `+`, `-`, `@`, tab, CR or LF are prefixed with `'`.
- **Owner commands** (`/reset`) check `msg.From.ID == h.ownerID` and register only under the owner's chat command scope.

## Important Files

- `cmd/bot/main.go` — composition root, listeners, scheduler jobs.
- `cmd/bot/backup.go` — nightly `pg_dump` with retention.
- `internal/config/config.go` & `.env.example` — required settings: `TELEGRAM_BOT_TOKEN`, `DATABASE_URL`; Discord OAuth needs `SESSION_SECRET`.
- `internal/adapter/postgres/migrations/0001_init.sql` — the whole schema.
- `internal/service/contracts.go` — every port the services depend on.
- `internal/domain/stats.go` — `WinRate`/`CountResults`/`CalculateStats`/`FilterTrades`.
- `internal/domain/xp.go` — XP rewards, `MaxDailyTradeXPCount`, the 10-level table.
- `internal/api/server.go` — auth middleware, Discord OAuth, Telegram widget verification.
- `internal/api/handlers.go` — trade/stat/profile/leaderboard/admin handlers.
- `web/DESIGN.md` — monochrome visual direction and the declared dials.
- `.github/workflows/ci.yml` — commitlint → lint + govulncheck → race tests → build → image → deploy.

## Runtime & Tooling

- **Go** minimum `1.26` (`toolchain go1.26.10`), required by `x/net v0.60+`.
- **Direct dependency**: `github.com/jackc/pgx/v5`. Keep the stdlib-first policy: use `net/http`, `encoding/json`, `mime/multipart`, `log/slog` rather than adding an ORM or web framework.
- **Frontend**: `foldkit` + `@foldkit/ui` + `effect` (TypeScript, Vite). Node ≥ 22.19 is required by `@foldkit/vite-plugin`; the repo pins `vite 8`.
- **Container**: non-root `appuser` on `alpine:3.20`, `postgresql17-client` for `pg_dump`, images on a named volume, `256M` memory limit for the bot and `512M` for Postgres. `/healthz` pings Postgres (readiness, not liveness).

## Testing & QA

- Standard library `testing` and `net/http/httptest` only; white-box tests in the same package. No testify, no codegen mocks.
- SQL tests use `TEST_DATABASE_URL` and truncate the schema between cases; they skip when the variable is unset so `go test ./...` stays hermetic.
- Telegram/API tests spin up `httptest.NewServer` and override the client base URL. Use `sync/atomic` for counters mutated inside handlers so `go test -race ./...` passes.
- **Pre-commit gate**: `gofmt`, `go test ./...`, and `golangci-lint run ./...` (0 issues). The frontend additionally needs `npx tsc --noEmit` and `npm run build`.
