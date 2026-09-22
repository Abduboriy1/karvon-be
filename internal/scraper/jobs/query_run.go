package jobs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/events"
	"github.com/bory/karvon-be/internal/scraper"
	"github.com/bory/karvon-be/internal/scraper/provider"
)

// statusAbortedSelf marks a run this service stopped on purpose. It is distinct from
// the vendor's own ABORTED because a run we killed for overspending must never be
// resumed: resuming it would start the same spending again.
const statusAbortedSelf = "ABORTED_BY_KARVON"

// pagesPerInvocation bounds how much of a dataset one invocation drains before handing
// back to the queue.
//
// A whole-state run can hold tens of thousands of places, and draining all of them in
// one invocation would outlive the worker's timeout. That failure would cost an attempt
// each time despite real progress being made, and three of them would fail a query
// whose data was already bought and paid for.
const pagesPerInvocation = 10

// workAsync advances one vendor-side run by exactly one step and then gets out of the
// way. River re-invokes this worker after the snooze, so a run lasting hours costs a
// few seconds of worker time per poll instead of holding a slot open.
//
// Every transition is written down before the step that depends on it, because the
// thing being protected is money: a run that exists but is not recorded has already
// been paid for, and starting a replacement pays for the same places again.
func (w *QueryWorker) workAsync(
	ctx context.Context,
	rj *river.Job[scraper.QueryArgs],
	run queryRun,
	async provider.AsyncProvider,
) error {
	switch run.row.RunState {
	case scraper.RunStateNone:
		return w.startRun(ctx, rj, run, async)
	case scraper.RunStateStarting:
		return w.recoverStart(ctx, run, async)
	case scraper.RunStatePolling:
		return w.pollRun(ctx, run, async)
	case scraper.RunStateIngesting:
		return w.drainRun(ctx, run, async)
	default:
		// Already finished; a duplicate queue entry has nothing to do.
		return nil
	}
}

// startRun claims one of the account's concurrent run slots and starts the vendor run.
func (w *QueryWorker) startRun(
	ctx context.Context,
	rj *river.Job[scraper.QueryArgs],
	run queryRun,
	async provider.AsyncProvider,
) error {
	d := w.deps

	claimed, err := d.Store.ClaimProviderRunSlot(ctx, run.queryID, run.source.ID, d.Config.MaxActiveProviderRuns)
	if err != nil {
		return fmt.Errorf("jobs: claim run slot: %w", err)
	}
	if !claimed {
		// Either the account is already running as many as it may, or another
		// worker claimed this row first. Slots free up when a run finishes, which
		// is noticed at the poll cadence, so that is how often to look again.
		return river.JobSnooze(d.Config.RunPollInterval)
	}

	handle, err := async.StartRun(ctx, run.searchQuery())
	if err != nil {
		// The slot goes back only because the run demonstrably did not start: the
		// request itself failed. Anything past this point keeps the claim.
		if relErr := d.Store.ReleaseProviderRunSlot(ctx, run.queryID); relErr != nil {
			d.Log.Warn("could not release run slot", "query_id", run.queryID, "error", relErr)
		}
		return w.handleSearchError(ctx, rj, run, err)
	}

	if err := d.Store.SaveProviderRun(ctx, dbgen.SaveProviderRunParams{
		ID:            run.queryID,
		ProviderRunID: &handle.RunID,
		DatasetID:     strOrNil(handle.DatasetID),
		RunStatus:     strPtr(scraper.RunStatePolling),
	}); err != nil {
		// The run is live and billing while this row says only "starting". The next
		// invocation adopts it by id rather than starting a second one.
		return fmt.Errorf("jobs: save provider run %s: %w", handle.RunID, err)
	}

	d.logLine(ctx, run.jobID, events.LevelInfo, "started provider run %s for %s (%d terms)",
		handle.RunID, run.label(), len(run.searchQuery().TermList()))
	return river.JobSnooze(d.Config.RunPollInterval)
}

