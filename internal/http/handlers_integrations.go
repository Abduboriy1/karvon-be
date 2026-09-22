package httpapi

import (
	"net/http"

	campaignsvc "github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/http/gen"
)

// webhookRegisterRequest mirrors the spec's WebhookRegisterRequest.
type webhookRegisterRequest struct {
	// Rotate replaces the token and signing secret. The previous URL stops working
	// the moment it succeeds, so deliveries in flight against it are lost.
	Rotate *bool `json:"rotate"`
}

// GetIntegrations implements GET /integrations.
func (s *Server) GetIntegrations(w http.ResponseWriter, r *http.Request) {
	status, err := s.campaigns.GetIntegrations(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIIntegrations(status))
}

// TestInstantlyConnection implements POST /integrations/instantly/test.
func (s *Server) TestInstantlyConnection(w http.ResponseWriter, r *http.Request) {
	result, err := s.campaigns.TestInstantly(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIProviderTestResult(result))
}

// RegisterInstantlyWebhook implements POST /integrations/instantly/webhook. The body is
// optional: without one an existing registration is returned as it stands.
func (s *Server) RegisterInstantlyWebhook(w http.ResponseWriter, r *http.Request) {
	var req webhookRegisterRequest
	if err := decodeJSONIfPresent(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	rotate := req.Rotate != nil && *req.Rotate
	status, err := s.campaigns.RegisterInstantlyWebhook(r.Context(), rotate)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIWebhookStatus(status))
}

// DeleteInstantlyWebhook implements DELETE /integrations/instantly/webhook.
func (s *Server) DeleteInstantlyWebhook(w http.ResponseWriter, r *http.Request) {
	if err := s.campaigns.DeleteInstantlyWebhook(r.Context()); err != nil {
		WriteError(w, r, err)
		return
	}
	writeNoContent(w)
}

// TestMailchimpConnection implements POST /integrations/mailchimp/test.
func (s *Server) TestMailchimpConnection(w http.ResponseWriter, r *http.Request) {
	result, err := s.campaigns.TestMailchimp(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIProviderTestResult(result))
}

// ListProviderEvents implements GET /integrations/events.
func (s *Server) ListProviderEvents(w http.ResponseWriter, r *http.Request, params gen.ListProviderEventsParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	filter := campaignsvc.ProviderEventFilter{
		Processed:  params.Processed,
		HasError:   params.HasError,
		ContactID:  params.ContactId,
		CampaignID: params.CampaignId,
	}
	if params.Provider != nil {
		provider := string(*params.Provider)
		filter.Provider = &provider
	}

	result, err := s.campaigns.ListProviderEvents(r.Context(), filter, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.ProviderEvent, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPIProviderEvent(row))
	}
	writeJSON(w, r, http.StatusOK, gen.ProviderEventList{
		Data: data,
		Meta: pageMeta(page, perPage, result.Total),
	})
}

// ReprocessProviderEvent implements POST /integrations/events/{id}/reprocess.
func (s *Server) ReprocessProviderEvent(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	row, err := s.campaigns.ReprocessProviderEvent(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// 202: the replay is queued, not done.
	writeJSON(w, r, http.StatusAccepted, toAPIProviderEvent(row))
}

// ListSyncRuns implements GET /integrations/sync-runs.
func (s *Server) ListSyncRuns(w http.ResponseWriter, r *http.Request, params gen.ListSyncRunsParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	var kind *string
	if params.Kind != nil {
		value := string(*params.Kind)
		kind = &value
	}

	result, err := s.campaigns.ListSyncRuns(r.Context(), kind, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.SyncRun, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPISyncRun(row))
	}
	writeJSON(w, r, http.StatusOK, gen.SyncRunList{
		Data: data,
		Meta: pageMeta(page, perPage, result.Total),
	})
}
