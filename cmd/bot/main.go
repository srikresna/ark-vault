package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	badgerdb "github.com/arkcode369/ark-vault/internal/adapter/badger"
	"github.com/arkcode369/ark-vault/internal/adapter/exporter"
	"github.com/arkcode369/ark-vault/internal/adapter/gemini"
	"github.com/arkcode369/ark-vault/internal/adapter/notion"
	"github.com/arkcode369/ark-vault/internal/adapter/telegram"
	"github.com/arkcode369/ark-vault/internal/config"
	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/internal/scheduler"
	"github.com/arkcode369/ark-vault/internal/service"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
	"github.com/dgraph-io/badger/v4"
)

var version = "dev"

func main() {
	// Config
	cfg, err := config.Load()
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Logger
	var logLevel slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	}))
	slog.SetDefault(logger)

	// BadgerDB for gamification & caching
	store, err := badgerdb.OpenStore(cfg.BadgerDBPath)
	if err != nil {
		logger.Error("failed to open badger store", "error", err)
		os.Exit(1)
	}
	defer func() { _ = store.Close() }()

	// Notion adapters (with BadgerDB persistent member index & trade cache)
	memberCacheRepo := badgerdb.NewMemberCacheRepo(store)
	tradeCacheRepo := badgerdb.NewTradeCacheRepo(store)
	notionClient := notion.NewClient(cfg.NotionToken)
	memberRepo := notion.NewMemberRepo(notionClient, cfg.NotionParentID, memberCacheRepo)
	tradeRepo := notion.NewTradeRepo(notionClient, memberRepo, tradeCacheRepo)
	imageRepo := notion.NewImageRepo(notionClient)

	// Services
	journalSvc := service.NewJournalService(tradeRepo, memberRepo, imageRepo)
	leaderboardSvc := service.NewLeaderboardService(tradeRepo, memberRepo)
	reportSvc := service.NewReportService(tradeRepo, memberRepo)
	gamRepo := badgerdb.NewGamificationRepo(store)
	gamSvc := service.NewGamificationService(gamRepo)

	// Badge & Challenge services
	badgeRepo := badgerdb.NewBadgeRepo(store)
	challengeRepo := badgerdb.NewChallengeRepo(store)
	badgeSvc := service.NewBadgeService(badgeRepo, tradeRepo, gamSvc)
	challengeSvc := service.NewChallengeService(challengeRepo, tradeRepo, memberRepo, gamSvc, badgeSvc)

	// Reminder & Goal services
	reminderRepo := badgerdb.NewReminderRepo(store)
	goalRepo := badgerdb.NewGoalRepo(store)
	reminderSvc := service.NewReminderService(reminderRepo, gamRepo, logger)
	goalSvc := service.NewGoalService(goalRepo, tradeRepo, gamSvc, badgeSvc)

	// Post-trade reward orchestration (XP, badges, monthly goal)
	postTradeSvc := service.NewPostTradeService(gamSvc, badgeSvc, goalSvc)

	// Gemini AI (optional)
	var aiAnalyzer service.AIAnalyzer
	if cfg.GeminiAPIKey != "" {
		aiAnalyzer = gemini.NewAnalyzer(cfg.GeminiAPIKey, cfg.GeminiModel)
		logger.Info("gemini AI analyzer enabled", "model", cfg.GeminiModel)
	}

	// Analytics & Report Card services
	analyticsCacheRepo := badgerdb.NewAnalyticsCacheRepo(store)
	reportCardRepo := badgerdb.NewReportCardRepo(store)
	var analyticsSvc *service.AnalyticsService
	var reportCardSvc *service.ReportCardService
	if aiAnalyzer != nil {
		analyticsSvc = service.NewAnalyticsService(analyticsCacheRepo, aiAnalyzer, tradeRepo)
		reportCardSvc = service.NewReportCardService(reportCardRepo, tradeRepo, gamRepo, badgeRepo, aiAnalyzer)
	}

	// Exporter (CSV + PDF)
	exp := exporter.NewExporter()

	// Rate limiter
	limiter := telegram.NewRateLimiter(cfg.RateLimitPerMin, 1*time.Minute)

	// Telegram
	sender := telegram.NewSender(cfg.TelegramToken)
	svc := telegram.Services{
		Journal:      journalSvc,
		Leaderboard:  leaderboardSvc,
		Report:       reportSvc,
		Gamification: gamSvc,
		Badge:        badgeSvc,
		Challenge:    challengeSvc,
		Reminder:     reminderSvc,
		Goal:         goalSvc,
		Analytics:    analyticsSvc,
		ReportCard:   reportCardSvc,
		PostTrade:    postTradeSvc,
	}
	handler := telegram.NewHandler(
		sender, svc, exp, tradeRepo, memberRepo, limiter, logger,
		cfg.CommunityGroupID, cfg.OwnerID,
	)
	defer handler.Close()
	bot := telegram.NewBot(cfg.TelegramToken, handler, logger)
	// Durable update ledger: a fetched batch is stored together with the fetch
	// cursor in one transaction, so polling never waits for handlers and
	// Telegram can forget an update only after this bot owns a copy. A
	// redelivered update never runs twice, and an interrupted one is reported
	// for manual verification. The ledger survives /reset (see
	// badger.Store.Reset).
	bot.SetReplayGuard(telegram.ReplayStoreFuncs{
		GetFn: func(key string, dest interface{}) (bool, error) {
			err := store.Get(key, dest)
			if errors.Is(err, badger.ErrKeyNotFound) {
				return false, nil
			}
			return err == nil, err
		},
		SetFn:        store.SetWithTTL,
		DeleteFn:     store.Delete,
		ScanFn:       store.Scan,
		ApplyBatchFn: store.ApplyBatch,
	})
	handler.SetBot(bot)
	handler.SetResetter(store)
	// Graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := sender.RegisterCommandScopes(ctx, cfg.OwnerID); err != nil {
		logger.Warn("failed to register telegram command scopes", "error", err)
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		logger.Info("shutdown signal received")
		cancel()
	}()

	// Scheduler
	sched := scheduler.NewScheduler(logger)

	// Weekly report auto-post
	if cfg.ReportChatID != 0 {
		sched.Add(scheduler.Job{
			Name:     "weekly-report",
			Interval: 10 * time.Minute, // 10-minute ticks + claim-on-success: transient failures retry within the window
			Run: func(ctx context.Context) error {
				now := time.Now().In(timeutil.WIB)
				dayMatch := isDayOfWeek(now, cfg.ReportDay)
				hourMatch := now.Hour() == cfg.ReportHour
				if !dayMatch || !hourMatch || !sched.ShouldFire("weekly-report", now) {
					return nil
				}
				logger.Info("posting scheduled weekly report", "chat_id", cfg.ReportChatID, "thread_id", cfg.ReportThreadID)
				if err := handler.SendScheduledReport(ctx, cfg.ReportChatID, cfg.ReportThreadID); err != nil {
					return err // unmarked → retried by the next tick in this hour
				}
				sched.MarkFired("weekly-report", now)
				return nil
			},
		})
		logger.Info("scheduler: weekly-report enabled", "report_day", cfg.ReportDay, "report_hour", cfg.ReportHour)
	}

	// Challenge scheduler jobs
	sched.Add(scheduler.Job{
		Name:     "challenge-finalize",
		Interval: 10 * time.Minute,
		Run: func(ctx context.Context) error {
			now := time.Now().In(timeutil.WIB)
			// Finalize on Sunday at report hour
			if now.Weekday() != time.Sunday || now.Hour() != cfg.ReportHour || !sched.ShouldFire("challenge-finalize", now) {
				return nil
			}
			yearWeek := domain.YearWeekString(now)
			results, err := challengeSvc.FinalizeChallenge(ctx, yearWeek)
			if err != nil {
				return err // unmarked → retried; FinalizeChallenge is idempotent via its Finalized flag
			}
			sched.MarkFired("challenge-finalize", now)
			if len(results) > 0 && cfg.ReportChatID != 0 {
				challenge, _ := challengeSvc.GetOrCreateChallenge(ctx, now)
				if challenge != nil {
					text := telegram.FormatChallengeResults(challenge, results)
					_, _ = sender.SendHTML(ctx, cfg.ReportChatID, text, cfg.ReportThreadID)
				}
			}
			return nil
		},
	})

	sched.Add(scheduler.Job{
		Name:     "challenge-announce",
		Interval: 10 * time.Minute,
		Run: func(ctx context.Context) error {
			now := time.Now().In(timeutil.WIB)
			// Announce on Monday at configured report hour (WIB)
			if now.Weekday() != time.Monday || now.Hour() != cfg.ReportHour || !sched.ShouldFire("challenge-announce", now) {
				return nil
			}
			challenge, err := challengeSvc.GetOrCreateChallenge(ctx, now)
			if err != nil {
				return err // unmarked → retried
			}
			if cfg.ReportChatID != 0 {
				text := fmt.Sprintf("⚔️ <b>New Weekly Challenge!</b>\n\n<b>%s</b>\n%s\n\nGunakan /challenge untuk melihat standings.", challenge.Title, challenge.Description)
				if _, err := sender.SendHTML(ctx, cfg.ReportChatID, text, cfg.ReportThreadID); err != nil {
					return err // unmarked → retried while the window lasts
				}
			}
			sched.MarkFired("challenge-announce", now)
			return nil
		},
	})

	sched.Add(scheduler.Job{
		Name:     "daily-reminder",
		Interval: 10 * time.Minute,
		Run: func(ctx context.Context) error {
			now := time.Now().In(timeutil.WIB)
			if !sched.ShouldFire("daily-reminder", now) {
				return nil
			}
			dueReminders, err := reminderSvc.GetDueReminders(ctx)
			if err != nil {
				return err // unmarked → retried within the user's reminder hour
			}
			sent, failed := 0, 0
			for _, pref := range dueReminders {
				var text string
				if pref.IsCommunity || pref.ChatID < 0 {
					text = telegram.FormatCommunityReminder()
				} else {
					streak, _ := gamSvc.GetStreak(ctx, pref.TelegramID)
					streakDays := 0
					if streak != nil {
						streakDays = streak.CurrentStreak
					}
					text = telegram.FormatDailyReminder(streakDays)
				}
				if _, err := sender.SendHTML(ctx, pref.ChatID, text, pref.ThreadID); err != nil {
					failed++
					continue
				}
				sent++
			}
			if sent == 0 && failed > 0 {
				// Every send failed — likely a Telegram-wide blip. Leave the
				// hour unmarked so the next tick retries; partial success is
				// kept to avoid duplicate reminders for delivered users.
				return fmt.Errorf("all %d reminder sends failed", failed)
			}
			sched.MarkFired("daily-reminder", now)
			return nil
		},
	})

	sched.Add(scheduler.Job{
		Name:     "monthly-report-card",
		Interval: 10 * time.Minute,
		Run: func(ctx context.Context) error {
			if reportCardSvc == nil {
				return nil
			}
			now := time.Now().In(timeutil.WIB)
			// Generate on 1st of month at report hour
			if now.Day() == 1 && now.Hour() == cfg.ReportHour && sched.ShouldFire("monthly-report-card", now) {
				lastMonth := timeutil.PreviousYearMonth(now)
				members, err := memberRepo.ListMembers(ctx)
				if err != nil {
					return err // unmarked → retried within the hour
				}
				for _, m := range members {
					report, err := reportCardSvc.GenerateMonthlyReport(ctx, m.TelegramID, lastMonth)
					if err != nil {
						logger.Error("report card generation failed", "member", m.TelegramID, "error", err)
						continue
					}
					if report.TotalTrades > 0 {
						text := telegram.FormatMonthlyReportCard(report)
						// Send to report chat if configured
						if cfg.ReportChatID != 0 {
							_, _ = sender.SendHTML(ctx, cfg.ReportChatID, text, cfg.ReportThreadID)
						}
					}
				}
				sched.MarkFired("monthly-report-card", now)
			}
			return nil
		},
	})

	// Daily BadgerDB backup (03:00 WIB, keeps 7 daily snapshots)
	sched.Add(scheduler.Job{
		Name:     "badger-daily-backup",
		Interval: 1 * time.Hour,
		Run: func(_ context.Context) error {
			now := time.Now().In(timeutil.WIB)
			if now.Hour() == 3 && sched.ShouldFire("badger-daily-backup", now) {
				backupDir := filepath.Join(cfg.BadgerDBPath, "backups")
				path, err := store.BackupToDir(backupDir, 7)
				if err != nil {
					logger.Error("badger daily backup failed", "error", err)
					return err // unmarked → retried on the next tick/restart
				}
				sched.MarkFired("badger-daily-backup", now)
				logger.Info("badger daily backup completed", "path", path)
			}
			return nil
		},
	})

	// Daily gamification pruning (04:00 WIB): XP events have no consumer older
	// than a couple of months (daily cap = today, report cards = current and
	// previous month), but they slow every scan and grow the Badger value log
	// unboundedly under the 128M container limit. Trade-XP grants are exempt:
	// they record what each trade actually earned and are still needed if that
	// trade is edited or deleted later; they go away with the trade itself.
	sched.Add(scheduler.Job{
		Name:     "gam-prune",
		Interval: 1 * time.Hour,
		Run: func(ctx context.Context) error {
			now := time.Now().In(timeutil.WIB)
			if now.Hour() == 4 && sched.ShouldFire("gam-prune", now) {
				cutoff := now.AddDate(0, 0, -90)
				deleted, err := gamRepo.PruneGamification(ctx, cutoff)
				if err != nil {
					logger.Error("gamification prune failed", "error", err)
					return err // unmarked → retried on the next tick/restart
				}
				sched.MarkFired("gam-prune", now)
				if deleted > 0 {
					logger.Info("gamification prune completed", "deleted", deleted, "cutoff", cutoff.Format("2006-01-02"))
				}
			}
			return nil
		},
	})

	go sched.Start(ctx)
	logger.Info("scheduler started")

	// Stdlib-only Prometheus metrics & health server
	if cfg.MetricsAddr != "" && cfg.MetricsAddr != "off" && cfg.MetricsAddr != "none" {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
		})
		startTime := time.Now()
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			m := handler.Metrics()
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			uptime := time.Since(startTime).Seconds()
			_, _ = fmt.Fprintf(w, "# HELP ark_vault_up Whether the bot process is running.\n# TYPE ark_vault_up gauge\nark_vault_up 1\n\n")
			_, _ = fmt.Fprintf(w, "# HELP ark_vault_build_info Build and version metadata.\n# TYPE ark_vault_build_info gauge\nark_vault_build_info{version=%q} 1\n\n", version)
			_, _ = fmt.Fprintf(w, "# HELP ark_vault_uptime_seconds Process uptime in seconds.\n# TYPE ark_vault_uptime_seconds gauge\nark_vault_uptime_seconds %.1f\n\n", uptime)
			_, _ = fmt.Fprintf(w, "# HELP ark_vault_goroutines Current goroutine count.\n# TYPE ark_vault_goroutines gauge\nark_vault_goroutines %d\n\n", runtime.NumGoroutine())
			_, _ = fmt.Fprintf(w, "# HELP ark_vault_memory_alloc_bytes Heap alloc in bytes.\n# TYPE ark_vault_memory_alloc_bytes gauge\nark_vault_memory_alloc_bytes %d\n\n", mem.Alloc)
			_, _ = fmt.Fprintf(w, "# HELP ark_vault_updates_total Telegram updates processed.\n# TYPE ark_vault_updates_total counter\nark_vault_updates_total %d\n\n", m.UpdatesTotal)
			_, _ = fmt.Fprintf(w, "# HELP ark_vault_commands_total Slash commands processed.\n# TYPE ark_vault_commands_total counter\nark_vault_commands_total %d\n\n", m.CommandsTotal)
			_, _ = fmt.Fprintf(w, "# HELP ark_vault_callbacks_total Inline callbacks processed.\n# TYPE ark_vault_callbacks_total counter\nark_vault_callbacks_total %d\n\n", m.CallbacksTotal)
			_, _ = fmt.Fprintf(w, "# HELP ark_vault_rate_limited_total Requests blocked by rate limiting.\n# TYPE ark_vault_rate_limited_total counter\nark_vault_rate_limited_total %d\n\n", m.RateLimitedTotal)
			_, _ = fmt.Fprintf(w, "# HELP ark_vault_panics_total Panics recovered in handler.\n# TYPE ark_vault_panics_total counter\nark_vault_panics_total %d\n", m.PanicsTotal)
		})
		metricsSrv := &http.Server{
			Addr:              cfg.MetricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			logger.Info("metrics server listening", "addr", cfg.MetricsAddr)
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("metrics server error", "error", err)
			}
		}()
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = metricsSrv.Shutdown(shutdownCtx)
		}()
	}
	logger.Info("ark-vault bot starting", "version", version)
	if err := bot.Start(ctx); err != nil && err != context.Canceled {
		logger.Error("bot stopped with error", "error", err)
		os.Exit(1)
	}
	logger.Info("bot stopped gracefully")
}

func isDayOfWeek(t time.Time, day string) bool {
	days := map[string]time.Weekday{
		"sunday":    time.Sunday,
		"monday":    time.Monday,
		"tuesday":   time.Tuesday,
		"wednesday": time.Wednesday,
		"thursday":  time.Thursday,
		"friday":    time.Friday,
		"saturday":  time.Saturday,
	}
	wd, ok := days[day]
	if !ok {
		return false
	}
	return t.Weekday() == wd
}
