package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/campaign/suppression"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// PushLeadsWorker pushes one batch of pending leads to Instantly.
//
// The claim is the guard: a lead is claimed only while it is pending and its
// contact is not suppressed, so a suppressed contact never reaches the provider.
// Each claimed lead gets its variant assignment (created once, never changed),
// the rendered subject and body go up as custom variables, and the assignment is
// locked the moment Instantly acknowledges the lead.
type PushLeadsWorker struct {
	river.WorkerDefaults[campaign.PushLeadsArgs]
	deps *Deps
}

// NewPushLeadsWorker builds the worker.
func NewPushLeadsWorker(deps *Deps) *PushLeadsWorker { return &PushLeadsWorker{deps: deps} }

// Work implements river.Worker.
func (w *PushLeadsWorker) Work(ctx context.Context, rj *river.Job[campaign.PushLeadsArgs]) error {
	d := w.deps
	id := rj.Args.CampaignID

	camp, live, err := d.campaignLive(ctx, id)
	if err != nil || !live {
		return err
	}
	if camp.InstantlyCampaignID == nil {
		return nil
	}
	client, err := d.Service.Instantly(ctx)
	if err != nil {
		return w.fatalCampaign(ctx, camp, "Instantly is not configured: "+err.Error())
	}

	leads, err := d.Store.ClaimPendingCampaignLeads(ctx, dbgen.ClaimPendingCampaignLeadsParams{CampaignID: id, Lim: campaign.Int32(d.Config.LeadBatch)})
	if err != nil {
		return fmt.Errorf("campaign jobs: claim leads: %w", err)
	}
	if len(leads) == 0 {
		return w.afterBatch(ctx, camp)
	}
	claimedIDs := make([]uuid.UUID, 0, len(leads))
	for _, l := range leads {
		claimedIDs = append(claimedIDs, l.ID)
	}

	// Assign, render and shape every lead before touching the provider, so a
	// rendering problem never leaves half a batch at Instantly.
	inputs := make([]instantly.LeadInput, 0, len(leads))
	byEmail := map[string]dbgen.CampaignLead{}
	varsByLead := map[uuid.UUID]map[string]any{}
	for _, lead := range leads {
		contact, err := d.Store.GetContact(ctx, lead.ContactID)
		if err != nil {
			w.release(ctx, id, claimedIDs, "load contact: "+err.Error())
			return fmt.Errorf("campaign jobs: load contact: %w", err)
		}
		var biz *service.BusinessInfo
		if lead.BusinessID.Valid {
			if b, err := d.Store.GetBusiness(ctx, lead.BusinessID.UUID); err == nil {
				biz = &service.BusinessInfo{Name: b.Name, City: campaign.Deref(b.City), State: campaign.Deref(b.State)}
			}
		}
		var assignments []dbgen.VariantAssignment
		err = d.Store.InTx(ctx, func(q *dbgen.Queries) error {
			assignments = assignments[:0]
			for step := 1; step <= int(camp.Steps); step++ {
				a, _, err := service.EnsureAssignment(ctx, q, camp, lead, contact, biz, step)
				if err != nil {
					return err
				}
				assignments = append(assignments, a)
			}
			return nil
		})
		if errors.Is(err, service.ErrNoVariants) {
			w.release(ctx, id, claimedIDs, "no active variant")
			return w.fatalCampaign(ctx, camp, "a step has no active variant with weight")
		}
		if err != nil {
			w.release(ctx, id, claimedIDs, err.Error())
			return fmt.Errorf("campaign jobs: assign: %w", err)
		}
		vars := service.CustomVars(assignments)
		varsByLead[lead.ID] = vars
		email := strings.ToLower(contact.Email)
		byEmail[email] = lead
		inputs = append(inputs, instantly.LeadInput{
			Email: email, FirstName: campaign.Deref(contact.FirstName), LastName: campaign.Deref(contact.LastName),
			CompanyName: campaign.Deref(contact.Company), JobTitle: campaign.Deref(contact.Title),
			Phone: campaign.Deref(contact.Phone), Website: campaign.Deref(contact.Website), CustomVariables: vars,
		})
	}

	if err := d.Limiter.Wait(ctx); err != nil {
		w.release(ctx, id, claimedIDs, "interrupted")
		return err
	}
	result, err := client.AddLeads(ctx, instantly.AddLeadsInput{CampaignID: *camp.InstantlyCampaignID, Leads: inputs, SkipIfInCampaign: true})
	if err != nil {
		return w.providerError(ctx, rj, camp, claimedIDs, err)
	}

	created := map[string]string{}
	for _, c := range result.CreatedLeads {
		created[strings.ToLower(c.Email)] = c.ID
	}
	invalid := map[string]bool{}
	for _, e := range result.InvalidEmails {
		invalid[strings.ToLower(e)] = true
	}

	now := d.Now()
	for email, lead := range byEmail {
		providerID, ok := created[email]
		switch {
		case ok:
			if err := w.markPushed(ctx, camp, lead, providerID, varsByLead[lead.ID]); err != nil {
				return err
			}
		case invalid[email]:
			if err := w.markInvalid(ctx, camp, lead); err != nil {
				return err
			}
		default:
			// Not created: a duplicate already in the campaign, blocklisted, or
			// invalid without the address being named. Ask Instantly which.
			if err := w.resolveMissing(ctx, client, camp, lead, email, varsByLead[lead.ID]); err != nil {
				return err
			}
		}
	}
	_ = now
	if _, err := d.Store.RecomputeCampaignCounts(ctx, id); err != nil {
		return fmt.Errorf("campaign jobs: recompute counts: %w", err)
	}
	if d.Config.LeadBatchGap > 0 && len(leads) == d.Config.LeadBatch {
		// Instantly asks for a pause between bulk calls; the next batch is its own
		// job so the pause never blocks a worker.
		if _, err := d.Queue.Insert(ctx, campaign.PushLeadsArgs{CampaignID: id, Batch: rj.Args.Batch + 1},
			&river.InsertOpts{ScheduledAt: d.Now().Add(d.Config.LeadBatchGap)}); err != nil {
			return fmt.Errorf("campaign jobs: enqueue next batch: %w", err)
		}
		return nil
	}
	return d.enqueue(ctx, campaign.PushLeadsArgs{CampaignID: id, Batch: rj.Args.Batch + 1})
}

