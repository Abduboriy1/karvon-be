package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/consent"
	"github.com/bory/karvon-be/internal/campaign/suppression"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// ContactLead is a contact's membership in one campaign.
type ContactLead struct {
	Lead     dbgen.CampaignLead
	Campaign dbgen.Campaign
}

// ContactDetail is a contact with everything attached to it.
type ContactDetail struct {
	Contact       db.ContactRow
	Leads         []ContactLead
	Consents      []dbgen.ContactConsent
	Suppressions  []dbgen.ContactSuppression
	Subscriptions []dbgen.NewsletterSubscription
}

// ContactUpdate edits profile fields; the address is immutable.
type ContactUpdate struct {
	FirstName  *string
	LastName   *string
	Company    *string
	Title      *string
	Phone      *string
	Website    *string
	Attributes map[string]any
}

// ConsentInput records permission.
type ConsentInput struct {
	Source         string
	CapturedAt     time.Time
	Evidence       string
	CapturedBy     string
	CampaignLeadID *uuid.UUID
}

// ListContacts returns one page.
func (s *Service) ListContacts(ctx context.Context, f db.ContactFilter, sort string, page, perPage int) (Page[db.ContactRow], error) {
	rows, err := s.store.ListContactRows(ctx, f, sort, perPage, (page-1)*perPage)
	if err != nil {
		return Page[db.ContactRow]{}, apperr.Internal(err)
	}
	total, err := s.store.CountContactRows(ctx, f)
	if err != nil {
		return Page[db.ContactRow]{}, apperr.Internal(err)
	}
	return Page[db.ContactRow]{Rows: rows, Total: total}, nil
}

// GetContact loads a contact in full.
func (s *Service) GetContact(ctx context.Context, id uuid.UUID) (ContactDetail, error) {
	row, err := s.store.GetContactRow(ctx, id)
	if err != nil {
		return ContactDetail{}, notFound("contact", err)
	}
	leads, err := s.store.ListCampaignLeadsForContact(ctx, id)
	if err != nil {
		return ContactDetail{}, apperr.Internal(err)
	}
	detail := ContactDetail{Contact: row}
	for _, l := range leads {
		camp, err := s.store.GetCampaign(ctx, l.CampaignID)
		if err != nil {
			return ContactDetail{}, apperr.Internal(err)
		}
		detail.Leads = append(detail.Leads, ContactLead{Lead: l, Campaign: camp})
	}
	if detail.Consents, err = s.store.ListContactConsents(ctx, id); err != nil {
		return ContactDetail{}, apperr.Internal(err)
	}
	if detail.Suppressions, err = s.store.ListContactSuppressions(ctx, id); err != nil {
		return ContactDetail{}, apperr.Internal(err)
	}
	if detail.Subscriptions, err = s.store.ListNewsletterSubscriptionsForContact(ctx, id); err != nil {
		return ContactDetail{}, apperr.Internal(err)
	}
	return detail, nil
}

// UpdateContact edits profile fields.
func (s *Service) UpdateContact(ctx context.Context, id uuid.UUID, in ContactUpdate) (ContactDetail, error) {
	if _, err := s.contact(ctx, id); err != nil {
		return ContactDetail{}, err
	}
	params := dbgen.UpdateContactProfileParams{ID: id, FirstName: in.FirstName, LastName: in.LastName, Company: in.Company,
		Title: in.Title, Phone: in.Phone, Website: in.Website}
	if in.Attributes != nil {
		params.Attributes = encodeMap(in.Attributes)
	}
	if _, err := s.store.UpdateContactProfile(ctx, params); err != nil {
		return ContactDetail{}, apperr.Internal(err)
	}
	return s.GetContact(ctx, id)
}

// ContactTimeline returns a contact's events, oldest first.
func (s *Service) ContactTimeline(ctx context.Context, id uuid.UUID, page, perPage int) (Page[dbgen.ContactEvent], error) {
	if _, err := s.contact(ctx, id); err != nil {
		return Page[dbgen.ContactEvent]{}, err
	}
	rows, err := s.store.ListContactEvents(ctx, dbgen.ListContactEventsParams{ContactID: id, Lim: campaign.Int32(perPage), Off: campaign.Int32((page - 1) * perPage)})
	if err != nil {
		return Page[dbgen.ContactEvent]{}, apperr.Internal(err)
	}
	total, err := s.store.CountContactEvents(ctx, id)
	if err != nil {
		return Page[dbgen.ContactEvent]{}, apperr.Internal(err)
	}
	return Page[dbgen.ContactEvent]{Rows: rows, Total: total}, nil
}

