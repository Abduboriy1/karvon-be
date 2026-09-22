package httpapi

import (
	"net/http"

	campaignsvc "github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/http/gen"
)

/* -------------------------------------------------------------- campaigns */

// ListCampaigns implements GET /campaigns.
func (s *Server) ListCampaigns(w http.ResponseWriter, r *http.Request, params gen.ListCampaignsParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	filter := db.CampaignFilter{Q: params.Q}
	if params.Status != nil {
		for _, status := range *params.Status {
			filter.Statuses = append(filter.Statuses, string(status))
		}
	}
	if params.IncludeArchived != nil {
		filter.IncludeArchived = *params.IncludeArchived
	}

	sort, err := resolveSortParam(params.Sort, db.CampaignSortKeys())
	if err != nil {
		WriteError(w, r, err)
		return
	}

	result, err := s.campaigns.ListCampaigns(r.Context(), filter, sort, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.Campaign, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPICampaign(row))
	}
	writeJSON(w, r, http.StatusOK, gen.CampaignList{
		Data: data,
		Meta: pageMeta(page, perPage, result.Total),
	})
}

// CreateCampaign implements POST /campaigns.
func (s *Server) CreateCampaign(w http.ResponseWriter, r *http.Request) {
	var body gen.CampaignCreate
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	in := campaignsvc.CampaignInput{
		Name:       &body.Name,
		Brief:      derefMap(body.Brief),
		Schedule:   derefMap(body.Schedule),
		Settings:   derefMap(body.Settings),
		Steps:      body.Steps,
		StepDelays: derefInts(body.StepDelays),
	}
	row, err := s.campaigns.CreateCampaign(r.Context(), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toAPICampaign(row))
}

// GetCampaign implements GET /campaigns/{id}.
func (s *Server) GetCampaign(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	detail, err := s.campaigns.GetCampaignDetail(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPICampaignDetail(detail))
}

// UpdateCampaign implements PATCH /campaigns/{id}.
func (s *Server) UpdateCampaign(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var body gen.CampaignUpdate
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	in := campaignsvc.CampaignInput{
		Name:       body.Name,
		Brief:      derefMap(body.Brief),
		Schedule:   derefMap(body.Schedule),
		Settings:   derefMap(body.Settings),
		Steps:      body.Steps,
		StepDelays: derefInts(body.StepDelays),
	}
	row, err := s.campaigns.UpdateCampaign(r.Context(), id, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPICampaign(row))
}

// ArchiveCampaign implements POST /campaigns/{id}/archive.
func (s *Server) ArchiveCampaign(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	row, err := s.campaigns.ArchiveCampaign(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPICampaign(row))
}

// GetCampaignChecklist implements GET /campaigns/{id}/checklist.
func (s *Server) GetCampaignChecklist(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	checklist, err := s.campaigns.GetChecklist(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIChecklist(checklist))
}

// LaunchCampaign implements POST /campaigns/{id}/launch.
func (s *Server) LaunchCampaign(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	row, err := s.campaigns.LaunchCampaign(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// 202: the leads are pushed in the background; the campaign is still `launching`.
	writeJSON(w, r, http.StatusAccepted, toAPICampaign(row))
}

// PauseCampaign implements POST /campaigns/{id}/pause.
func (s *Server) PauseCampaign(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	row, err := s.campaigns.PauseCampaign(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPICampaign(row))
}

// ResumeCampaign implements POST /campaigns/{id}/resume.
func (s *Server) ResumeCampaign(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	row, err := s.campaigns.ResumeCampaign(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPICampaign(row))
}

// SyncCampaign implements POST /campaigns/{id}/sync.
func (s *Server) SyncCampaign(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	if err := s.campaigns.SyncCampaign(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	// 202 with no body: reconciliation is queued, not done.
	writeJSON(w, r, http.StatusAccepted, nil)
}

// SetCampaignSendingAccounts implements PUT /campaigns/{id}/sending-accounts.
func (s *Server) SetCampaignSendingAccounts(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var body gen.CampaignSendingAccountsRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	accounts, err := s.campaigns.SetSendingAccounts(r.Context(), id, body.SendingAccountIds)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.SendingAccount, 0, len(accounts))
	for _, account := range accounts {
		data = append(data, toAPISendingAccount(account))
	}
	writeJSON(w, r, http.StatusOK, data)
}

// ListCampaignVariants implements GET /campaigns/{id}/variants.
func (s *Server) ListCampaignVariants(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	rows, err := s.campaigns.ListCampaignVariants(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, apiCampaignVariants(rows))
}

// SetCampaignVariants implements PUT /campaigns/{id}/variants.
func (s *Server) SetCampaignVariants(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var body gen.CampaignVariantsRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	items := make([]campaignsvc.VariantWeight, 0, len(body.Items))
	for _, item := range body.Items {
		weight := campaignsvc.VariantWeight{
			VariantID: item.VariantId,
			Step:      item.Step,
			Weight:    item.Weight,
		}
		if item.Status != nil {
			weight.Status = string(*item.Status)
		}
		items = append(items, weight)
	}

	rows, err := s.campaigns.SetCampaignVariants(r.Context(), id, items)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, apiCampaignVariants(rows))
}

