package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
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

// launchTimeout covers freeing contact slots, a run of bulk deletes, before the
// campaign is created.
const launchTimeout = 10 * time.Minute

// Timeout implements river.Worker.
func (w *LaunchWorker) Timeout(*river.Job[campaign.LaunchArgs]) time.Duration { return launchTimeout }

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
	// A job from an earlier launch request — rescheduled, unscheduled, or
	// launched now instead — stands down.
	if rj.Args.RequestID != uuid.Nil && (!camp.LaunchRequestID.Valid || camp.LaunchRequestID.UUID != rj.Args.RequestID) {
		d.Log.Info("stale launch job skipped", "campaign_id", id, "request_id", rj.Args.RequestID)
		return nil
	}
	switch camp.Status {
	case campaign.CampaignScheduled:
		// River holds the job until scheduled_launch_at, and a moved schedule
		// gets a new job and request id, so reaching here means it is time.
		// The campaign may have been edited while it waited: the gate is
		// evaluated again, and a failing one fails the launch with its reasons.
		checklist, err := d.Service.GetChecklist(ctx, id)
		if err != nil {
			return fmt.Errorf("campaign jobs: checklist: %w", err)
		}
		if !checklist.Ready {
			return failLaunch(ctx, d, camp, "the scheduled launch found the campaign not ready: "+blockingItems(checklist))
		}
		fallthrough
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
		return failLaunch(ctx, d, camp, "Instantly is not configured: "+err.Error())
	}
	camp, done, err := readyAtInstantly(ctx, d, client, camp, rj.Attempt, rj.MaxAttempts)
	if done || err != nil {
		return err
	}
	if _, err := d.Store.RecomputeCampaignCounts(ctx, id); err != nil {
		return fmt.Errorf("campaign jobs: recompute counts: %w", err)
	}
	// A scheduled campaign was prepared when it was scheduled, so this push only
	// carries leads added since; with none left it goes straight to activation.
	return d.enqueue(ctx, campaign.PushLeadsArgs{CampaignID: id, Batch: 0, Round: "launch:" + camp.LaunchRequestID.UUID.String()})
}

// readyAtInstantly frees contact slots and makes sure the Instantly campaign
// exists, which is everything a push needs. done is true when the campaign was
// marked failed, so the caller stops without an error.
func readyAtInstantly(ctx context.Context, d *Deps, client instantly.Client, camp dbgen.Campaign, attempt, maxAttempts int) (dbgen.Campaign, bool, error) {
	if n, err := d.Store.RequeueUnpushedCampaignLeads(ctx, camp.ID); err != nil {
		return camp, false, fmt.Errorf("campaign jobs: requeue unpushed leads: %w", err)
	} else if n > 0 {
		d.Log.Info("leads an earlier push gave up on are queued again", "campaign_id", camp.ID, "leads", n)
	}
	// Best effort: a failure here is retried, and on the last attempt the launch
	// goes ahead anyway, since the workspace may well have room already.
	if _, err := freeInstantlySlots(ctx, d, client); err != nil {
		switch {
		case errors.Is(err, provider.ErrRateLimited):
			return camp, false, snoozeFor(err)
		case attempt < maxAttempts && !fatal(err):
			return camp, false, err
		}
		d.Log.Warn("could not free Instantly contact slots; pushing anyway", "campaign_id", camp.ID, "error", err)
	}

	if camp.InstantlyCampaignID != nil {
		return camp, false, nil
	}
	accounts, err := d.Store.ListCampaignSendingAccounts(ctx, camp.ID)
	if err != nil {
		return camp, false, fmt.Errorf("campaign jobs: list accounts: %w", err)
	}
	in, err := service.CreateInputFor(camp, accounts)
	if err != nil {
		return camp, true, failLaunch(ctx, d, camp, "campaign cannot be shaped for Instantly: "+err.Error())
	}
	if err := d.Limiter.Wait(ctx); err != nil {
		return camp, false, err
	}
	created, err := client.CreateCampaign(ctx, in)
	if err != nil {
		const what = "creating the Instantly campaign"
		switch {
		case errors.Is(err, provider.ErrRateLimited):
			return camp, false, snoozeFor(err)
		case fatal(err), attempt >= maxAttempts:
			return camp, true, failLaunch(ctx, d, camp, what+": "+err.Error())
		}
		return camp, false, fmt.Errorf("campaign jobs: %s: %w", what, err)
	}
	// Store the provider id before anything else, so a crash right here
	// leaves a resumable state rather than a duplicate campaign.
	stored, err := d.Store.SetCampaignInstantlyID(ctx, dbgen.SetCampaignInstantlyIDParams{ID: camp.ID, InstantlyCampaignID: &created.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		// A prepare and a launch raced and the other stored its campaign first;
		// carry on with that one.
		d.Log.Warn("a second Instantly campaign was created and is unused", "campaign_id", camp.ID, "instantly_campaign_id", created.ID)
		stored, err = d.Store.GetCampaign(ctx, camp.ID)
	}
	if err != nil {
		return camp, false, fmt.Errorf("campaign jobs: store instantly id: %w", err)
	}
	if err := d.Store.ActivateCampaignVariants(ctx, camp.ID); err != nil {
		return camp, false, fmt.Errorf("campaign jobs: activate variants: %w", err)
	}
	d.Log.Info("Instantly campaign created", "campaign_id", camp.ID, "instantly_campaign_id", created.ID)
	return stored, false, nil
}

func failLaunch(ctx context.Context, d *Deps, camp dbgen.Campaign, reason string) error {
	d.Log.Error("campaign launch failed", "campaign_id", camp.ID, "reason", reason)
	if _, err := d.Store.MarkCampaignFailed(ctx, dbgen.MarkCampaignFailedParams{ID: camp.ID, Error: &reason}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("campaign jobs: mark failed: %w", err)
	}
	return nil
}

// blockingItems names the failing blocking checklist items, for the failure reason.
func blockingItems(c service.Checklist) string {
	var msgs []string
	for _, it := range c.Items {
		if it.Blocking && !it.OK {
			msgs = append(msgs, it.Message)
		}
	}
	return strings.Join(msgs, "; ")
}
