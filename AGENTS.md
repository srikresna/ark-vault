# Repository Guidelines

## Project Overview

**ARK Vault** (`github.com/arkcode369/ark-vault`) is a community-focused Telegram bot written in Go (minimum 1.26, toolchain 1.26.10) that automates trade journal recording into per-member Notion pages/databases, paired with a local BadgerDB gamification engine (XP, 10 levels, daily streaks, 13 badges, rotating weekly challenges, monthly goals, dual-mode reminders) and optional Google Gemini AI trade coaching (`/analyze`, `/reportcard`).

## Architecture & Data Flow

The codebase follows **Hexagonal Architecture (Ports & Adapters)** with idiomatic Go **consumer-side interfaces**:

1. **Composition Root (`cmd/bot/main.go`)**: Loads env config (`config.Load`), opens BadgerDB (`badgerdb.OpenStore`), wires Notion/Badger/Gemini/Exporter adapters into application services, registers 5 hourly deduplicated jobs in `scheduler.Scheduler`, registers Telegram command scopes (`sender.RegisterCommandScopes`), and starts long-polling (`bot.Start`).
2. **Primary Adapters (`internal/adapter/telegram/`, `internal/scheduler/`)**:
   - `Bot.Start` polls Telegram `getUpdates` (`message`, `callback_query`) and admits each batch to a durable Badger ledger (`gam:idem:<bot-scope>:…`, preserved by `Store.Reset`): the update payloads and the fetch cursor commit in one transaction, so the Telegram offset advances immediately and polling never waits for handlers. Per-update states are `admitted` (never ran, replayed on restart), `running` (may have written; never replayed, the user is told to verify via `/history`), `done` (handler returned; not proof of success) and `verify`. `updateDispatcher` preserves per-user FIFO with separate limits for active and pending updates. Shutdown gives handlers 8s, then cancels them and waits for every worker before `main` closes the store. This is at-most-once per `update_id` for side effects, not exactly-once: Notion and Badger cannot commit together.
   - `Handler.HandleUpdate` enforces per-user rate limiting (`RateLimiter.Allow`) and optional community group membership (`isAuthorized`), routes active stateful flows (`TradeEditSession` or 9-step `GuidedFlow`), and dispatches `/command` or `#journal` messages. Slash commands and `#journal` entries always escape pending prompts so they can never be swallowed as flow input.
3. **Application Services (`internal/service/`)**:
   - Depend only on consumer-side interfaces declared in `internal/service/contracts.go` (`TradeRepository`, `MemberRepository`, `ImageStore`, `AIAnalyzer`, `GamificationStore`, `BadgeStore`, `ChallengeStore`, `ReminderStore`, `GoalStore`, `AnalyticsCacheStore`, `ReportCardStore`).
4. **Secondary Adapters (`internal/adapter/{notion,badger,gemini,exporter}`)**:
   - **Dual Storage**: Notion stores canonical member journal pages and inline `Trades` databases; BadgerDB stores high-frequency gamification state (`gam:*` keys), AI caches, persistent member mappings (`gam:member:<id>`), and 10-minute TTL trade caches (`gam:trades_cache:<id>`).

## Key Directories

