package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/campaign/provider/mailchimp"
	"github.com/bory/karvon-be/internal/campaign/suppression"
	"github.com/bory/karvon-be/internal/campaign/webhook"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// ProcessEventWorker applies one stored provider event to the campaign state.
//
// The whole application is one transaction, keyed by the provider event id, and a
// timeline entry per (event, type) is unique, so a job that ran twice — or a
// webhook delivered twice — changes nothing the second time.
type ProcessEventWorker struct {
	river.WorkerDefaults[campaign.ProcessEventArgs]
	deps *Deps
}

// NewProcessEventWorker builds the worker.
func NewProcessEventWorker(deps *Deps) *ProcessEventWorker { return &ProcessEventWorker{deps: deps} }

// Work implements river.Worker.
func (w *ProcessEventWorker) Work(ctx context.Context, rj *river.Job[campaign.ProcessEventArgs]) error {
	d := w.deps
	ev, err := d.Store.GetProviderEvent(ctx, rj.Args.ProviderEventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("campaign jobs: load event: %w", err)
	}
	if ev.ProcessedAt != nil {
		return nil
	}
	var outcome outcome
	switch ev.Provider {
	case campaign.ProviderInstantly:
		outcome, err = w.applyInstantly(ctx, ev)
	case campaign.ProviderMailchimp:
		outcome, err = w.applyMailchimp(ctx, ev)
	default:
		outcome = outcome.withError("unknown provider")
	}
	if err != nil {
		_ = d.Store.MarkProviderEventAttempt(ctx, dbgen.MarkProviderEventAttemptParams{ID: ev.ID, Error: campaign.Ptr(err.Error())})
		if rj.Attempt >= rj.MaxAttempts {
			d.Log.Error("giving up on a provider event", "event_id", ev.ID, "type", ev.EventType, "error", err)
			return d.Store.MarkProviderEventProcessed(ctx, dbgen.MarkProviderEventProcessedParams{ID: ev.ID, Error: campaign.Ptr("gave up: " + err.Error())})
		}
		return err
	}
	return d.Store.MarkProviderEventProcessed(ctx, dbgen.MarkProviderEventProcessedParams{
		ID: ev.ID, Error: outcome.err, CampaignID: campaign.NullUUID(outcome.campaignID),
		ContactID: campaign.NullUUID(outcome.contactID), CampaignLeadID: campaign.NullUUID(outcome.leadID),
	})
}

type outcome struct {
	campaignID *uuid.UUID
	contactID  *uuid.UUID
	leadID     *uuid.UUID
	err        *string
}

func (o outcome) withError(msg string) outcome {
	o.err = &msg
	return o
}

/* ------------------------------------------------------------- instantly */

func (w *ProcessEventWorker) applyInstantly(ctx context.Context, row dbgen.ProviderEvent) (outcome, error) {
	d := w.deps
	ev, err := webhook.ParseInstantly(row.Raw)
	if err != nil {
		//nolint:nilerr // a payload we cannot read will never become readable, so it
		// is recorded with its reason and closed rather than retried.
		return outcome{}.withError("unparseable: " + err.Error()), nil
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = row.ReceivedAt
	}
	var out outcome

	if ev.EventType == campaign.InstantlyAccountError {
		if ev.EmailAccount != "" {
			if acct, err := d.Store.GetSendingAccountByEmail(ctx, strings.ToLower(ev.EmailAccount)); err == nil {
				d.Log.Warn("Instantly reports a sending account error", "account", acct.Email, "campaign", ev.CampaignName)
			}
		}
		return out.withError("account_error noted"), nil
	}

	camp, err := d.Store.GetCampaignByInstantlyID(ctx, campaign.Optional(ev.CampaignID))
	if errors.Is(err, pgx.ErrNoRows) {
		return out.withError("unmatched: campaign " + ev.CampaignID + " is not ours"), nil
	}
	if err != nil {
		return out, fmt.Errorf("load campaign: %w", err)
	}
	out.campaignID = &camp.ID

	if ev.EventType == campaign.InstantlyCampaignCompleted {
		if _, err := d.Store.MarkCampaignCompleted(ctx, camp.ID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return out, fmt.Errorf("mark completed: %w", err)
		}
		return out, nil
	}
	if ev.LeadEmail == "" {
		return out.withError("unmatched: no lead_email"), nil
	}
	contact, err := d.Store.GetContactByEmail(ctx, ev.LeadEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		return out.withError("unmatched: contact " + ev.LeadEmail + " is unknown"), nil
	}
	if err != nil {
		return out, fmt.Errorf("load contact: %w", err)
	}
	out.contactID = &contact.ID
	lead, err := d.Store.GetCampaignLeadByContact(ctx, dbgen.GetCampaignLeadByContactParams{CampaignID: camp.ID, ContactID: contact.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return out.withError("unmatched: contact is not a lead of this campaign"), nil
	}
	if err != nil {
		return out, fmt.Errorf("load lead: %w", err)
	}
	out.leadID = &lead.ID

	err = d.Store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		a := applier{d: d, q: q, tx: tx, row: row, camp: camp, contact: contact, lead: lead, at: ev.Timestamp.UTC()}
		return a.apply(ctx, ev)
	})
	if err != nil {
		return out, err
	}
	return out, nil
}

