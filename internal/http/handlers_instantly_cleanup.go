package httpapi

import (
	"net/http"

	"github.com/bory/karvon-be/internal/campaign"
	campaignsvc "github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/http/gen"
)

// Defaults for a cleanup preview whose query leaves a field out; they match the
// saved policy's column defaults.
const (
	defaultCleanupScope       = campaign.CleanupScopeFinished
	defaultCleanupMinIdleDays = 3
)

// GetInstantlyCleanup implements GET /integrations/instantly/cleanup.
func (s *Server) GetInstantlyCleanup(w http.ResponseWriter, r *http.Request) {
	overview, err := s.campaigns.GetCleanupOverview(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPICleanupOverview(overview))
}

// UpdateInstantlyCleanupSettings implements PUT /integrations/instantly/cleanup/settings.
func (s *Server) UpdateInstantlyCleanupSettings(w http.ResponseWriter, r *http.Request) {
	var body gen.InstantlyCleanupSettings
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	saved, err := s.campaigns.UpdateCleanupSettings(r.Context(), campaignsvc.CleanupSettings{
		CleanupPolicy: campaignsvc.CleanupPolicy{Scope: string(body.Scope), MinIdleDays: body.MinIdleDays, IncludeReplied: body.IncludeReplied},
		AutoEnabled:   body.AutoEnabled,
		ContactLimit:  body.ContactLimit,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPICleanupSettings(saved))
}

// PreviewInstantlyCleanup implements GET /integrations/instantly/cleanup/preview.
func (s *Server) PreviewInstantlyCleanup(w http.ResponseWriter, r *http.Request, params gen.PreviewInstantlyCleanupParams) {
	in := campaignsvc.CleanupRequest{
		CleanupPolicy: campaignsvc.CleanupPolicy{Scope: defaultCleanupScope, MinIdleDays: defaultCleanupMinIdleDays},
		MaxLeads:      params.MaxLeads,
	}
	if params.Scope != nil {
		in.Scope = string(*params.Scope)
	}
	if params.MinIdleDays != nil {
		in.MinIdleDays = *params.MinIdleDays
	}
	if params.IncludeReplied != nil {
		in.IncludeReplied = *params.IncludeReplied
	}
	if params.CampaignId != nil {
		in.CampaignIDs = *params.CampaignId
	}
	preview, err := s.campaigns.PreviewCleanup(r.Context(), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPICleanupPreview(preview))
}

// ListInstantlyCleanupRuns implements GET /integrations/instantly/cleanup/runs.
func (s *Server) ListInstantlyCleanupRuns(w http.ResponseWriter, r *http.Request, params gen.ListInstantlyCleanupRunsParams) {
	page, perPage := paginate(params.Page, params.PerPage)
	result, err := s.campaigns.ListCleanupRuns(r.Context(), page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	data := make([]gen.InstantlyCleanupRun, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPICleanupRun(row))
	}
	writeJSON(w, r, http.StatusOK, gen.InstantlyCleanupRunList{Data: data, Meta: pageMeta(page, perPage, result.Total)})
}

// StartInstantlyCleanup implements POST /integrations/instantly/cleanup/runs.
func (s *Server) StartInstantlyCleanup(w http.ResponseWriter, r *http.Request) {
	var body gen.InstantlyCleanupRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	in := campaignsvc.CleanupRequest{
		CleanupPolicy: campaignsvc.CleanupPolicy{Scope: string(body.Scope), MinIdleDays: body.MinIdleDays, IncludeReplied: body.IncludeReplied},
		MaxLeads:      body.MaxLeads,
	}
	if body.CampaignIds != nil {
		in.CampaignIDs = *body.CampaignIds
	}
	run, err := s.campaigns.StartCleanup(r.Context(), in, campaign.CleanupTriggerManual)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// 202: the deletions run in the background, in batches.
	writeJSON(w, r, http.StatusAccepted, toAPICleanupRun(run))
}

// GetInstantlyCleanupRun implements GET /integrations/instantly/cleanup/runs/{id}.
func (s *Server) GetInstantlyCleanupRun(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	run, err := s.campaigns.GetCleanupRun(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPICleanupRun(run))
}
