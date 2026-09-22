package jobs

import (
	"context"
	"fmt"

	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/verify"
)

// FinalizeWorker closes a run out once every address has reached a terminal state.
type FinalizeWorker struct {
	river.WorkerDefaults[verify.FinalizeArgs]
	deps *Deps
}

// NewFinalizeWorker builds the closing worker.
func NewFinalizeWorker(deps *Deps) *FinalizeWorker { return &FinalizeWorker{deps: deps} }

// Work implements river.Worker.
func (w *FinalizeWorker) Work(ctx context.Context, rj *river.Job[verify.FinalizeArgs]) error {
	d := w.deps
	runID := rj.Args.RunID

	run, err := d.Store.RecomputeVerificationRunStats(ctx, runID)
	if err != nil {
		// A run deleted while its workers were still going has nothing to close.
		return nil //nolint:nilerr // the run is gone, stop cleanly
	}

	// Another address is still outstanding, which happens when a retry was
	// scheduled after this job was enqueued.
	if pending, err := d.Store.CountPendingVerificationRunItems(ctx, runID); err != nil {
		return fmt.Errorf("verify jobs: count pending items: %w", err)
	} else if pending > 0 {
		return nil
	}

	// A run cancelled or failed earlier keeps that status.
	if run.Status != verify.RunQueued && run.Status != verify.RunRunning {
		return nil
	}

	status := verify.RunDone
	var failure *string
	// Every single address failing is a failed run; a few failures among many is a
	// completed run with recorded errors.
	if run.Total > 0 && run.Failed == run.Total {
		status = verify.RunFailed
		msg := fmt.Sprintf("all %d addresses failed", run.Failed)
		failure = &msg
	}

	if err := d.Store.MarkVerificationRunTerminal(ctx, dbgen.MarkVerificationRunTerminalParams{
		ID:     runID,
		Status: status,
		Error:  failure,
	}); err != nil {
		return fmt.Errorf("verify jobs: mark run terminal: %w", err)
	}

	d.Log.Info("verification run finished",
		"run_id", runID, "pass", run.Pass, "status", status,
		"done", run.Done, "failed", run.Failed, "skipped", run.Skipped,
		"credits", run.CreditsUsed)
	return nil
}
