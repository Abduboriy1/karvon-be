package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/consent"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/provider/mailchimp"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// AudienceUpdate edits our settings on an audience.
type AudienceUpdate struct {
	AllowSingleOptIn *bool
	DefaultTags      []string
	IsDefault        *bool
}

// EligibleContact is a contact at the newsletter gate, with the gate's verdict.
type EligibleContact struct {
	Contact  dbgen.ListNewsletterEligibleContactsRow
	Decision consent.Decision
}

// PushRejection explains why one contact was not queued.
type PushRejection struct {
	ContactID uuid.UUID
	Reason    string
}

// PushResult reports a push request.
type PushResult struct {
	Queued   []uuid.UUID
	Rejected []PushRejection
}

// SubscriptionFilter narrows the subscriptions list.
type SubscriptionFilter struct {
	Statuses     []string
	SyncStatuses []string
	AudienceID   *uuid.UUID
	Q            *string
}

// NewsletterStats summarises the newsletter stage.
type NewsletterStats struct {
	ByStatus       map[string]int64
	Eligible       int64
	Subscribed     int64
	Pending        int64
	ConversionRate float64
}

// ListAudiences lists the mirrored audiences.
func (s *Service) ListAudiences(ctx context.Context) ([]dbgen.NewsletterAudience, error) {
	rows, err := s.store.ListNewsletterAudiences(ctx)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	return rows, nil
}

// SyncAudiences queues an audience refresh.
func (s *Service) SyncAudiences(ctx context.Context) error {
	return s.enqueue(ctx, campaign.NewsletterSyncAudiencesArgs{RequestID: ids.New()})
}

