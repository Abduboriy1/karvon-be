package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// SyncCampaignWorker reconciles one campaign with Instantly: status, analytics
// snapshot, lead status mirror, and sends the webhook never told us about.
type SyncCampaignWorker struct {
	river.WorkerDefaults[campaign.SyncCampaignArgs]
	deps *Deps
}

// NewSyncCampaignWorker builds the worker.
func NewSyncCampaignWorker(deps *Deps) *SyncCampaignWorker { return &SyncCampaignWorker{deps: deps} }

// Work implements river.Worker.
func (w *SyncCampaignWorker) Work(ctx context.Context, rj *river.Job[campaign.SyncCampaignArgs]) error {
	d := w.deps
	camp, err := d.Store.GetCampaign(ctx, rj.Args.CampaignID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("campaign jobs: load campaign: %w", err)
	}
	if camp.InstantlyCampaignID == nil || camp.Status == campaign.CampaignArchived {
		return nil
	}
	client, err := d.Service.Instantly(ctx)
	if err != nil {
		//nolint:nilerr // Instantly is not configured, so there is nothing to
		// reconcile against; the job succeeds quietly rather than retrying for ever.
		return nil
	}
	run, err := d.startSync(ctx, campaign.SyncKindInstantlyCampaign, &camp.ID)
	if err != nil {
		return err
	}
	syncErr := w.sync(ctx, client, camp, run)
	run.finish(ctx, syncErr)
	if syncErr != nil {
		_ = d.Store.SetCampaignSyncError(ctx, dbgen.SetCampaignSyncErrorParams{ID: camp.ID, Error: campaign.Ptr(syncErr.Error())})
		if errors.Is(syncErr, provider.ErrRateLimited) {
			return snoozeFor(syncErr)
		}
		if fatal(syncErr) || rj.Attempt >= rj.MaxAttempts {
			return nil
		}
		return syncErr
	}
	return nil
}

func (w *SyncCampaignWorker) sync(ctx context.Context, client instantly.Client, camp dbgen.Campaign, run *syncRun) error {
	d := w.deps
	id := *camp.InstantlyCampaignID

	if err := d.Limiter.Wait(ctx); err != nil {
		return err
	}
	remote, err := client.GetCampaign(ctx, id)
	if errors.Is(err, provider.ErrNotFound) {
		reason := "the Instantly campaign was deleted"
		_ = d.Store.SetCampaignSyncError(ctx, dbgen.SetCampaignSyncErrorParams{ID: camp.ID, Error: &reason})
		run.details["deleted_at_provider"] = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("get campaign: %w", err)
	}
	var sendingStatus *string
	if err := d.Limiter.Wait(ctx); err != nil {
		return err
	}
	if st, err := client.SendingStatus(ctx, id); err == nil {
		sendingStatus = campaign.Optional(st.Status)
	}
	if err := d.Store.SetCampaignProviderState(ctx, dbgen.SetCampaignProviderStateParams{
		ID: camp.ID, InstantlyStatus: campaign.Ptr(campaign.Int32(remote.Status)), SendingStatus: sendingStatus,
		NotSendingStatus: int32Ptr(remote.NotSendingStatus),
	}); err != nil {
		return fmt.Errorf("store provider state: %w", err)
	}
	switch remote.Status {
	case instantly.CampaignStatusCompleted:
		_, _ = d.Store.MarkCampaignCompleted(ctx, camp.ID)
	case instantly.CampaignStatusPaused:
		if camp.Status == campaign.CampaignActive {
			_, _ = d.Store.MarkCampaignPaused(ctx, camp.ID)
		}
	case instantly.CampaignStatusActive:
		if camp.Status == campaign.CampaignPaused {
			_, _ = d.Store.MarkCampaignActive(ctx, camp.ID)
		}
	}

	// Analytics snapshot, labelled as Instantly's.
	if err := d.Limiter.Wait(ctx); err != nil {
		return err
	}
	if rows, err := client.CampaignAnalytics(ctx, []string{id}); err == nil && len(rows) > 0 {
		metrics := map[string]any{}
		raw, _ := json.Marshal(rows[0])
		_ = json.Unmarshal(raw, &metrics)
		if steps, err := client.CampaignStepAnalytics(ctx, id); err == nil {
			metrics["steps"] = steps
		}
		snap, _ := json.Marshal(metrics)
		if err := d.Store.UpsertCampaignAnalyticsSnapshot(ctx, dbgen.UpsertCampaignAnalyticsSnapshotParams{
			ID: newID(), CampaignID: camp.ID, Day: pgtype.Date{Time: d.Now().Truncate(24 * time.Hour), Valid: true},
			Source: campaign.SnapshotInstantly, Metrics: snap,
		}); err != nil {
			return fmt.Errorf("store snapshot: %w", err)
		}
	}

	// Lead mirror: status, interest, counters, and events the webhook missed.
	cursor := ""
	for {
		if err := d.Limiter.Wait(ctx); err != nil {
			return err
		}
		page, err := client.ListLeads(ctx, instantly.ListLeadsInput{CampaignID: id, Limit: 100, StartingAfter: cursor})
		if err != nil {
			return fmt.Errorf("list leads: %w", err)
		}
		for _, remoteLead := range page.Items {
			run.seen++
			changed, err := w.mirrorLead(ctx, camp, remoteLead)
			if err != nil {
				return err
			}
			if changed {
				run.updated++
			}
		}
		if page.NextStartingAfter == "" || len(page.Items) == 0 {
			break
		}
		cursor = page.NextStartingAfter
	}

	// Sends the webhook never delivered.
	if err := w.backfillSends(ctx, client, camp, run); err != nil {
		return err
	}
	if _, err := d.Store.RecomputeCampaignCounts(ctx, camp.ID); err != nil {
		return err
	}
	return nil
}

