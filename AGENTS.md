# Repository Guidelines

## Project Overview

**ARK Vault** (`github.com/arkcode369/ark-vault`) is a community-focused Telegram bot written in Go 1.23 that automates trade journal recording into per-member Notion pages/databases, paired with a local BadgerDB gamification engine (XP, 10 levels, daily streaks, 13 badges, rotating weekly challenges, monthly goals, dual-mode reminders) and optional Google Gemini AI trade coaching (`/analyze`, `/reportcard`).

## Architecture & Data Flow

The codebase follows **Hexagonal Architecture (Ports & Adapters)** with idiomatic Go **consumer-side interfaces**:

1. **Composition Root (`cmd/bot/main.go`)**: Loads env config (`config.Load`), opens BadgerDB (`badgerdb.OpenStore`), wires Notion/Badger/Gemini/Exporter adapters into application services, registers 5 hourly deduplicated jobs in `scheduler.Scheduler`, registers Telegram command scopes (`sender.RegisterCommandScopes`), and starts long-polling (`bot.Start`).
2. **Primary Adapters (`internal/adapter/telegram/`, `internal/scheduler/`)**:
   - `Bot.Start` polls Telegram `getUpdates` (`message`, `callback_query`) and dispatches each update concurrently (`go b.handler.HandleUpdate(ctx, u)`).
   - `Handler.HandleUpdate` enforces per-user rate limiting (`RateLimiter.Allow`) and optional community group membership (`isAuthorized`), routes active stateful flows (`TradeEditSession` or 9-step `GuidedFlow`), and dispatches `/command` or `#journal` messages.
3. **Application Services (`internal/service/`)**:
   - Depend only on consumer-side interfaces declared in `internal/service/contracts.go` (`TradeRepository`, `MemberRepository`, `ImageStore`, `AIAnalyzer`, `GamificationStore`, `BadgeStore`, `ChallengeStore`, `ReminderStore`, `GoalStore`, `AnalyticsCacheStore`, `ReportCardStore`).
4. **Secondary Adapters (`internal/adapter/{notion,badger,gemini,exporter}`)**:
   - **Dual Storage**: Notion stores canonical member journal pages and inline `Trades` databases; BadgerDB stores high-frequency gamification state (`gam:*` keys), AI caches, persistent member mappings (`gam:member:<id>`), and 10-minute TTL trade caches (`gam:trades_cache:<id>`).

## Key Directories

- `cmd/bot/` — Application entrypoint (`main.go`), manual dependency injection, signal handling (`SIGINT`/`SIGTERM`), and scheduled job definitions.
- `internal/domain/` — Pure domain models and business rules with zero external/I/O dependencies (`trade.go`, `stats.go`, `xp.go`, `asset.go`, `gamification.go`, `badge.go`, `challenge.go`, `goal.go`, `reminder.go`, `analytics.go`).
- `internal/service/` — Use-case orchestration (`journal.go`, `gamification.go`, `badge.go`, `challenge.go`, `goal.go`, `reminder.go`, `leaderboard.go`, `report.go`, `analytics.go`, `reportcard.go`) and consumer-side interfaces (`contracts.go`).
- `internal/adapter/telegram/` — Telegram Bot API client (`bot.go`, `sender.go`), single-source command catalog (`commands.go`), update router (`handler.go`), `#journal` parser (`parser.go`), 9-step wizard (`guided.go`), HTML message builders (`formatter.go`), and sliding-window rate limiter (`ratelimit.go`).
- `internal/adapter/notion/` — Notion REST client (`client.go`, API version `2022-06-28`), 3-tier member provisioning/lookup (`member.go`), cached trade CRUD (`trade.go`), and binary screenshot uploader (`image.go`).
- `internal/adapter/badger/` — BadgerDB v4 wrapper with 5-minute value-log GC (`store.go`) and 9 repositories using the `gam:` key namespace (`gamification.go`, `badge.go`, `challenge.go`, `goal.go`, `reminder.go`, `analytics.go`, `cache.go`).
- `internal/adapter/exporter/` — Zero-dependency CSV exporter with formula-injection sanitization (`csv.go`) and minimal raw PDF generator (`pdf.go`).
- `internal/adapter/gemini/` — Google Gemini `generateContent` REST client (`analyzer.go`).
- `internal/scheduler/` — Hourly ticker with `MarkAndCheck(jobName, now)` clock-hour deduplication (`scheduler.go`).
- `pkg/` — Shared stateless utilities (`pkg/timeutil/tz.go`).

## Development Commands

```bash
# Build static binary
CGO_ENABLED=0 go build -ldflags="-s -w" -o ark-vault ./cmd/bot

# Run all tests (or with race detector when CGO is enabled)
go test -v ./...
go test -v -race -coverprofile=coverage.out ./...

# Run a single package or test
go test -v ./internal/service -run TestGamificationService_DailyTradeXPCap

# Lint (must report 0 issues before committing)
golangci-lint run ./...

# Run via Docker Compose (requires .env)
docker compose up -d --build
```

## Code Conventions & Common Patterns