/* ------------------------------------------------------------------ leads */

// ListCampaignLeads implements GET /campaigns/{id}/leads.
func (s *Server) ListCampaignLeads(w http.ResponseWriter, r *http.Request, id gen.IdPath, params gen.ListCampaignLeadsParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	filter := db.LeadFilter{CampaignID: id, Q: params.Q}
	if params.Status != nil {
		for _, status := range *params.Status {
			filter.Statuses = append(filter.Statuses, string(status))
		}
	}
	if params.Stage != nil {
		for _, stage := range *params.Stage {
			filter.Stages = append(filter.Stages, string(stage))
		}
	}

	sort, err := resolveSortParam(params.Sort, db.LeadSortKeys())
	if err != nil {
		WriteError(w, r, err)
		return
	}

	result, err := s.campaigns.ListLeads(r.Context(), filter, sort, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.CampaignLead, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPICampaignLead(row))
	}
	writeJSON(w, r, http.StatusOK, gen.CampaignLeadList{
		Data: data,
		Meta: pageMeta(page, perPage, result.Total),
	})
}

// ImportCampaignLeads implements POST /campaigns/{id}/leads/import.
func (s *Server) ImportCampaignLeads(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var body gen.LeadImportRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	result, err := s.campaigns.ImportLeads(r.Context(), id, toImportFilter(body.Filter))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPILeadImportResult(result))
}

// EstimateCampaignLeadImport implements POST /campaigns/{id}/leads/estimate.
func (s *Server) EstimateCampaignLeadImport(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var body gen.LeadImportRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	result, err := s.campaigns.EstimateImport(r.Context(), id, toImportFilter(body.Filter))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPILeadImportResult(result))
}

// GetCampaignLead implements GET /campaigns/{id}/leads/{lead_id}.
func (s *Server) GetCampaignLead(w http.ResponseWriter, r *http.Request, id gen.IdPath, leadID gen.LeadIdPath) {
	detail, err := s.campaigns.GetLead(r.Context(), id, leadID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPICampaignLeadDetail(detail))
}

// DeleteCampaignLead implements DELETE /campaigns/{id}/leads/{lead_id}.
func (s *Server) DeleteCampaignLead(w http.ResponseWriter, r *http.Request, id gen.IdPath, leadID gen.LeadIdPath) {
	if err := s.campaigns.RemoveLead(r.Context(), id, leadID); err != nil {
		WriteError(w, r, err)
		return
	}
	writeNoContent(w)
}

/* --------------------------------------------------------------- activity */

// ListCampaignActivity implements GET /campaigns/{id}/activity.
func (s *Server) ListCampaignActivity(w http.ResponseWriter, r *http.Request, id gen.IdPath, params gen.ListCampaignActivityParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	result, err := s.campaigns.ListCampaignActivity(r.Context(), id, eventTypes(params.Type), page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, apiContactEventList(result, page, perPage))
}

// ListActivity implements GET /activity.
func (s *Server) ListActivity(w http.ResponseWriter, r *http.Request, params gen.ListActivityParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	filter := campaignsvc.ActivityFilter{
		CampaignID: params.CampaignId,
		ContactID:  params.ContactId,
		Types:      eventTypes(params.Type),
	}
	result, err := s.campaigns.ListActivity(r.Context(), filter, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, apiContactEventList(result, page, perPage))
}

/* ---------------------------------------------------------------- helpers */

// derefMap unwraps an optional JSON object body field. A body that omitted the
// field leaves the stored document untouched, which the service reads as nil.
func derefMap(in *map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	return *in
}

func derefInts(in *[]int) []int {
	if in == nil {
		return nil
	}
	return *in
}

// eventTypes converts the repeatable enum query parameter into the plain strings
// the service filters on.
func eventTypes(in *[]gen.ContactEventType) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(*in))
	for _, value := range *in {
		out = append(out, string(value))
	}
	return out
}

// toImportFilter maps the wire import filter onto the data layer's. `primary_only`
// defaults to true: an import takes each business's primary address unless the
// client asks for every address it holds.
func toImportFilter(in gen.LeadImportFilter) db.ImportFilter {
	out := db.ImportFilter{
		State:       in.State,
		City:        in.City,
		Category:    in.Category,
		JobID:       in.JobId,
		PrimaryOnly: in.PrimaryOnly == nil || *in.PrimaryOnly,
	}
	if in.BusinessIds != nil {
		out.BusinessIDs = *in.BusinessIds
	}
	if in.VerificationTag != nil {
		for _, tag := range *in.VerificationTag {
			out.VerificationTags = append(out.VerificationTags, string(tag))
		}
	}
	return out
}

func apiCampaignVariants(rows []dbgen.ListCampaignVariantsRow) []gen.CampaignVariant {
	out := make([]gen.CampaignVariant, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAPICampaignVariant(row))
	}
	return out
}

func apiContactEventList(result campaignsvc.Page[dbgen.ContactEvent], page, perPage int) gen.ContactEventList {
	data := make([]gen.ContactEvent, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPIContactEvent(row))
	}
	return gen.ContactEventList{Data: data, Meta: pageMeta(page, perPage, result.Total)}
}