// applier holds one event's context inside the transaction.
type applier struct {
	d       *Deps
	q       *dbgen.Queries
	tx      pgx.Tx
	row     dbgen.ProviderEvent
	camp    dbgen.Campaign
	contact dbgen.Contact
	lead    dbgen.CampaignLead
	at      time.Time
}

func (a *applier) event(ctx context.Context, typ string, send *dbgen.EmailSend, step *int, before, after campaign.Stage, data map[string]any) error {
	in := campaign.EventInput{
		ContactID: a.contact.ID, CampaignID: &a.camp.ID, CampaignLeadID: &a.lead.ID, Type: typ, OccurredAt: a.at,
		Source: campaign.EventSourceInstantly, ProviderEventID: &a.row.ID, StageBefore: before, StageAfter: after, Data: data,
	}
	if a.row.Source == campaign.ProviderEventReplay {
		in.Source = campaign.EventSourceReconcile
	}
	if step != nil {
		in.Step = step
	}
	if send != nil {
		in.SendID = &send.ID
		in.AssignmentID = campaign.UUIDPtr(send.AssignmentID)
		in.VariantID = campaign.UUIDPtr(send.VariantID)
	}
	_, _, err := campaign.RecordEvent(ctx, a.q, in)
	return err
}

func (a *applier) advance(ctx context.Context, next campaign.Stage) (campaign.Stage, campaign.Stage, error) {
	before := campaign.Stage(a.contact.LifecycleStage)
	updated, _, err := campaign.Advance(ctx, a.q, a.contact, next)
	if err != nil {
		return before, before, err
	}
	a.contact = updated
	return before, campaign.Stage(updated.LifecycleStage), nil
}

// send finds (or, when the sent event was missed, stubs) the send an engagement
// belongs to. A stub is marked as reconciled so its provenance stays visible.
func (a *applier) send(ctx context.Context, ev webhook.InstantlyEvent, create bool) (*dbgen.EmailSend, int, error) {
	step := ev.Step
	if step <= 0 {
		latest, err := a.q.GetLatestEmailSendForLead(ctx, a.lead.ID)
		if err == nil {
			return &latest, int(latest.Step), nil
		}
		step = 1
	}
	existing, err := a.q.GetEmailSend(ctx, dbgen.GetEmailSendParams{CampaignLeadID: a.lead.ID, Step: campaign.Int32(step)})
	if err == nil {
		return &existing, step, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, step, fmt.Errorf("load send: %w", err)
	}
	if !create {
		return nil, step, nil
	}
	source := campaign.SendSourceWebhook
	if ev.EventType != campaign.InstantlyEmailSent {
		source = campaign.SendSourceReconcile
	}
	params := dbgen.UpsertEmailSendParams{
		ID: newID(), CampaignLeadID: a.lead.ID, CampaignID: a.camp.ID, ContactID: a.contact.ID, Step: campaign.Int32(step),
		SendingAccountEmail: campaign.Optional(strings.ToLower(ev.EmailAccount)), InstantlyEmailID: campaign.Optional(ev.EmailID),
		SubjectSnapshot: campaign.Optional(ev.EmailSubject), SentAt: a.at, Source: source,
	}
	if assignment, err := a.q.GetVariantAssignment(ctx, dbgen.GetVariantAssignmentParams{CampaignLeadID: a.lead.ID, Step: campaign.Int32(step)}); err == nil {
		params.AssignmentID = uuid.NullUUID{UUID: assignment.ID, Valid: true}
		params.VariantID = uuid.NullUUID{UUID: assignment.VariantID, Valid: true}
		if params.SubjectSnapshot == nil {
			params.SubjectSnapshot = &assignment.RenderedSubject
		}
	}
	if ev.EmailAccount != "" {
		if acct, err := a.q.GetSendingAccountByEmail(ctx, strings.ToLower(ev.EmailAccount)); err == nil {
			params.SendingAccountID = uuid.NullUUID{UUID: acct.ID, Valid: true}
		}
	}
	created, err := a.q.UpsertEmailSend(ctx, params)
	if err != nil {
		return nil, step, fmt.Errorf("upsert send: %w", err)
	}
	return &created, step, nil
}

