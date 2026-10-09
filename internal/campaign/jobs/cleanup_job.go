package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// cleanupBatch is how many leads one cleanup job deletes. Each is its own call,
// so a batch is kept small enough to finish well inside the job timeout.
const cleanupBatch = 50

// InstantlyCleanupWorker deletes one batch of a cleanup run's leads from
// Instantly, then queues the next batch until the run's filter matches nothing
// or its cap is reached.
//
// The filter is re-read for every batch rather than frozen at the start, so a lead
// that replied in the meantime is no longer taken. A lead Instantly no longer has
// counts as already gone and is stamped all the same: either way it holds no slot.
type InstantlyCleanupWorker struct {
	river.WorkerDefaults[campaign.InstantlyCleanupArgs]
	deps *Deps
}

// NewInstantlyCleanupWorker builds the worker.
func NewInstantlyCleanupWorker(deps *Deps) *InstantlyCleanupWorker {
	return &InstantlyCleanupWorker{deps: deps}
}

// Work implements river.Worker.
func (w *InstantlyCleanupWorker) Work(ctx context.Context, rj *river.Job[campaign.InstantlyCleanupArgs]) error {
	d := w.deps
	run, err := d.Store.GetCleanupRun(ctx, rj.Args.RunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("campaign jobs: load cleanup run: %w", err)
	}
	if run.Status != campaign.CleanupQueued && run.Status != campaign.CleanupRunning {
		return nil
	}
	if run, err = d.Store.StartCleanupRun(ctx, dbgen.StartCleanupRunParams{ID: run.ID, At: campaign.Ptr(d.Now())}); err != nil {
		return fmt.Errorf("campaign jobs: start cleanup run: %w", err)
	}

	limit := cleanupBatch
	if run.MaxLeads != nil {
		left := int(*run.MaxLeads - run.Removed - run.AlreadyGone)
		if left <= 0 {
			return w.finish(ctx, run, campaign.CleanupDone, nil)
		}
		limit = min(limit, left)
	}
	candidates, err := d.Store.ListCleanupCandidates(ctx, service.CleanupFilterForRun(run), limit)
	if err != nil {
		return fmt.Errorf("campaign jobs: list cleanup candidates: %w", err)
	}
	if len(candidates) == 0 {
		return w.finish(ctx, run, campaign.CleanupDone, nil)
	}
	client, err := d.Service.Instantly(ctx)
	if err != nil {
		return w.finish(ctx, run, campaign.CleanupFailed, campaign.Ptr("Instantly is not configured: "+err.Error()))
	}

	var removed, gone, failed int32
	// Counts are saved even when the batch stops early, so progress survives a
	// snooze or a retry.
	defer func() {
		if removed+gone+failed == 0 {
			return
		}
		if err := d.Store.AddCleanupRunCounts(context.WithoutCancel(ctx), dbgen.AddCleanupRunCountsParams{
			ID: run.ID, Removed: removed, AlreadyGone: gone, Failed: failed,
		}); err != nil {
			d.Log.Warn("could not save cleanup run counts", "run_id", run.ID, "error", err)
		}
	}()

	runID := uuid.NullUUID{UUID: run.ID, Valid: true}
	for _, c := range candidates {
		if err := d.Limiter.Wait(ctx); err != nil {
			return err
		}
		err := client.DeleteLead(ctx, c.InstantlyLeadID)
		switch {
		case err == nil, errors.Is(err, provider.ErrNotFound):
			lead, loadErr := d.Store.GetCampaignLead(ctx, c.ID)
			if loadErr != nil {
				return fmt.Errorf("campaign jobs: load lead: %w", loadErr)
			}
			if markErr := MarkRemovedFromProvider(ctx, d, lead, campaign.ProviderRemovedCleanup, runID); markErr != nil {
				return markErr
			}
			if err == nil {
				removed++
			} else {
				gone++
			}
		case errors.Is(err, provider.ErrRateLimited):
			return snoozeFor(err)
		case errors.Is(err, provider.ErrInvalid):
			// Instantly will refuse this lead every time; tag it so the run moves on.
			d.Log.Warn("Instantly refused to delete a lead during cleanup", "lead_id", c.ID, "error", err)
			if tagErr := d.Store.TagCampaignLeadCleanupRun(ctx, dbgen.TagCampaignLeadCleanupRunParams{ID: c.ID, CleanupRunID: runID}); tagErr != nil {
				return fmt.Errorf("campaign jobs: tag lead: %w", tagErr)
			}
			failed++
		case fatal(err):
			return w.finish(ctx, run, campaign.CleanupFailed, campaign.Ptr("Instantly refused the cleanup: "+err.Error()))
		default:
			if rj.Attempt >= rj.MaxAttempts {
				return w.finish(ctx, run, campaign.CleanupFailed, campaign.Ptr("Instantly kept failing: "+err.Error()))
			}
			return fmt.Errorf("campaign jobs: delete lead: %w", err)
		}
	}
	return d.enqueue(ctx, campaign.InstantlyCleanupArgs{RunID: run.ID, Batch: rj.Args.Batch + 1})
}

func (w *InstantlyCleanupWorker) finish(ctx context.Context, run dbgen.InstantlyCleanupRun, status string, reason *string) error {
	d := w.deps
	if reason != nil {
		d.Log.Error("Instantly cleanup failed", "run_id", run.ID, "reason", *reason)
	} else {
		d.Log.Info("Instantly cleanup finished", "run_id", run.ID)
	}
	_, err := d.Store.FinishCleanupRun(ctx, dbgen.FinishCleanupRunParams{ID: run.ID, Status: status, Error: reason, At: campaign.Ptr(d.Now())})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// InstantlyCleanupAutoWorker starts a cleanup run with the saved policy when
// automatic cleanup is switched on.
type InstantlyCleanupAutoWorker struct {
	river.WorkerDefaults[campaign.InstantlyCleanupAutoArgs]
	deps *Deps
}

// NewInstantlyCleanupAutoWorker builds the worker.
func NewInstantlyCleanupAutoWorker(deps *Deps) *InstantlyCleanupAutoWorker {
	return &InstantlyCleanupAutoWorker{deps: deps}
}

// Work implements river.Worker.
func (w *InstantlyCleanupAutoWorker) Work(ctx context.Context, _ *river.Job[campaign.InstantlyCleanupAutoArgs]) error {
	run, err := w.deps.Service.StartAutoCleanup(ctx)
	if err != nil {
		// Instantly unconfigured or refusing: the next pass tries again.
		w.deps.Log.Warn("automatic Instantly cleanup did not start", "error", err)
		return nil
	}
	if run != nil {
		w.deps.Log.Info("automatic Instantly cleanup started", "run_id", run.ID, "selected", run.Selected)
	}
	return nil
}