func (w *SyncCampaignWorker) mirrorLead(ctx context.Context, camp dbgen.Campaign, remote instantly.Lead) (bool, error) {
	d := w.deps
	contact, err := d.Store.GetContactByEmail(ctx, strings.ToLower(remote.Email))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // added at Instantly by hand; not ours to track
	}
	if err != nil {
		return false, err
	}
	lead, err := d.Store.GetCampaignLeadByContact(ctx, dbgen.GetCampaignLeadByContactParams{CampaignID: camp.ID, ContactID: contact.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	status := instantly.LeadStatus(remote.Status)
	switch lead.Status {
	case campaign.LeadSuppressed, campaign.LeadSkipped, campaign.LeadFailed, campaign.LeadPending, campaign.LeadPushing:
		status = lead.Status // our own states are not overwritten by the mirror
	case campaign.LeadReplied:
		if status == campaign.LeadActive || status == campaign.LeadCompleted || status == campaign.LeadPaused {
			status = campaign.LeadReplied
		}
	}
	var label *string
	var interest *int32
	if remote.InterestStatus != nil {
		label = campaign.Ptr(instantly.InterestLabel(*remote.InterestStatus))
		interest = campaign.Ptr(campaign.Int32(*remote.InterestStatus))
	}
	changed := lead.Status != status || lead.InstantlyLeadID == nil || int(lead.OpenCount) < remote.OpenCount ||
		int(lead.ReplyCount) < remote.ReplyCount || int(lead.ClickCount) < remote.ClickCount
	err = d.Store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := q.SetCampaignLeadProviderState(ctx, dbgen.SetCampaignLeadProviderStateParams{
			ID: lead.ID, InstantlyLeadID: campaign.Optional(remote.ID), InstantlyStatus: campaign.Ptr(campaign.Int32(remote.Status)),
			InterestStatus: interest, InterestLabel: label, Status: status,
			OpenCount: campaign.Int32(remote.OpenCount), ClickCount: campaign.Int32(remote.ClickCount), ReplyCount: campaign.Int32(remote.ReplyCount),
			LastContactedAt: remote.LastContact, LastOpenedAt: remote.LastOpen, LastClickedAt: remote.LastClick, LastRepliedAt: remote.LastReply,
		}); err != nil {
			return err
		}
		// Derive the stage events the webhook missed. Each is recorded once per
		// lead, and only when the counters say it happened.
		derive := func(cond bool, typ string, stage campaign.Stage, at *time.Time) error {
			if !cond {
				return nil
			}
			has, err := q.ContactHasEvent(ctx, dbgen.ContactHasEventParams{CampaignLeadID: uuid.NullUUID{UUID: lead.ID, Valid: true}, Type: typ})
			if err != nil || has {
				return err
			}
			c, err := q.GetContact(ctx, contact.ID)
			if err != nil {
				return err
			}
			before := campaign.Stage(c.LifecycleStage)
			updated, _, err := campaign.Advance(ctx, q, c, stage)
			if err != nil {
				return err
			}
			when := d.Now()
			if at != nil {
				when = *at
			}
			_, _, err = campaign.RecordEvent(ctx, q, campaign.EventInput{
				ContactID: contact.ID, CampaignID: &camp.ID, CampaignLeadID: &lead.ID, Type: typ, OccurredAt: when,
				Source: campaign.EventSourceReconcile, StageBefore: before, StageAfter: campaign.Stage(updated.LifecycleStage),
				Data: map[string]any{"derived_from": "instantly lead counters"},
			})
			return err
		}
		if err := derive(remote.LastContact != nil, campaign.EventSent, campaign.StageContacted, remote.LastContact); err != nil {
			return err
		}
		if err := derive(remote.OpenCount > 0, campaign.EventOpened, campaign.StageEngaged, remote.LastOpen); err != nil {
			return err
		}
		if err := derive(remote.ClickCount > 0, campaign.EventClicked, campaign.StageEngaged, remote.LastClick); err != nil {
			return err
		}
		if err := derive(remote.ReplyCount > 0, campaign.EventReplied, campaign.StageReplied, remote.LastReply); err != nil {
			return err
		}
		if remote.InterestStatus != nil && instantly.InterestPositive(*remote.InterestStatus) {
			if err := derive(true, campaign.EventInterested, campaign.StageInterested, remote.LastInterestChange); err != nil {
				return err
			}
			if send, err := q.GetLatestEmailSendForLead(ctx, lead.ID); err == nil && send.ReplyClassification == nil {
				_ = q.SetSendReplyClassification(ctx, dbgen.SetSendReplyClassificationParams{ID: send.ID, Classification: campaign.Ptr(campaign.ReplyPositive)})
			}
		}
		// Terminal states at Instantly become suppressions here, through the
		// same door as a webhook would.
		switch remote.Status {
		case instantly.LeadStatusBounced, instantly.LeadStatusUnsubscribed:
			reason := campaign.SuppressBounced
			if remote.Status == instantly.LeadStatusUnsubscribed {
				reason = campaign.SuppressUnsubscribed
			}
			return applySuppression(ctx, d, tx, q, contact.ID, lead.ID, reason, campaign.SuppressionSourceReconcile, "mirrored from Instantly lead status")
		}
		return nil
	})
	return changed, err
}

