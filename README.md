# ARK Vault

**ARK Vault** is a community-focused Telegram bot written in Go (minimum 1.25, toolchain 1.26) that automates trade journal recording into per-member Notion pages and inline databases, paired with a local BadgerDB gamification engine (XP, 10 career levels, daily streaks, 13 achievement badges, rotating weekly challenges, monthly goals, dual-mode reminders) and optional Google Gemini AI trade coaching (`/analyze`, `/reportcard`).

## Features

- **Dual Journaling Flows**:
  - `/journal` — 9-step interactive guided wizard with inline buttons and optional screenshot upload.
  - `#journal` — Fast single-message text parser (`Pair`, `Type`, `RR`, `Session`, `Confluence`) with photo caption support.
- **Interactive Trade Management**:
  - `/open` — View and close open trades (`WIN`, `LOSS`, `BE`).
  - `/history` — Paginated trade history with inline edit and delete actions.
  - `/undo` — Delete the most recently recorded trade and reconcile XP.
- **Analytics & Reports**:
  - `/stats [week|month] [pair] [session] [@username]` — Win rate, total/avg RR, expectancy, profit factor, max drawdown, per-asset/session breakdown, Unicode R-Curve (cumulative equity sparkline), and revenge-trading warnings.
  - `/leaderboard [week|month] [rr]` — Community Top 10 rankings.
  - `/report` — Weekly community summary (also auto-posted by the scheduler).
  - `/export [week|month] [pdf]` — Sanitized CSV and zero-dependency PDF trade journal export with period filtering.
- **Gamification & AI Coaching**:
  - `/profile`, `/badges`, `/challenge`, `/goal`, `/reminder` — Progression, streaks, badges, rotating weekly challenges, monthly targets, and daily/community reminders (all evaluated in `Asia/Jakarta` / `WIB`).
  - `/analyze` & `/reportcard [current]` — AI-powered trade diagnostics and monthly report cards via Google Gemini.

- **Observability & Reliability**:
  - Native daily BadgerDB snapshot backup at 03:00 WIB into `/data/badger/backups` with 7-day automatic retention.
  - Stdlib-only Prometheus metrics exposition at `http://localhost:9102/metrics` (`ark_vault_updates_total`, `ark_vault_commands_total`, `ark_vault_rate_limited_total`, etc.) and `/healthz` endpoint.
  - Concurrency-bounded update worker pool (max 32 concurrent goroutines) with panic recovery and graceful shutdown drain.
## Architecture

The project follows **Hexagonal Architecture (Ports & Adapters)** with consumer-side interfaces and a strict stdlib-first policy (`github.com/dgraph-io/badger/v4` is the sole direct dependency):

```
cmd/bot/main.go                  # Composition root, DI wiring, signal handling, scheduler jobs
internal/
  config/                        # Environment variable parsing & validation
  domain/                        # Pure domain models & business rules (zero external deps)
  service/                       # Application use-cases & consumer-side port interfaces
  scheduler/                     # Hourly clock-deduplicated job runner with panic recovery
  adapter/
    telegram/                    # Long-polling Bot API client, command catalog, handler, wizard
    notion/                      # Notion REST client with retry/backoff, pagination & trade CRUD
    badger/                      # BadgerDB v4 store with GC, atomic badge claims & TTL caches
    gemini/                      # Google Gemini REST analyzer
    exporter/                    # Formula-safe CSV & raw PDF generators
pkg/timeutil/                    # Canonical WIB (UTC+7) timezone & week-boundary helpers
```

## Quick Start

### 1. Configure Environment

Copy `.env.example` to `.env` and fill in the required credentials:

```bash
cp .env.example .env
```

Required variables:
- `TELEGRAM_BOT_TOKEN` — Telegram Bot token from `@BotFather`
- `NOTION_TOKEN` — Notion internal integration secret
- `NOTION_PARENT_PAGE_ID` — Notion page ID shared with the integration under which member journals are provisioned

### 2. Run with Docker Compose

```bash
docker compose up -d --build
```

### 3. Build & Test Locally

```bash
# Build static binary
CGO_ENABLED=0 go build -ldflags="-s -w" -o ark-vault ./cmd/bot

# Run unit tests with race detector
go test -v -race ./...

# Run linter
golangci-lint run ./...
```

## CI/CD

Every push to `main` runs: **commitlint** (Conventional Commits are mandatory) → **govulncheck** + **golangci-lint** → **tests with coverage gate (40%)** → **binary build** → **Docker image** → **deploy**.

The deploy job SSHes into the production host, pulls the commit, rebuilds the container, and waits up to 2 minutes for the Docker healthcheck; if the new build is unhealthy it automatically rolls back to the previous commit. To enable it, configure repository secrets:

| Secret | Value |
|---|---|
| `SSH_HOST` | production host address |
| `SSH_KEY` | private key whose public counterpart is in the host's `authorized_keys` |
| `SSH_USERNAME` | optional, defaults to `kresna` |
| `SSH_PORT` | optional, defaults to `22` |

Without these secrets the deploy step is skipped and CI stays green.