func (a *applier) apply(ctx context.Context, ev webhook.InstantlyEvent) error {
	data := map[string]any{"instantly_event": ev.EventType, "email_account": ev.EmailAccount, "step": ev.Step, "variant": ev.Variant}
	switch ev.EventType {
	case campaign.InstantlyEmailSent:
		send, step, err := a.send(ctx, ev, true)
		if err != nil {
			return err
		}
		if err := a.q.RecordCampaignLeadContact(ctx, dbgen.RecordCampaignLeadContactParams{ID: a.lead.ID, At: &a.at}); err != nil {
			return err
		}
		before, after, err := a.advance(ctx, campaign.StageContacted)
		if err != nil {
			return err
		}
		data["subject"] = ev.EmailSubject
		return a.event(ctx, campaign.EventSent, send, &step, before, after, data)

	case campaign.InstantlyEmailOpened:
		send, step, err := a.send(ctx, ev, true)
		if err != nil {
			return err
		}
		if err := a.q.RecordSendOpen(ctx, dbgen.RecordSendOpenParams{ID: send.ID, At: &a.at}); err != nil {
			return err
		}
		if err := a.q.RecordCampaignLeadOpen(ctx, dbgen.RecordCampaignLeadOpenParams{ID: a.lead.ID, At: &a.at}); err != nil {
			return err
		}
		before, after, err := a.advance(ctx, campaign.StageEngaged)
		if err != nil {
			return err
		}
		return a.event(ctx, campaign.EventOpened, send, &step, before, after, data)

	case campaign.InstantlyEmailLinkClicked:
		send, step, err := a.send(ctx, ev, true)
		if err != nil {
			return err
		}
		if err := a.q.RecordSendClick(ctx, dbgen.RecordSendClickParams{ID: send.ID, At: &a.at}); err != nil {
			return err
		}
		if err := a.q.RecordCampaignLeadClick(ctx, dbgen.RecordCampaignLeadClickParams{ID: a.lead.ID, At: &a.at}); err != nil {
			return err
		}
		before, after, err := a.advance(ctx, campaign.StageEngaged)
		if err != nil {
			return err
		}
		return a.event(ctx, campaign.EventClicked, send, &step, before, after, data)

	case campaign.InstantlyReplyReceived:
		send, step, err := a.send(ctx, ev, true)
		if err != nil {
			return err
		}
		if err := a.q.RecordSendReply(ctx, dbgen.RecordSendReplyParams{ID: send.ID, At: &a.at, Classification: campaign.Ptr(campaign.ReplyUnknown)}); err != nil {
			return err
		}
		if err := a.q.RecordCampaignLeadReply(ctx, dbgen.RecordCampaignLeadReplyParams{ID: a.lead.ID, At: &a.at}); err != nil {
			return err
		}
		before, after, err := a.advance(ctx, campaign.StageReplied)
		if err != nil {
			return err
		}
		data["reply_snippet"] = snippet(ev.ReplyTextSnippet, ev.ReplyText)
		data["email_id"] = ev.EmailID
		return a.event(ctx, campaign.EventReplied, send, &step, before, after, data)

	case campaign.InstantlyAutoReplyReceived, campaign.InstantlyLeadOutOfOffice:
		send, step, err := a.send(ctx, ev, false)
		if err != nil {
			return err
		}
		classification := campaign.ReplyAutoReply
		if ev.EventType == campaign.InstantlyLeadOutOfOffice {
			classification = campaign.ReplyOutOfOffice
		}
		if send != nil {
			if err := a.q.RecordSendReply(ctx, dbgen.RecordSendReplyParams{ID: send.ID, At: &a.at, Classification: &classification}); err != nil {
				return err
			}
		}
		data["classification"] = classification
		cur := campaign.Stage(a.contact.LifecycleStage)
		return a.event(ctx, campaign.EventAutoReplied, send, &step, cur, cur, data)

	case campaign.InstantlyLeadInterested, campaign.InstantlyLeadMeetingBooked, campaign.InstantlyLeadMeetingCompleted, campaign.InstantlyLeadClosed:
		send, step, err := a.send(ctx, ev, false)
		if err != nil {
			return err
		}
		if send != nil {
			if err := a.q.SetSendReplyClassification(ctx, dbgen.SetSendReplyClassificationParams{ID: send.ID, Classification: campaign.Ptr(campaign.ReplyPositive)}); err != nil {
				return err
			}
		}
		code, label := interestFor(ev.EventType)
		if err := a.q.SetCampaignLeadInterest(ctx, dbgen.SetCampaignLeadInterestParams{ID: a.lead.ID, InterestStatus: campaign.Ptr(campaign.Int32(code)), InterestLabel: &label}); err != nil {
			return err
		}
		before, after, err := a.advance(ctx, campaign.StageInterested)
		if err != nil {
			return err
		}
		typ := campaign.EventInterested
		if ev.EventType == campaign.InstantlyLeadMeetingBooked || ev.EventType == campaign.InstantlyLeadMeetingCompleted {
			typ = campaign.EventMeetingBooked
		}
		data["interest"] = label
		return a.event(ctx, typ, send, &step, before, after, data)

	case campaign.InstantlyLeadNeutral, campaign.InstantlyLeadNoShow:
		send, step, err := a.send(ctx, ev, false)
		if err != nil {
			return err
		}
		if send != nil && ev.EventType == campaign.InstantlyLeadNeutral {
			if err := a.q.SetSendReplyClassification(ctx, dbgen.SetSendReplyClassificationParams{ID: send.ID, Classification: campaign.Ptr(campaign.ReplyNeutral)}); err != nil {
				return err
			}
		}
		cur := campaign.Stage(a.contact.LifecycleStage)
		return a.event(ctx, campaign.EventNote, send, &step, cur, cur, data)

	case campaign.InstantlyLeadNotInterested, campaign.InstantlyLeadWrongPerson:
		send, step, err := a.send(ctx, ev, false)
		if err != nil {
			return err
		}
		reason, typ := campaign.SuppressNotInterested, campaign.EventNotInterested
		if ev.EventType == campaign.InstantlyLeadWrongPerson {
			reason, typ = campaign.SuppressWrongPerson, campaign.EventWrongPerson
		}
		if send != nil {
			if err := a.q.SetSendReplyClassification(ctx, dbgen.SetSendReplyClassificationParams{ID: send.ID, Classification: campaign.Ptr(campaign.ReplyNegative)}); err != nil {
				return err
			}
		}
		code, label := interestFor(ev.EventType)
		if err := a.q.SetCampaignLeadInterest(ctx, dbgen.SetCampaignLeadInterestParams{ID: a.lead.ID, InterestStatus: campaign.Ptr(campaign.Int32(code)), InterestLabel: &label}); err != nil {
			return err
		}
		before := campaign.Stage(a.contact.LifecycleStage)
		if err := a.event(ctx, typ, send, &step, before, before, data); err != nil {
			return err
		}
		return a.suppress(ctx, reason)

	case campaign.InstantlyEmailBounced:
		send, step, err := a.send(ctx, ev, true)
		if err != nil {
			return err
		}
		if err := a.q.RecordSendBounce(ctx, dbgen.RecordSendBounceParams{ID: send.ID, At: &a.at}); err != nil {
			return err
		}
		if err := a.q.MarkCampaignLeadTerminal(ctx, dbgen.MarkCampaignLeadTerminalParams{ID: a.lead.ID, Status: campaign.LeadBounced}); err != nil {
			return err
		}
		before := campaign.Stage(a.contact.LifecycleStage)
		if err := a.event(ctx, campaign.EventBounced, send, &step, before, campaign.StageBounced, data); err != nil {
			return err
		}
		return a.suppress(ctx, campaign.SuppressBounced)

	case campaign.InstantlyLeadUnsubscribed:
		send, step, err := a.send(ctx, ev, false)
		if err != nil {
			return err
		}
		if send != nil {
			if err := a.q.RecordSendUnsubscribe(ctx, dbgen.RecordSendUnsubscribeParams{ID: send.ID, At: &a.at}); err != nil {
				return err
			}
		}
		if err := a.q.MarkCampaignLeadTerminal(ctx, dbgen.MarkCampaignLeadTerminalParams{ID: a.lead.ID, Status: campaign.LeadUnsubscribed}); err != nil {
			return err
		}
		before := campaign.Stage(a.contact.LifecycleStage)
		if err := a.event(ctx, campaign.EventUnsubscribed, send, &step, before, campaign.StageUnsubscribed, data); err != nil {
			return err
		}
		return a.suppress(ctx, campaign.SuppressUnsubscribed)
	}
	// Custom labels and anything new: keep it on the timeline as a note.
	cur := campaign.Stage(a.contact.LifecycleStage)
	return a.event(ctx, campaign.EventNote, nil, nil, cur, cur, data)
}

