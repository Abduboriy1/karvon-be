package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// LaunchWorker creates the Instantly campaign and starts the lead push.
type LaunchWorker struct {
	river.WorkerDefaults[campaign.LaunchArgs]
	deps *Deps
}

// NewLaunchWorker builds the worker.
func NewLaunchWorker(deps *Deps) *LaunchWorker { return &LaunchWorker{deps: deps} }

// Work implements river.Worker.
func (w *LaunchWorker) Work(ctx context.Context, rj *river.Job[campaign.LaunchArgs]) error {
	d := w.deps
	id := rj.Args.CampaignID

	camp, err := d.Store.GetCampaign(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("campaign jobs: load campaign: %w", err)
	}
	switch camp.Status {
	case campaign.CampaignReady:
		claimed, err := d.Store.ClaimCampaignLaunch(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // someone else claimed it
		}
		if err != nil {
			return fmt.Errorf("campaign jobs: claim launch: %w", err)
		}
		camp = claimed
	case campaign.CampaignLaunching:
		// A retry after a partial launch: continue from wherever we got to.
	default:
		return nil
	}

	client, err := d.Service.Instantly(ctx)
	if err != nil {
		return w.fail(ctx, camp, "Instantly is not configured: "+err.Error())
	}

	if camp.InstantlyCampaignID == nil {
		accounts, err := d.Store.ListCampaignSendingAccounts(ctx, id)
		if err != nil {
			return fmt.Errorf("campaign jobs: list accounts: %w", err)
		}
		in, err := service.CreateInputFor(camp, accounts)
		if err != nil {
			return w.fail(ctx, camp, "campaign cannot be shaped for Instantly: "+err.Error())
		}
		if err := d.Limiter.Wait(ctx); err != nil {
			return err
		}
		created, err := client.CreateCampaign(ctx, in)
		if err != nil {
			return w.providerError(ctx, rj, camp, "creating the Instantly campaign", err)
		}
		// Store the provider id before anything else, so a crash right here
		// leaves a resumable state rather than a duplicate campaign.
		if _, err := d.Store.SetCampaignInstantlyID(ctx, dbgen.SetCampaignInstantlyIDParams{ID: id, InstantlyCampaignID: &created.ID}); err != nil {
			return fmt.Errorf("campaign jobs: store instantly id: %w", err)
		}
		if err := d.Store.ActivateCampaignVariants(ctx, id); err != nil {
			return fmt.Errorf("campaign jobs: activate variants: %w", err)
		}
		d.Log.Info("Instantly campaign created", "campaign_id", id, "instantly_campaign_id", created.ID)
	}

	if _, err := d.Store.RecomputeCampaignCounts(ctx, id); err != nil {
		return fmt.Errorf("campaign jobs: recompute counts: %w", err)
	}
	return d.enqueue(ctx, campaign.PushLeadsArgs{CampaignID: id, Batch: 0})
}

func (w *LaunchWorker) providerError(ctx context.Context, rj *river.Job[campaign.LaunchArgs], camp dbgen.Campaign, what string, err error) error {
	switch {
	case errors.Is(err, provider.ErrRateLimited):
		return snoozeFor(err)
	case fatal(err):
		return w.fail(ctx, camp, what+": "+err.Error())
	}
	if rj.Attempt >= rj.MaxAttempts {
		return w.fail(ctx, camp, what+": "+err.Error())
	}
	return fmt.Errorf("campaign jobs: %s: %w", what, err)
}

func (w *LaunchWorker) fail(ctx context.Context, camp dbgen.Campaign, reason string) error {
	w.deps.Log.Error("campaign launch failed", "campaign_id", camp.ID, "reason", reason)
	if _, err := w.deps.Store.MarkCampaignFailed(ctx, dbgen.MarkCampaignFailedParams{ID: camp.ID, Error: &reason}); err != nil {
		return fmt.Errorf("campaign jobs: mark failed: %w", err)
	}
	return nil
}
