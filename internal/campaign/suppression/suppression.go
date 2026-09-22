// Package suppression is the one place a contact is taken out of circulation.
//
// Every caller — an Instantly unsubscribe webhook, a Mailchimp hard bounce, a
// reconcile pass, an operator — goes through Apply, so the cascade is always the
// same: the contact moves to its terminal stage, every live campaign lead stops,
// pushed leads are removed from Instantly, and a newsletter subscription is
// unsubscribed where the reason calls for it.
package suppression

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// Enqueue is how Apply schedules follow-up work inside the caller's transaction.
type Enqueue func(ctx context.Context, args river.JobArgs) error

// ApplyInput describes one suppression.
type ApplyInput struct {
	ContactID       uuid.UUID
	Reason          string
	Source          string
	Note            string
	CampaignLeadID  *uuid.UUID
	ProviderEventID *uuid.UUID
	OccurredAt      time.Time
	// Enqueue may be nil, in which case follow-ups are returned for the caller.
	Enqueue Enqueue
}

// Result reports what Apply did.
type Result struct {
	Contact dbgen.Contact
	// Created is false when the contact was already suppressed for this reason.
	Created bool
	// LeadsStopped are the campaign leads that were moved to suppressed.
	LeadsStopped []dbgen.CampaignLead
	// RemoveFromInstantly lists pushed leads that should be deleted at Instantly.
	RemoveFromInstantly []dbgen.CampaignLead
	// UnsubscribeFromMailchimp lists subscriptions that should be unsubscribed.
	UnsubscribeFromMailchimp []dbgen.NewsletterSubscription
}

// StopsNewsletter reports whether a reason also ends the newsletter relationship.
// Not interested is a cold-email answer, not a newsletter one; the rest are.
func StopsNewsletter(reason string) bool {
	return reason != campaign.SuppressNotInterested
}

// Apply suppresses a contact. It is idempotent: the same reason twice does nothing
// new, and a weaker reason never replaces a stronger one.
func Apply(ctx context.Context, q *dbgen.Queries, in ApplyInput) (Result, error) {
	if in.OccurredAt.IsZero() {
		in.OccurredAt = time.Now().UTC()
	}
	contact, err := q.GetContact(ctx, in.ContactID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, apperr.NotFound("contact")
	}
	if err != nil {
		return Result{}, fmt.Errorf("suppression: load contact: %w", err)
	}
	stage := campaign.StageForSuppression(in.Reason)
	result := Result{Contact: contact}

	if contact.SuppressedAt != nil {
		current := campaign.Deref(contact.SuppressionReason)
		if current == in.Reason || current == campaign.SuppressUnsubscribed {
			return result, nil
		}
		if in.Reason != campaign.SuppressUnsubscribed && campaign.PermanentSuppression(current) {
			// A permanent reason stands; only an unsubscribe outranks it.
			return result, nil
		}
		// Supersede the active record when it can be lifted; permanent records
		// stay as history and only the contact's reason changes.
		if active, err := q.GetActiveSuppression(ctx, contact.ID); err == nil && !campaign.PermanentSuppression(active.Reason) {
			note := "superseded by " + in.Reason
			if _, err := q.LiftSuppression(ctx, dbgen.LiftSuppressionParams{ID: active.ID, Note: &note}); err != nil {
				return Result{}, fmt.Errorf("suppression: supersede: %w", err)
			}
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Result{}, fmt.Errorf("suppression: load active: %w", err)
		}
	}

	if _, err := q.GetActiveSuppression(ctx, contact.ID); errors.Is(err, pgx.ErrNoRows) {
		if _, err := q.CreateContactSuppression(ctx, dbgen.CreateContactSuppressionParams{
			ID: ids.New(), ContactID: contact.ID, Reason: in.Reason, Source: in.Source,
			Note: campaign.Optional(in.Note), CampaignLeadID: campaign.NullUUID(in.CampaignLeadID),
			ProviderEventID: campaign.NullUUID(in.ProviderEventID),
		}); err != nil {
			return Result{}, fmt.Errorf("suppression: record: %w", err)
		}
	} else if err != nil {
		return Result{}, fmt.Errorf("suppression: load active: %w", err)
	}

	before := campaign.Stage(contact.LifecycleStage)
	updated, err := q.SuppressContact(ctx, dbgen.SuppressContactParams{ID: contact.ID, Reason: &in.Reason, Stage: string(stage)})
	if err != nil {
		return Result{}, fmt.Errorf("suppression: update contact: %w", err)
	}
	result.Contact = updated
	result.Created = true

	leads, err := q.SuppressCampaignLeadsForContact(ctx, contact.ID)
	if err != nil {
		return Result{}, fmt.Errorf("suppression: stop leads: %w", err)
	}
	result.LeadsStopped = leads

	// Removal is decided by "did this lead ever reach the provider", not by the
	// rows just stopped: a bounce marks the lead terminal before it gets here, and
	// a terminal lead left in Instantly would still be counted, reported on, and in
	// an evergreen campaign mailed again.
	pushed, err := q.ListPushedCampaignLeadsForContact(ctx, contact.ID)
	if err != nil {
		return Result{}, fmt.Errorf("suppression: list pushed leads: %w", err)
	}
	result.RemoveFromInstantly = pushed

	if StopsNewsletter(in.Reason) {
		subs, err := q.ListNewsletterSubscriptionsForContact(ctx, contact.ID)
		if err != nil {
			return Result{}, fmt.Errorf("suppression: list subscriptions: %w", err)
		}
		for _, sub := range subs {
			if sub.Status == campaign.SubPending || sub.Status == campaign.SubSubscribed || sub.Status == campaign.SubLocalPending {
				result.UnsubscribeFromMailchimp = append(result.UnsubscribeFromMailchimp, sub)
			}
		}
	}

	if _, _, err := campaign.RecordEvent(ctx, q, campaign.EventInput{
		ContactID: contact.ID, CampaignLeadID: in.CampaignLeadID, Type: campaign.EventSuppressed,
		OccurredAt: in.OccurredAt, Source: eventSource(in.Source), ProviderEventID: in.ProviderEventID,
		StageBefore: before, StageAfter: stage,
		Data: map[string]any{"reason": in.Reason, "note": in.Note, "leads_stopped": len(leads)},
	}); err != nil {
		return Result{}, err
	}

	if in.Enqueue != nil {
		for _, lead := range result.RemoveFromInstantly {
			if err := in.Enqueue(ctx, campaign.RemoveLeadArgs{CampaignLeadID: lead.ID}); err != nil {
				return Result{}, err
			}
		}
		for _, sub := range result.UnsubscribeFromMailchimp {
			if _, err := q.RequeueNewsletterSubscription(ctx, dbgen.RequeueNewsletterSubscriptionParams{
				ID: sub.ID, RequestedStatus: campaign.Ptr(campaign.RequestUnsubscribed),
			}); err != nil {
				return Result{}, fmt.Errorf("suppression: requeue subscription: %w", err)
			}
			if err := in.Enqueue(ctx, campaign.NewsletterPushArgs{SubscriptionID: sub.ID, Action: campaign.NewsletterActionUnsubscribe}); err != nil {
				return Result{}, err
			}
		}
	}
	return result, nil
}

