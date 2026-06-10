package gdmusic

import (
	"context"
	"sync"
	"time"
)

// rateLimiter is a simple sliding-window limiter: at most `limit` events may
// occur within any `window`. wait blocks until a slot is free or ctx is done.
//
// The GD Studio docs publish a quota of 50 requests per 5 minutes; this keeps
// us inside it without coordinating across processes (single-instance server).
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	events []time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window}
}

func (r *rateLimiter) wait(ctx context.Context) error {
	for {
		r.mu.Lock()
		now := time.Now()
		r.prune(now)
		if len(r.events) < r.limit {
			r.events = append(r.events, now)
			r.mu.Unlock()
			return nil
		}
		// Sleep until the oldest event leaves the window.
		sleep := r.window - now.Sub(r.events[0])
		r.mu.Unlock()
		if sleep <= 0 {
			continue
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *rateLimiter) prune(now time.Time) {
	cutoff := now.Add(-r.window)
	i := 0
	for i < len(r.events) && !r.events[i].After(cutoff) {
		i++
	}
	if i > 0 {
		r.events = append(r.events[:0], r.events[i:]...)
	}
}
