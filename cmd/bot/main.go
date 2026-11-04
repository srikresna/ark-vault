package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/arkcode369/ark-vault/internal/adapter/exporter"
	"github.com/arkcode369/ark-vault/internal/adapter/gemini"
	"github.com/arkcode369/ark-vault/internal/adapter/postgres"
	"github.com/arkcode369/ark-vault/internal/adapter/telegram"
	"github.com/arkcode369/ark-vault/internal/api"
	"github.com/arkcode369/ark-vault/internal/config"
	"github.com/arkcode369/ark-vault/internal/domain"
	"github.com/arkcode369/ark-vault/internal/scheduler"
	"github.com/arkcode369/ark-vault/internal/service"
	"github.com/arkcode369/ark-vault/pkg/timeutil"
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
	// Postgres is the single system of record: it replaced the Notion journal
	// and the BadgerDB gamification store.
	db, err := postgres.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		logger.Error("failed to open postgres", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	imageRepo, err := postgres.NewImageStore(cfg.ImageDir)
	if err != nil {
		logger.Error("failed to open image store", "error", err)
		os.Exit(1)
	}

	memberRepo := postgres.NewMemberRepo(db)
	tradeRepo := postgres.NewTradeRepo(db, memberRepo)

	// Services
	journalSvc := service.NewJournalService(tradeRepo, memberRepo, imageRepo)
	leaderboardSvc := service.NewLeaderboardService(tradeRepo, memberRepo)
	reportSvc := service.NewReportService(tradeRepo, memberRepo)
	gamRepo := postgres.NewGamificationRepo(db, memberRepo)
	gamSvc := service.NewGamificationService(gamRepo)

	// Badge & Challenge services
	badgeRepo := postgres.NewBadgeRepo(db, memberRepo)
	challengeRepo := postgres.NewChallengeRepo(db)
	badgeSvc := service.NewBadgeService(badgeRepo, tradeRepo, gamSvc)
	challengeSvc := service.NewChallengeService(challengeRepo, tradeRepo, memberRepo, gamSvc, badgeSvc)

	// Reminder & Goal services
	reminderRepo := postgres.NewReminderRepo(db, memberRepo)
	goalRepo := postgres.NewGoalRepo(db, memberRepo)
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
	analyticsCacheRepo := postgres.NewAnalyticsCacheRepo(db, memberRepo)
	reportCardRepo := postgres.NewReportCardRepo(db, memberRepo)
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
	// for manual verification. The ledger lives in Postgres and survives /reset.
	ledger := postgres.NewKV(db)
	bot.SetReplayGuard(telegram.ReplayStoreFuncs{
		GetFn: func(key string, dest interface{}) (bool, error) {
			return ledger.Get(context.Background(), key, dest)
		},
		SetFn: func(key string, value interface{}, ttl time.Duration) error {
			return ledger.SetWithTTL(context.Background(), key, value, ttl)
		},
		DeleteFn: func(key string) error {
			return ledger.Delete(context.Background(), key)
		},
		ScanFn: func(prefix string) ([][]byte, error) {
			return ledger.Scan(context.Background(), prefix)
		},
		ApplyBatchFn: func(insertIfAbsent, put map[string][]byte) ([]string, error) {
			return ledger.ApplyBatch(context.Background(), insertIfAbsent, put)
		},
	})
	handler.SetBot(bot)
	handler.SetResetter(postgres.NewResetter(db))

	// Web CRM API. It shares the Postgres store and the same services the bot
	// uses, so a trade recorded from the web goes through identical validation,
	// XP and badge logic.
	sessionRepo := postgres.NewSessionRepo(db)
	apiSrv := api.New(api.Config{
		TelegramToken:       cfg.TelegramToken,
		DiscordClientID:     cfg.DiscordClientID,
		DiscordClientSecret: cfg.DiscordClientSecret,
		DiscordRedirectURL:  cfg.DiscordRedirectURL,
		PublicBaseURL:       cfg.PublicBaseURL,
		CookieSecure:        strings.HasPrefix(cfg.PublicBaseURL, "https://"),
	}, api.Deps{
		Members:     memberRepo,
		Trades:      tradeRepo,
		Gam:         gamRepo,
		Badges:      badgeRepo,
		Sessions:    sessionRepo,
		Images:      imageRepo,
		Journal:     journalSvc,
		Leaderboard: leaderboardSvc,
		GamService:  gamSvc,
		PostTrade:   postTradeSvc,
		BadgeSvc:    badgeSvc,
		Logger:      logger,
	})
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

	// Daily Postgres backup (03:00 WIB, keeps 7 daily snapshots).
	//
	// The bot no longer owns its storage: Postgres does, and a logical dump is
	// the portable artifact. The dump runs inside the container through pg_dump
	// so no host-side client is required.
	sched.Add(scheduler.Job{
		Name:     "postgres-daily-backup",
		Interval: 1 * time.Hour,
		Run: func(ctx context.Context) error {
			now := time.Now().In(timeutil.WIB)
			if now.Hour() == 3 && sched.ShouldFire("postgres-daily-backup", now) {
				path, err := backupDatabase(ctx, cfg.DatabaseURL, cfg.ImageDir, now)
				if err != nil {
					logger.Error("postgres daily backup failed", "error", err)
					return err // unmarked → retried on the next tick/restart
				}
				sched.MarkFired("postgres-daily-backup", now)
				logger.Info("postgres daily backup completed", "path", path)
			}
			return nil
		},
	})

	// Daily gamification pruning (04:00 WIB): XP events have no consumer older
	// than a couple of months (daily cap = today, report cards = current and
	// previous month). Trade-XP grants are exempt: they record what each trade
	// actually earned and are still needed if that trade is edited or deleted
	// later; they go away with the trade itself.
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

	// Web API listener. It is a separate server from the metrics one so a
	// public origin never exposes /metrics, and the metrics port stays bound to
	// loopback in compose.
	if cfg.APIAddr != "" && cfg.APIAddr != "off" && cfg.APIAddr != "none" {
		apiSrvHTTP := &http.Server{
			Addr:              cfg.APIAddr,
			Handler:           apiSrv,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		go func() {
			logger.Info("api server listening", "addr", cfg.APIAddr)
			if err := apiSrvHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("api server error", "error", err)
			}
		}()
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = apiSrvHTTP.Shutdown(shutdownCtx)
		}()
	}

	// Stdlib-only Prometheus metrics & health server
	if cfg.MetricsAddr != "" && cfg.MetricsAddr != "off" && cfg.MetricsAddr != "none" {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			// Readiness, not liveness: a process that is alive but cannot reach
			// Postgres fails every journal write, and reporting healthy there
			// would let a deploy gate pass on a broken bot.
			pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer pingCancel()
			if err := db.Ping(pingCtx); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("database unavailable\n"))
				return
			}
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