// UpdateAudience edits our own settings on an audience.
func (s *Service) UpdateAudience(ctx context.Context, id uuid.UUID, in AudienceUpdate) (dbgen.NewsletterAudience, error) {
	if _, err := s.store.GetNewsletterAudience(ctx, id); err != nil {
		return dbgen.NewsletterAudience{}, notFound("audience", err)
	}
	err := s.store.InTx(ctx, func(q *dbgen.Queries) error {
		var tags []string
		if in.DefaultTags != nil {
			tags = cleanTags(in.DefaultTags)
		}
		if _, err := q.UpdateNewsletterAudienceSettings(ctx, dbgen.UpdateNewsletterAudienceSettingsParams{ID: id, AllowSingleOptIn: in.AllowSingleOptIn, DefaultTags: tags}); err != nil {
			return err
		}
		if in.IsDefault != nil && *in.IsDefault {
			if err := q.ClearDefaultNewsletterAudience(ctx); err != nil {
				return err
			}
			if _, err := q.SetDefaultNewsletterAudience(ctx, id); err != nil {
				return err
			}
			if err := q.SetDefaultAudience(ctx, uuid.NullUUID{UUID: id, Valid: true}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return dbgen.NewsletterAudience{}, apperr.Internal(err)
	}
	row, err := s.store.GetNewsletterAudience(ctx, id)
	if err != nil {
		return dbgen.NewsletterAudience{}, apperr.Internal(err)
	}
	return row, nil
}

// RegisterMailchimpWebhook creates the audience webhook at Mailchimp and stores
// its signing secret. Requires KARVON_PUBLIC_BASE_URL.
func (s *Service) RegisterMailchimpWebhook(ctx context.Context, id uuid.UUID) (dbgen.NewsletterAudience, error) {
	audience, err := s.store.GetNewsletterAudience(ctx, id)
	if err != nil {
		return dbgen.NewsletterAudience{}, notFound("audience", err)
	}
	if s.cfg.PublicBaseURL == "" {
		return dbgen.NewsletterAudience{}, apperr.Conflict("KARVON_PUBLIC_BASE_URL must be set before a webhook can be registered")
	}
	client, err := s.Mailchimp(ctx)
	if err != nil {
		return dbgen.NewsletterAudience{}, err
	}
	if audience.WebhookID != nil {
		if err := client.DeleteWebhook(ctx, audience.MailchimpListID, *audience.WebhookID); err != nil && !errors.Is(err, provider.ErrNotFound) {
			return dbgen.NewsletterAudience{}, providerErr("Mailchimp could not remove the old webhook", err)
		}
	}
	token := randomToken()
	url := strings.TrimSuffix(s.cfg.PublicBaseURL, "/") + "/api/v1/webhooks/mailchimp/" + token
	hook, err := client.CreateWebhook(ctx, audience.MailchimpListID, mailchimp.WebhookInput{
		URL:     url,
		Events:  map[string]bool{"subscribe": true, "unsubscribe": true, "profile": true, "cleaned": true, "upemail": true, "campaign": false},
		Sources: map[string]bool{"user": true, "admin": true, "api": true},
	})
	if err != nil {
		return dbgen.NewsletterAudience{}, providerErr("Mailchimp could not create the webhook", err)
	}
	var secretEnc []byte
	if hook.SigningSecret != "" {
		secretEnc, err = s.cipher.EncryptString(hook.SigningSecret)
		if err != nil {
			return dbgen.NewsletterAudience{}, apperr.Internal(err)
		}
	}
	row, err := s.store.SetNewsletterAudienceWebhook(ctx, dbgen.SetNewsletterAudienceWebhookParams{
		ID: id, Token: &token, WebhookID: &hook.ID, SecretEnc: secretEnc, WebhookUrl: &url,
	})
	if err != nil {
		return dbgen.NewsletterAudience{}, apperr.Internal(err)
	}
	if hook.SigningSecret == "" {
		s.log.Warn("Mailchimp returned no signing secret; inbound webhooks for this audience will be rejected until one is available",
			"audience_id", id)
	}
	return row, nil
}

// DeleteMailchimpWebhook removes the audience webhook.
func (s *Service) DeleteMailchimpWebhook(ctx context.Context, id uuid.UUID) (dbgen.NewsletterAudience, error) {
	audience, err := s.store.GetNewsletterAudience(ctx, id)
	if err != nil {
		return dbgen.NewsletterAudience{}, notFound("audience", err)
	}
	if audience.WebhookID != nil {
		client, err := s.Mailchimp(ctx)
		if err != nil {
			return dbgen.NewsletterAudience{}, err
		}
		if err := client.DeleteWebhook(ctx, audience.MailchimpListID, *audience.WebhookID); err != nil && !errors.Is(err, provider.ErrNotFound) {
			return dbgen.NewsletterAudience{}, providerErr("Mailchimp could not delete the webhook", err)
		}
	}
	row, err := s.store.SetNewsletterAudienceWebhook(ctx, dbgen.SetNewsletterAudienceWebhookParams{ID: id})
	if err != nil {
		return dbgen.NewsletterAudience{}, apperr.Internal(err)
	}
	return row, nil
}

// ListEligible lists the contacts at the newsletter gate with the gate's verdict.
func (s *Service) ListEligible(ctx context.Context, campaignID *uuid.UUID, audienceID *uuid.UUID, page, perPage int) (Page[EligibleContact], error) {
	audience, err := s.resolveAudience(ctx, audienceID)
	if err != nil && !errors.Is(err, errNoAudience) {
		return Page[EligibleContact]{}, err
	}
	rows, err := s.store.ListNewsletterEligibleContacts(ctx, dbgen.ListNewsletterEligibleContactsParams{
		CampaignID: campaign.NullUUID(campaignID), Lim: campaign.Int32(perPage), Off: campaign.Int32((page - 1) * perPage)})
	if err != nil {
		return Page[EligibleContact]{}, apperr.Internal(err)
	}
	total, err := s.store.CountNewsletterEligibleContacts(ctx, campaign.NullUUID(campaignID))
	if err != nil {
		return Page[EligibleContact]{}, apperr.Internal(err)
	}
	emails := make([]string, 0, len(rows))
	for _, r := range rows {
		emails = append(emails, r.Email)
	}
	excluded, err := s.store.ExcludedEmails(ctx, emails)
	if err != nil {
		return Page[EligibleContact]{}, apperr.Internal(err)
	}
	out := make([]EligibleContact, 0, len(rows))
	for _, r := range rows {
		_, isExcluded := excluded[strings.ToLower(r.Email)]
		out = append(out, EligibleContact{Contact: r, Decision: consent.Evaluate(consent.Input{
			Suppressed: r.SuppressedAt != nil, SuppressionReason: campaign.Deref(r.SuppressionReason),
			Stage: campaign.Stage(r.LifecycleStage), HasActiveConsent: r.ConsentID.Valid,
			ConsentSource: campaign.Deref(r.ConsentSource), AllowSingleOptIn: audience.AllowSingleOptIn,
			Excluded: isExcluded,
		})})
	}
	return Page[EligibleContact]{Rows: out, Total: total}, nil
}

var errNoAudience = errors.New("no audience")

func (s *Service) resolveAudience(ctx context.Context, id *uuid.UUID) (dbgen.NewsletterAudience, error) {
	if id != nil {
		row, err := s.store.GetNewsletterAudience(ctx, *id)
		if err != nil {
			return dbgen.NewsletterAudience{}, notFound("audience", err)
		}
		return row, nil
	}
	row, err := s.store.GetDefaultNewsletterAudience(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.NewsletterAudience{}, errNoAudience
	}
	if err != nil {
		return dbgen.NewsletterAudience{}, apperr.Internal(err)
	}
	return row, nil
}

// PushSubscriptions queues eligible contacts for Mailchimp. Every contact is run
// through the consent gate here and again inside the job.
func (s *Service) PushSubscriptions(ctx context.Context, contactIDs []uuid.UUID, audienceID *uuid.UUID) (PushResult, error) {
	audience, err := s.resolveAudience(ctx, audienceID)
	if errors.Is(err, errNoAudience) {
		return PushResult{}, apperr.Conflict("no Mailchimp audience is configured; sync audiences and pick a default")
	}
	if err != nil {
		return PushResult{}, err
	}
	result := PushResult{Queued: []uuid.UUID{}, Rejected: []PushRejection{}}
	for _, contactID := range uniqueIDs(contactIDs) {
		contact, err := s.store.GetContact(ctx, contactID)
		if errors.Is(err, pgx.ErrNoRows) {
			result.Rejected = append(result.Rejected, PushRejection{ContactID: contactID, Reason: "not_found"})
			continue
		}
		if err != nil {
			return PushResult{}, apperr.Internal(err)
		}
		active, err := s.store.GetActiveConsent(ctx, contactID)
		hasConsent := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return PushResult{}, apperr.Internal(err)
		}
		exclusion, err := s.store.MatchEmail(ctx, contact.Email)
		if err != nil {
			return PushResult{}, apperr.Internal(err)
		}
		decision := consent.Evaluate(consent.Input{
			Suppressed: contact.SuppressedAt != nil, SuppressionReason: campaign.Deref(contact.SuppressionReason),
			Stage: campaign.Stage(contact.LifecycleStage), HasActiveConsent: hasConsent, ConsentSource: active.Source,
			AllowSingleOptIn: audience.AllowSingleOptIn, Excluded: exclusion != nil,
		})
		if !decision.Eligible {
			result.Rejected = append(result.Rejected, PushRejection{ContactID: contactID, Reason: decision.Reason})
			continue
		}
		if existing, err := s.store.GetNewsletterSubscriptionByContact(ctx, dbgen.GetNewsletterSubscriptionByContactParams{ContactID: contactID, AudienceID: audience.ID}); err == nil {
			switch existing.Status {
			case campaign.SubSubscribed, campaign.SubPending:
				result.Rejected = append(result.Rejected, PushRejection{ContactID: contactID, Reason: "already_subscribed"})
				continue
			case campaign.SubComplianceBlocked, campaign.SubCleaned, campaign.SubUnsubscribed:
				result.Rejected = append(result.Rejected, PushRejection{ContactID: contactID, Reason: "compliance_blocked"})
				continue
			}
			// A failed or local_pending subscription is simply requeued.
			err = s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
				if _, err := dbgen.New(tx).RequeueNewsletterSubscription(ctx, dbgen.RequeueNewsletterSubscriptionParams{ID: existing.ID, RequestedStatus: &decision.RequestedStatus}); err != nil {
					return err
				}
				// Requeuing an existing subscription is a retry, so it carries a fresh
				// request id rather than colliding with the attempt that failed.
				return s.enqueueTx(ctx, tx, campaign.NewsletterPushArgs{
					SubscriptionID: existing.ID, Action: campaign.NewsletterActionUpsert, RequestID: ids.New(),
				})
			})
			if err != nil {
				return PushResult{}, apperr.Internal(err)
			}
			result.Queued = append(result.Queued, contactID)
			continue
		}
		var consentID uuid.NullUUID
		if hasConsent {
			consentID = uuid.NullUUID{UUID: active.ID, Valid: true}
		}
		err = s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
			q := dbgen.New(tx)
			sub, err := q.CreateNewsletterSubscription(ctx, dbgen.CreateNewsletterSubscriptionParams{
				ID: ids.New(), ContactID: contactID, AudienceID: audience.ID, ConsentID: consentID,
				RequestedStatus: decision.RequestedStatus, SubscriberHash: campaign.Ptr(mailchimp.SubscriberHash(contact.Email)),
				Tags: audience.DefaultTags,
			})
			if err != nil {
				return err
			}
			if _, _, err := campaign.RecordEvent(ctx, q, campaign.EventInput{
				ContactID: contactID, OccurredAt: s.now(), Type: campaign.EventNewsletterPushed, Source: campaign.EventSourceManual,
				Data: map[string]any{"subscription_id": sub.ID, "audience_id": audience.ID, "requested_status": decision.RequestedStatus},
			}); err != nil {
				return err
			}
			return s.enqueueTx(ctx, tx, campaign.NewsletterPushArgs{SubscriptionID: sub.ID, Action: campaign.NewsletterActionUpsert})
		})
		if err != nil {
			return PushResult{}, apperr.Internal(err)
		}
		result.Queued = append(result.Queued, contactID)
	}
	return result, nil
}