// RequestPermission records that we asked the contact for newsletter permission.
func (s *Service) RequestPermission(ctx context.Context, id uuid.UUID, note string) (ContactDetail, error) {
	contact, err := s.contact(ctx, id)
	if err != nil {
		return ContactDetail{}, err
	}
	if contact.SuppressedAt != nil {
		return ContactDetail{}, apperr.Conflict("the contact is suppressed (%s)", campaign.Deref(contact.SuppressionReason))
	}
	err = s.store.InTx(ctx, func(q *dbgen.Queries) error {
		updated, _, err := campaign.Advance(ctx, q, contact, campaign.StagePermissionRequested)
		if err != nil {
			return err
		}
		_, _, err = campaign.RecordEvent(ctx, q, campaign.EventInput{
			ContactID: id, OccurredAt: s.now(), Type: campaign.EventPermissionRequested, Source: campaign.EventSourceManual,
			StageBefore: campaign.Stage(contact.LifecycleStage), StageAfter: campaign.Stage(updated.LifecycleStage),
			Data: map[string]any{"note": note},
		})
		return err
	})
	if err != nil {
		return ContactDetail{}, apperr.Internal(err)
	}
	return s.GetContact(ctx, id)
}

// CaptureConsent records permission and, when nothing blocks it, makes the contact
// newsletter-eligible. This is the only door into the newsletter stages.
func (s *Service) CaptureConsent(ctx context.Context, id uuid.UUID, in ConsentInput) (dbgen.ContactConsent, error) {
	var fields []apperr.FieldError
	validSource := false
	for _, src := range campaign.ConsentSources {
		if src == in.Source {
			validSource = true
		}
	}
	if !validSource {
		fields = append(fields, apperr.FieldError{Field: "source", Message: "must be one of: " + strings.Join(campaign.ConsentSources, ", ")})
	}
	if strings.TrimSpace(in.Evidence) == "" {
		fields = append(fields, apperr.FieldError{Field: "evidence", Message: "is required: quote the reply, form or conversation"})
	}
	if strings.TrimSpace(in.CapturedBy) == "" {
		fields = append(fields, apperr.FieldError{Field: "captured_by", Message: "is required"})
	}
	if in.CapturedAt.IsZero() {
		in.CapturedAt = s.now()
	}
	if in.CapturedAt.After(s.now().Add(time.Minute)) {
		fields = append(fields, apperr.FieldError{Field: "captured_at", Message: "cannot be in the future"})
	}
	if len(fields) > 0 {
		return dbgen.ContactConsent{}, apperr.Validation("consent is invalid", fields...)
	}
	contact, err := s.contact(ctx, id)
	if err != nil {
		return dbgen.ContactConsent{}, err
	}
	if contact.SuppressedAt != nil {
		return dbgen.ContactConsent{}, apperr.Conflict("the contact is suppressed (%s); consent cannot be recorded", campaign.Deref(contact.SuppressionReason))
	}
	if in.CampaignLeadID != nil {
		lead, err := s.store.GetCampaignLead(ctx, *in.CampaignLeadID)
		if err != nil || lead.ContactID != id {
			return dbgen.ContactConsent{}, apperr.Validation("consent is invalid", apperr.FieldError{Field: "campaign_lead_id", Message: "does not belong to this contact"})
		}
	}
	if _, err := s.store.GetActiveConsent(ctx, id); err == nil {
		return dbgen.ContactConsent{}, apperr.Conflict("the contact already has an active consent record; revoke it first to replace it")
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return dbgen.ContactConsent{}, apperr.Internal(err)
	}

	var created dbgen.ContactConsent
	err = s.store.InTx(ctx, func(q *dbgen.Queries) error {
		row, err := q.CreateContactConsent(ctx, dbgen.CreateContactConsentParams{
			ID: ids.New(), ContactID: id, Source: in.Source, CapturedAt: in.CapturedAt.UTC(),
			Evidence: strings.TrimSpace(in.Evidence), CapturedBy: strings.TrimSpace(in.CapturedBy),
			CampaignLeadID: campaign.NullUUID(in.CampaignLeadID),
		})
		if err != nil {
			return err
		}
		created = row
		before := campaign.Stage(contact.LifecycleStage)
		updated, _, err := campaign.Advance(ctx, q, contact, campaign.StagePermissionCaptured)
		if err != nil {
			return err
		}
		var campaignID *uuid.UUID
		if in.CampaignLeadID != nil {
			if lead, err := q.GetCampaignLead(ctx, *in.CampaignLeadID); err == nil {
				campaignID = &lead.CampaignID
			}
		}
		if _, _, err := campaign.RecordEvent(ctx, q, campaign.EventInput{
			ContactID: id, CampaignID: campaignID, CampaignLeadID: in.CampaignLeadID, Type: campaign.EventConsentCaptured,
			OccurredAt: in.CapturedAt.UTC(), Source: campaign.EventSourceManual,
			StageBefore: before, StageAfter: campaign.Stage(updated.LifecycleStage),
			Data: map[string]any{"consent_id": row.ID, "source": in.Source, "captured_by": in.CapturedBy},
		}); err != nil {
			return err
		}
		decision := consent.Evaluate(consent.Input{Stage: campaign.Stage(updated.LifecycleStage), HasActiveConsent: true, ConsentSource: in.Source})
		if decision.Eligible {
			eligible, changed, err := campaign.Advance(ctx, q, updated, campaign.StageNewsletterEligible)
			if err != nil {
				return err
			}
			if changed {
				if _, _, err := campaign.RecordEvent(ctx, q, campaign.EventInput{
					ContactID: id, CampaignID: campaignID, CampaignLeadID: in.CampaignLeadID, OccurredAt: s.now(), Type: campaign.EventNewsletterEligible,
					Source: campaign.EventSourceSystem, StageBefore: campaign.Stage(updated.LifecycleStage), StageAfter: campaign.Stage(eligible.LifecycleStage),
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return dbgen.ContactConsent{}, apperr.Internal(err)
	}
	return created, nil
}

// RevokeConsent withdraws permission; a live subscription is unsubscribed.
func (s *Service) RevokeConsent(ctx context.Context, contactID, consentID uuid.UUID, reason string) (ContactDetail, error) {
	contact, err := s.contact(ctx, contactID)
	if err != nil {
		return ContactDetail{}, err
	}
	row, err := s.store.GetContactConsent(ctx, consentID)
	if err != nil || row.ContactID != contactID {
		return ContactDetail{}, apperr.NotFound("consent")
	}
	if row.RevokedAt != nil {
		return ContactDetail{}, apperr.Conflict("this consent was already revoked")
	}
	err = s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.RevokeConsent(ctx, dbgen.RevokeConsentParams{ID: consentID, Reason: campaign.Optional(reason)}); err != nil {
			return err
		}
		before := campaign.Stage(contact.LifecycleStage)
		after := before
		if before.Newsletter() || before == campaign.StagePermissionCaptured {
			// Consent is gone, so the newsletter stages no longer apply. The trigger
			// allows this move because it only guards entry, not exit.
			updated, err := q.SetContactStage(ctx, dbgen.SetContactStageParams{ID: contactID, Stage: string(campaign.StageInterested)})
			if err != nil {
				return err
			}
			after = campaign.Stage(updated.LifecycleStage)
		}
		if _, _, err := campaign.RecordEvent(ctx, q, campaign.EventInput{
			ContactID: contactID, OccurredAt: s.now(), Type: campaign.EventConsentRevoked, Source: campaign.EventSourceManual,
			StageBefore: before, StageAfter: after, Data: map[string]any{"consent_id": consentID, "reason": reason},
		}); err != nil {
			return err
		}
		subs, err := q.ListNewsletterSubscriptionsForContact(ctx, contactID)
		if err != nil {
			return err
		}
		for _, sub := range subs {
			if sub.Status == campaign.SubPending || sub.Status == campaign.SubSubscribed {
				if _, err := q.RequeueNewsletterSubscription(ctx, dbgen.RequeueNewsletterSubscriptionParams{ID: sub.ID, RequestedStatus: campaign.Ptr(campaign.RequestUnsubscribed)}); err != nil {
					return err
				}
				if err := s.enqueueTx(ctx, tx, campaign.NewsletterPushArgs{SubscriptionID: sub.ID, Action: campaign.NewsletterActionUnsubscribe}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return ContactDetail{}, apperr.Internal(err)
	}
	return s.GetContact(ctx, contactID)
}

// SuppressContact is the operator's suppression.
func (s *Service) SuppressContact(ctx context.Context, id uuid.UUID, reason, note string) (ContactDetail, error) {
	switch reason {
	case campaign.SuppressDoNotContact, campaign.SuppressNotInterested, campaign.SuppressWrongPerson, campaign.SuppressUnsubscribed:
	default:
		return ContactDetail{}, apperr.Validation("suppression is invalid", apperr.FieldError{Field: "reason",
			Message: "must be one of: do_not_contact, not_interested, wrong_person, unsubscribed"})
	}
	if _, err := s.contact(ctx, id); err != nil {
		return ContactDetail{}, err
	}
	err := s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		_, err := suppression.Apply(ctx, dbgen.New(tx), suppression.ApplyInput{
			ContactID: id, Reason: reason, Source: campaign.SuppressionSourceManual, Note: note, OccurredAt: s.now(),
			Enqueue: func(ctx context.Context, args river.JobArgs) error { return s.enqueueTx(ctx, tx, args) },
		})
		return err
	})
	if err != nil {
		var appErr *apperr.Error
		if errors.As(err, &appErr) {
			return ContactDetail{}, err
		}
		return ContactDetail{}, apperr.Internal(err)
	}
	return s.GetContact(ctx, id)
}

// LiftSuppression reopens a contact after a liftable suppression.
func (s *Service) LiftSuppression(ctx context.Context, contactID, suppressionID uuid.UUID, note string) (ContactDetail, error) {
	err := s.store.InTx(ctx, func(q *dbgen.Queries) error {
		_, err := suppression.Lift(ctx, q, suppression.LiftInput{ContactID: contactID, SuppressionID: suppressionID, Note: strings.TrimSpace(note)})
		return err
	})
	if err != nil {
		var appErr *apperr.Error
		if errors.As(err, &appErr) {
			return ContactDetail{}, err
		}
		return ContactDetail{}, apperr.Internal(err)
	}
	return s.GetContact(ctx, contactID)
}
