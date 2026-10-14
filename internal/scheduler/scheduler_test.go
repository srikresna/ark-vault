package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestShouldFireMarkFired pins the claim-on-success API: ShouldFire only
// checks the dedup key; MarkFired records the fire. A job body that fails must
// NOT be marked, so the next tick in the same clock hour retries it.
func TestShouldFireMarkFired(t *testing.T) {
	s := NewScheduler(testLogger())

	t1 := time.Date(2026, 10, 11, 10, 15, 0, 0, time.UTC)
	t1SameHour := time.Date(2026, 10, 11, 10, 45, 0, 0, time.UTC)
	t2DiffHour := time.Date(2026, 10, 11, 11, 0, 0, 0, time.UTC)

	if !s.ShouldFire("jobA", t1) {
		t.Errorf("expected true on first call for jobA at %v", t1)
	}
	// Not marked yet: still fireable within the same hour.
	if !s.ShouldFire("jobA", t1SameHour) {
		t.Errorf("ShouldFire must stay true until MarkFired (checked at %v)", t1SameHour)
	}
	s.MarkFired("jobA", t1)
	if s.ShouldFire("jobA", t1SameHour) {
		t.Errorf("expected false after MarkFired in same hour (%v)", t1SameHour)
	}
	if !s.ShouldFire("jobA", t2DiffHour) {
		t.Errorf("expected true for jobA in new hour %v", t2DiffHour)
	}
	s.MarkFired("jobA", t2DiffHour)
	if s.ShouldFire("jobA", t2DiffHour) {
		t.Errorf("expected false after MarkFired in same hour %v", t2DiffHour)
	}
	// Different job is independent.
	if !s.ShouldFire("jobB", t1) {
		t.Errorf("expected true on first call for jobB at %v", t1)
	}
}

// TestShouldFire_RetryAfterFailedJob pins the transient-failure fix: when a
// weekly/monthly job body fails before MarkFired, the same clock hour must
// still be fireable so the next tick retries instead of skipping the event
// permanently.
func TestShouldFire_RetryAfterFailedJob(t *testing.T) {
	s := NewScheduler(testLogger())

	now := time.Date(2026, 10, 11, 20, 5, 0, 0, time.UTC)
	if !s.ShouldFire("weekly-report", now) {
		t.Fatal("first ShouldFire should be true")
	}
	// Simulate a failed job body: it never calls MarkFired.
	if !s.ShouldFire("weekly-report", now) {
		t.Fatal("a failed (unmarked) job must remain fireable within the same hour")
	}
	s.MarkFired("weekly-report", now)
	if s.ShouldFire("weekly-report", now) {
		t.Fatal("after a successful MarkFired, dedup must block the same hour")
	}
}

func TestStart(t *testing.T) {
	s := NewScheduler(testLogger())

	var runCount int64
	jobExecuted := make(chan struct{}, 10)

	s.Add(Job{
		Name:     "test-job",
		Interval: 10 * time.Millisecond,
		Run: func(_ context.Context) error {
			atomic.AddInt64(&runCount, 1)
			select {
			case jobExecuted <- struct{}{}:
			default:
			}
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan struct{})

	go func() {
		defer close(startDone)
		s.Start(ctx)
	}()

	// Wait for at least one job execution
	select {
	case <-jobExecuted:
		// Succeeded in running at least once
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for scheduled job execution")
	}

	// Cancel context and verify Start returns cleanly
	cancel()

	select {
	case <-startDone:
		// Start stopped cleanly
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for scheduler to stop after context cancellation")
	}

	if count := atomic.LoadInt64(&runCount); count < 1 {
		t.Errorf("expected runCount >= 1, got %d", count)
	}
}

// TestScheduler_RecoversFromJobPanic verifies that a panic inside one job is
// caught and logged rather than crashing the entire process, and subsequent
// ticks still run.
func TestScheduler_RecoversFromJobPanic(t *testing.T) {
	s := NewScheduler(testLogger())

	var calls int64
	secondTick := make(chan struct{}, 1)

	s.Add(Job{
		Name:     "panicking-job",
		Interval: 10 * time.Millisecond,
		Run: func(_ context.Context) error {
			n := atomic.AddInt64(&calls, 1)
			if n == 1 {
				panic("boom")
			}
			select {
			case secondTick <- struct{}{}:
			default:
			}
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Start(ctx)

	select {
	case <-secondTick:
	case <-time.After(1 * time.Second):
		t.Fatal("scheduler did not recover and continue after job panic")
	}
}
