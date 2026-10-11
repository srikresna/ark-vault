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

func TestMarkAndCheck(t *testing.T) {
	s := NewScheduler(testLogger())

	t1 := time.Date(2026, 10, 11, 10, 15, 0, 0, time.UTC)
	t1SameHour := time.Date(2026, 10, 11, 10, 45, 0, 0, time.UTC)
	t2DiffHour := time.Date(2026, 10, 11, 11, 0, 0, 0, time.UTC)

	// First call for jobA in hour 10 should return true
	if !s.MarkAndCheck("jobA", t1) {
		t.Errorf("expected true on first call for jobA at %v", t1)
	}

	// Second call for jobA in same hour 10 should return false
	if s.MarkAndCheck("jobA", t1SameHour) {
		t.Errorf("expected false on second call for jobA at %v", t1SameHour)
	}

	// First call for different job (jobB) in hour 10 should return true
	if !s.MarkAndCheck("jobB", t1) {
		t.Errorf("expected true on first call for jobB at %v", t1)
	}

	// First call for jobA in different hour (11) should return true
	if !s.MarkAndCheck("jobA", t2DiffHour) {
		t.Errorf("expected true on first call for jobA in new hour %v", t2DiffHour)
	}

	// Second call for jobA in hour 11 should return false
	if s.MarkAndCheck("jobA", t2DiffHour) {
		t.Errorf("expected false on second call for jobA in same hour %v", t2DiffHour)
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
