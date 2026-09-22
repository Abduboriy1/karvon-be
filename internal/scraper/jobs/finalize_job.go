package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/events"
	"github.com/bory/karvon-be/internal/scraper"
)

// failureRatioDenominator expresses the "more than half the queries failed" rule.
const failureRatioDenominator = 2

// FinalizeWorker is stage 4: recompute the stats from the tables (rather than trusting
// the incremental counters) and close the job out.
type FinalizeWorker struct {
	river.WorkerDefaults[scraper.FinalizeArgs]
	deps *Deps
}

// NewFinalizeWorker builds the stage 4 worker.
func NewFinalizeWorker(deps *Deps) *FinalizeWorker { return &FinalizeWorker{deps: deps} }

// Work implements river.Worker.
func (w *FinalizeWorker) Work(ctx context.Context, rj *river.Job[scraper.FinalizeArgs]) error {
	d := w.deps
	jobID := rj.Args.JobID

	row, err := d.Store.GetJob(ctx, jobID)
	if err != nil {
		// A job deleted while its workers were still running has nothing left to
		// finalize; retrying would never succeed.
		return nil //nolint:nilerr // the job is gone, stop cleanly
	}

	computed, err := d.Store.RecomputeJobStats(ctx, jobID)
	if err != nil {
		return fmt.Errorf("jobs: recompute stats: %w", err)
	}
	current, err := d.stats(ctx, jobID)
	if err != nil {
		return err
	}

	listings := int(computed.ListingsFound)
	if computed.QueriesTotal == 0 {
		// A re-crawl job ran no queries: its listings were copied in at creation,
		// so the counter written then is the truth.
		listings = current.ListingsFound
	}
	// Every listing was paid for; the ones that landed on a business the job already
	// had are the ones that were paid for twice. Areas that do not overlap should
	// produce almost none, so this is the number that says whether the locations in
	// this job were really disjoint.
	unique, err := d.Store.CountJobResults(ctx, jobID)
	if err != nil {
		return fmt.Errorf("jobs: count job results: %w", err)
	}
	duplicates := listings - int(unique)
	if duplicates < 0 {
		duplicates = 0
	}

	final := scraper.Stats{
		QueriesTotal:  int(computed.QueriesTotal),
		QueriesDone:   int(computed.QueriesDone),
		QueriesFailed: int(computed.QueriesFailed),
		ListingsFound: listings,
		SitesTotal:    int(computed.SitesTotal),
		SitesCrawled:  int(computed.SitesCrawled),
		EmailsFound:   int(computed.EmailsFound),
		Duplicates:    duplicates,
		// Spend is only knowable from the provider price at run time, so it is
		// carried over rather than recomputed.
		CostCents: current.CostCents,
	}
	if err := d.saveStats(ctx, jobID, final); err != nil {
		return err
	}

	// A job cancelled or failed earlier keeps that status; only an active job is closed.
	if row.Status != scraper.StatusQueued && row.Status != scraper.StatusRunning {
		d.publishProgress(ctx, jobID)
		return nil
	}

	status := scraper.StatusDone
	var failureMsg *string
	if final.QueriesTotal > 0 && final.QueriesFailed*failureRatioDenominator > final.QueriesTotal {
		status = scraper.StatusFailed
		msg := fmt.Sprintf("%d of %d queries failed", final.QueriesFailed, final.QueriesTotal)
		failureMsg = &msg
	}

	if err := d.Store.MarkJobTerminal(ctx, dbgen.MarkJobTerminalParams{
		ID:     jobID,
		Status: status,
		Error:  failureMsg,
	}); err != nil {
		return fmt.Errorf("jobs: mark job terminal: %w", err)
	}

	d.publishProgress(ctx, jobID)
	finishedAt := time.Now().UTC()
	if err := d.Publisher.Status(ctx, jobID, events.Status{
		Status:     status,
		Error:      failureMsg,
		FinishedAt: &finishedAt,
	}); err != nil {
		d.Log.Warn("could not publish terminal status", "job_id", jobID, "error", err)
	}
	d.logLine(ctx, jobID, events.LevelInfo, "job %s: %d listings, %d unique businesses, %d duplicates, %d emails",
		status, final.ListingsFound, unique, duplicates, final.EmailsFound)
	return nil
}
