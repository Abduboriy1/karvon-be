package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/campaign"
)

// PrepareLaunchWorker readies a scheduled campaign at Instantly as soon as it is
// scheduled: contact slots freed, the Instantly campaign created, the leads
// pushed. The campaign stays scheduled and Instantly's copy stays a draft; the
// scheduled LaunchWorker activates it when the time comes. Doing the slow,
// failure-prone part early means a problem shows up while there is still time to
// fix it, not at launch time.
type PrepareLaunchWorker struct {
	river.WorkerDefaults[campaign.PrepareLaunchArgs]
	deps *Deps
}

// NewPrepareLaunchWorker builds the worker.
func NewPrepareLaunchWorker(deps *Deps) *PrepareLaunchWorker { return &PrepareLaunchWorker{deps: deps} }

// Timeout implements river.Worker.
func (w *PrepareLaunchWorker) Timeout(*river.Job[campaign.PrepareLaunchArgs]) time.Duration {
	return launchTimeout
}

// Work implements river.Worker.
func (w *PrepareLaunchWorker) Work(ctx context.Context, rj *river.Job[campaign.PrepareLaunchArgs]) error {
	d := w.deps
	id := rj.Args.CampaignID

	camp, err := d.Store.GetCampaign(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("campaign jobs: load campaign: %w", err)
	}
	// Only the current request of a campaign still waiting to launch; once the
	// launch itself has claimed it, the launch does the work.
	if camp.Status != campaign.CampaignScheduled || !camp.LaunchRequestID.Valid || camp.LaunchRequestID.UUID != rj.Args.RequestID {
		return nil
	}
	checklist, err := d.Service.GetChecklist(ctx, id)
	if err != nil {
		return fmt.Errorf("campaign jobs: checklist: %w", err)
	}
	if !checklist.Ready {
		return failLaunch(ctx, d, camp, "the scheduled launch could not be prepared: "+blockingItems(checklist))
	}

	client, err := d.Service.Instantly(ctx)
	if err != nil {
		return failLaunch(ctx, d, camp, "Instantly is not configured: "+err.Error())
	}
	camp, done, err := readyAtInstantly(ctx, d, client, camp, rj.Attempt, rj.MaxAttempts)
	if done || err != nil {
		return err
	}
	if _, err := d.Store.RecomputeCampaignCounts(ctx, id); err != nil {
		return fmt.Errorf("campaign jobs: recompute counts: %w", err)
	}
	return d.enqueue(ctx, campaign.PushLeadsArgs{CampaignID: id, Batch: 0, Round: "prepare:" + rj.Args.RequestID.String()})
}