// afterBatch runs when nothing is left to push: activate once, then stop.
func (w *PushLeadsWorker) afterBatch(ctx context.Context, camp dbgen.Campaign) error {
	if camp.Status == campaign.CampaignLaunching {
		return w.deps.enqueue(ctx, campaign.ActivateArgs{CampaignID: camp.ID})
	}
	return nil
}

func (w *PushLeadsWorker) markPushed(ctx context.Context, camp dbgen.Campaign, lead dbgen.CampaignLead, providerID string, vars map[string]any) error {
	d := w.deps
	return d.Store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		pushed, err := q.MarkCampaignLeadPushed(ctx, dbgen.MarkCampaignLeadPushedParams{ID: lead.ID, InstantlyLeadID: &providerID, CustomVars: service.EncodeCustomVars(vars)})
		if err != nil {
			return fmt.Errorf("mark pushed: %w", err)
		}
		if pushed.Status == campaign.LeadExcluded {
			// A global exclusion landed while this push was in flight; the sweep
			// could not remove what Instantly had not created yet, so do it now.
			if err := d.enqueueTx(ctx, tx, campaign.RemoveLeadArgs{CampaignLeadID: lead.ID}); err != nil {
				return err
			}
		}
		if err := q.LockVariantAssignments(ctx, lead.ID); err != nil {
			return fmt.Errorf("lock assignments: %w", err)
		}
		contact, err := q.GetContact(ctx, lead.ContactID)
		if err != nil {
			return err
		}
		before := campaign.Stage(contact.LifecycleStage)
		updated, _, err := campaign.Advance(ctx, q, contact, campaign.StageQueuedForInstantly)
		if err != nil {
			return err
		}
		assignments, _ := q.ListVariantAssignmentsForLead(ctx, lead.ID)
		var assignmentID, variantID *uuid.UUID
		if len(assignments) > 0 {
			assignmentID, variantID = &assignments[0].ID, &assignments[0].VariantID
		}
		_, _, err = campaign.RecordEvent(ctx, q, campaign.EventInput{
			ContactID: lead.ContactID, CampaignID: &camp.ID, CampaignLeadID: &lead.ID, AssignmentID: assignmentID, VariantID: variantID,
			Type: campaign.EventPushed, OccurredAt: d.Now(), Source: campaign.EventSourceSystem,
			StageBefore: before, StageAfter: campaign.Stage(updated.LifecycleStage),
			Data: map[string]any{"instantly_lead_id": providerID, "instantly_campaign_id": campaign.Deref(camp.InstantlyCampaignID)},
		})
		return err
	})
}