func (a *applier) suppress(ctx context.Context, reason string) error {
	source := campaign.SuppressionSourceInstantly
	if a.row.Source == campaign.ProviderEventReplay {
		source = campaign.SuppressionSourceReconcile
	}
	_, err := suppression.Apply(ctx, a.q, suppression.ApplyInput{
		ContactID: a.contact.ID, Reason: reason, Source: source, CampaignLeadID: &a.lead.ID, ProviderEventID: &a.row.ID,
		OccurredAt: a.at, Enqueue: func(ctx context.Context, args river.JobArgs) error { return a.d.enqueueTx(ctx, a.tx, args) },
	})
	return err
}

func interestFor(eventType string) (int, string) {
	switch eventType {
	case campaign.InstantlyLeadInterested:
		return instantly.InterestInterested, "interested"
	case campaign.InstantlyLeadMeetingBooked:
		return instantly.InterestMeetingBooked, "meeting_booked"
	case campaign.InstantlyLeadMeetingCompleted:
		return instantly.InterestMeetingCompleted, "meeting_completed"
	case campaign.InstantlyLeadClosed:
		return instantly.InterestWon, "won"
	case campaign.InstantlyLeadNotInterested:
		return instantly.InterestNotInterested, "not_interested"
	case campaign.InstantlyLeadWrongPerson:
		return instantly.InterestWrongPerson, "wrong_person"
	default:
		return 0, "neutral"
	}
}