// backfillSends walks the sent emails Instantly holds and upserts any send row we
// do not have, marked as reconciled.
func (w *SyncCampaignWorker) backfillSends(ctx context.Context, client instantly.Client, camp dbgen.Campaign, run *syncRun) error {
	d := w.deps
	cursor := ""
	pages := 0
	for pages < 50 {
		if err := d.Limiter.Wait(ctx); err != nil {
			return err
		}
		page, err := client.ListEmails(ctx, instantly.ListEmailsInput{CampaignID: *camp.InstantlyCampaignID, EmailType: "sent", Limit: 100, StartingAfter: cursor, SortOrder: "desc"})
		if err != nil {
			return fmt.Errorf("list emails: %w", err)
		}
		pages++
		stop := false
		for _, email := range page.Items {
			if email.UEType != instantly.EmailTypeSentFromCampaign && email.UEType != instantly.EmailTypeSent {
				continue
			}
			if camp.LastSyncedAt != nil && email.TimestampEmail.Before(camp.LastSyncedAt.Add(-time.Duration(d.Config.SyncWindowDays)*24*time.Hour)) {
				stop = true
				break
			}
			contact, err := d.Store.GetContactByEmail(ctx, strings.ToLower(email.LeadEmail))
			if err != nil {
				continue
			}
			lead, err := d.Store.GetCampaignLeadByContact(ctx, dbgen.GetCampaignLeadByContactParams{CampaignID: camp.ID, ContactID: contact.ID})
			if err != nil {
				continue
			}
			step := 1
			if n, err := strconv.Atoi(strings.TrimSpace(email.Step)); err == nil && n > 0 {
				step = n
			}
			if _, err := d.Store.GetEmailSend(ctx, dbgen.GetEmailSendParams{CampaignLeadID: lead.ID, Step: campaign.Int32(step)}); err == nil {
				continue
			}
			err = d.Store.InTx(ctx, func(q *dbgen.Queries) error {
				params := dbgen.UpsertEmailSendParams{
					ID: newID(), CampaignLeadID: lead.ID, CampaignID: camp.ID, ContactID: contact.ID, Step: campaign.Int32(step),
					SendingAccountEmail: campaign.Optional(strings.ToLower(email.EAccount)), InstantlyEmailID: campaign.Optional(email.ID),
					ProviderMessageID: campaign.Optional(email.MessageID), SubjectSnapshot: campaign.Optional(email.Subject),
					SentAt: email.TimestampEmail.UTC(), Source: campaign.SendSourceReconcile,
				}
				if a, err := q.GetVariantAssignment(ctx, dbgen.GetVariantAssignmentParams{CampaignLeadID: lead.ID, Step: campaign.Int32(step)}); err == nil {
					params.AssignmentID = uuid.NullUUID{UUID: a.ID, Valid: true}
					params.VariantID = uuid.NullUUID{UUID: a.VariantID, Valid: true}
				}
				if acct, err := q.GetSendingAccountByEmail(ctx, strings.ToLower(email.EAccount)); err == nil {
					params.SendingAccountID = uuid.NullUUID{UUID: acct.ID, Valid: true}
				}
				send, err := q.UpsertEmailSend(ctx, params)
				if err != nil {
					return err
				}
				if err := q.RecordCampaignLeadContact(ctx, dbgen.RecordCampaignLeadContactParams{ID: lead.ID, At: &send.SentAt}); err != nil {
					return err
				}
				c, err := q.GetContact(ctx, contact.ID)
				if err != nil {
					return err
				}
				before := campaign.Stage(c.LifecycleStage)
				updated, _, err := campaign.Advance(ctx, q, c, campaign.StageContacted)
				if err != nil {
					return err
				}
				_, _, err = campaign.RecordEvent(ctx, q, campaign.EventInput{
					ContactID: contact.ID, CampaignID: &camp.ID, CampaignLeadID: &lead.ID, SendID: &send.ID, Step: &step,
					AssignmentID: campaign.UUIDPtr(send.AssignmentID), VariantID: campaign.UUIDPtr(send.VariantID),
					Type: campaign.EventSent, OccurredAt: send.SentAt, Source: campaign.EventSourceReconcile,
					StageBefore: before, StageAfter: campaign.Stage(updated.LifecycleStage),
					Data: map[string]any{"instantly_email_id": email.ID, "subject": email.Subject, "email_account": email.EAccount},
				})
				return err
			})
			if err != nil {
				return err
			}
			run.updated++
		}
		if stop || page.NextStartingAfter == "" || len(page.Items) == 0 {
			break
		}
		cursor = page.NextStartingAfter
	}
	return nil
}

