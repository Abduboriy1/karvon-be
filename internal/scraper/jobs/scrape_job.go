package jobs

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/events"
	"github.com/bory/karvon-be/internal/ids"
	"github.com/bory/karvon-be/internal/scraper"
)

// ScrapeWorker is stage 1: expand the job config into job_queries and fan out one
// QueryJob per row.
type ScrapeWorker struct {
	river.WorkerDefaults[scraper.ScrapeArgs]
	deps *Deps
}

// NewScrapeWorker builds the stage 1 worker.
func NewScrapeWorker(deps *Deps) *ScrapeWorker { return &ScrapeWorker{deps: deps} }

// Work implements river.Worker.
func (w *ScrapeWorker) Work(ctx context.Context, rj *river.Job[scraper.ScrapeArgs]) error {
	d := w.deps
	jobID := rj.Args.JobID

	row, active, err := d.jobActive(ctx, jobID)
	if err != nil {
		return err
	}
	if !active {
		d.Log.Info("skipping scrape for inactive job", "job_id", jobID, "status", row.Status)
		return nil
	}

	cfg, err := decodeConfig(row.Config)
	if err != nil {
		// The stored config is immutable, so a retry would decode the same bytes.
		d.failJob(ctx, jobID, "job configuration could not be read")
		return nil //nolint:nilerr // the job is already marked failed
	}

	if err := d.Store.MarkJobRunning(ctx, jobID); err != nil {
		return fmt.Errorf("jobs: mark running: %w", err)
	}
	if err := d.Publisher.Status(ctx, jobID, events.Status{Status: scraper.StatusRunning}); err != nil {
		d.Log.Warn("could not publish running event", "job_id", jobID, "error", err)
	}

	expanded := cfg.Queries()
	if len(expanded) == 0 {
		d.failJob(ctx, jobID, "job has no queries to run")
		return nil
	}

	inserts := make([]river.InsertManyParams, 0, len(expanded))
	for _, q := range expanded {
		queryRow, err := d.Store.CreateJobQuery(ctx, dbgen.CreateJobQueryParams{
			ID:    ids.New(),
			JobID: jobID,
			Term:  q.Label(),
			Terms: q.Terms,
			City:  q.City,
			State: q.State,
		})
		if err != nil {
			return fmt.Errorf("jobs: create job query: %w", err)
		}
		inserts = append(inserts, river.InsertManyParams{
			Args: scraper.QueryArgs{JobID: jobID, QueryID: queryRow.ID},
		})
	}

	// queries_total is authoritative from here on; the estimate written at creation
	// time may differ if the config contained duplicates.
	if err := d.setQueriesTotal(ctx, jobID, len(inserts)); err != nil {
		return err
	}

	if _, err := d.Queue.InsertMany(ctx, inserts); err != nil {
		return fmt.Errorf("jobs: enqueue queries: %w", err)
	}

	// One query per location, carrying every term: a place matched by two terms is
	// then inside one provider run and paid for once.
	d.logLine(ctx, jobID, events.LevelInfo, "expanded %d locations into %d queries, %d terms each",
		len(cfg.Locations), len(inserts), len(cfg.Terms))
	d.publishProgress(ctx, jobID)
	return nil
}

// setQueriesTotal rewrites just the queries_total counter, leaving the others intact.
func (d *Deps) setQueriesTotal(ctx context.Context, jobID uuid.UUID, total int) error {
	st, err := d.stats(ctx, jobID)
	if err != nil {
		return err
	}
	st.QueriesTotal = total
	return d.saveStats(ctx, jobID, st)
}
