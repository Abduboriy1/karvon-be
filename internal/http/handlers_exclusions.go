package httpapi

import (
	"net/http"
	"slices"
	"strings"

	"github.com/bory/karvon-be/internal/apperr"

	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/exclusion"
	"github.com/bory/karvon-be/internal/http/gen"
)

// ListExclusions implements GET /exclusions.
func (s *Server) ListExclusions(w http.ResponseWriter, r *http.Request, params gen.ListExclusionsParams) {
	page, perPage := paginate(params.Page, params.PerPage)
	filter := db.ExclusionFilter{Q: params.Q}
	if params.Kind != nil {
		for _, kind := range *params.Kind {
			if !slices.Contains(exclusion.Kinds, string(kind)) {
				WriteError(w, r, apperr.Validation("invalid query parameters", apperr.FieldError{
					Field: "kind", Message: "must be one of " + strings.Join(exclusion.Kinds, ", "),
				}))
				return
			}
			filter.Kinds = append(filter.Kinds, string(kind))
		}
	}
	if params.Status != nil {
		switch status := string(*params.Status); status {
		case "active", "removed", "all":
			filter.Status = status
		default:
			WriteError(w, r, apperr.Validation("invalid query parameters", apperr.FieldError{
				Field: "status", Message: "must be one of active, removed, all",
			}))
			return
		}
	}
	sort, err := resolveSortParam(params.Sort, db.ExclusionSortKeys())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	result, err := s.exclusions.List(r.Context(), filter, sort, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	data := make([]gen.GlobalExclusion, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPIExclusion(row))
	}
	writeJSON(w, r, http.StatusOK, gen.GlobalExclusionList{Data: data, Meta: pageMeta(page, perPage, result.Total)})
}

// CreateExclusion implements POST /exclusions.
func (s *Server) CreateExclusion(w http.ResponseWriter, r *http.Request) {
	var req gen.GlobalExclusionCreate
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	row, err := s.exclusions.Create(r.Context(), exclusionInput(req))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toAPIExclusion(row))
}

// PreviewExclusion implements POST /exclusions/preview.
func (s *Server) PreviewExclusion(w http.ResponseWriter, r *http.Request) {
	var req gen.GlobalExclusionCreate
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	preview, err := s.exclusions.Preview(r.Context(), exclusionInput(req))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := gen.ExclusionPreview{
		Kind:         gen.ExclusionKind(preview.Kind),
		Value:        preview.Value,
		DisplayValue: preview.DisplayValue,
		MatchMode:    gen.ExclusionMatchMode(preview.MatchMode),
		ExistingId:   preview.ExistingID,
		Affected:     toAPIExclusionAffected(preview.Affected),
		SampleEmails: preview.SampleEmails,
		Warning:      preview.Warning,
	}
	if out.SampleEmails == nil {
		out.SampleEmails = []string{}
	}
	out.SampleBusinesses = make([]gen.ExclusionPreviewBusiness, 0, len(preview.SampleBusinesses))
	for _, b := range preview.SampleBusinesses {
		out.SampleBusinesses = append(out.SampleBusinesses, gen.ExclusionPreviewBusiness{Id: b.ID, Name: b.Name, Domain: b.Domain})
	}
	writeJSON(w, r, http.StatusOK, out)
}

// CheckExclusion implements POST /exclusions/check.
func (s *Server) CheckExclusion(w http.ResponseWriter, r *http.Request) {
	var req gen.ExclusionCheckRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	subject := exclusion.Subject{BusinessID: req.BusinessId}
	if req.Email != nil {
		subject.Email = *req.Email
	}
	if req.Domain != nil {
		subject.Domain = *req.Domain
	}
	if req.Company != nil {
		subject.Company = *req.Company
	}
	ref, err := s.exclusions.Check(r.Context(), subject)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, gen.ExclusionCheckResult{Excluded: ref != nil, Exclusion: toAPIExclusionRef(ref)})
}

// GetExclusion implements GET /exclusions/{id}.
func (s *Server) GetExclusion(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	row, err := s.exclusions.Get(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIExclusion(row))
}

// RemoveExclusion implements DELETE /exclusions/{id}.
func (s *Server) RemoveExclusion(w http.ResponseWriter, r *http.Request, id gen.IdPath, params gen.RemoveExclusionParams) {
	note := ""
	if params.Note != nil {
		note = *params.Note
	}
	if err := s.exclusions.Remove(r.Context(), id, note); err != nil {
		WriteError(w, r, err)
		return
	}
	writeNoContent(w)
}

func exclusionInput(req gen.GlobalExclusionCreate) exclusion.Input {
	in := exclusion.Input{Kind: string(req.Kind), Value: req.Value, Reason: req.Reason, SourceRefID: req.SourceRefId}
	if req.MatchMode != nil {
		in.MatchMode = string(*req.MatchMode)
	}
	if req.Source != nil {
		in.Source = string(*req.Source)
	}
	return in
}

func toAPIExclusion(row exclusion.Rule) gen.GlobalExclusion {
	out := gen.GlobalExclusion{
		Id:           row.ID,
		Kind:         gen.ExclusionKind(row.Kind),
		Value:        row.Value,
		DisplayValue: row.DisplayValue,
		MatchMode:    gen.ExclusionMatchMode(row.MatchMode),
		Reason:       row.Reason,
		Source:       gen.ExclusionSource(row.Source),
		CreatedAt:    utc(row.CreatedAt),
		RemovedAt:    utcPtr(row.RemovedAt),
		RemovedNote:  row.RemovedNote,
	}
	if row.SourceRefID.Valid {
		out.SourceRefId = &row.SourceRefID.UUID
	}
	affected := toAPIExclusionAffected(row.Affected)
	out.Affected = &affected
	return out
}

func toAPIExclusionAffected(a db.ExclusionAffected) gen.ExclusionAffected {
	return gen.ExclusionAffected{Businesses: a.Businesses, Emails: a.Emails, Contacts: a.Contacts, LiveLeads: a.LiveLeads}
}

// toAPIExclusionRef maps the rule behind an excluded record; nil stays nil.
func toAPIExclusionRef(ref *db.ExclusionRef) *gen.ExclusionRef {
	if ref == nil {
		return nil
	}
	return &gen.ExclusionRef{Id: ref.ID, Kind: gen.ExclusionKind(ref.Kind), Value: ref.Value, DisplayValue: ref.DisplayValue}
}
