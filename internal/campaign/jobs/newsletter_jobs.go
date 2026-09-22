package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/consent"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/provider/mailchimp"
	"github.com/bory/karvon-be/internal/campaign/suppression"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// NewsletterPushWorker pushes one subscription to Mailchimp.
//
// The consent gate runs again here, at execution time, so a contact suppressed
// or revoked between the click and the job never reaches Mailchimp, and a
// "subscribed" request is downgraded to "pending" unless the audience allows
// single opt-in and a consent record exists.
type NewsletterPushWorker struct {
	river.WorkerDefaults[campaign.NewsletterPushArgs]
	deps *Deps
}

// NewNewsletterPushWorker builds the worker.
func NewNewsletterPushWorker(deps *Deps) *NewsletterPushWorker {
	return &NewsletterPushWorker{deps: deps}
}

// Work implements river.Worker.
func (w *NewsletterPushWorker) Work(ctx context.Context, rj *river.Job[campaign.NewsletterPushArgs]) error {
	d := w.deps
	sub, err := d.Store.ClaimNewsletterSubscription(ctx, rj.Args.SubscriptionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already synced, or being synced by someone else
	}
	if err != nil {
		return fmt.Errorf("campaign jobs: claim subscription: %w", err)
	}
	contact, err := d.Store.GetContact(ctx, sub.ContactID)
	if err != nil {
		return w.giveUp(ctx, sub, "contact missing: "+err.Error())
	}
	audience, err := d.Store.GetNewsletterAudience(ctx, sub.AudienceID)
	if err != nil {
		return w.giveUp(ctx, sub, "audience missing: "+err.Error())
	}
	client, err := d.Service.Mailchimp(ctx)
	if err != nil {
		return w.giveUp(ctx, sub, "Mailchimp is not configured: "+err.Error())
	}

	if rj.Args.Action == campaign.NewsletterActionUnsubscribe {
		return w.unsubscribe(ctx, rj, client, sub, contact, audience)
	}

	active, err := d.Store.GetActiveConsent(ctx, contact.ID)
	hasConsent := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("campaign jobs: load consent: %w", err)
	}
	decision := consent.Evaluate(consent.Input{
		Suppressed: contact.SuppressedAt != nil, SuppressionReason: campaign.Deref(contact.SuppressionReason),
		Stage: campaign.Stage(contact.LifecycleStage), HasActiveConsent: hasConsent, ConsentSource: active.Source,
		AllowSingleOptIn: audience.AllowSingleOptIn,
	})
	if !decision.Eligible {
		d.Log.Info("newsletter push refused at execution time", "subscription_id", sub.ID, "reason", decision.Reason)
		_, err := d.Store.MarkNewsletterSubscriptionSynced(ctx, dbgen.MarkNewsletterSubscriptionSyncedParams{
			ID: sub.ID, Status: campaign.SubError, RequestedStatus: sub.RequestedStatus,
		})
		if err != nil {
			return err
		}
		return d.Store.FailNewsletterSubscription(ctx, dbgen.FailNewsletterSubscriptionParams{ID: sub.ID, Error: campaign.Ptr("refused: " + decision.Explanation)})
	}

	in := mailchimp.MemberInput{
		EmailAddress: contact.Email, StatusIfNew: decision.RequestedStatus, EmailType: "html",
		MergeFields: map[string]string{}, Tags: sub.Tags,
	}
	if v := campaign.Deref(contact.FirstName); v != "" {
		in.MergeFields["FNAME"] = v
	}
	if v := campaign.Deref(contact.LastName); v != "" {
		in.MergeFields["LNAME"] = v
	}
	if sub.Status == campaign.SubLocalPending || sub.Status == campaign.SubError {
		in.Status = decision.RequestedStatus
	}
	if decision.RequestedStatus == campaign.RequestSubscribed && hasConsent {
		in.TimestampOpt = active.CapturedAt.UTC().Format("2006-01-02 15:04:05")
	}
	if err := d.Limiter.Wait(ctx); err != nil {
		return err
	}
	member, err := client.UpsertMember(ctx, audience.MailchimpListID, in)
	if err != nil {
		return w.providerError(ctx, rj, sub, err)
	}
	return w.stored(ctx, sub, contact, audience, member, decision.RequestedStatus)
}

