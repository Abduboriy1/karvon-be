package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/events"
	"github.com/bory/karvon-be/internal/scraper"
	"github.com/bory/karvon-be/internal/scraper/provider"
)

// QueryWorker is stage 2: one provider search per location, covering every term.
//
// Which of the two paths below runs depends on the vendor. A vendor that answers
// inside one HTTP request is called and drained on the spot. A vendor whose runs last
// hours is started, then polled across many short invocations of this same worker, so
// nothing holds a worker slot or an HTTP connection open while it waits.
type QueryWorker struct {
	river.WorkerDefaults[scraper.QueryArgs]
	deps *Deps
}

// NewQueryWorker builds the stage 2 worker.
func NewQueryWorker(deps *Deps) *QueryWorker { return &QueryWorker{deps: deps} }

// Timeout implements river.Worker: provider runs are slow, so this stage gets its own
// budget rather than the default.
func (w *QueryWorker) Timeout(*river.Job[scraper.QueryArgs]) time.Duration {
	return w.deps.Config.ProviderTimeout + time.Minute
}

// queryRun is everything one invocation needs, resolved once.
type queryRun struct {
	jobID   uuid.UUID
	queryID uuid.UUID
	row     dbgen.JobQuery
	cfg     scraper.Config
	source  dbgen.Source
	prov    provider.Provider
}

// searchQuery renders the row as provider input.
func (q queryRun) searchQuery() provider.SearchQuery {
	terms := q.row.Terms
	if len(terms) == 0 {
		terms = []string{q.row.Term}
	}
	return provider.SearchQuery{
		Term:  terms[0],
		Terms: terms,
		City:  q.row.City,
		State: q.row.State,
		Max:   q.cfg.MaxPerQuery,
	}
}

func (q queryRun) label() string { return locationLabel(q.row.City, q.row.State) }

// Work implements river.Worker.
func (w *QueryWorker) Work(ctx context.Context, rj *river.Job[scraper.QueryArgs]) error {
	d := w.deps
	jobID, queryID := rj.Args.JobID, rj.Args.QueryID

	jobRow, active, err := d.jobActive(ctx, jobID)
	if err != nil {
		return err
	}
	if !active {
		return nil
	}

	queryRow, err := d.Store.GetJobQuery(ctx, queryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("jobs: load query: %w", err)
	}
	switch queryRow.Status {
	case scraper.StatusDone, scraper.StatusFailed, scraper.StatusCancelled:
		return nil
	}

	cfg, err := decodeConfig(jobRow.Config)
	if err != nil {
		d.failJob(ctx, jobID, "job configuration could not be read")
		return nil //nolint:nilerr // the job is already marked failed
	}

	source, err := d.Store.GetSource(ctx, jobRow.SourceID)
	if err != nil {
		return fmt.Errorf("jobs: load source: %w", err)
	}
	prov, err := d.Providers.For(ctx, source)
	if err != nil {
		// A misconfigured provider will not fix itself between retries.
		d.failJob(ctx, jobID, "provider is not usable: "+err.Error())
		return nil //nolint:nilerr // the job is already marked failed
	}

	run := queryRun{jobID: jobID, queryID: queryID, row: queryRow, cfg: cfg, source: source, prov: prov}
	if async, ok := prov.(provider.AsyncProvider); ok {
		return w.workAsync(ctx, rj, run, async)
	}
	return w.workSync(ctx, rj, run)
}

