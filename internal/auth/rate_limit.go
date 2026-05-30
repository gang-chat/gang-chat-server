package auth

import (
	"sync"
	"time"
)

type FailedLogin struct {
	Count       int
	FirstFailed time.Time
}

type RateLimiter struct {
	mu       sync.Mutex
	attempts map[string]*FailedLogin
	max      int
	window   time.Duration
}

func NewRateLimiter(maxAttempts int, windowSeconds int64) *RateLimiter {
	return &RateLimiter{
		attempts: make(map[string]*FailedLogin),
		max:      maxAttempts,
		window:   time.Duration(windowSeconds) * time.Second,
	}
}

func (r *RateLimiter) Check(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	f, ok := r.attempts[key]
	if !ok {
		return true
	}
	if time.Since(f.FirstFailed) > r.window {
		delete(r.attempts, key)
		return true
	}
	return f.Count < r.max
}

func (r *RateLimiter) RecordFailure(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	f, ok := r.attempts[key]
	if !ok || time.Since(f.FirstFailed) > r.window {
		r.attempts[key] = &FailedLogin{Count: 1, FirstFailed: time.Now()}
		return
	}
	f.Count++
}

func (r *RateLimiter) Clear(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.attempts, key)
}