func (w *NewsletterPushWorker) stored(ctx context.Context, sub dbgen.NewsletterSubscription, contact dbgen.Contact,
	audience dbgen.NewsletterAudience, member mailchimp.Member, requested string,
) error {
	d := w.deps
	status := mailchimp.LocalStatus(member.Status)
	return d.Store.InTx(ctx, func(q *dbgen.Queries) error {
		if _, err := q.MarkNewsletterSubscriptionSynced(ctx, dbgen.MarkNewsletterSubscriptionSyncedParams{
			ID: sub.ID, Status: status, RequestedStatus: requested, SubscriberHash: campaign.Optional(member.ID),
			UniqueEmailID: campaign.Optional(member.UniqueEmailID), MailchimpContactID: campaign.Optional(member.ContactID),
			WebID: nonZero(member.WebID),
		}); err != nil {
			return fmt.Errorf("mark synced: %w", err)
		}
		c, err := q.GetContact(ctx, contact.ID)
		if err != nil {
			return err
		}
		before := campaign.Stage(c.LifecycleStage)
		var typ string
		var stage campaign.Stage
		switch status {
		case campaign.SubSubscribed:
			typ, stage = campaign.EventNewsletterSubscribed, campaign.StageMailchimpSubscribed
		case campaign.SubPending:
			typ, stage = campaign.EventNewsletterPending, campaign.StageMailchimpPending
		default:
			typ, stage = campaign.EventNote, before
		}
		updated, _, err := campaign.Advance(ctx, q, c, stage)
		if err != nil {
			return err
		}
		_, _, err = campaign.RecordEvent(ctx, q, campaign.EventInput{
			ContactID: contact.ID, Type: typ, OccurredAt: d.Now(), Source: campaign.EventSourceSystem,
			StageBefore: before, StageAfter: campaign.Stage(updated.LifecycleStage),
			Data: map[string]any{"audience_id": audience.ID, "list_id": audience.MailchimpListID, "mailchimp_status": member.Status,
				"requested_status": requested, "subscriber_hash": member.ID},
		})
		return err
	})
}

func (w *NewsletterPushWorker) unsubscribe(ctx context.Context, rj *river.Job[campaign.NewsletterPushArgs], client mailchimp.Client,
	sub dbgen.NewsletterSubscription, contact dbgen.Contact, audience dbgen.NewsletterAudience,
) error {
	d := w.deps
	if sub.Status == campaign.SubCleaned || sub.Status == campaign.SubUnsubscribed || sub.Status == campaign.SubLocalPending || sub.Status == campaign.SubError {
		// Nothing at Mailchimp to undo, or Mailchimp already did it.
		_, err := d.Store.MarkNewsletterSubscriptionSynced(ctx, dbgen.MarkNewsletterSubscriptionSyncedParams{
			ID: sub.ID, Status: firstNonEmpty(sub.Status, campaign.SubUnsubscribed), RequestedStatus: campaign.RequestUnsubscribed,
		})
		return err
	}
	if err := d.Limiter.Wait(ctx); err != nil {
		return err
	}
	hash := campaign.Deref(sub.SubscriberHash)
	if hash == "" {
		hash = mailchimp.SubscriberHash(contact.Email)
	}
	member, err := client.UpsertMember(ctx, audience.MailchimpListID, mailchimp.MemberInput{EmailAddress: contact.Email, Status: mailchimp.StatusUnsubscribed, StatusIfNew: mailchimp.StatusUnsubscribed})
	if err != nil {
		if errors.Is(err, provider.ErrComplianceState) || errors.Is(err, provider.ErrNotFound) {
			member = mailchimp.Member{ID: hash, Status: mailchimp.StatusUnsubscribed}
		} else {
			return w.providerError(ctx, rj, sub, err)
		}
	}
	return d.Store.InTx(ctx, func(q *dbgen.Queries) error {
		if _, err := q.MarkNewsletterSubscriptionSynced(ctx, dbgen.MarkNewsletterSubscriptionSyncedParams{
			ID: sub.ID, Status: campaign.SubUnsubscribed, RequestedStatus: campaign.RequestUnsubscribed,
		}); err != nil {
			return err
		}
		c, err := q.GetContact(ctx, contact.ID)
		if err != nil {
			return err
		}
		cur := campaign.Stage(c.LifecycleStage)
		_, _, err = campaign.RecordEvent(ctx, q, campaign.EventInput{
			ContactID: contact.ID, Type: campaign.EventNewsletterUnsubscribed, OccurredAt: d.Now(), Source: campaign.EventSourceSystem,
			StageBefore: cur, StageAfter: cur, Data: map[string]any{"audience_id": audience.ID, "mailchimp_status": member.Status, "by": "karvon"},
		})
		return err
	})
}