func applySuppression(ctx context.Context, d *Deps, tx pgx.Tx, q *dbgen.Queries, contactID, leadID uuid.UUID, reason, source, note string) error {
	_, err := suppressionApply(ctx, q, contactID, leadID, reason, source, note, d, tx)
	return err
}

// SyncAllWorker fans out a sync for every live campaign and checks the webhook.
type SyncAllWorker struct {
	river.WorkerDefaults[campaign.SyncAllArgs]
	deps *Deps
}

// NewSyncAllWorker builds the worker.
func NewSyncAllWorker(deps *Deps) *SyncAllWorker { return &SyncAllWorker{deps: deps} }

// Work implements river.Worker.
func (w *SyncAllWorker) Work(ctx context.Context, _ *river.Job[campaign.SyncAllArgs]) error {
	d := w.deps
	rows, err := d.Store.ListCampaignsByStatus(ctx, []string{campaign.CampaignActive, campaign.CampaignPaused, campaign.CampaignLaunching})
	if err != nil {
		return fmt.Errorf("campaign jobs: list campaigns: %w", err)
	}
	for _, c := range rows {
		if c.InstantlyCampaignID == nil {
			continue
		}
		if err := d.enqueue(ctx, campaign.SyncCampaignArgs{CampaignID: c.ID}); err != nil {
			return err
		}
	}
	w.checkWebhook(ctx)
	return nil
}

