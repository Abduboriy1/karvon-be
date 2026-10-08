package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/webhook"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// IngestResult reports what the webhook endpoint did with a delivery.
type IngestResult struct {
	EventID   uuid.UUID
	Duplicate bool
	Stored    bool
}

// ErrUnknownWebhookToken means the path token matches nothing.
var ErrUnknownWebhookToken = errors.New("campaign: unknown webhook token")

// ErrBadWebhookSecret means the shared secret did not match.
var ErrBadWebhookSecret = errors.New("campaign: webhook secret mismatch")

// AuthenticateInstantlyWebhook checks the path token and the shared secret header.
func (s *Service) AuthenticateInstantlyWebhook(ctx context.Context, token, secret string) error {
	settings, err := s.store.GetCampaignSettings(ctx)
	if err != nil {
		return apperr.Internal(err)
	}
	if settings.InstantlyWebhookToken == nil || !webhook.VerifySecret(token, *settings.InstantlyWebhookToken) {
		return ErrUnknownWebhookToken
	}
	if len(settings.InstantlyWebhookSecretEnc) == 0 {
		return ErrBadWebhookSecret
	}
	want, err := s.cipher.DecryptString(settings.InstantlyWebhookSecretEnc)
	if err != nil {
		return apperr.Internal(err)
	}
	if !webhook.VerifySecret(secret, want) {
		return ErrBadWebhookSecret
	}
	return nil
}

// IngestInstantly stores one Instantly delivery and queues its processing. The
// same payload twice is stored once and queued once. Unparseable payloads are
// stored with an error so nothing is lost, and still count as accepted.
func (s *Service) IngestInstantly(ctx context.Context, body []byte, source string) (IngestResult, error) {
	ev, parseErr := webhook.ParseInstantly(body)
	raw := json.RawMessage(body)
	if !json.Valid(body) {
		raw, _ = json.Marshal(map[string]any{"unparseable": string(body)})
	}
	eventType := ev.EventType
	if eventType == "" {
		eventType = "unknown"
	}
	key := webhook.InstantlyDedupeKey(ev)
	if parseErr != nil {
		key = "instantly|unparseable|" + ids.New().String()
	}
	var occurred *time.Time
	if !ev.Timestamp.IsZero() {
		t := ev.Timestamp.UTC()
		occurred = &t
	}
	result, err := s.storeEvent(ctx, dbgen.InsertProviderEventParams{
		ID: ids.New(), Provider: campaign.ProviderInstantly, EventType: eventType, DedupeKey: key,
		OccurredAt: occurred, Raw: raw, Source: source,
	}, parseErr)
	// A reply puts a new email in the Unibox: mirror it now rather than at the next
	// periodic pass. The delivery is already stored, so a failure here only delays
	// the inbox and must not make Instantly redeliver.
	if err == nil && result.Stored && (eventType == campaign.InstantlyReplyReceived || eventType == campaign.InstantlyAutoReplyReceived) {
		if err := s.SyncInbox(ctx); err != nil {
			s.log.Warn("could not queue an inbox sync after a reply", "error", err)
		}
	}
	return result, err
}

// AuthenticateMailchimpWebhook resolves the audience by token and verifies the signature.
func (s *Service) AuthenticateMailchimpWebhook(ctx context.Context, token, signature string, body []byte, tolerance time.Duration) (dbgen.NewsletterAudience, error) {
	audience, err := s.store.GetNewsletterAudienceByToken(ctx, campaign.Optional(token))
	if errors.Is(err, pgx.ErrNoRows) || token == "" {
		return dbgen.NewsletterAudience{}, ErrUnknownWebhookToken
	}
	if err != nil {
		return dbgen.NewsletterAudience{}, apperr.Internal(err)
	}
	if len(audience.WebhookSecretEnc) == 0 {
		return dbgen.NewsletterAudience{}, ErrBadWebhookSecret
	}
	secret, err := s.cipher.DecryptString(audience.WebhookSecretEnc)
	if err != nil {
		return dbgen.NewsletterAudience{}, apperr.Internal(err)
	}
	if err := webhook.VerifyMailchimpSignature(signature, body, secret, s.now(), tolerance); err != nil {
		return dbgen.NewsletterAudience{}, fmt.Errorf("%w: %w", ErrBadWebhookSecret, err)
	}
	return audience, nil
}

// IngestMailchimp stores one Mailchimp delivery and queues its processing.
func (s *Service) IngestMailchimp(ctx context.Context, audience dbgen.NewsletterAudience, form url.Values, source string) (IngestResult, error) {
	ev, parseErr := webhook.ParseMailchimp(form)
	if ev.ListID == "" {
		ev.ListID = audience.MailchimpListID
	}
	raw, _ := json.Marshal(map[string]any{"form": form, "audience_id": audience.ID})
	eventType := ev.Type
	if eventType == "" {
		eventType = "unknown"
	}
	key := webhook.MailchimpDedupeKey(ev)
	if parseErr != nil {
		key = "mailchimp|unparseable|" + ids.New().String()
	}
	var occurred *time.Time
	if !ev.FiredAt.IsZero() {
		t := ev.FiredAt.UTC()
		occurred = &t
	}
	return s.storeEvent(ctx, dbgen.InsertProviderEventParams{
		ID: ids.New(), Provider: campaign.ProviderMailchimp, EventType: eventType, DedupeKey: key,
		OccurredAt: occurred, Raw: raw, Source: source,
	}, parseErr)
}

func (s *Service) storeEvent(ctx context.Context, params dbgen.InsertProviderEventParams, parseErr error) (IngestResult, error) {
	var result IngestResult
	err := s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		row, err := q.InsertProviderEvent(ctx, params)
		if errors.Is(err, pgx.ErrNoRows) {
			result.Duplicate = true
			return nil
		}
		if err != nil {
			return err
		}
		result.EventID, result.Stored = row.ID, true
		if parseErr != nil {
			return q.MarkProviderEventProcessed(ctx, dbgen.MarkProviderEventProcessedParams{ID: row.ID, Error: campaign.Ptr("unparseable: " + parseErr.Error())})
		}
		return s.enqueueTx(ctx, tx, campaign.ProcessEventArgs{ProviderEventID: row.ID})
	})
	if err != nil {
		return IngestResult{}, apperr.Internal(err)
	}
	return result, nil
}