func (w *NewsletterPushWorker) providerError(ctx context.Context, rj *river.Job[campaign.NewsletterPushArgs], sub dbgen.NewsletterSubscription, err error) error {
	d := w.deps
	switch {
	case errors.Is(err, provider.ErrComplianceState):
		// Only the member can undo that, by re-confirming through Mailchimp.
		_, uerr := d.Store.MarkNewsletterSubscriptionSynced(ctx, dbgen.MarkNewsletterSubscriptionSyncedParams{
			ID: sub.ID, Status: campaign.SubComplianceBlocked, RequestedStatus: sub.RequestedStatus,
		})
		if uerr != nil {
			return uerr
		}
		_ = d.Store.FailNewsletterSubscription(ctx, dbgen.FailNewsletterSubscriptionParams{ID: sub.ID, Error: campaign.Ptr(err.Error())})
		_, _ = d.Store.MarkNewsletterSubscriptionSynced(ctx, dbgen.MarkNewsletterSubscriptionSyncedParams{ID: sub.ID, Status: campaign.SubComplianceBlocked, RequestedStatus: sub.RequestedStatus})
		return nil
	case errors.Is(err, provider.ErrRateLimited):
		if _, rerr := d.Store.RequeueNewsletterSubscription(ctx, dbgen.RequeueNewsletterSubscriptionParams{ID: sub.ID, Error: campaign.Ptr("rate limited")}); rerr != nil {
			return rerr
		}
		return snoozeFor(err)
	case fatal(err):
		return w.giveUp(ctx, sub, err.Error())
	}
	if rj.Attempt >= rj.MaxAttempts {
		return w.giveUp(ctx, sub, err.Error())
	}
	if _, rerr := d.Store.RequeueNewsletterSubscription(ctx, dbgen.RequeueNewsletterSubscriptionParams{ID: sub.ID, Error: campaign.Ptr(err.Error())}); rerr != nil {
		return rerr
	}
	return fmt.Errorf("campaign jobs: mailchimp: %w", err)
}

func (w *NewsletterPushWorker) giveUp(ctx context.Context, sub dbgen.NewsletterSubscription, reason string) error {
	w.deps.Log.Warn("newsletter push failed", "subscription_id", sub.ID, "reason", reason)
	return w.deps.Store.FailNewsletterSubscription(ctx, dbgen.FailNewsletterSubscriptionParams{ID: sub.ID, Error: &reason})
}

// NewsletterSyncMembersWorker mirrors member status from Mailchimp for
// subscriptions we pushed, and suppresses contacts Mailchimp lost.
type NewsletterSyncMembersWorker struct {
	river.WorkerDefaults[campaign.NewsletterSyncMembersArgs]
	deps *Deps
}

// NewNewsletterSyncMembersWorker builds the worker.
func NewNewsletterSyncMembersWorker(deps *Deps) *NewsletterSyncMembersWorker {
	return &NewsletterSyncMembersWorker{deps: deps}
}