// ListSubscriptions returns one page.
func (s *Service) ListSubscriptions(ctx context.Context, f SubscriptionFilter, page, perPage int) (Page[dbgen.ListNewsletterSubscriptionsRow], error) {
	rows, err := s.store.ListNewsletterSubscriptions(ctx, dbgen.ListNewsletterSubscriptionsParams{
		Statuses: orEmpty(f.Statuses), SyncStatuses: orEmpty(f.SyncStatuses), AudienceID: campaign.NullUUID(f.AudienceID), Q: f.Q,
		Lim: campaign.Int32(perPage), Off: campaign.Int32((page - 1) * perPage)})
	if err != nil {
		return Page[dbgen.ListNewsletterSubscriptionsRow]{}, apperr.Internal(err)
	}
	total, err := s.store.CountNewsletterSubscriptions(ctx, dbgen.CountNewsletterSubscriptionsParams{
		Statuses: orEmpty(f.Statuses), SyncStatuses: orEmpty(f.SyncStatuses), AudienceID: campaign.NullUUID(f.AudienceID), Q: f.Q})
	if err != nil {
		return Page[dbgen.ListNewsletterSubscriptionsRow]{}, apperr.Internal(err)
	}
	return Page[dbgen.ListNewsletterSubscriptionsRow]{Rows: rows, Total: total}, nil
}