// checkWebhook mirrors the webhook's status and resumes it if Instantly disabled it.
func (w *SyncAllWorker) checkWebhook(ctx context.Context) {
	d := w.deps
	settings, err := d.Store.GetCampaignSettings(ctx)
	if err != nil || settings.InstantlyWebhookID == nil {
		return
	}
	client, err := d.Service.Instantly(ctx)
	if err != nil {
		return
	}
	hook, err := client.GetWebhook(ctx, *settings.InstantlyWebhookID)
	if errors.Is(err, provider.ErrNotFound) {
		_ = d.Store.SetInstantlyWebhookStatus(ctx, dbgen.SetInstantlyWebhookStatusParams{Status: campaign.Ptr(int32(-1)), Error: campaign.Ptr("the webhook no longer exists at Instantly; register it again")})
		return
	}
	if err != nil {
		return
	}
	if hook.Status != nil && *hook.Status < 0 {
		d.Log.Warn("the Instantly webhook is in an error state; resuming it", "webhook_id", hook.ID)
		if err := client.ResumeWebhook(ctx, hook.ID); err == nil {
			_ = d.Store.SetInstantlyWebhookStatus(ctx, dbgen.SetInstantlyWebhookStatusParams{Status: campaign.Ptr(int32(1)), Error: campaign.Ptr("resumed after an error at " + d.Now().Format(time.RFC3339))})
			return
		}
	}
	_ = d.Store.SetInstantlyWebhookStatus(ctx, dbgen.SetInstantlyWebhookStatusParams{Status: int32Ptr(hook.Status)})
}

// SyncAccountsWorker mirrors the sending accounts and their daily analytics.
type SyncAccountsWorker struct {
	river.WorkerDefaults[campaign.SyncAccountsArgs]
	deps *Deps
}

// NewSyncAccountsWorker builds the worker.
func NewSyncAccountsWorker(deps *Deps) *SyncAccountsWorker { return &SyncAccountsWorker{deps: deps} }

// Work implements river.Worker.
func (w *SyncAccountsWorker) Work(ctx context.Context, rj *river.Job[campaign.SyncAccountsArgs]) error {
	d := w.deps
	client, err := d.Service.Instantly(ctx)
	if err != nil {
		//nolint:nilerr // not configured: nothing to reconcile against.
		return nil
	}
	run, err := d.startSync(ctx, campaign.SyncKindInstantlyAccounts, nil)
	if err != nil {
		return err
	}
	syncErr := w.sync(ctx, client, run)
	run.finish(ctx, syncErr)
	if syncErr != nil {
		if errors.Is(syncErr, provider.ErrRateLimited) {
			return snoozeFor(syncErr)
		}
		if fatal(syncErr) || rj.Attempt >= rj.MaxAttempts {
			return nil
		}
		return syncErr
	}
	return nil
}

