package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
	"github.com/bory/karvon-be/internal/verify"
)

// RunWorker is stage 1: turn the run's filter into a concrete set of addresses and
// fan out one job per address.
type RunWorker struct {
	river.WorkerDefaults[verify.RunArgs]
	deps *Deps
}

// NewRunWorker builds the fan-out worker.
func NewRunWorker(deps *Deps) *RunWorker { return &RunWorker{deps: deps} }

// Work implements river.Worker.
func (w *RunWorker) Work(ctx context.Context, rj *river.Job[verify.RunArgs]) error {
	d := w.deps
	runID := rj.Args.RunID

	run, active, err := d.runActive(ctx, runID)
	if err != nil {
		return err
	}
	if !active {
		d.Log.Info("skipping an inactive verification run", "run_id", runID, "status", run.Status)
		return nil
	}

	filter, err := decodeFilter(run.Filter)
	if err != nil {
		d.failRun(ctx, runID, "the run filter could not be read")
		return nil //nolint:nilerr // the run is already marked failed
	}

	if err := d.Store.MarkVerificationRunRunning(ctx, runID); err != nil {
		return fmt.Errorf("verify jobs: mark run running: %w", err)
	}

	pass := verify.Pass(run.Pass)
	targets, err := w.expand(ctx, pass, filter)
	if err != nil {
		return err
	}

	if err := d.Store.SetVerificationRunTotal(ctx, dbgen.SetVerificationRunTotalParams{
		ID:    runID,
		Total: clampInt32(len(targets)),
	}); err != nil {
		return fmt.Errorf("verify jobs: set run total: %w", err)
	}

	if len(targets) == 0 {
		d.Log.Info("verification run matched no addresses", "run_id", runID, "pass", run.Pass)
		return d.enqueue(ctx, verify.FinalizeArgs{RunID: runID})
	}

	if _, err := d.Store.InsertVerificationRunItems(ctx, dbgen.InsertVerificationRunItemsParams{
		RunID:           runID,
		VerificationIds: targets,
	}); err != nil {
		return fmt.Errorf("verify jobs: insert run items: %w", err)
	}

	inserts := make([]river.InsertManyParams, 0, len(targets))
	for _, id := range targets {
		if pass == verify.PassSelf {
			inserts = append(inserts, river.InsertManyParams{
				Args: verify.SelfArgs{RunID: runID, VerificationID: id},
			})
			continue
		}
		inserts = append(inserts, river.InsertManyParams{
			Args: verify.ThirdPartyArgs{RunID: runID, VerificationID: id},
		})
	}
	if _, err := d.Queue.InsertMany(ctx, inserts); err != nil {
		return fmt.Errorf("verify jobs: enqueue run items: %w", err)
	}

	d.Log.Info("verification run started", "run_id", runID, "pass", run.Pass, "addresses", len(targets))
	return nil
}

// expand resolves the filter into verification rows, creating rows for addresses on
// the master list that have never been verified.
func (w *RunWorker) expand(ctx context.Context, pass verify.Pass, filter verify.RunFilter) ([]uuid.UUID, error) {
	d := w.deps
	base := baseFilter(filter)

	// A selection names existing rows, so there is nothing to create. Any other
	// scope may cover addresses that have never been scored.
	if filter.Scope != verify.ScopeSelection && pass == verify.PassSelf {
		if err := w.seed(ctx, base); err != nil {
			return nil, err
		}
	}

	selector := base
	if pass == verify.PassSelf {
		if filter.StaleAfterDays != nil {
			cutoff := time.Now().UTC().AddDate(0, 0, -*filter.StaleAfterDays)
			selector.Pass1VerifiedBefore = &cutoff
		}
	} else {
		// The band and the send lock are re-applied here, not just at estimate time,
		// so a run created minutes ago cannot bill for an address that has since
		// been sent, downgraded, or scored high enough that paying adds nothing.
		complete, notSent := true, false
		band := verify.PaidBandFor(d.Settings.Settings(ctx), filter)
		minScore, maxScore := band.Min, band.Max
		selector.FreeComplete = &complete
		selector.MinFreeScore = &minScore
		selector.MaxFreeScore = &maxScore
		selector.ThirdPartySent = &notSent
	}

	return d.Store.SelectVerificationIDs(ctx, selector, d.Config.MaxRunEmails)
}

// seed creates verification rows for addresses that have none, in batches so a run
// over the whole master list does not build one enormous statement.
func (w *RunWorker) seed(ctx context.Context, base db.VerificationFilter) error {
	d := w.deps

	addresses, err := d.Store.ListAddressesWithoutVerification(ctx, base, d.Config.MaxRunEmails)
	if err != nil {
		return fmt.Errorf("verify jobs: list unverified addresses: %w", err)
	}
	if len(addresses) == 0 {
		return nil
	}

	const batch = 1000
	for start := 0; start < len(addresses); start += batch {
		end := min(start+batch, len(addresses))
		chunk := addresses[start:end]

		newIDs := make([]uuid.UUID, len(chunk))
		for i := range chunk {
			newIDs[i] = ids.New()
		}
		if err := d.Store.InsertVerifications(ctx, newIDs, chunk); err != nil {
			return err
		}
	}
	d.Log.Info("created verification rows", "addresses", len(addresses))
	return nil
}

// baseFilter mirrors the service's translation so a worker re-resolving a filter
// sees exactly the set the estimate was computed over.
func baseFilter(filter verify.RunFilter) db.VerificationFilter {
	notExcluded := false
	out := db.VerificationFilter{
		BusinessIDs:       filter.BusinessIDs,
		JobID:             filter.JobID,
		Tags:              filter.Tags,
		MinScore:          filter.MinScore,
		IncludeSuppressed: filter.IncludeSuppressed,
		Excluded:          &notExcluded,
	}
	if filter.Scope == verify.ScopeSelection {
		out.IDs = filter.IDs
	}
	if filter.Unscored {
		unscored := false
		out.FreeComplete = &unscored
	}
	return out
}
