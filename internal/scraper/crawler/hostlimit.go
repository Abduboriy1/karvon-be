package crawler

import (
	"context"
	"sync"
	"time"
)

// HostLimiter enforces a minimum interval between requests to the same host. It is
// the mechanism behind the "never more than 1 request per second per host" rule.
type HostLimiter struct {
	interval time.Duration

	mu    sync.Mutex
	hosts map[string]*hostGate
}

type hostGate struct {
	mu   sync.Mutex
	next time.Time
}

// NewHostLimiter builds a limiter with the given minimum spacing.
func NewHostLimiter(interval time.Duration) *HostLimiter {
	if interval <= 0 {
		interval = time.Second
	}
	return &HostLimiter{interval: interval, hosts: make(map[string]*hostGate)}
}

// Wait blocks until it is this caller's turn to hit host, or until ctx is done.
// Waiters for the same host are serialized, so spacing holds under concurrency.
func (l *HostLimiter) Wait(ctx context.Context, host string) error {
	l.mu.Lock()
	gate, ok := l.hosts[host]
	if !ok {
		gate = &hostGate{}
		l.hosts[host] = gate
	}
	l.mu.Unlock()

	gate.mu.Lock()
	defer gate.mu.Unlock()

	if delay := time.Until(gate.next); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	gate.next = time.Now().Add(l.interval)
	return nil
}
