package jobs

import (
	"context"
	"fmt"

	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/events"
	"github.com/bory/karvon-be/internal/scraper"
)

// RecrawlWorker is the entry stage of a re-crawl job. The job already owns its
// businesses (copied from the job it was derived from), so there is nothing to
// search: the worker marks the job running and hands straight over to the crawl
// stage, which queues one CrawlJob per website that still has no address.
type RecrawlWorker struct {
	river.WorkerDefaults[scraper.RecrawlArgs]
	deps *Deps
}

// NewRecrawlWorker builds the re-crawl entry worker.
func NewRecrawlWorker(deps *Deps) *RecrawlWorker { return &RecrawlWorker{deps: deps} }

// Work implements river.Worker.
func (w *RecrawlWorker) Work(ctx context.Context, rj *river.Job[scraper.RecrawlArgs]) error {
	d := w.deps
	jobID := rj.Args.JobID

	row, active, err := d.jobActive(ctx, jobID)
	if err != nil {
		return err
	}
	if !active {
		d.Log.Info("skipping re-crawl for inactive job", "job_id", jobID, "status", row.Status)
		return nil
	}

	cfg, err := decodeConfig(row.Config)
	if err != nil {
		d.failJob(ctx, jobID, "job configuration could not be read")
		return nil //nolint:nilerr // the job is already marked failed
	}
	// The row was written by Service.Recrawl, which always sets both; guard anyway
	// so a hand-edited config cannot silently skip the crawl stage.
	cfg.CrawlEmails = true

	if err := d.Store.MarkJobRunning(ctx, jobID); err != nil {
		return fmt.Errorf("jobs: mark running: %w", err)
	}
	if err := d.Publisher.Status(ctx, jobID, events.Status{Status: scraper.StatusRunning}); err != nil {
		d.Log.Warn("could not publish running event", "job_id", jobID, "error", err)
	}

	results, err := d.Store.CountJobResults(ctx, jobID)
	if err != nil {
		return fmt.Errorf("jobs: count job results: %w", err)
	}
	if cfg.RecrawlOf != nil {
		d.logLine(ctx, jobID, events.LevelInfo,
			"re-crawling the websites of %d business(es) from job %s; no provider search is run",
			results, cfg.RecrawlOf)
	} else {
		d.logLine(ctx, jobID, events.LevelInfo,
			"re-crawling the websites of %d business(es); no provider search is run", results)
	}

	// No queries exist for this job, so the query stage counts as complete and the
	// shared advance logic queues the crawls (or finalizes when nothing is left).
	return d.advanceAfterQueries(ctx, jobID, cfg)
}