// RetrySubscription requeues a failed push.
func (s *Service) RetrySubscription(ctx context.Context, id uuid.UUID) (dbgen.NewsletterSubscription, error) {
	sub, err := s.store.GetNewsletterSubscription(ctx, id)
	if err != nil {
		return dbgen.NewsletterSubscription{}, notFound("subscription", err)
	}
	if sub.SyncStatus != campaign.SyncFailed && sub.Status != campaign.SubError {
		return dbgen.NewsletterSubscription{}, apperr.Conflict("only a failed subscription can be retried")
	}
	err = s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		if _, err := dbgen.New(tx).RequeueNewsletterSubscription(ctx, dbgen.RequeueNewsletterSubscriptionParams{ID: id}); err != nil {
			return err
		}
		// A fresh request id: the previous attempt with these arguments has already
		// run, and without it River would treat the retry as a duplicate.
		return s.enqueueTx(ctx, tx, campaign.NewsletterPushArgs{
			SubscriptionID: id, Action: campaign.NewsletterActionUpsert, RequestID: ids.New(),
		})
	})
	if err != nil {
		return dbgen.NewsletterSubscription{}, apperr.Internal(err)
	}
	row, err := s.store.GetNewsletterSubscription(ctx, id)
	if err != nil {
		return dbgen.NewsletterSubscription{}, apperr.Internal(err)
	}
	return row, nil
}

// GetNewsletterStats summarises the stage.
func (s *Service) GetNewsletterStats(ctx context.Context) (NewsletterStats, error) {
	rows, err := s.store.CountNewsletterSubscriptionsByStatus(ctx)
	if err != nil {
		return NewsletterStats{}, apperr.Internal(err)
	}
	stats := NewsletterStats{ByStatus: map[string]int64{}}
	for _, r := range rows {
		stats.ByStatus[r.Status] = r.Total
	}
	stats.Subscribed = stats.ByStatus[campaign.SubSubscribed]
	stats.Pending = stats.ByStatus[campaign.SubPending]
	stages, err := s.store.CountContactsByStage(ctx)
	if err != nil {
		return NewsletterStats{}, apperr.Internal(err)
	}
	var contacted int64
	for _, st := range stages {
		stage := campaign.Stage(st.LifecycleStage)
		if stage == campaign.StageNewsletterEligible {
			stats.Eligible += st.Total
		}
		if stage.Rank() >= campaign.StageContacted.Rank() && !stage.Terminal() {
			contacted += st.Total
		}
	}
	if contacted > 0 {
		stats.ConversionRate = float64(stats.Subscribed) / float64(contacted)
	}
	return stats, nil
}

func randomToken() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return hex.EncodeToString([]byte(ids.New().String()))
	}
	return hex.EncodeToString(buf)
}

var _ = json.Marshal