// Work implements river.Worker.
func (w *NewsletterSyncMembersWorker) Work(ctx context.Context, rj *river.Job[campaign.NewsletterSyncMembersArgs]) error {
	d := w.deps
	client, err := d.Service.Mailchimp(ctx)
	if err != nil {
		//nolint:nilerr // Mailchimp is not configured, so there is nothing to
		// mirror; the job succeeds quietly rather than retrying for ever.
		return nil
	}
	run, err := d.startSync(ctx, campaign.SyncKindMailchimpMembers, nil)
	if err != nil {
		return err
	}
	subs, err := d.Store.ListNewsletterSubscriptionsToReconcile(ctx, dbgen.ListNewsletterSubscriptionsToReconcileParams{Before: campaign.Ptr(d.Now().Add(-10 * time.Minute)), Lim: 500})
	if err != nil {
		run.finish(ctx, err)
		return err
	}
	var syncErr error
	for _, sub := range subs {
		run.seen++
		audience, err := d.Store.GetNewsletterAudience(ctx, sub.AudienceID)
		if err != nil {
			continue
		}
		contact, err := d.Store.GetContact(ctx, sub.ContactID)
		if err != nil {
			continue
		}
		hash := campaign.Deref(sub.SubscriberHash)
		if hash == "" {
			hash = mailchimp.SubscriberHash(contact.Email)
		}
		if err := d.Limiter.Wait(ctx); err != nil {
			syncErr = err
			break
		}
		member, err := client.GetMember(ctx, audience.MailchimpListID, hash)
		if errors.Is(err, provider.ErrNotFound) {
			_, _ = d.Store.MirrorNewsletterSubscriptionStatus(ctx, dbgen.MirrorNewsletterSubscriptionStatusParams{ID: sub.ID, Status: campaign.SubArchived, At: campaign.Ptr(d.Now())})
			continue
		}
		if err != nil {
			syncErr = fmt.Errorf("get member: %w", err)
			break
		}
		status := mailchimp.LocalStatus(member.Status)
		if status == sub.Status {
			_, _ = d.Store.MirrorNewsletterSubscriptionStatus(ctx, dbgen.MirrorNewsletterSubscriptionStatusParams{ID: sub.ID, Status: status, At: campaign.Ptr(d.Now())})
			continue
		}
		run.updated++
		err = d.Store.InTxRaw(ctx, func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			at := d.Now()
			if member.LastChanged != nil {
				at = *member.LastChanged
			}
			if _, err := q.MirrorNewsletterSubscriptionStatus(ctx, dbgen.MirrorNewsletterSubscriptionStatusParams{
				ID: sub.ID, Status: status, UnsubscribeReason: campaign.Optional(member.UnsubscribeReason),
				UniqueEmailID: campaign.Optional(member.UniqueEmailID), MailchimpContactID: campaign.Optional(member.ContactID), WebID: nonZero(member.WebID), At: &at,
			}); err != nil {
				return err
			}
			c, err := q.GetContact(ctx, contact.ID)
			if err != nil {
				return err
			}
			cur := campaign.Stage(c.LifecycleStage)
			data := map[string]any{"audience_id": audience.ID, "mailchimp_status": member.Status, "reason": member.UnsubscribeReason}
			switch status {
			case campaign.SubSubscribed:
				updated, _, err := campaign.Advance(ctx, q, c, campaign.StageMailchimpSubscribed)
				if err != nil {
					return err
				}
				_, _, err = campaign.RecordEvent(ctx, q, campaign.EventInput{ContactID: contact.ID, Type: campaign.EventNewsletterSubscribed, OccurredAt: at,
					Source: campaign.EventSourceReconcile, StageBefore: cur, StageAfter: campaign.Stage(updated.LifecycleStage), Data: data})
				return err
			case campaign.SubUnsubscribed, campaign.SubCleaned:
				typ, reason := campaign.EventNewsletterUnsubscribed, campaign.SuppressUnsubscribed
				if status == campaign.SubCleaned {
					typ, reason = campaign.EventNewsletterCleaned, campaign.SuppressBounced
				}
				if _, _, err := campaign.RecordEvent(ctx, q, campaign.EventInput{ContactID: contact.ID, Type: typ, OccurredAt: at,
					Source: campaign.EventSourceReconcile, StageBefore: cur, StageAfter: campaign.StageForSuppression(reason), Data: data}); err != nil {
					return err
				}
				_, err := suppression.Apply(ctx, q, suppression.ApplyInput{
					ContactID: contact.ID, Reason: reason, Source: campaign.SuppressionSourceReconcile, Note: "mirrored from Mailchimp member status",
					OccurredAt: at, Enqueue: func(ctx context.Context, args river.JobArgs) error { return d.enqueueTx(ctx, tx, args) },
				})
				return err
			default:
				_, _, err := campaign.RecordEvent(ctx, q, campaign.EventInput{ContactID: contact.ID, Type: campaign.EventNote, OccurredAt: at,
					Source: campaign.EventSourceReconcile, StageBefore: cur, StageAfter: cur, Data: data})
				return err
			}
		})
		if err != nil {
			syncErr = err
			break
		}
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

// NewsletterSyncAudiencesWorker mirrors the audiences.
type NewsletterSyncAudiencesWorker struct {
	river.WorkerDefaults[campaign.NewsletterSyncAudiencesArgs]
	deps *Deps
}

// NewNewsletterSyncAudiencesWorker builds the worker.
func NewNewsletterSyncAudiencesWorker(deps *Deps) *NewsletterSyncAudiencesWorker {
	return &NewsletterSyncAudiencesWorker{deps: deps}
}

// Work implements river.Worker.
func (w *NewsletterSyncAudiencesWorker) Work(ctx context.Context, rj *river.Job[campaign.NewsletterSyncAudiencesArgs]) error {
	d := w.deps
	client, err := d.Service.Mailchimp(ctx)
	if err != nil {
		//nolint:nilerr // Mailchimp is not configured, so there is nothing to
		// mirror; the job succeeds quietly rather than retrying for ever.
		return nil
	}
	run, err := d.startSync(ctx, campaign.SyncKindMailchimpAudiences, nil)
	if err != nil {
		return err
	}
	if err := d.Limiter.Wait(ctx); err != nil {
		run.finish(ctx, err)
		return err
	}
	audiences, err := client.ListAudiences(ctx)
	if err != nil {
		run.finish(ctx, err)
		if errors.Is(err, provider.ErrRateLimited) {
			return snoozeFor(err)
		}
		if fatal(err) || rj.Attempt >= rj.MaxAttempts {
			return nil
		}
		return err
	}
	for _, a := range audiences {
		run.seen++
		stats, _ := json.Marshal(a.Stats)
		if _, err := d.Store.UpsertNewsletterAudience(ctx, dbgen.UpsertNewsletterAudienceParams{
			ID: newID(), MailchimpListID: a.ID, Name: a.Name, DoubleOptin: a.DoubleOptin,
			MemberCount: campaign.Ptr(campaign.Int32(a.Stats.MemberCount)), Stats: stats,
		}); err != nil {
			run.finish(ctx, err)
			return err
		}
		run.updated++
	}
	// One audience and no default yet: it is the default.
	if _, err := d.Store.GetDefaultNewsletterAudience(ctx); errors.Is(err, pgx.ErrNoRows) {
		if rows, err := d.Store.ListNewsletterAudiences(ctx); err == nil && len(rows) == 1 {
			if _, err := d.Store.SetDefaultNewsletterAudience(ctx, rows[0].ID); err == nil {
				_ = d.Store.SetDefaultAudience(ctx, uuid.NullUUID{UUID: rows[0].ID, Valid: true})
			}
		}
	}
	run.finish(ctx, nil)
	return nil
}

func nonZero(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
