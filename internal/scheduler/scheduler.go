package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Job represents a scheduled task.
type Job struct {
	Name     string
	Interval time.Duration
	Run      func(ctx context.Context) error
}

// Scheduler runs jobs at fixed intervals.
// Dedup is per-process and in-memory: within a running process each job fires
// at most once per clock-hour, but a restart within an already-fired hour may
// re-fire that hour's job — job bodies must tolerate that (e.g. idempotency
// flags), never assume cross-restart exactly-once delivery.
type Scheduler struct {
	jobs      []Job
	logger    *slog.Logger
	mu        sync.Mutex
	lastFired map[string]string // job name -> "2006-01-02T15" key
}

// NewScheduler creates a Scheduler.
func NewScheduler(logger *slog.Logger) *Scheduler {
	return &Scheduler{
		logger:    logger,
		lastFired: make(map[string]string),
	}
}

// Add registers a new job.
func (s *Scheduler) Add(job Job) {
	s.jobs = append(s.jobs, job)
}

// Start runs all registered jobs and blocks until ctx is cancelled and all
// running job goroutines have exited.
func (s *Scheduler) Start(ctx context.Context) {
	var wg sync.WaitGroup
	for _, job := range s.jobs {
		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			s.runJob(ctx, j)
		}(job)
	}
	<-ctx.Done()
	wg.Wait()
}

// hourKey returns a dedup key for the current clock-hour.
func hourKey(t time.Time) string {
	return fmt.Sprintf("%d-%02d-%02dT%02d", t.Year(), t.Month(), t.Day(), t.Hour())
}

// ShouldFire reports whether the job's dedup key allows a fire at time t,
// WITHOUT marking it. Pair with MarkFired after the job body succeeds so a
// transient failure (Notion 5xx, Telegram timeout) is retried by the next
// tick instead of permanently consuming the occurrence. Each job runs
// sequentially in its own goroutine, so check→work→mark cannot double-fire
// within a process.
func (s *Scheduler) ShouldFire(jobName string, t time.Time) bool {
	key := hourKey(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastFired[jobName] != key
}

// MarkFired records the job as fired for the clock-hour of t.
func (s *Scheduler) MarkFired(jobName string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastFired[jobName] = hourKey(t)
}

func (s *Scheduler) runJob(ctx context.Context, job Job) {
	s.logger.Info("scheduler: job registered", "name", job.Name, "interval", job.Interval)

	// Run once immediately at startup. The hourly ticker below fires on the
	// process-start anniversary of each hour, so without this a job whose
	// trigger hour is between start and the first tick would be missed for a
	// full hour (e.g. the 20:00 reminder when the bot starts at 20:30).
	// ShouldFire/MarkFired still deduplicate within the clock-hour; the only
	// cost is that a restart within an already-fired hour may re-fire that
	// hour's job.
	if ctx.Err() == nil {
		s.logger.Info("scheduler: running job", "name", job.Name, "reason", "startup")
		s.executeJobSafely(ctx, job)
	}

	ticker := time.NewTicker(job.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("scheduler: job stopped", "name", job.Name)
			return
		case <-ticker.C:
			s.logger.Info("scheduler: running job", "name", job.Name)
			s.executeJobSafely(ctx, job)
		}
	}
}

func (s *Scheduler) executeJobSafely(ctx context.Context, job Job) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("scheduler: job panicked",
				"name", job.Name,
				"panic", r,
				"stack", string(debug.Stack()),
			)
		}
	}()
	if err := job.Run(ctx); err != nil {
		s.logger.Error("scheduler: job failed", "name", job.Name, "error", err)
	}
}
