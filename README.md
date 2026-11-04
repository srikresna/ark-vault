# ARK Vault

**ARK Vault** is a trade journal for a trading community: a Telegram bot (Go) that records journals and drives a gamification engine (XP, 10 levels, daily streaks, 13 badges, rotating weekly challenges, monthly goals, reminders) plus optional Google Gemini coaching, and a web CRM where the same members read their journal and statistics.

Storage is **PostgreSQL only**. Journals, gamification state, web sessions and the Telegram delivery ledger live in one database, so a multi-row write is a transaction instead of a set of compensating writes.

## Features

- **Dual journaling flows**
  - `/journal` — 9-step guided wizard with inline buttons and optional screenshot.
  - `#journal` — single-message parser (`Pair`, `Type`, `RR`, `Session`, `Confluence`) with photo caption support.
- **Interactive trade management**: `/open`, `/history`, `/undo`, `/cancel`, plus edit/delete callbacks.
- **Analytics**: `/stats`, `/leaderboard`, `/report`, `/export` (formula-safe CSV, raw PDF). Win rate counts closed trades only (WIN, LOSS, BE); OPEN is excluded.
- **Gamification**: `/profile`, `/badges`, `/challenge`, `/goal`, `/reminder`, all evaluated in `Asia/Jakarta`.
- **AI coaching**: `/analyze`, `/reportcard` via Gemini (optional).
- **Web CRM**: dashboard, journal, statistics, profile, leaderboard, and an admin roster. Discord OAuth or the Telegram Login Widget; roles `member`/`admin`/`owner`.
- **Reliability**: per-user ordered update processing over a durable admission ledger, readiness healthcheck that pings Postgres, nightly `pg_dump` with retention, Prometheus metrics on loopback.

## Architecture

Hexagonal (Ports & Adapters) with consumer-side interfaces and a stdlib-first policy (`github.com/jackc/pgx/v5` is the only direct dependency):

```
cmd/bot/main.go          # Composition root, DI, listeners, scheduler jobs, backup
internal/
  config/                # Environment parsing & validation
  domain/                # Pure models & business rules (zero I/O)
  service/               # Use cases + consumer-side ports (contracts.go)
  scheduler/             # Hourly clock-deduplicated job runner
  adapter/
    telegram/            # Long-polling Bot API client, command catalog, router, wizard
    postgres/            # Pool, embedded migrations, repositories, reset, ledger, sessions
    gemini/              # Google Gemini REST analyzer
    exporter/            # Formula-safe CSV & raw PDF
  api/                   # Web JSON API: cookie sessions, OAuth, RBAC
web/                     # Foldkit SPA (TypeScript + Effect)
repos/foldkit/           # Vendored Foldkit reference subtree (never compiled)
pkg/timeutil/            # Canonical WIB (UTC+7) helpers
```

## Quick Start

### 1. Configure Environment

```bash
cp .env.example .env
```

Required:

- `TELEGRAM_BOT_TOKEN` — from `@BotFather`; also verifies Telegram Login Widget signatures.
- `DATABASE_URL` — e.g. `postgres://arkvault:secret@postgres:5432/arkvault?sslmode=disable`

Optional:

- `SESSION_SECRET`, `PUBLIC_BASE_URL`, `DISCORD_CLIENT_ID`/`DISCORD_CLIENT_SECRET`/`DISCORD_REDIRECT_URL` — required for the web app's Discord login.
- `GEMINI_API_KEY` — enables `/analyze` and `/reportcard`.
- `COMMUNITY_GROUP_ID`, `OWNER_ID`, `REPORT_CHAT_ID`, `RATE_LIMIT_PER_MIN`.

### 2. Run

```bash
docker compose up -d --build
```

Compose starts Postgres (`ark-vault-postgres`, no published port) and the bot. Migrations apply automatically on start.

### 3. Build & Test Locally

```bash
go build ./...
go test ./...
golangci-lint run ./...

cd web && npm install && npm run build
```

SQL adapter tests run against a real database:

```bash
docker run -d --rm --name ark-pg -e POSTGRES_PASSWORD=t -e POSTGRES_USER=t -e POSTGRES_DB=t -p 55432:5432 postgres:17-alpine
TEST_DATABASE_URL="postgres://t:t@127.0.0.1:55432/t?sslmode=disable" go test ./internal/adapter/postgres
```

## Deployment

Nginx Proxy Manager fronts the API over the shared `nginx-net` Docker network: it serves the built SPA and proxies `/api` to the bot container on port 8080, so the session cookie stays first-party and no CORS is needed. `/metrics` and `/healthz` stay bound to loopback (9102) and are scraped over `grafana_monitoring-net`.

Every push to `main` runs: commitlint → govulncheck + golangci-lint → race tests with a coverage gate → binary build → image build → deploy. The deploy job runs on a self-hosted runner on the production host; it rebuilds the container and rolls back to the previous commit if the healthcheck does not pass within two minutes.

## Notes

- Screenshots are stored under `IMAGE_DIR` on a named volume; the database holds only the object key, so moving to S3-compatible storage changes one adapter.
- `/reset` (owner only) deletes journals and gamification state in one transaction and keeps member identities plus the delivery ledger.
- Interrupted Telegram actions are not replayed automatically: the member is told to verify via `/history`.
