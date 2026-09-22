package jobs

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestJobLimiterCapsConcurrencyPerJob(t *testing.T) {
	limiter := newJobLimiter()
	jobID := uuid.New()

	var (
		inFlight atomic.Int32
		peak     atomic.Int32
		wg       sync.WaitGroup
	)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			release, err := limiter.Acquire(context.Background(), jobID, 3)
			if err != nil {
				t.Error(err)
				return
			}
			defer release()

			current := inFlight.Add(1)
			for {
				best := peak.Load()
				if current <= best || peak.CompareAndSwap(best, current) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > 3 {
		t.Fatalf("%d crawls ran at once, the job allows 3", got)
	}
}

func TestJobLimiterKeepsJobsIndependent(t *testing.T) {
	limiter := newJobLimiter()
	first, second := uuid.New(), uuid.New()

	// Fill the first job's only slot.
	release, err := limiter.Acquire(context.Background(), first, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	done := make(chan struct{})
	go func() {
		defer close(done)
		otherRelease, err := limiter.Acquire(context.Background(), second, 1)
		if err != nil {
			t.Error(err)
			return
		}
		otherRelease()
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a second job was blocked by the first job's limit")
	}
}

func TestJobLimiterForgetsFinishedJobs(t *testing.T) {
	limiter := newJobLimiter()
	jobID := uuid.New()

	release, err := limiter.Acquire(context.Background(), jobID, 2)
	if err != nil {
		t.Fatal(err)
	}
	release()
	// Releasing twice must not corrupt the bookkeeping.
	release()

	limiter.mu.Lock()
	remaining := len(limiter.entries)
	limiter.mu.Unlock()

	if remaining != 0 {
		t.Fatalf("%d limiter entries leaked after the job finished", remaining)
	}
}

func TestJobLimiterHonoursContextCancellation(t *testing.T) {
	limiter := newJobLimiter()
	jobID := uuid.New()

	release, err := limiter.Acquire(context.Background(), jobID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, err := limiter.Acquire(ctx, jobID, 1); err == nil {
		t.Fatal("Acquire should fail once the context is done")
	}
}

func TestClampInt32(t *testing.T) {
	if got := clampInt32(-5); got != 0 {
		t.Errorf("clampInt32(-5) = %d, want 0", got)
	}
	if got := clampInt32(30); got != 30 {
		t.Errorf("clampInt32(30) = %d", got)
	}
	if got := clampInt32(1 << 40); got != 1<<31-1 {
		t.Errorf("clampInt32 overflowed: %d", got)
	}
}