func (w *SyncAccountsWorker) sync(ctx context.Context, client instantly.Client, run *syncRun) error {
	d := w.deps
	cursor := ""
	var emails []string
	for {
		if err := d.Limiter.Wait(ctx); err != nil {
			return err
		}
		page, err := client.ListAccounts(ctx, cursor)
		if err != nil {
			return fmt.Errorf("list accounts: %w", err)
		}
		for _, a := range page.Items {
			run.seen++
			raw, _ := json.Marshal(a.Raw)
			if len(raw) == 0 || string(raw) == "null" {
				raw = []byte("{}")
			}
			if _, err := d.Store.UpsertSendingAccount(ctx, dbgen.UpsertSendingAccountParams{
				ID: newID(), Email: strings.ToLower(a.Email), FirstName: campaign.Optional(a.FirstName), LastName: campaign.Optional(a.LastName),
				ProviderCode: int32Ptr(a.ProviderCode), Status: campaign.Int32(a.Status), WarmupStatus: int32Ptr(a.WarmupStatus),
				DailyLimit: int32Ptr(a.DailyLimit), SendingGap: int32Ptr(a.SendingGap), WarmupScore: int32Ptr(a.WarmupScore),
				StatusMessage: a.StatusMessage, TrackingDomain: a.TrackingDomainName, TrackingDomainStatus: a.TrackingDomainStatus,
				SetupPending: a.SetupPending, IsManaged: a.IsManagedAccount, Raw: raw,
			}); err != nil {
				return fmt.Errorf("upsert account: %w", err)
			}
			run.updated++
			emails = append(emails, strings.ToLower(a.Email))
		}
		if page.NextStartingAfter == "" || len(page.Items) == 0 {
			break
		}
		cursor = page.NextStartingAfter
	}
	if len(emails) == 0 {
		return nil
	}
	to := d.Now()
	from := to.AddDate(0, 0, -d.Config.SyncWindowDays)
	for start := 0; start < len(emails); start += 200 {
		end := min(start+200, len(emails))
		if err := d.Limiter.Wait(ctx); err != nil {
			return err
		}
		rows, err := client.AccountDailyAnalytics(ctx, from, to, emails[start:end])
		if err != nil {
			return fmt.Errorf("account analytics: %w", err)
		}
		for _, r := range rows {
			acct, err := d.Store.GetSendingAccountByEmail(ctx, strings.ToLower(r.EmailAccount))
			if err != nil {
				continue
			}
			day, err := time.Parse("2006-01-02", r.Date)
			if err != nil {
				continue
			}
			if err := d.Store.UpsertSendingAccountStatsDaily(ctx, dbgen.UpsertSendingAccountStatsDailyParams{
				SendingAccountID: acct.ID, Day: pgtype.Date{Time: day, Valid: true},
				Sent: campaign.Int32(r.Sent), Bounced: campaign.Int32(r.Bounced), Contacted: campaign.Int32(r.Contacted),
				NewLeadsContacted: campaign.Int32(r.NewLeadsContacted), Opened: campaign.Int32(r.Opened), UniqueOpened: campaign.Int32(r.UniqueOpened),
				Replies: campaign.Int32(r.Replies), UniqueReplies: campaign.Int32(r.UniqueReplies), Clicks: campaign.Int32(r.Clicks), UniqueClicks: campaign.Int32(r.UniqueClicks),
			}); err != nil {
				return fmt.Errorf("upsert account stats: %w", err)
			}
		}
	}
	return nil
}

// ReplayWebhookEventsWorker re-ingests deliveries Instantly reports as failed.
type ReplayWebhookEventsWorker struct {
	river.WorkerDefaults[campaign.ReplayWebhookEventsArgs]
	deps *Deps
}

// NewReplayWebhookEventsWorker builds the worker.
func NewReplayWebhookEventsWorker(deps *Deps) *ReplayWebhookEventsWorker {
	return &ReplayWebhookEventsWorker{deps: deps}
}