func (w *PushLeadsWorker) markInvalid(ctx context.Context, _ dbgen.Campaign, lead dbgen.CampaignLead) error {
	d := w.deps
	return d.Store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.MarkCampaignLeadStatus(ctx, dbgen.MarkCampaignLeadStatusParams{ID: lead.ID, Status: campaign.LeadSkipped, Error: campaign.Ptr("Instantly rejected the address as invalid")}); err != nil {
			return err
		}
		if err := q.DeleteUnlockedAssignmentsForLead(ctx, lead.ID); err != nil {
			return err
		}
		_, err := suppression.Apply(ctx, q, suppression.ApplyInput{
			ContactID: lead.ContactID, Reason: campaign.SuppressInvalidEmail, Source: campaign.SuppressionSourceImport,
			Note: "Instantly rejected the address on import", CampaignLeadID: &lead.ID, OccurredAt: d.Now(),
			Enqueue: func(ctx context.Context, args river.JobArgs) error { return d.enqueueTx(ctx, tx, args) },
		})
		return err
	})
}

// resolveMissing handles an address Instantly did not create: if it already
// exists in the campaign we adopt its id; otherwise it is recorded as skipped.
func (w *PushLeadsWorker) resolveMissing(ctx context.Context, client instantly.Client, camp dbgen.Campaign, lead dbgen.CampaignLead, email string, vars map[string]any) error {
	d := w.deps
	if err := d.Limiter.Wait(ctx); err != nil {
		return err
	}
	page, err := client.ListLeads(ctx, instantly.ListLeadsInput{CampaignID: *camp.InstantlyCampaignID, Contacts: []string{email}, Limit: 5})
	if err == nil {
		for _, item := range page.Items {
			if strings.EqualFold(item.Email, email) {
				return w.markPushed(ctx, camp, lead, item.ID, vars)
			}
		}
	}
	note := "Instantly did not create this lead (blocklisted, duplicate outside the campaign, or invalid)"
	return d.Store.InTx(ctx, func(q *dbgen.Queries) error {
		if _, err := q.MarkCampaignLeadStatus(ctx, dbgen.MarkCampaignLeadStatusParams{ID: lead.ID, Status: campaign.LeadSkipped, Error: &note}); err != nil {
			return err
		}
		if err := q.DeleteUnlockedAssignmentsForLead(ctx, lead.ID); err != nil {
			return err
		}
		_, _, err := campaign.RecordEvent(ctx, q, campaign.EventInput{
			ContactID: lead.ContactID, CampaignID: &camp.ID, CampaignLeadID: &lead.ID, Type: campaign.EventSkipped,
			OccurredAt: d.Now(), Source: campaign.EventSourceSystem, Data: map[string]any{"reason": note},
		})
		return err
	})
}

func (w *PushLeadsWorker) release(ctx context.Context, campaignID uuid.UUID, ids []uuid.UUID, reason string) {
	if _, err := w.deps.Store.ReleaseCampaignLeadClaims(ctx, dbgen.ReleaseCampaignLeadClaimsParams{CampaignID: campaignID, Ids: ids, Error: campaign.Optional(reason)}); err != nil {
		w.deps.Log.Error("could not release lead claims", "campaign_id", campaignID, "error", err)
	}
}

func (w *PushLeadsWorker) providerError(ctx context.Context, rj *river.Job[campaign.PushLeadsArgs], camp dbgen.Campaign, claimed []uuid.UUID, err error) error {
	d := w.deps
	switch {
	case errors.Is(err, provider.ErrRateLimited):
		w.release(ctx, camp.ID, claimed, "rate limited")
		return snoozeFor(err)
	case errors.Is(err, provider.ErrNotFound):
		w.release(ctx, camp.ID, claimed, "campaign missing at Instantly")
		return w.fatalCampaign(ctx, camp, "the Instantly campaign no longer exists")
	case fatal(err):
		w.release(ctx, camp.ID, claimed, err.Error())
		return w.fatalCampaign(ctx, camp, "Instantly rejected the lead push: "+err.Error())
	}
	if rj.Attempt >= rj.MaxAttempts {
		d.Log.Warn("giving up on a lead batch", "campaign_id", camp.ID, "error", err)
		for _, id := range claimed {
			_, _ = d.Store.MarkCampaignLeadStatus(ctx, dbgen.MarkCampaignLeadStatusParams{ID: id, Status: campaign.LeadFailed, Error: campaign.Ptr(err.Error())})
		}
		_ = d.Store.SetCampaignSyncError(ctx, dbgen.SetCampaignSyncErrorParams{ID: camp.ID, Error: campaign.Ptr("lead push failed: " + err.Error())})
		return nil
	}
	w.release(ctx, camp.ID, claimed, err.Error())
	return fmt.Errorf("campaign jobs: add leads: %w", err)
}

