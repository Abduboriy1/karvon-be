package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	campaignsvc "github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/http/gen"
)

// newsletterAudienceUpdateRequest mirrors the spec's NewsletterAudienceUpdate. Every
// field is optional; an omitted one leaves the stored value alone.
type newsletterAudienceUpdateRequest struct {
	AllowSingleOptIn *bool     `json:"allow_single_opt_in"`
	DefaultTags      *[]string `json:"default_tags" validate:"omitempty,max=20,dive,min=1,max=60"`
	IsDefault        *bool     `json:"is_default"`
}

// newsletterPushRequest mirrors the spec's NewsletterPushRequest.
type newsletterPushRequest struct {
	AudienceID *uuid.UUID  `json:"audience_id"`
	ContactIDs []uuid.UUID `json:"contact_ids" validate:"required,min=1,max=5000"`
}

// ListNewsletterAudiences implements GET /newsletter/audiences.
func (s *Server) ListNewsletterAudiences(w http.ResponseWriter, r *http.Request) {
	rows, err := s.campaigns.ListAudiences(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	payload := make([]gen.NewsletterAudience, 0, len(rows))
	for _, row := range rows {
		payload = append(payload, toAPINewsletterAudience(row))
	}
	writeJSON(w, r, http.StatusOK, payload)
}

// SyncNewsletterAudiences implements POST /newsletter/audiences/sync.
func (s *Server) SyncNewsletterAudiences(w http.ResponseWriter, r *http.Request) {
	if err := s.campaigns.SyncAudiences(r.Context()); err != nil {
		WriteError(w, r, err)
		return
	}
	// 202: the refresh is queued, not done.
	writeJSON(w, r, http.StatusAccepted, nil)
}

// UpdateNewsletterAudience implements PATCH /newsletter/audiences/{id}.
func (s *Server) UpdateNewsletterAudience(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var req newsletterAudienceUpdateRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := validateStruct(req); err != nil {
		WriteError(w, r, err)
		return
	}

	in := campaignsvc.AudienceUpdate{
		AllowSingleOptIn: req.AllowSingleOptIn,
		IsDefault:        req.IsDefault,
	}
	if req.DefaultTags != nil {
		in.DefaultTags = *req.DefaultTags
	}

	row, err := s.campaigns.UpdateAudience(r.Context(), id, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPINewsletterAudience(row))
}

// RegisterNewsletterWebhook implements POST /newsletter/audiences/{id}/webhook.
func (s *Server) RegisterNewsletterWebhook(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	row, err := s.campaigns.RegisterMailchimpWebhook(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPINewsletterAudience(row))
}

// DeleteNewsletterWebhook implements DELETE /newsletter/audiences/{id}/webhook. The
// spec answers with the updated audience rather than 204, so the UI sees the
// registration cleared without a second round trip.
func (s *Server) DeleteNewsletterWebhook(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	row, err := s.campaigns.DeleteMailchimpWebhook(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPINewsletterAudience(row))
}

// ListNewsletterEligibility implements GET /newsletter/eligibility.
func (s *Server) ListNewsletterEligibility(w http.ResponseWriter, r *http.Request, params gen.ListNewsletterEligibilityParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	result, err := s.campaigns.ListEligible(r.Context(), params.CampaignId, params.AudienceId, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.EligibleContact, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPIEligibleContact(row))
	}
	writeJSON(w, r, http.StatusOK, gen.EligibleContactList{
		Data: data,
		Meta: pageMeta(page, perPage, result.Total),
	})
}

// PushNewsletterContacts implements POST /newsletter/push. The consent gate is applied
// per contact, so a rejected contact comes back with its reason rather than failing
// the whole request.
func (s *Server) PushNewsletterContacts(w http.ResponseWriter, r *http.Request) {
	var req newsletterPushRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := validateStruct(req); err != nil {
		WriteError(w, r, err)
		return
	}

	result, err := s.campaigns.PushSubscriptions(r.Context(), req.ContactIDs, req.AudienceID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPINewsletterPushResult(result))
}

// ListNewsletterSubscriptions implements GET /newsletter/subscriptions.
func (s *Server) ListNewsletterSubscriptions(w http.ResponseWriter, r *http.Request, params gen.ListNewsletterSubscriptionsParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	filter := campaignsvc.SubscriptionFilter{
		AudienceID: params.AudienceId,
		Q:          params.Q,
	}
	if params.Status != nil {
		for _, status := range *params.Status {
			filter.Statuses = append(filter.Statuses, string(status))
		}
	}
	if params.SyncStatus != nil {
		for _, status := range *params.SyncStatus {
			filter.SyncStatuses = append(filter.SyncStatuses, string(status))
		}
	}

	result, err := s.campaigns.ListSubscriptions(r.Context(), filter, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.NewsletterSubscription, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPINewsletterSubscription(newsletterSubscriptionFromRow(row)))
	}
	writeJSON(w, r, http.StatusOK, gen.NewsletterSubscriptionList{
		Data: data,
		Meta: pageMeta(page, perPage, result.Total),
	})
}

// RetryNewsletterSubscription implements POST /newsletter/subscriptions/{id}/retry.
func (s *Server) RetryNewsletterSubscription(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	row, err := s.campaigns.RetrySubscription(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPINewsletterSubscription(row))
}

// GetNewsletterStats implements GET /newsletter/stats.
func (s *Server) GetNewsletterStats(w http.ResponseWriter, r *http.Request) {
	result, err := s.campaigns.GetNewsletterStats(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPINewsletterStats(result))
}

// newsletterSubscriptionFromRow narrows the list row to the subscription record the
// wire type is built from; the row's joined columns (the contact's address, the
// audience's name) are not part of NewsletterSubscription.
func newsletterSubscriptionFromRow(row dbgen.ListNewsletterSubscriptionsRow) dbgen.NewsletterSubscription {
	return dbgen.NewsletterSubscription{
		ID:                 row.ID,
		ContactID:          row.ContactID,
		AudienceID:         row.AudienceID,
		ConsentID:          row.ConsentID,
		RequestedStatus:    row.RequestedStatus,
		Status:             row.Status,
		SyncStatus:         row.SyncStatus,
		SubscriberHash:     row.SubscriberHash,
		UniqueEmailID:      row.UniqueEmailID,
		MailchimpContactID: row.MailchimpContactID,
		WebID:              row.WebID,
		ClaimedAt:          row.ClaimedAt,
		PushedAt:           row.PushedAt,
		LastSyncedAt:       row.LastSyncedAt,
		LastError:          row.LastError,
		SyncAttempts:       row.SyncAttempts,
		SubscribedAt:       row.SubscribedAt,
		UnsubscribedAt:     row.UnsubscribedAt,
		UnsubscribeReason:  row.UnsubscribeReason,
		Tags:               row.Tags,
		CreatedAt:          row.CreatedAt,
		UpdatedAt:          row.UpdatedAt,
	}
}
