package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/config"
	"github.com/bory/karvon-be/internal/scraper/provider/fake"
)

// asyncHarness wires the pipeline to a vendor whose runs are started, polled and
// drained rather than awaited, with the timings compressed so a test can watch it.
func asyncHarness(t *testing.T, perQuery int, tune func(*fake.AsyncProvider)) (*harness, *fake.AsyncProvider) {
	t.Helper()

	async := fake.NewAsync(perQuery)
	if tune != nil {
		tune(async)
	}
	h := newHarness(t, defaultPages(), withConfig(func(c *config.Config) {
		c.ProviderPollInterval = time.Second
		c.ProviderPageSize = 1000
	}))
	h.swapProvider(async)
	h.mustRequest(http.MethodPut, "/api/v1/sources/"+apifySourceID, `{"max_active_runs":2}`, http.StatusOK)
	return h, async
}

func TestAsyncRunIsStartedPolledAndDrained(t *testing.T) {
	h, async := asyncHarness(t, 4, func(a *fake.AsyncProvider) {
		// Two pages of two, so the paging and the resumable offset are real.
		a.PageSize = 2
		a.PollsBeforeDone = 2
	})

	h.mustRequest(http.MethodPut, "/api/v1/sources/"+apifySourceID,
		`{"api_key":"integration-provider-key","enabled":true,"cost_per_1k_cents":5000}`, http.StatusOK)

	created := h.createJob("Async gyms", []string{"gyms", "crossfit"}, []string{"Austin"}, false)
	job := h.waitForJob(created.ID)
	if job.Status != "done" {
		t.Fatalf("job finished as %q (%s)", job.Status, job.Error)
	}

	// One location, two terms, one run. Two runs here would be the same places
	// scraped and billed twice.
	if got := async.Starts(); got != 1 {
		t.Fatalf("the provider was asked to start %d runs, want exactly 1", got)
	}
	queries := async.Queries()
	if len(queries) != 1 || len(queries[0].TermList()) != 2 {
		t.Fatalf("run query = %+v, want one run carrying both terms", queries)
	}
	if job.Stats.ListingsFound != 4 {
		t.Errorf("listings_found = %d, want 4", job.Stats.ListingsFound)
	}
	if job.Stats.QueriesTotal != 1 || job.Stats.QueriesDone != 1 {
		t.Errorf("query counters = %+v, want one query, done", job.Stats)
	}
}

func TestAsyncRunSurvivesARestartWithoutStartingASecondRun(t *testing.T) {
	// The vendor keeps the run alive for many polls, which gives the test room to
	// interrupt the worker while a run is in flight.
	h, async := asyncHarness(t, 2, func(a *fake.AsyncProvider) { a.PollsBeforeDone = 3 })

	h.mustRequest(http.MethodPut, "/api/v1/sources/"+apifySourceID,
		`{"api_key":"integration-provider-key","enabled":true}`, http.StatusOK)

	created := h.createJob("Async restart", []string{"gyms"}, []string{"Austin"}, false)
	job := h.waitForJob(created.ID)

	if job.Status != "done" {
		t.Fatalf("job finished as %q (%s)", job.Status, job.Error)
	}
	// Every poll re-enters the worker. A worker that treated a re-entry as new work
	// would start a run per poll.
	if got := async.Starts(); got != 1 {
		t.Fatalf("the provider was asked to start %d runs across the whole job, want 1", got)
	}
}

func TestAsyncRunThatEndsBadlyIsResumedOnce(t *testing.T) {
	h, async := asyncHarness(t, 3, func(a *fake.AsyncProvider) {
		a.FailWith = "TIMED-OUT"
		a.PollsBeforeDone = 1
	})

	h.mustRequest(http.MethodPut, "/api/v1/sources/"+apifySourceID,
		`{"api_key":"integration-provider-key","enabled":true}`, http.StatusOK)

	created := h.createJob("Async resume", []string{"gyms"}, []string{"Austin"}, false)
	job := h.waitForJob(created.ID)

	if job.Status != "done" {
		t.Fatalf("job finished as %q (%s)", job.Status, job.Error)
	}
	// Resuming continues into the same dataset; restarting would pay again for the
	// places the run already collected.
	if got := async.Resurrects(); got != 1 {
		t.Errorf("resurrects = %d, want 1", got)
	}
	if got := async.Starts(); got != 1 {
		t.Errorf("starts = %d, want 1: a stalled run is resumed, never restarted", got)
	}
}

func TestConcurrentRunsAreCappedAtTheAccountLimit(t *testing.T) {
	h, async := asyncHarness(t, 1, func(a *fake.AsyncProvider) { a.PollsBeforeDone = 2 })

	h.mustRequest(http.MethodPut, "/api/v1/sources/"+apifySourceID,
		`{"api_key":"integration-provider-key","enabled":true}`, http.StatusOK)

	// Six locations against a two-run cap: the rest wait for a slot rather than
	// piling onto the vendor account all at once.
	cities := []string{"Austin", "Dallas", "Houston", "El Paso", "Waco", "Lubbock"}
	created := h.createJob("Async many states", []string{"gyms"}, cities, false)

	deadline := time.Now().Add(20 * time.Second)
	peak := 0
	for time.Now().Before(deadline) {
		live := h.countLiveRuns(created.ID)
		if live > peak {
			peak = live
		}
		if live == 0 && peak > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	job := h.waitForJob(created.ID)
	if job.Status != "done" {
		t.Fatalf("job finished as %q (%s)", job.Status, job.Error)
	}
	if peak > 2 {
		t.Errorf("%d runs were in flight at once, want at most the configured 2", peak)
	}
	if got := async.Starts(); got != len(cities) {
		t.Errorf("starts = %d, want one per location (%d)", got, len(cities))
	}
	if job.Stats.QueriesDone != len(cities) {
		t.Errorf("queries_done = %d, want %d", job.Stats.QueriesDone, len(cities))
	}
}

// countLiveRuns reports how many of a job's queries hold a vendor run right now.
func (h *harness) countLiveRuns(jobID string) int {
	h.t.Helper()

	var live int
	err := h.app.Store().Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM job_queries
		 WHERE job_id = $1::uuid AND run_state IN ('starting', 'polling', 'ingesting')`,
		jobID).Scan(&live)
	if err != nil {
		h.t.Fatalf("could not count live runs: %v", err)
	}
	return live
}

func TestALargeDatasetIsDrainedAcrossInvocations(t *testing.T) {
	// More pages than one invocation drains, so the resumable offset is what carries
	// the work across the hand-backs.
	h, async := asyncHarness(t, 60, func(a *fake.AsyncProvider) { a.PageSize = 2 })

	h.mustRequest(http.MethodPut, "/api/v1/sources/"+apifySourceID,
		`{"api_key":"integration-provider-key","enabled":true}`, http.StatusOK)

	created := h.createJob("Async big dataset", []string{"gyms"}, []string{"Austin"}, false)
	job := h.waitForJob(created.ID)

	if job.Status != "done" {
		t.Fatalf("job finished as %q (%s)", job.Status, job.Error)
	}
	if job.Stats.ListingsFound != 60 {
		t.Errorf("listings_found = %d, want every one of the 60 places", job.Stats.ListingsFound)
	}
	if got := async.Starts(); got != 1 {
		t.Errorf("starts = %d, want 1: resuming a drain must not restart the run", got)
	}
}