func snippet(short, long string) string {
	if short != "" {
		return short
	}
	if len(long) > 280 {
		return long[:280] + "…"
	}
	return long
}

/* ------------------------------------------------------------- mailchimp */

func (w *ProcessEventWorker) applyMailchimp(ctx context.Context, row dbgen.ProviderEvent) (outcome, error) {
	d := w.deps
	var stored struct {
		Form       url.Values `json:"form"`
		AudienceID uuid.UUID  `json:"audience_id"`
	}
	if err := json.Unmarshal(row.Raw, &stored); err != nil {
		//nolint:nilerr // see above: unreadable payloads are closed, not retried.
		return outcome{}.withError("unparseable: " + err.Error()), nil
	}
	ev, err := webhook.ParseMailchimp(stored.Form)
	if err != nil {
		//nolint:nilerr // see above: unreadable payloads are closed, not retried.
		return outcome{}.withError("unparseable: " + err.Error()), nil
	}
	if ev.FiredAt.IsZero() {
		ev.FiredAt = row.ReceivedAt
	}
	at := ev.FiredAt.UTC()
	var out outcome

	audience, err := d.Store.GetNewsletterAudience(ctx, stored.AudienceID)
	if errors.Is(err, pgx.ErrNoRows) && ev.ListID != "" {
		audience, err = d.Store.GetNewsletterAudienceByListID(ctx, ev.ListID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return out.withError("unmatched: audience unknown"), nil
	}
	if err != nil {
		return out, fmt.Errorf("load audience: %w", err)
	}
	if ev.Type == campaign.MailchimpCampaign {
		return out.withError("campaign events are not tracked"), nil
	}
	email := ev.Email
	if ev.Type == campaign.MailchimpUpEmail && ev.OldEmail != "" {
		email = strings.ToLower(ev.OldEmail)
	}
	if email == "" {
		return out.withError("unmatched: no email"), nil
	}
	contact, err := d.Store.GetContactByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return out.withError("unmatched: contact " + email + " is unknown"), nil
	}
	if err != nil {
		return out, fmt.Errorf("load contact: %w", err)
	}
	out.contactID = &contact.ID
	sub, err := d.Store.GetNewsletterSubscriptionByContact(ctx, dbgen.GetNewsletterSubscriptionByContactParams{ContactID: contact.ID, AudienceID: audience.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		// A contact we know, in an audience we mirror, but never pushed by us —
		// they signed up elsewhere. Record it as an event without a subscription.
		sub = dbgen.NewsletterSubscription{}
	} else if err != nil {
		return out, fmt.Errorf("load subscription: %w", err)
	}
	hasSub := sub.ID != uuid.Nil

	err = d.Store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		record := func(typ string, before, after campaign.Stage, data map[string]any) error {
			if data == nil {
				data = map[string]any{}
			}
			data["audience_id"] = audience.ID
			data["list_id"] = ev.ListID
			_, _, err := campaign.RecordEvent(ctx, q, campaign.EventInput{
				ContactID: contact.ID, Type: typ, OccurredAt: at, Source: campaign.EventSourceMailchimp,
				ProviderEventID: &row.ID, StageBefore: before, StageAfter: after, Data: data,
			})
			return err
		}
		mirror := func(status string) error {
			if !hasSub {
				return nil
			}
			_, err := q.MirrorNewsletterSubscriptionStatus(ctx, dbgen.MirrorNewsletterSubscriptionStatusParams{
				ID: sub.ID, Status: status, UnsubscribeReason: campaign.Optional(ev.Reason),
				UniqueEmailID: campaign.Optional(ev.ID), WebID: webID(ev.WebID), At: &at,
			})
			return err
		}
		cur := campaign.Stage(contact.LifecycleStage)
		switch ev.Type {
		case campaign.MailchimpSubscribe:
			if err := mirror(campaign.SubSubscribed); err != nil {
				return err
			}
			updated, _, err := campaign.Advance(ctx, q, contact, campaign.StageMailchimpSubscribed)
			if err != nil {
				return err
			}
			return record(campaign.EventNewsletterSubscribed, cur, campaign.Stage(updated.LifecycleStage), nil)
		case campaign.MailchimpUnsubscribe:
			if err := mirror(campaign.SubUnsubscribed); err != nil {
				return err
			}
			if err := record(campaign.EventNewsletterUnsubscribed, cur, campaign.StageUnsubscribed, map[string]any{"reason": ev.Reason}); err != nil {
				return err
			}
			_, err := suppression.Apply(ctx, q, suppression.ApplyInput{
				ContactID: contact.ID, Reason: campaign.SuppressUnsubscribed, Source: campaign.SuppressionSourceMailchimp,
				Note: "unsubscribed from the newsletter", ProviderEventID: &row.ID, OccurredAt: at,
				Enqueue: func(ctx context.Context, args river.JobArgs) error { return d.enqueueTx(ctx, tx, args) },
			})
			return err
		case campaign.MailchimpCleaned:
			if err := mirror(campaign.SubCleaned); err != nil {
				return err
			}
			reason := campaign.SuppressBounced
			if strings.EqualFold(ev.Reason, "abuse") {
				reason = campaign.SuppressUnsubscribed
			}
			if err := record(campaign.EventNewsletterCleaned, cur, campaign.StageForSuppression(reason), map[string]any{"reason": ev.Reason}); err != nil {
				return err
			}
			_, err := suppression.Apply(ctx, q, suppression.ApplyInput{
				ContactID: contact.ID, Reason: reason, Source: campaign.SuppressionSourceMailchimp,
				Note: "cleaned by Mailchimp (" + ev.Reason + ")", ProviderEventID: &row.ID, OccurredAt: at,
				Enqueue: func(ctx context.Context, args river.JobArgs) error { return d.enqueueTx(ctx, tx, args) },
			})
			return err
		case campaign.MailchimpProfile:
			return record(campaign.EventNewsletterProfileUpdated, cur, cur, map[string]any{"merges": ev.Merges})
		case campaign.MailchimpUpEmail:
			return record(campaign.EventNote, cur, cur, map[string]any{"note": "Mailchimp address changed", "old_email": ev.OldEmail, "new_email": ev.NewEmail})
		default:
			return record(campaign.EventNote, cur, cur, map[string]any{"mailchimp_event": ev.Type})
		}
	})
	if err != nil {
		return out, err
	}
	return out, nil
}

func webID(raw string) *int64 {
	if raw == "" {
		return nil
	}
	var v int64
	if _, err := fmt.Sscan(raw, &v); err != nil {
		return nil
	}
	return &v
}

var _ = mailchimp.SubscriberHash