// LiftInput lifts a liftable suppression.
type LiftInput struct {
	ContactID     uuid.UUID
	SuppressionID uuid.UUID
	Note          string
	Source        string
}

// Lift reopens a contact after a not-interested, wrong-person or do-not-contact
// suppression. Permanent reasons cannot be lifted, and a lifted contact comes back
// at "contacted", never higher: consent has to be captured again.
func Lift(ctx context.Context, q *dbgen.Queries, in LiftInput) (dbgen.Contact, error) {
	sup, err := q.GetContactSuppression(ctx, in.SuppressionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.Contact{}, apperr.NotFound("suppression")
	}
	if err != nil {
		return dbgen.Contact{}, fmt.Errorf("suppression: load: %w", err)
	}
	if sup.ContactID != in.ContactID {
		return dbgen.Contact{}, apperr.NotFound("suppression")
	}
	if sup.LiftedAt != nil {
		return dbgen.Contact{}, apperr.Conflict("this suppression was already lifted")
	}
	if campaign.PermanentSuppression(sup.Reason) {
		return dbgen.Contact{}, apperr.Conflict("a %s suppression is permanent and cannot be lifted", sup.Reason)
	}
	if in.Note == "" {
		return dbgen.Contact{}, apperr.Validation("a note is required to lift a suppression",
			apperr.FieldError{Field: "note", Message: "is required"})
	}
	contact, err := q.GetContact(ctx, in.ContactID)
	if err != nil {
		return dbgen.Contact{}, fmt.Errorf("suppression: load contact: %w", err)
	}
	if _, err := q.LiftSuppression(ctx, dbgen.LiftSuppressionParams{ID: sup.ID, Note: &in.Note}); err != nil {
		return dbgen.Contact{}, fmt.Errorf("suppression: lift: %w", err)
	}
	updated, err := q.LiftContactSuppression(ctx, dbgen.LiftContactSuppressionParams{ID: contact.ID, Stage: string(campaign.StageContacted)})
	if err != nil {
		return dbgen.Contact{}, fmt.Errorf("suppression: reopen contact: %w", err)
	}
	if _, _, err := campaign.RecordEvent(ctx, q, campaign.EventInput{
		ContactID: contact.ID, Type: campaign.EventSuppressionLifted, Source: campaign.EventSourceManual,
		StageBefore: campaign.Stage(contact.LifecycleStage), StageAfter: campaign.StageContacted,
		Data: map[string]any{"reason": sup.Reason, "note": in.Note},
	}); err != nil {
		return dbgen.Contact{}, err
	}
	return updated, nil
}

func eventSource(suppressionSource string) string {
	switch suppressionSource {
	case campaign.SuppressionSourceInstantly:
		return campaign.EventSourceInstantly
	case campaign.SuppressionSourceMailchimp:
		return campaign.EventSourceMailchimp
	case campaign.SuppressionSourceReconcile:
		return campaign.EventSourceReconcile
	case campaign.SuppressionSourceImport:
		return campaign.EventSourceImport
	default:
		return campaign.EventSourceManual
	}
}