// Work implements river.Worker.
func (w *ReplayWebhookEventsWorker) Work(ctx context.Context, rj *river.Job[campaign.ReplayWebhookEventsArgs]) error {
	d := w.deps
	settings, err := d.Store.GetCampaignSettings(ctx)
	if err != nil || settings.InstantlyWebhookID == nil {
		//nolint:nilerr // no webhook is registered, so there is nothing to replay.
		return nil
	}
	client, err := d.Service.Instantly(ctx)
	if err != nil {
		//nolint:nilerr // not configured: nothing to reconcile against.
		return nil
	}
	run, err := d.startSync(ctx, campaign.SyncKindInstantlyReplay, nil)
	if err != nil {
		return err
	}
	from := d.Now().AddDate(0, 0, -d.Config.SyncWindowDays)
	if last, err := d.Store.LastSyncRun(ctx, campaign.SyncKindInstantlyReplay); err == nil && last.FinishedAt != nil {
		from = last.FinishedAt.AddDate(0, 0, -1)
	}
	failed := false
	cursor := ""
	var syncErr error
	for pages := 0; pages < 20; pages++ {
		if err := d.Limiter.Wait(ctx); err != nil {
			syncErr = err
			break
		}
		page, err := client.ListWebhookEvents(ctx, instantly.WebhookEventsInput{Success: &failed, From: from.Format("2006-01-02"), Limit: 100, StartingAfter: cursor})
		if err != nil {
			syncErr = fmt.Errorf("list webhook events: %w", err)
			break
		}
		for _, ev := range page.Items {
			run.seen++
			if ev.WillRetry || len(ev.Payload) == 0 {
				continue
			}
			body, err := json.Marshal(ev.Payload)
			if err != nil {
				continue
			}
			result, err := d.Service.IngestInstantly(ctx, body, campaign.ProviderEventReplay)
			if err != nil {
				syncErr = err
				break
			}
			if result.Stored {
				run.updated++
			}
		}
		if syncErr != nil || page.NextStartingAfter == "" || len(page.Items) == 0 {
			break
		}
		cursor = page.NextStartingAfter
	}
	run.finish(ctx, syncErr)
	if syncErr != nil && !fatal(syncErr) && rj.Attempt < rj.MaxAttempts {
		if errors.Is(syncErr, provider.ErrRateLimited) {
			return snoozeFor(syncErr)
		}
		return syncErr
	}
	return nil
}

// SyncLeadsFullWorker queues a full reconcile for every launched campaign.
type SyncLeadsFullWorker struct {
	river.WorkerDefaults[campaign.SyncLeadsFullArgs]
	deps *Deps
}

// NewSyncLeadsFullWorker builds the worker.
func NewSyncLeadsFullWorker(deps *Deps) *SyncLeadsFullWorker { return &SyncLeadsFullWorker{deps: deps} }

// Work implements river.Worker.
func (w *SyncLeadsFullWorker) Work(ctx context.Context, _ *river.Job[campaign.SyncLeadsFullArgs]) error {
	d := w.deps
	rows, err := d.Store.ListCampaignsByStatus(ctx, []string{campaign.CampaignActive, campaign.CampaignPaused, campaign.CampaignLaunching, campaign.CampaignCompleted})
	if err != nil {
		return fmt.Errorf("campaign jobs: list campaigns: %w", err)
	}
	run, err := d.startSync(ctx, campaign.SyncKindInstantlyLeadsFull, nil)
	if err != nil {
		return err
	}
	for _, c := range rows {
		if c.InstantlyCampaignID == nil {
			continue
		}
		run.seen++
		if err := d.enqueue(ctx, campaign.SyncCampaignArgs{CampaignID: c.ID}); err != nil {
			run.finish(ctx, err)
			return err
		}
	}
	run.finish(ctx, nil)
	return nil
}

func int32Ptr(v *int) *int32 {
	if v == nil {
		return nil
	}
	return campaign.Ptr(campaign.Int32(*v))
}
