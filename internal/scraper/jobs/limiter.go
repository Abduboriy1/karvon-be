package jobs

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"
)

// jobLimiter caps how many websites one job crawls at the same time, which is what a
// job's `concurrency` setting means.
//
// The cap is per process. A deployment running several worker replicas multiplies it,
// exactly like the per-host rate limiter; the hard protection against hammering a site
// is the per-host limiter in the crawler, not this.
type jobLimiter struct {
	mu      sync.Mutex
	entries map[uuid.UUID]*limiterEntry
}

type limiterEntry struct {
	sem  *semaphore.Weighted
	refs int
}

func newJobLimiter() *jobLimiter {
	return &jobLimiter{entries: make(map[uuid.UUID]*limiterEntry)}
}

// Acquire blocks until this job has a free slot. The returned function must be called
// to give the slot back.
func (l *jobLimiter) Acquire(ctx context.Context, jobID uuid.UUID, limit int) (func(), error) {
	if limit < 1 {
		limit = 1
	}

	l.mu.Lock()
	entry, ok := l.entries[jobID]
	if !ok {
		entry = &limiterEntry{sem: semaphore.NewWeighted(int64(limit))}
		l.entries[jobID] = entry
	}
	entry.refs++
	l.mu.Unlock()

	if err := entry.sem.Acquire(ctx, 1); err != nil {
		l.drop(jobID)
		return nil, err
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			entry.sem.Release(1)
			l.drop(jobID)
		})
	}, nil
}

// drop forgets a job's semaphore once nothing is using it, so a long-lived worker
// process does not accumulate one entry per job it has ever run.
func (l *jobLimiter) drop(jobID uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.entries[jobID]
	if !ok {
		return
	}
	entry.refs--
	if entry.refs <= 0 {
		delete(l.entries, jobID)
	}
}
