package httpapi

import (
	"bytes"
	"io"
	"net/http"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/ai"
	campaignsvc "github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/http/gen"
)

// aiBriefRequest mirrors the spec's AIBrief. It is spelled out here rather than
// decoded into gen.AIBrief so the bounds the spec documents are enforced before the
// brief reaches the prompt builder.
type aiBriefRequest struct {
	CampaignGoal      *string  `json:"campaign_goal" validate:"omitempty,max=2000"`
	Company           *string  `json:"company" validate:"omitempty,max=200"`
	Product           *string  `json:"product" validate:"omitempty,max=500"`
	TargetIndustry    *string  `json:"target_industry" validate:"omitempty,max=200"`
	TargetJobTitle    *string  `json:"target_job_title" validate:"omitempty,max=200"`
	TargetCompanySize *string  `json:"target_company_size" validate:"omitempty,max=100"`
	PainPoints        []string `json:"pain_points" validate:"max=20,dive,max=500"`
	ValueProposition  *string  `json:"value_proposition" validate:"omitempty,max=2000"`
	DesiredCTA        *string  `json:"desired_cta" validate:"omitempty,max=500"`
	Tone              *string  `json:"tone" validate:"omitempty,max=100"`
	Language          *string  `json:"language" validate:"omitempty,max=10"`
	AdditionalContext *string  `json:"additional_context" validate:"omitempty,max=4000"`
	SubjectCount      *int     `json:"subject_count" validate:"omitempty,gte=0,lte=50"`
	HookCount         *int     `json:"hook_count" validate:"omitempty,gte=0,lte=50"`
	BodyCount         *int     `json:"body_count" validate:"omitempty,gte=0,lte=50"`
	CTACount          *int     `json:"cta_count" validate:"omitempty,gte=0,lte=50"`
	VariantCount      *int     `json:"variant_count" validate:"omitempty,gte=0,lte=50"`
}

// aiGenerationCreateRequest mirrors the spec's AIGenerationCreate.
type aiGenerationCreateRequest struct {
	Brief      aiBriefRequest `json:"brief"`
	CampaignID *gen.IdPath    `json:"campaign_id"`
}

// aiParseRequest mirrors the spec's AIParseRequest.
type aiParseRequest struct {
	RawOutput string `json:"raw_output" validate:"required,min=1,max=200000"`
}

// aiImportRequest mirrors the spec's AIImportRequest. Omitting both index lists
// imports everything the generation parsed.
type aiImportRequest struct {
	AttachToCampaignID *gen.IdPath `json:"attach_to_campaign_id"`
	ComponentIndexes   *[]int      `json:"component_indexes" validate:"omitempty,max=200,dive,gte=0"`
	VariantIndexes     *[]int      `json:"variant_indexes" validate:"omitempty,max=200,dive,gte=0"`
}

// GetAiProvider implements GET /ai/provider.
func (s *Server) GetAiProvider(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, toAPIAIProvider(s.campaigns.AIProviderInfo()))
}

// ListAiGenerations implements GET /ai/generations.
func (s *Server) ListAiGenerations(w http.ResponseWriter, r *http.Request, params gen.ListAiGenerationsParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	result, err := s.campaigns.ListGenerations(r.Context(), params.CampaignId, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.AIGeneration, 0, len(result.Rows))
	for _, view := range result.Rows {
		data = append(data, toAPIAIGeneration(view))
	}
	writeJSON(w, r, http.StatusOK, gen.AIGenerationList{
		Data: data,
		Meta: pageMeta(page, perPage, result.Total),
	})
}

// CreateAiGeneration implements POST /ai/generations.
func (s *Server) CreateAiGeneration(w http.ResponseWriter, r *http.Request) {
	var req aiGenerationCreateRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := validateStruct(req); err != nil {
		WriteError(w, r, err)
		return
	}

	view, err := s.campaigns.CreateGeneration(r.Context(), aiBriefFromRequest(req.Brief), req.CampaignID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toAPIAIGeneration(view))
}

// GetAiGeneration implements GET /ai/generations/{id}.
func (s *Server) GetAiGeneration(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	view, err := s.campaigns.GetGeneration(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIAIGeneration(view))
}

// ParseAiGeneration implements POST /ai/generations/{id}/parse.
func (s *Server) ParseAiGeneration(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var req aiParseRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := validateStruct(req); err != nil {
		WriteError(w, r, err)
		return
	}

	view, err := s.campaigns.ParseGeneration(r.Context(), id, req.RawOutput)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIAIGeneration(view))
}

// ImportAiGeneration implements POST /ai/generations/{id}/import. The body is
// optional: without one, everything the generation parsed is imported.
func (s *Server) ImportAiGeneration(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var req aiImportRequest
	if err := decodeJSONIfPresent(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := validateStruct(req); err != nil {
		WriteError(w, r, err)
		return
	}

	// A nil index list means "all", which is not the same as an empty one.
	selection := campaignsvc.ImportSelection{CampaignID: req.AttachToCampaignID}
	if req.ComponentIndexes != nil {
		selection.ComponentIndexes = *req.ComponentIndexes
	}
	if req.VariantIndexes != nil {
		selection.VariantIndexes = *req.VariantIndexes
	}

	outcome, err := s.campaigns.ImportGeneration(r.Context(), id, selection)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIAIImportResult(outcome))
}

// aiBriefFromRequest maps the wire brief onto the generator's own.
func aiBriefFromRequest(in aiBriefRequest) ai.Brief {
	return ai.Brief{
		CampaignGoal:      campaign.Deref(in.CampaignGoal),
		Company:           campaign.Deref(in.Company),
		Product:           campaign.Deref(in.Product),
		TargetIndustry:    campaign.Deref(in.TargetIndustry),
		TargetJobTitle:    campaign.Deref(in.TargetJobTitle),
		TargetCompanySize: campaign.Deref(in.TargetCompanySize),
		PainPoints:        in.PainPoints,
		ValueProposition:  campaign.Deref(in.ValueProposition),
		DesiredCTA:        campaign.Deref(in.DesiredCTA),
		Tone:              campaign.Deref(in.Tone),
		Language:          campaign.Deref(in.Language),
		AdditionalContext: campaign.Deref(in.AdditionalContext),
		SubjectCount:      campaign.Deref(in.SubjectCount),
		HookCount:         campaign.Deref(in.HookCount),
		BodyCount:         campaign.Deref(in.BodyCount),
		CTACount:          campaign.Deref(in.CTACount),
		VariantCount:      campaign.Deref(in.VariantCount),
	}
}

// decodeJSONIfPresent decodes a body the spec marks optional. An absent or empty body
// leaves dst untouched; anything else is decoded with the usual strictness.
func decodeJSONIfPresent(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	if err != nil {
		return apperr.BadRequest("the request body could not be read")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return decodeJSON(r, dst)
}
