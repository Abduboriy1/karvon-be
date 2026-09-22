package httpapi

import (
	"net/http"

	"github.com/bory/karvon-be/internal/http/gen"
)

// defaultAnalyticsDays is the length of the daily series when the client asks for none.
const defaultAnalyticsDays = 30

// GetCampaignOverview implements GET /campaign-analytics/overview.
func (s *Server) GetCampaignOverview(w http.ResponseWriter, r *http.Request) {
	overview, err := s.campaigns.GetOverview(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIOverview(overview))
}

// GetCampaignAnalytics implements GET /campaign-analytics/campaigns/{id}.
func (s *Server) GetCampaignAnalytics(w http.ResponseWriter, r *http.Request, id gen.IdPath, params gen.GetCampaignAnalyticsParams) {
	days := defaultAnalyticsDays
	if params.Days != nil {
		days = *params.Days
	}

	analytics, err := s.campaigns.GetCampaignAnalytics(r.Context(), id, days)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPICampaignAnalytics(analytics))
}

// GetCampaignVariantAnalytics implements GET /campaign-analytics/campaigns/{id}/variants.
func (s *Server) GetCampaignVariantAnalytics(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	rows, err := s.campaigns.GetVariantAnalytics(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	payload := make([]gen.VariantAnalytics, 0, len(rows))
	for _, row := range rows {
		payload = append(payload, toAPIVariantAnalytics(row))
	}
	writeJSON(w, r, http.StatusOK, payload)
}

// GetComponentAnalytics implements GET /campaign-analytics/components.
func (s *Server) GetComponentAnalytics(w http.ResponseWriter, r *http.Request, params gen.GetComponentAnalyticsParams) {
	var componentType string
	if params.Type != nil {
		componentType = string(*params.Type)
	}

	rows, err := s.campaigns.GetComponentAnalytics(r.Context(), componentType, params.CampaignId, params.Since)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	payload := make([]gen.ComponentAnalytics, 0, len(rows))
	for _, row := range rows {
		payload = append(payload, toAPIComponentAnalytics(row))
	}
	writeJSON(w, r, http.StatusOK, payload)
}

// GetAccountAnalytics implements GET /campaign-analytics/sending-accounts.
func (s *Server) GetAccountAnalytics(w http.ResponseWriter, r *http.Request) {
	rows, err := s.campaigns.GetSendingAccountAnalytics(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	payload := make([]gen.AccountAnalytics, 0, len(rows))
	for _, row := range rows {
		payload = append(payload, toAPIAccountAnalytics(row))
	}
	writeJSON(w, r, http.StatusOK, payload)
}

// GetFunnelAnalytics implements GET /campaign-analytics/funnel.
func (s *Server) GetFunnelAnalytics(w http.ResponseWriter, r *http.Request, params gen.GetFunnelAnalyticsParams) {
	steps, err := s.campaigns.GetFunnel(r.Context(), params.CampaignId)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	payload := make([]gen.FunnelStep, 0, len(steps))
	for _, step := range steps {
		payload = append(payload, toAPIFunnelStep(step))
	}
	writeJSON(w, r, http.StatusOK, payload)
}