// workSync is the single-request path: call the vendor, ingest what comes back.
func (w *QueryWorker) workSync(ctx context.Context, rj *river.Job[scraper.QueryArgs], run queryRun) error {
	d := w.deps

	if err := d.Store.MarkJobQueryRunning(ctx, run.queryID); err != nil {
		return fmt.Errorf("jobs: mark query running: %w", err)
	}

	searchCtx, cancel := context.WithTimeout(ctx, d.Config.ProviderTimeout)
	defer cancel()

	listings, err := run.prov.Search(searchCtx, run.searchQuery())
	if err != nil {
		return w.handleSearchError(ctx, rj, run, err)
	}

	// The job may have been cancelled while the provider call was in flight.
	if _, stillActive, err := d.jobActive(ctx, run.jobID); err != nil {
		return err
	} else if !stillActive {
		return nil
	}

	var runID *string
	listingsFound := 0
	for i := range listings {
		if err := ctx.Err(); err != nil {
			return err
		}
		if listings[i].RunID != "" && runID == nil {
			id := listings[i].RunID
			runID = &id
		}
		if _, err := d.Ingestor.IngestListing(ctx, run.jobID, run.queryID, listings[i]); err != nil {
			return fmt.Errorf("jobs: ingest listing: %w", err)
		}
		listingsFound++
	}

	cost := scraper.ActualCostCents(listingsFound, int(run.source.CostPer1kCents))
	if err := d.Store.MarkJobQueryDone(ctx, dbgen.MarkJobQueryDoneParams{
		ID:            run.queryID,
		ListingsFound: clampInt32(listingsFound),
		ProviderRunID: runID,
		CostCents:     cost,
	}); err != nil {
		return fmt.Errorf("jobs: mark query done: %w", err)
	}

	if err := d.bump(ctx, run.jobID, scraper.StatKeyQueriesDone, 1); err != nil {
		return err
	}
	if err := d.bump(ctx, run.jobID, scraper.StatKeyListingsFound, int64(listingsFound)); err != nil {
		return err
	}
	if err := d.bump(ctx, run.jobID, scraper.StatKeyCostCents, cost); err != nil {
		return err
	}

	d.logLine(ctx, run.jobID, events.LevelInfo, "%q in %s: %d listings",
		run.row.Term, run.label(), listingsFound)
	d.publishProgress(ctx, run.jobID)

	return d.advanceAfterQueries(ctx, run.jobID, run.cfg)
}

// handleSearchError decides between failing the whole job, retrying, and giving up on
// a single query.
func (w *QueryWorker) handleSearchError(
	ctx context.Context,
	rj *river.Job[scraper.QueryArgs],
	run queryRun,
	searchErr error,
) error {
	d := w.deps

	if errors.Is(searchErr, context.Canceled) {
		return searchErr
	}

	if errors.Is(searchErr, provider.ErrAuth) {
		return w.failJobForAuth(ctx, run, searchErr)
	}

	lastAttempt := rj.Attempt >= rj.MaxAttempts
	if !lastAttempt {
		d.logLine(ctx, run.jobID, events.LevelWarn, "query failed (attempt %d/%d), retrying: %v",
			rj.Attempt, rj.MaxAttempts, searchErr)
		return searchErr
	}

	return w.failQuery(ctx, run, searchErr.Error(),
		fmt.Sprintf("query gave up after %d attempts: %v", rj.MaxAttempts, searchErr))
}

// failJobForAuth stops the whole job: a rejected key will not fix itself, and every
// retry against it is another wasted round trip.
func (w *QueryWorker) failJobForAuth(ctx context.Context, run queryRun, authErr error) error {
	d := w.deps

	if err := d.Store.MarkJobQueryFailed(ctx, dbgen.MarkJobQueryFailedParams{
		ID:     run.queryID,
		Status: scraper.StatusFailed,
		Error:  strPtr(authErr.Error()),
	}); err != nil {
		d.Log.Warn("could not mark query failed", "query_id", run.queryID, "error", err)
	}
	d.logLine(ctx, run.jobID, events.LevelError, "provider rejected the API key")
	d.failJob(ctx, run.jobID, "provider_auth: the provider rejected the stored API key")
	return nil
}

// failQuery ends one query without ending the job, and lets the pipeline move on.
func (w *QueryWorker) failQuery(ctx context.Context, run queryRun, reason, logLine string) error {
	d := w.deps

	if err := d.Store.MarkJobQueryFailed(ctx, dbgen.MarkJobQueryFailedParams{
		ID:     run.queryID,
		Status: scraper.StatusFailed,
		Error:  strPtr(reason),
	}); err != nil {
		return fmt.Errorf("jobs: mark query failed: %w", err)
	}
	if err := d.bump(ctx, run.jobID, scraper.StatKeyQueriesDone, 1); err != nil {
		return err
	}
	if err := d.bump(ctx, run.jobID, scraper.StatKeyQueriesFailed, 1); err != nil {
		return err
	}

	d.logLine(ctx, run.jobID, events.LevelError, "%s", logLine)
	d.publishProgress(ctx, run.jobID)
	return d.advanceAfterQueries(ctx, run.jobID, run.cfg)
}

func locationLabel(city, state string) string {
	switch {
	case city == "":
		return state
	case state == "":
		return city
	default:
		return city + ", " + state
	}
}

func strPtr(s string) *string { return &s }