- **Consumer-Side Interfaces**: Define interfaces in the consuming package (`internal/service/contracts.go`, `internal/adapter/telegram/handler.go`, `internal/adapter/notion/{member,trade}.go`). Adapters return concrete structs (`*MemberRepo`, `*TradeRepo`, `*GamificationRepo`).
- **Go 1.22+ Loop Idioms**: Prefer `for i := range n` over `for i := 0; i < n; i++`.
- **String Builders & Linter Compliance**:
  - Always use `fmt.Fprintf(&sb, ...)` instead of `sb.WriteString(fmt.Sprintf(...))` (`staticcheck` `QF1012`).
  - Always handle or explicitly ignore error returns from best-effort calls (`_, _ = h.sender.SendHTML(...)`, `defer func() { _ = resp.Body.Close() }()`) to satisfy `golangci-lint` (`errcheck`).
- **Timezone Invariant (`WIB` / `Asia/Jakarta`)**: All user-facing date boundaries (daily streaks, `MaxDailyTradeXPCount = 3` daily cap, period filters, reminders, weekly challenges, monthly goals/report cards) MUST evaluate timestamps in `Asia/Jakarta` (`UTC+7`).
- **Cache Consistency**: Any mutation to Notion trades (`SaveTrade`, `UpdateTrade`, `DeleteTrade`) MUST call `InvalidateCachedTrades(ctx, memberID)` on `TradeRepo.cacheStore`.
- **BadgerDB Key Schema**: All keys use the `gam:` prefix (`gam:profile:<id>`, `gam:streak:<id>`, `gam:xp_log:<id>:<ns>:<rand>`, `gam:badge:<id>:<badge>`, `gam:challenge:<yearWeek>`, `gam:goal:<id>:<ym>`, `gam:reminder_pref:<id>`, `gam:analytics_cache:<id>`, `gam:report:<id>:<ym>`, `gam:member:<id>`, `gam:trades_cache:<id>`).
- **Security Invariants**:
  - Never embed `TELEGRAM_BOT_TOKEN` in external URLs; download file bytes via `Bot.GetFile` and upload binary content via `Notion.CreateFileUpload` / `SendFileUpload`.
  - Sanitize CSV fields starting with `=`, `+`, `-`, `@`, `\t`, `\r`, `\n` by prefixing `'` (`internal/adapter/exporter/csv.go`).
  - Owner-only commands (`/reset`) must check `msg.From.ID == h.ownerID` (silent ignore for non-owners) and register only under the owner's `chat` command scope in `Sender.RegisterCommandScopes`.

## Important Files

- `cmd/bot/main.go` — Composition root and background scheduler wiring.
- `internal/config/config.go` & `.env.example` — Environment variable parsing, validation, and defaults (`TELEGRAM_BOT_TOKEN`, `NOTION_TOKEN`, `NOTION_PARENT_PAGE_ID` required).
- `internal/service/contracts.go` — Service-layer repository, store, and AI interfaces.
- `internal/domain/xp.go` — XP reward constants, `MaxDailyTradeXPCount` (`3`), and the 10-level `LevelTable` (`0` to `50,000 XP`).
- `internal/domain/stats.go` — `CalculateStats` (WinRate, Expectancy, ProfitFactor, `ByAsset`, `BySession`, `TopSymbols`), `FilterTradesByPeriod`, and `ConsecutiveLossesToday`.
- `internal/adapter/telegram/handler.go` — Command/callback router, guided journaling, `/open` & `/history` interactive edit/delete flows, and `/reset`.
- `internal/adapter/notion/member.go` & `trade.go` — Notion page/database auto-provisioning, child DB resolution, and cached trade CRUD.
- `.github/workflows/ci.yml` — GitHub Actions CI/CD pipeline (`Lint`, `Test & Coverage`, `Build Binary`, `Build Docker Image`).

## Runtime & Tooling Preferences

- **Runtime**: Go `1.23+` (`go 1.23.0` in `go.mod`; `golang:1.23-alpine` in `Dockerfile`).
- **Stdlib-First Policy**: Keep `github.com/dgraph-io/badger/v4` as the sole direct dependency in `go.mod`. Use standard `net/http`, `encoding/json`, `mime/multipart`, and `log/slog` rather than adding third-party HTTP/bot/ORM frameworks.
- **Container Constraints**: Production runs as non-root `appuser` on `alpine:3.20` with `/data/badger` mounted to a persistent volume and a `128M` memory limit (`docker-compose.yml`).

## Testing & QA

- **Framework**: Standard library `testing` and `net/http/httptest` only (white-box tests in the same package, no `testify` or codegen mocks).
- **Mock Conventions**:
  - Service tests (`internal/service/*_test.go`) use in-memory map-backed structs (`mockGamificationStore`, `mockGamificationTradeRepo`, `mockReminderStore`, `mockChallengeStore`, `mockBadgeStore`) with defensive copies.
  - Adapter HTTP tests (`internal/adapter/notion/*_test.go`, `internal/adapter/telegram/*_test.go`) spin up `httptest.NewServer` and override `client.baseURL = srv.URL + "/v1"` or `sender.baseAPI = srv.URL + "/bot"`. Use `sync/atomic` for counters mutated inside `httptest` handlers so `go test -race ./...` passes.
- **Pre-Commit Gate**: Every change MUST pass `golangci-lint run ./...` (`0 issues`) and `go test -v ./...` before pushing.