func (w *PushLeadsWorker) fatalCampaign(ctx context.Context, camp dbgen.Campaign, reason string) error {
	w.deps.Log.Error("lead push stopped", "campaign_id", camp.ID, "reason", reason)
	if camp.Status == campaign.CampaignLaunching {
		_, err := w.deps.Store.MarkCampaignFailed(ctx, dbgen.MarkCampaignFailedParams{ID: camp.ID, Error: &reason})
		return err
	}
	return w.deps.Store.SetCampaignSyncError(ctx, dbgen.SetCampaignSyncErrorParams{ID: camp.ID, Error: &reason})
}

// ActivateWorker activates the Instantly campaign once the leads are in.
type ActivateWorker struct {
	river.WorkerDefaults[campaign.ActivateArgs]
	deps *Deps
}

// NewActivateWorker builds the worker.
func NewActivateWorker(deps *Deps) *ActivateWorker { return &ActivateWorker{deps: deps} }

// Work implements river.Worker.
func (w *ActivateWorker) Work(ctx context.Context, rj *river.Job[campaign.ActivateArgs]) error {
	d := w.deps
	camp, live, err := d.campaignLive(ctx, rj.Args.CampaignID)
	if err != nil || !live {
		return err
	}
	if camp.Status != campaign.CampaignLaunching || camp.InstantlyCampaignID == nil {
		return nil
	}
	client, err := d.Service.Instantly(ctx)
	if err != nil {
		return err
	}
	if err := d.Limiter.Wait(ctx); err != nil {
		return err
	}
	if err := client.ActivateCampaign(ctx, *camp.InstantlyCampaignID); err != nil {
		if errors.Is(err, provider.ErrRateLimited) {
			return snoozeFor(err)
		}
		if fatal(err) || rj.Attempt >= rj.MaxAttempts {
			reason := "Instantly could not activate the campaign: " + err.Error()
			_, _ = d.Store.MarkCampaignFailed(ctx, dbgen.MarkCampaignFailedParams{ID: camp.ID, Error: &reason})
			return nil
		}
		return fmt.Errorf("campaign jobs: activate: %w", err)
	}
	if _, err := d.Store.MarkCampaignActive(ctx, camp.ID); err != nil {
		return fmt.Errorf("campaign jobs: mark active: %w", err)
	}
	d.Log.Info("campaign active", "campaign_id", camp.ID)
	return d.enqueue(ctx, campaign.SyncCampaignArgs{CampaignID: camp.ID})
}

// RemoveLeadWorker deletes a suppressed lead from Instantly.
type RemoveLeadWorker struct {
	river.WorkerDefaults[campaign.RemoveLeadArgs]
	deps *Deps
}

// NewRemoveLeadWorker builds the worker.
func NewRemoveLeadWorker(deps *Deps) *RemoveLeadWorker { return &RemoveLeadWorker{deps: deps} }

// Work implements river.Worker.
func (w *RemoveLeadWorker) Work(ctx context.Context, rj *river.Job[campaign.RemoveLeadArgs]) error {
	d := w.deps
	lead, err := d.Store.GetCampaignLead(ctx, rj.Args.CampaignLeadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("campaign jobs: load lead: %w", err)
	}
	if lead.InstantlyLeadID == nil || *lead.InstantlyLeadID == "" {
		return nil
	}
	client, err := d.Service.Instantly(ctx)
	if err != nil {
		if fatal(err) {
			d.Log.Warn("cannot remove lead at Instantly; provider not configured", "lead_id", lead.ID)
			return nil
		}
		return err
	}
	if err := d.Limiter.Wait(ctx); err != nil {
		return err
	}
	err = client.DeleteLead(ctx, *lead.InstantlyLeadID)
	switch {
	case err == nil, errors.Is(err, provider.ErrNotFound):
		return d.Store.SetCampaignLeadProviderState(ctx, dbgen.SetCampaignLeadProviderStateParams{
			ID: lead.ID, Status: lead.Status, InstantlyStatus: lead.InstantlyStatus, InterestStatus: lead.InterestStatus,
			InterestLabel: lead.InterestLabel, OpenCount: lead.OpenCount, ClickCount: lead.ClickCount, ReplyCount: lead.ReplyCount,
		})
	case errors.Is(err, provider.ErrRateLimited):
		return snoozeFor(err)
	case fatal(err):
		d.Log.Warn("Instantly refused to delete a lead", "lead_id", lead.ID, "error", err)
		return nil
	}
	return fmt.Errorf("campaign jobs: delete lead: %w", err)
}
