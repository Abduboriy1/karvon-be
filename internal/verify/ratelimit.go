package verify

import (
	"context"
	"sync"
	"time"
)

// RateLimiter spaces outbound provider calls evenly. It is deliberately a spacing
// limiter rather than a bucket: vendors throttle on short-term rate, and an even
// stream is both kinder to them and easier to reason about than a burst.
//
// The limit is per process. Several worker replicas multiply it, exactly like the
// crawler's per-host limiter; the queue's MaxWorkers is the other half of the guard.
type RateLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
	now      func() time.Time
}

// NewRateLimiter builds a limiter allowing roughly rps calls per second. A
// non-positive rate disables limiting.
func NewRateLimiter(rps float64) *RateLimiter {
	limiter := &RateLimiter{now: time.Now}
	if rps > 0 {
		limiter.interval = time.Duration(float64(time.Second) / rps)
	}
	return limiter
}

// Wait blocks until the caller may make its request, or until ctx is done.
func (r *RateLimiter) Wait(ctx context.Context) error {
	if r == nil || r.interval <= 0 {
		return ctx.Err()
	}

	r.mu.Lock()
	now := r.now()
	if r.next.Before(now) {
		r.next = now
	}
	wait := r.next.Sub(now)
	r.next = r.next.Add(r.interval)
	r.mu.Unlock()

	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
