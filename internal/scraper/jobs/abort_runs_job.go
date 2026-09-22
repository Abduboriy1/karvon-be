package jobs

import (
	"context"
	"fmt"

	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/events"
	"github.com/bory/karvon-be/internal/scraper"
	"github.com/bory/karvon-be/internal/scraper/provider"
)

// AbortRunsWorker stops the vendor-side runs a cancelled or failed job left behind.
//
// Cancelling a job stops this service from doing more work, but a run already started
// at the vendor keeps scraping and keeps charging for every place it finds. Nothing
// reads those places any more, so without this the cancellation buys nothing and costs
// full price.
type AbortRunsWorker struct {
	river.WorkerDefaults[scraper.AbortRunsArgs]
	deps *Deps
}

// NewAbortRunsWorker builds the cleanup worker.
func NewAbortRunsWorker(deps *Deps) *AbortRunsWorker { return &AbortRunsWorker{deps: deps} }

// Work implements river.Worker.
func (w *AbortRunsWorker) Work(ctx context.Context, rj *river.Job[scraper.AbortRunsArgs]) error {
	d := w.deps
	jobID := rj.Args.JobID

	row, err := d.Store.GetJob(ctx, jobID)
	if err != nil {
		// The job is gone, so nothing records what its runs were; there is nothing
		// left to act on.
		return nil //nolint:nilerr // no job, no runs to stop
	}

	queries, err := d.Store.ListJobQueries(ctx, jobID)
	if err != nil {
		return fmt.Errorf("jobs: list queries for abort: %w", err)
	}

	live := make([]dbgen.JobQuery, 0, len(queries))
	for _, q := range queries {
		if q.ProviderRunID != nil && *q.ProviderRunID != "" && q.RunState != scraper.RunStateNone {
			live = append(live, q)
		}
	}
	if len(live) == 0 {
		return nil
	}

	source, err := d.Store.GetSource(ctx, row.SourceID)
	if err != nil {
		return fmt.Errorf("jobs: load source for abort: %w", err)
	}
	prov, err := d.Providers.For(ctx, source)
	if err != nil {
		d.Log.Warn("cannot reach the provider to stop its runs", "job_id", jobID, "error", err)
		return nil //nolint:nilerr // retrying with a broken provider config changes nothing
	}
	async, ok := prov.(provider.AsyncProvider)
	if !ok {
		return nil
	}

	stopped := 0
	for _, q := range live {
		runID := *q.ProviderRunID

		// Ask before acting: a run that already finished must not be reported as
		// something this cancellation saved.
		state, err := async.RunState(ctx, runID)
		if err != nil {
			return fmt.Errorf("jobs: read run %s before aborting: %w", runID, err)
		}
		if state.Terminal {
			continue
		}
		if err := async.AbortRun(ctx, runID); err != nil {
			return fmt.Errorf("jobs: abort run %s: %w", runID, err)
		}
		stopped++

		if err := d.Store.SetProviderRunStatus(ctx, dbgen.SetProviderRunStatusParams{
			ID:        q.ID,
			RunState:  scraper.RunStateFinished,
			RunStatus: strPtr(statusAbortedSelf),
		}); err != nil {
			return fmt.Errorf("jobs: record aborted run: %w", err)
		}
	}

	if stopped > 0 {
		d.logLine(ctx, jobID, events.LevelWarn,
			"stopped %d provider run(s) still spending after the job ended", stopped)
	}
	return nil
}
