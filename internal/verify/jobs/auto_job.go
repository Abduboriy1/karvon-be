package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
	"github.com/bory/karvon-be/internal/verify"
)

// autoWaitWindow bounds how long a self run may block the sweep. A self run that is
// still queued or running after this long has almost certainly wedged, and letting
// it hold off the sweep would switch automatic verification off without a word.
const autoWaitWindow = 6 * time.Hour

// AutoSelfWorker is the periodic sweep behind the auto_self_verify setting. Each
// pass starts one self run over every address that has never been scored, which is
// how emails found by a scrape get verified without anyone clicking.
//
// It only ever starts the free self pass. The third-party pass spends money and
// stays a manual decision.
type AutoSelfWorker struct {
	river.WorkerDefaults[verify.AutoSelfArgs]
	deps *Deps
}

// NewAutoSelfWorker builds the sweep worker.
func NewAutoSelfWorker(deps *Deps) *AutoSelfWorker { return &AutoSelfWorker{deps: deps} }

// Work implements river.Worker.
func (w *AutoSelfWorker) Work(ctx context.Context, _ *river.Job[verify.AutoSelfArgs]) error {
	d := w.deps

	if !d.Settings.Settings(ctx).AutoSelfVerify {
		return nil
	}

	// One self run at a time, whoever started it. Addresses a scrape finds while it
	// runs stay unscored and are picked up by the next sweep after it finishes, so
	// nothing is scored twice and run history gets one entry per batch, not one per
	// minute.
	active, err := d.Store.CountRecentActiveRunsForPass(ctx, dbgen.CountRecentActiveRunsForPassParams{
		Pass:  string(verify.PassSelf),
		Since: time.Now().UTC().Add(-autoWaitWindow),
	})
	if err != nil {
		return fmt.Errorf("verify jobs: count active self runs: %w", err)
	}
	if active > 0 {
		return nil
	}

	filter := verify.RunFilter{Scope: verify.ScopeAll, Unscored: true}
	pending, err := w.pending(ctx, filter)
	if err != nil {
		return err
	}
	if pending == 0 {
		return nil
	}

	runID, err := w.startRun(ctx, filter)
	if err != nil {
		return err
	}
	d.Log.Info("started an automatic self verification run", "run_id", runID, "addresses", pending)
	return nil
}

// pending counts what the run would cover: scored-never rows plus addresses on the
// master list that have no verification row yet. The run seeds the latter itself.
func (w *AutoSelfWorker) pending(ctx context.Context, filter verify.RunFilter) (int64, error) {
	d := w.deps
	base := baseFilter(filter)

	unscored, err := d.Store.CountVerifications(ctx, base)
	if err != nil {
		return 0, fmt.Errorf("verify jobs: count unscored addresses: %w", err)
	}
	missing, err := d.Store.CountAddressesWithoutVerification(ctx, base)
	if err != nil {
		return 0, fmt.Errorf("verify jobs: count addresses without a verification: %w", err)
	}
	return unscored + missing, nil
}

// startRun records the run and enqueues its fan-out in one transaction, as an
// operator-started run is. The run size cap is left to the fan-out, which takes at
// most MaxRunEmails addresses; the rest wait for the next sweep.
func (w *AutoSelfWorker) startRun(ctx context.Context, filter verify.RunFilter) (string, error) {
	d := w.deps

	filterJSON, err := json.Marshal(filter)
	if err != nil {
		return "", fmt.Errorf("verify jobs: marshal auto filter: %w", err)
	}

	runID := ids.New()
	err = d.Store.InTxRaw(ctx, func(tx pgx.Tx) error {
		if _, err := dbgen.New(tx).CreateVerificationRun(ctx, dbgen.CreateVerificationRunParams{
			ID:     runID,
			Pass:   string(verify.PassSelf),
			Filter: filterJSON,
			Auto:   true,
		}); err != nil {
			return fmt.Errorf("verify jobs: create auto run: %w", err)
		}
		if _, err := d.Queue.InsertTx(ctx, tx, verify.RunArgs{RunID: runID}, nil); err != nil {
			return fmt.Errorf("verify jobs: enqueue auto run: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return runID.String(), nil
}