- `cmd/bot/` â€” Application entrypoint (`main.go`), manual dependency injection, signal handling (`SIGINT`/`SIGTERM`), and scheduled job definitions.
- `internal/domain/` â€” Pure domain models and business rules with zero external/I/O dependencies (`trade.go`, `stats.go`, `xp.go`, `asset.go`, `gamification.go`, `badge.go`, `challenge.go`, `goal.go`, `reminder.go`, `analytics.go`).
- `internal/service/` — Use-case orchestration (`journal.go`, `gamification.go`, `badge.go`, `challenge.go`, `goal.go`, `reminder.go`, `leaderboard.go`, `report.go`, `analytics.go`, `reportcard.go`, `posttrade.go`) and consumer-side interfaces (`contracts.go`).
- `internal/adapter/telegram/` â€” Telegram Bot API client (`bot.go`, `sender.go`), single-source command catalog (`commands.go`), update router (`handler.go`), `#journal` parser (`parser.go`), 9-step wizard (`guided.go`), HTML message builders (`formatter.go`), and sliding-window rate limiter (`ratelimit.go`).
- `internal/adapter/notion/` â€” Notion REST client (`client.go`, API version `2022-06-28`), member provisioning/lookup (`member.go`) that records the journal page before its Trades database exists, falls back to parent-page callout discovery (Notion search indexes page titles only), repairs journals missing their inline database, and evicts archived mappings, cached trade CRUD that surfaces unreadable/unparseable histories instead of returning empty ones (`trade.go`), and binary screenshot uploader (`image.go`).
- `internal/adapter/badger/` â€” BadgerDB v4 wrapper with 5-minute value-log GC (`store.go`) and 9 repositories using the `gam:` key namespace (`gamification.go`, `badge.go`, `challenge.go`, `goal.go`, `reminder.go`, `analytics.go`, `cache.go`).
- `internal/adapter/exporter/` â€” Zero-dependency CSV exporter with formula-injection sanitization (`csv.go`) and minimal raw PDF generator (`pdf.go`).
- `internal/adapter/gemini/` â€” Google Gemini `generateContent` REST client (`analyzer.go`).
- `internal/scheduler/` â€” Hourly ticker with `ShouldFire(jobName, now)`/`MarkFired(jobName, now)` clock-hour deduplication, claimed only after a job body succeeds (`scheduler.go`).
- `pkg/` â€” Shared stateless utilities (`pkg/timeutil/tz.go`).

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
- **Notion Write Idempotency**: `Client.CreatePage` and `Client.CreateDatabase` are non-idempotent and use `doCreate`, which retries only 429 (rejected, never applied). Transport failures (including cancellation), unreadable responses and 5xx surface as ambiguous writes (`IsAmbiguousWriteError` / the `MayHaveBeenSaved() bool` marker), because Notion may have committed the write: retrying would create a second page/database (duplicate journal, split trade history) or a duplicate trade. A 2xx whose body cannot be used (malformed JSON, missing id) is also ambiguous (`notion.AmbiguousWriteError`) — the object exists but cannot be addressed. Callers MUST reconcile by reading Notion (parent-page discovery for journals, page children for the Trades database) instead of retrying; `SaveTrade` drops the cached history and the handler tells the user to check `/history` rather than resend. Idempotent reads and updates keep the full transient retry policy.
- **Journal/Reset Exclusivity**: `MemberRepo.ResetAll`/`ResetAllWith` hold the journaling gate from before discovery through archiving; `ListMembers` takes the same gate for reading so a roster is never built from a half-reset state (and never re-adds an archived mapping); journal writes (`EnsureMember` provisioning/repair, `SaveTrade`, `UpdateTrade`, `DeleteTrade`) take it for reading and fail fast with `ErrResetInProgress`. The `/reset` handler passes the BadgerDB wipe as `ResetAllWith`'s finalize step so the wipe also runs under the gate. Reset evicts the persisted mapping *before* archiving the page: if the mapping cannot be removed the journal is left live and the reset reports an error. An ambiguous archive response is reconciled with `GetPage`: a page confirmed archived is counted and its cached mapping dropped; an unverifiable outcome drops the mapping anyway (a live page is rediscovered by parent-scoped lookup, an archived one must never be reused). A failed archive never wipes BadgerDB.
- **BadgerDB Key Schema**: All keys use the `gam:` prefix (`gam:profile:<id>`, `gam:streak:<id>`, `gam:xp_log:<id>:<ns>:<rand>`, `gam:badge:<id>:<badge>`, `gam:challenge:<yearWeek>`, `gam:goal:<id>:<ym>`, `gam:reminder_pref:<id>`, `gam:analytics_cache:<id>`, `gam:report:<id>:<ym>`, `gam:member:<id>`, `gam:trades_cache:<id>`, `gam:trade_xp:<id>:<tradeID>`). XP events older than 90 days are pruned daily at 04:00 WIB by the `gam-prune` scheduler job; `gam:trade_xp:*` grants are never pruned by age (they are needed to reconcile a trade edit/delete at any later time) and are deleted only with their trade. Recording a trade commits its profile, streak, grant and XP event in one Badger transaction (`CommitTradeRecord`); editing or deleting a trade commits its profile, XP event and grant mutation together (`CommitTradeAdjustment`), so a zero-XP-delta deletion still removes the grant atomically. A grant is the authoritative record of a trade's contribution: zero means it earned nothing (daily cap, or a failed record, which `OnTradeRecorded` compensates with a best-effort zero grant), and a missing grant means the contribution is unknown (pre-grant data) — reconciliation must never assume a nominal base for it.
- **Security Invariants**:
  - Never embed `TELEGRAM_BOT_TOKEN` in external URLs; download file bytes via `Bot.GetFile` and upload binary content via `Notion.CreateFileUpload` / `SendFileUpload`.
  - Sanitize CSV fields starting with `=`, `+`, `-`, `@`, `\t`, `\r`, `\n` by prefixing `'` (`internal/adapter/exporter/csv.go`).
  - Owner-only commands (`/reset`) must check `msg.From.ID == h.ownerID` (silent ignore for non-owners) and register only under the owner's `chat` command scope in `Sender.RegisterCommandScopes`.

