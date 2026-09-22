package crawler

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestHostLimiterSpacesRequestsToTheSameHost(t *testing.T) {
	const interval = 40 * time.Millisecond
	limiter := NewHostLimiter(interval)
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := limiter.Wait(ctx, "gym.com"); err != nil {
			t.Fatal(err)
		}
	}
	// The first call is free; the next two each wait one interval.
	if elapsed := time.Since(start); elapsed < 2*interval {
		t.Fatalf("three requests took %s, want at least %s", elapsed, 2*interval)
	}
}

func TestHostLimiterDoesNotBlockDifferentHosts(t *testing.T) {
	limiter := NewHostLimiter(500 * time.Millisecond)
	ctx := context.Background()

	start := time.Now()
	var wg sync.WaitGroup
	for _, host := range []string{"a.com", "b.com", "c.com", "d.com"} {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			if err := limiter.Wait(ctx, host); err != nil {
				t.Error(err)
			}
		}(host)
	}
	wg.Wait()

	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("different hosts blocked each other: took %s", elapsed)
	}
}

func TestHostLimiterHonoursContextCancellation(t *testing.T) {
	limiter := NewHostLimiter(2 * time.Second)
	ctx := context.Background()
	if err := limiter.Wait(ctx, "gym.com"); err != nil {
		t.Fatal(err)
	}

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := limiter.Wait(cancelCtx, "gym.com"); err == nil {
		t.Fatal("Wait should return the context error instead of sleeping")
	}
}

func TestHostLimiterSerializesConcurrentWaitersForOneHost(t *testing.T) {
	const interval = 30 * time.Millisecond
	limiter := NewHostLimiter(interval)

	var (
		mu     sync.Mutex
		stamps []time.Time
		wg     sync.WaitGroup
	)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := limiter.Wait(context.Background(), "gym.com"); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			stamps = append(stamps, time.Now())
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(stamps) != 4 {
		t.Fatalf("got %d stamps", len(stamps))
	}
	// Sort and verify the spacing rule held despite concurrency.
	for i := 0; i < len(stamps); i++ {
		for j := i + 1; j < len(stamps); j++ {
			if stamps[j].Before(stamps[i]) {
				stamps[i], stamps[j] = stamps[j], stamps[i]
			}
		}
	}
	for i := 1; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap < interval/2 {
			t.Fatalf("requests %d and %d were only %s apart", i-1, i, gap)
		}
	}
}