// recoverStart deals with a row that claimed a slot but has no run id: the worker died
// in the seconds between the vendor accepting a run and the id being written down.
//
// Guessing either way costs something. Starting a fresh run pays for the location
// twice; giving up abandons a run that will bill in full and deliver nothing. So the
// vendor is asked what it actually started.
func (w *QueryWorker) recoverStart(ctx context.Context, run queryRun, async provider.AsyncProvider) error {
	d := w.deps

	if run.row.ProviderRunID != nil && *run.row.ProviderRunID != "" {
		// The id is there after all; just move the row on.
		if err := d.Store.SetProviderRunStatus(ctx, dbgen.SetProviderRunStatusParams{
			ID:        run.queryID,
			RunState:  scraper.RunStatePolling,
			RunStatus: run.row.RunStatus,
		}); err != nil {
			return fmt.Errorf("jobs: resume run: %w", err)
		}
		return river.JobSnooze(d.Config.RunPollInterval)
	}

	startedAt := time.Now().UTC()
	if run.row.StartedAt != nil {
		startedAt = *run.row.StartedAt
	}

	known, err := d.knownRunIDs(ctx, run.source.ID)
	if err != nil {
		return err
	}

	handle, matches, err := async.FindOrphanRun(ctx, startedAt, known)
	if err != nil {
		d.Log.Warn("could not look for an orphaned run", "query_id", run.queryID, "error", err)
		return river.JobSnooze(d.Config.RunPollInterval)
	}

	switch matches {
	case 1:
		if err := d.Store.SaveProviderRun(ctx, dbgen.SaveProviderRunParams{
			ID:            run.queryID,
			ProviderRunID: &handle.RunID,
			DatasetID:     strOrNil(handle.DatasetID),
			RunStatus:     strPtr(scraper.RunStatePolling),
		}); err != nil {
			return fmt.Errorf("jobs: adopt provider run %s: %w", handle.RunID, err)
		}
		d.logLine(ctx, run.jobID, events.LevelWarn,
			"adopted provider run %s for %s: it was started but never recorded",
			handle.RunID, run.label())
		return river.JobSnooze(d.Config.RunPollInterval)

	case 0:
		// Nothing was started, so the slot is free and the start can be retried.
		if err := d.Store.ReleaseProviderRunSlot(ctx, run.queryID); err != nil {
			return fmt.Errorf("jobs: release run slot: %w", err)
		}
		d.logLine(ctx, run.jobID, events.LevelWarn,
			"no provider run was started for %s, retrying", run.label())
		return river.JobSnooze(d.Config.RunPollInterval)

	default:
		// Several unrecorded runs started in the same window. Picking one could file
		// another location's places under this query, and starting another would add
		// a third bill, so a person decides.
		return w.failQuery(ctx, run,
			fmt.Sprintf("%d unrecorded provider runs started around the same time; "+
				"check the provider console and reconcile them by hand", matches),
			fmt.Sprintf("%s: ambiguous provider runs, stopping rather than paying twice", run.label()))
	}
}

// pollRun asks the vendor whether the run has finished.
func (w *QueryWorker) pollRun(ctx context.Context, run queryRun, async provider.AsyncProvider) error {
	d := w.deps

	runID := ""
	if run.row.ProviderRunID != nil {
		runID = *run.row.ProviderRunID
	}
	if runID == "" {
		// Polling with no id to poll: treat it as the start-recovery case.
		return w.recoverStart(ctx, run, async)
	}

	state, err := async.RunState(ctx, runID)
	if err != nil {
		if errors.Is(err, provider.ErrAuth) {
			return w.failJobForAuth(ctx, run, err)
		}
		// A poll that fails is not a run that failed. Retrying costs nothing, and
		// the age check below is what stops this going on forever.
		d.Log.Warn("could not poll provider run", "run_id", runID, "error", err)
		if w.runIsOverdue(run) {
			return w.abandonRun(ctx, run, async, runID, "the provider stopped answering")
		}
		return river.JobSnooze(d.Config.RunPollInterval)
	}

	if err := d.Store.SetProviderRunStatus(ctx, dbgen.SetProviderRunStatusParams{
		ID:        run.queryID,
		RunState:  scraper.RunStatePolling,
		RunStatus: strPtr(state.Status),
	}); err != nil {
		return fmt.Errorf("jobs: record run status: %w", err)
	}

	if !state.Terminal {
		if w.runIsOverdue(run) {
			return w.abandonRun(ctx, run, async, runID,
				fmt.Sprintf("it ran longer than %s", d.Config.MaxRunDuration))
		}
		return river.JobSnooze(d.Config.RunPollInterval)
	}

	// Finished, one way or another: everything it produced is already paid for, so
	// it all gets drained before the outcome is judged.
	run.row.DatasetID = preferDataset(run.row.DatasetID, state.DatasetID)
	if err := d.moveToIngesting(ctx, run, state.Status); err != nil {
		return err
	}
	run.row.RunState = scraper.RunStateIngesting
	run.row.RunStatus = strPtr(state.Status)
	return w.drainRun(ctx, run, async)
}