## Important Files

- `cmd/bot/main.go` â€” Composition root and background scheduler wiring.
- `internal/config/config.go` & `.env.example` â€” Environment variable parsing, validation, and defaults (`TELEGRAM_BOT_TOKEN`, `NOTION_TOKEN`, `NOTION_PARENT_PAGE_ID` required).
- `internal/service/contracts.go` â€” Service-layer repository, store, and AI interfaces.
- `internal/domain/xp.go` â€” XP reward constants, `MaxDailyTradeXPCount` (`3`), and the 10-level `LevelTable` (`0` to `50,000 XP`).
- `internal/domain/stats.go` â€” `WinRate`/`ClosedTrades`/`CountResults` (the single win-rate definition: wins ÷ wins+losses+BE, OPEN never counted), `CalculateStats` (WinRate, Expectancy, ProfitFactor, `ByAsset`, `BySession`, `TopSymbols`), `FilterTradesByPeriod`, and `ConsecutiveLossesToday`. Every service computing a win rate (stats, report card, challenge, goal, badge, leaderboard, analytics, community report) MUST call `domain.WinRate`/`domain.CountResults` rather than dividing locally.
- `internal/adapter/telegram/handler.go` â€” Command/callback router, guided journaling, `/open` & `/history` interactive edit/delete flows, and `/reset`.
- `internal/adapter/notion/member.go` & `trade.go` â€” Notion page/database auto-provisioning (page persisted before its Trades database, parent-scoped callout discovery, in-place repair of partially provisioned journals, mapping eviction on reset) and cached trade CRUD that never reports a failed read as an empty history.
- `.github/workflows/ci.yml` â€” GitHub Actions CI/CD pipeline (`Lint`, `Test & Coverage`, `Build Binary`, `Build Docker Image`).

## Runtime & Tooling Preferences

- **Runtime**: Go minimum `1.26` (`go 1.26.0` + `toolchain go1.26.10` in `go.mod`, required by `x/net v0.60+` for patched stdlib transitive versions); build toolchain `golang:1.26-alpine` in `Dockerfile` and CI `GO_VERSION=1.26.10` — Go 1.23/1.24/1.25 are EOL and no longer receive security patches, which fails the govulncheck CI gate.
- **Stdlib-First Policy**: Keep `github.com/dgraph-io/badger/v4` as the sole direct dependency in `go.mod`. Use standard `net/http`, `encoding/json`, `mime/multipart`, and `log/slog` rather than adding third-party HTTP/bot/ORM frameworks.
- **Container Constraints**: Production runs as non-root `appuser` on `alpine:3.20` with `/data/badger` mounted to a persistent volume and a `128M` memory limit (`docker-compose.yml`).

## Testing & QA

- **Framework**: Standard library `testing` and `net/http/httptest` only (white-box tests in the same package, no `testify` or codegen mocks).
- **Mock Conventions**:
  - Service tests (`internal/service/*_test.go`) use in-memory map-backed structs (`mockGamificationStore`, `mockGamificationTradeRepo`, `mockReminderStore`, `mockChallengeStore`, `mockBadgeStore`) with defensive copies.
  - Adapter HTTP tests (`internal/adapter/notion/*_test.go`, `internal/adapter/telegram/*_test.go`) spin up `httptest.NewServer` and override `client.baseURL = srv.URL + "/v1"` or `sender.baseAPI = srv.URL + "/bot"`. Use `sync/atomic` for counters mutated inside `httptest` handlers so `go test -race ./...` passes.
- **Pre-Commit Gate**: Every change MUST pass `golangci-lint run ./...` (`0 issues`) and `go test -v ./...` before pushing.
