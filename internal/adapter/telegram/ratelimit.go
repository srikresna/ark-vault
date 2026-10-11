package telegram

import (
	"sync"
	"time"
)

// RateLimiter implements a per-user sliding window rate limiter.
type RateLimiter struct {
	mu       sync.Mutex
	limits   map[int64][]time.Time
	maxReqs  int
	window   time.Duration
	cleanTTL time.Duration
	done     chan struct{}
	stopOnce sync.Once
}

// NewRateLimiter creates a RateLimiter.
// maxReqs is the maximum requests allowed within the window duration.
func NewRateLimiter(maxReqs int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
		limits:   make(map[int64][]time.Time),
		maxReqs:  maxReqs,
		window:   window,
		cleanTTL: 10 * time.Minute,
		done:     make(chan struct{}),
	}
	go rl.cleanupLoop()
	return rl
}

// Stop terminates the background cleanup goroutine.
func (rl *RateLimiter) Stop() {
	if rl == nil {
		return
	}
	rl.stopOnce.Do(func() {
		close(rl.done)
	})
}

// Allow returns true if the user is within the rate limit.
func (rl *RateLimiter) Allow(userID int64) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Remove expired entries
	times := rl.limits[userID]
	valid := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}

	if len(valid) >= rl.maxReqs {
		rl.limits[userID] = valid
		return false
	}

	rl.limits[userID] = append(valid, now)
	return true
}

// Remaining returns how many requests the user has left in the current window.
func (rl *RateLimiter) Remaining(userID int64) int {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)
	count := 0
	for _, t := range rl.limits[userID] {
		if t.After(cutoff) {
			count++
		}
	}
	rem := rl.maxReqs - count
	if rem < 0 {
		return 0
	}
	return rem
}

func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(rl.cleanTTL)
	defer ticker.Stop()
	for {
		select {
		case <-rl.done:
			return
		case <-ticker.C:
			rl.mu.Lock()
			cutoff := time.Now().Add(-rl.window)
			for uid, times := range rl.limits {
				valid := times[:0]
				for _, t := range times {
					if t.After(cutoff) {
						valid = append(valid, t)
					}
				}
				if len(valid) == 0 {
					delete(rl.limits, uid)
				} else {
					rl.limits[uid] = valid
				}
			}
			rl.mu.Unlock()
		}
	}
}