// abortRun stops a run that has outlived its budget, so it stops spending, then drains
// whatever it produced.
func (w *QueryWorker) abandonRun(
	ctx context.Context,
	run queryRun,
	async provider.AsyncProvider,
	runID, reason string,
) error {
	d := w.deps

	if err := async.AbortRun(ctx, runID); err != nil {
		d.Log.Warn("could not abort overdue run", "run_id", runID, "error", err)
	}
	d.logLine(ctx, run.jobID, events.LevelWarn,
		"aborted provider run %s for %s: %s", runID, run.label(), reason)

	if err := d.moveToIngesting(ctx, run, statusAbortedSelf); err != nil {
		return err
	}
	run.row.RunState = scraper.RunStateIngesting
	run.row.RunStatus = strPtr(statusAbortedSelf)
	return w.drainRun(ctx, run, async)
}

// drainRun copies the run's dataset into the database, resuming from wherever a
// previous invocation stopped, and then closes the query out.
func (w *QueryWorker) drainRun(ctx context.Context, run queryRun, async provider.AsyncProvider) error {
	d := w.deps

	datasetID := ""
	if run.row.DatasetID != nil {
		datasetID = *run.row.DatasetID
	}
	runID := ""
	if run.row.ProviderRunID != nil {
		runID = *run.row.ProviderRunID
	}

	if datasetID == "" {
		return w.failQuery(ctx, run, "the provider run produced no dataset",
			fmt.Sprintf("%s: provider run %s has no dataset to read", run.label(), runID))
	}

	offset := int(run.row.IngestedOffset)
	total := int(run.row.ListingsFound)
	pageSize := d.Config.RunPageSize

	for pages := 0; ; pages++ {
		if pages >= pagesPerInvocation {
			// More dataset left than fits in one invocation. The offset is already
			// committed, so the next one picks up exactly here.
			return river.JobSnooze(time.Second)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, active, err := d.jobActive(ctx, run.jobID); err != nil {
			return err
		} else if !active {
			return nil
		}

		page, err := async.FetchPage(ctx, datasetID, offset, pageSize)
		if err != nil {
			if errors.Is(err, provider.ErrAuth) {
				return w.failJobForAuth(ctx, run, err)
			}
			d.Log.Warn("could not read provider dataset", "dataset_id", datasetID, "error", err)
			return river.JobSnooze(d.Config.RunPollInterval)
		}
		// A short page is not the end of the dataset: only an empty one is. The
		// vendor may return fewer rows than asked for, and stopping early would
		// throw away places this job has already paid for.
		if page.Items == 0 {
			break
		}

		listings := page.Listings
		for i := range listings {
			if listings[i].RunID == "" {
				listings[i].RunID = runID
			}
			if _, err := d.Ingestor.IngestListing(ctx, run.jobID, run.queryID, listings[i]); err != nil {
				return fmt.Errorf("jobs: ingest listing: %w", err)
			}
			total++
		}
		offset += page.Items

		// The offset is committed after every page, so a crash re-reads at most one
		// page instead of the whole dataset.
		if err := d.Store.AdvanceProviderRunIngest(ctx, dbgen.AdvanceProviderRunIngestParams{
			ID:             run.queryID,
			IngestedOffset: clampInt32(offset),
			ListingsFound:  clampInt32(total),
		}); err != nil {
			return fmt.Errorf("jobs: record ingest progress: %w", err)
		}
		if err := d.bump(ctx, run.jobID, scraper.StatKeyListingsFound, int64(len(listings))); err != nil {
			return err
		}
		d.publishProgress(ctx, run.jobID)
	}

	return w.finishRun(ctx, run, async, runID, total)
}

// finishRun judges the outcome once the dataset has been drained.
func (w *QueryWorker) finishRun(
	ctx context.Context,
	run queryRun,
	async provider.AsyncProvider,
	runID string,
	total int,
) error {
	d := w.deps

	status := ""
	if run.row.RunStatus != nil {
		status = *run.row.RunStatus
	}
	// Read from the row, not from the vendor: the vendor reports a run we killed as
	// a plain ABORTED, which is a status worth resuming, and resuming it would undo
	// the reason it was killed.
	abortedBySelf := status == statusAbortedSelf

	state, stateErr := async.RunState(ctx, runID)
	if stateErr == nil {
		status = state.Status
	}

	cost := scraper.ActualCostCents(total, int(run.source.CostPer1kCents))
	if stateErr == nil && state.CostUSD > 0 {
		// The vendor's own figure beats the per-1k estimate, and is the number that
		// makes a pilot run's real price per place knowable.
		cost = int64(math.Round(state.CostUSD * 100))
	}

	succeeded := stateErr == nil && state.OK
	if !succeeded && !abortedBySelf && !run.row.Resurrected && stateErr == nil && canResurrect(status) {
		// A resurrected run continues into the same dataset, so it neither re-scrapes
		// nor re-bills what it already collected. One attempt only.
		if err := async.Resurrect(ctx, runID); err != nil {
			d.Log.Warn("could not resurrect run", "run_id", runID, "error", err)
		} else {
			if err := d.Store.MarkProviderRunResurrected(ctx, run.queryID); err != nil {
				return fmt.Errorf("jobs: mark run resurrected: %w", err)
			}
			d.logLine(ctx, run.jobID, events.LevelWarn,
				"provider run %s for %s ended as %s, resuming it", runID, run.label(), status)
			return river.JobSnooze(d.Config.RunPollInterval)
		}
	}

	if !succeeded {
		// The partial listings are kept and counted: they were paid for.
		if err := d.bump(ctx, run.jobID, scraper.StatKeyCostCents, cost); err != nil {
			return err
		}
		return w.failQuery(ctx, run,
			fmt.Sprintf("provider run %s ended as %s after %d listings", runID, status, total),
			fmt.Sprintf("%s: provider run %s ended as %s, keeping %d listings",
				run.label(), runID, status, total))
	}

	if err := d.Store.MarkJobQueryDone(ctx, dbgen.MarkJobQueryDoneParams{
		ID:            run.queryID,
		ListingsFound: clampInt32(total),
		ProviderRunID: &runID,
		CostCents:     cost,
	}); err != nil {
		return fmt.Errorf("jobs: mark query done: %w", err)
	}
	if err := d.bump(ctx, run.jobID, scraper.StatKeyQueriesDone, 1); err != nil {
		return err
	}
	if err := d.bump(ctx, run.jobID, scraper.StatKeyCostCents, cost); err != nil {
		return err
	}

	d.logLine(ctx, run.jobID, events.LevelInfo, "%s: %d listings from run %s",
		run.label(), total, runID)
	d.publishProgress(ctx, run.jobID)
	return d.advanceAfterQueries(ctx, run.jobID, run.cfg)
}

// runIsOverdue reports whether a run has been going longer than the configured budget.
func (w *QueryWorker) runIsOverdue(run queryRun) bool {
	limit := w.deps.Config.MaxRunDuration
	if limit <= 0 || run.row.StartedAt == nil {
		return false
	}
	return time.Since(*run.row.StartedAt) > limit
}

// canResurrect reports whether a vendor status describes a run worth resuming. A run
// this service aborted on purpose is not.
func canResurrect(status string) bool {
	switch status {
	case "FAILED", "TIMED-OUT", "ABORTED":
		return true
	default:
		return false
	}
}

// moveToIngesting records that the run is over and its dataset is being read.
func (d *Deps) moveToIngesting(ctx context.Context, run queryRun, status string) error {
	if run.row.DatasetID != nil && *run.row.DatasetID != "" {
		if err := d.Store.SaveProviderRun(ctx, dbgen.SaveProviderRunParams{
			ID:            run.queryID,
			ProviderRunID: run.row.ProviderRunID,
			DatasetID:     run.row.DatasetID,
			RunStatus:     strPtr(status),
		}); err != nil {
			return fmt.Errorf("jobs: record dataset: %w", err)
		}
	}
	if err := d.Store.SetProviderRunStatus(ctx, dbgen.SetProviderRunStatusParams{
		ID:        run.queryID,
		RunState:  scraper.RunStateIngesting,
		RunStatus: strPtr(status),
	}); err != nil {
		return fmt.Errorf("jobs: mark run ingesting: %w", err)
	}
	return nil
}

// knownRunIDs is every run this source has already recorded, so an orphan search can
// tell a stray run from one that is accounted for.
func (d *Deps) knownRunIDs(ctx context.Context, sourceID uuid.UUID) (map[string]struct{}, error) {
	ids, err := d.Store.ListProviderRunIDsForSource(ctx, sourceID)
	if err != nil {
		return nil, fmt.Errorf("jobs: list provider runs: %w", err)
	}
	known := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != nil && *id != "" {
			known[*id] = struct{}{}
		}
	}
	return known, nil
}

// preferDataset keeps the id already stored and falls back to the one just reported.
func preferDataset(stored *string, reported string) *string {
	if stored != nil && *stored != "" {
		return stored
	}
	return strOrNil(reported)
}

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
