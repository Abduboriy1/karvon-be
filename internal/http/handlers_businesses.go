package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/business"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/http/gen"
)

// businessUpdateRequest mirrors the spec's BusinessUpdate; notes is raw JSON so an
// explicit null can clear the note.
type businessUpdateRequest struct {
	Suppressed *bool           `json:"suppressed"`
	Notes      json.RawMessage `json:"notes"`
}

type businessBulkRequest struct {
	IDs    []uuid.UUID `json:"ids" validate:"required,min=1,max=5000"`
	Action string      `json:"action" validate:"required,oneof=suppress unsuppress"`
}

type businessExportRequest struct {
	JobID       *uuid.UUID `json:"job_id"`
	Category    *string    `json:"category" validate:"omitempty,max=200"`
	State       *string    `json:"state" validate:"omitempty,max=100"`
	City        *string    `json:"city" validate:"omitempty,max=200"`
	HasEmail    *bool      `json:"has_email"`
	Suppressed  *bool      `json:"suppressed"`
	Q           *string    `json:"q" validate:"omitempty,max=200"`
	EmailSource *string    `json:"email_source" validate:"omitempty,oneof=mailto regex provider"`
	//nolint:lll // the rule list is clearer on one line
	VerificationTag []string    `json:"verification_tag" validate:"omitempty,dive,oneof=green light_green yellow orange red"`
	IDs             []uuid.UUID `json:"ids" validate:"omitempty,max=5000"`
}

// ListBusinesses implements GET /businesses.
func (s *Server) ListBusinesses(w http.ResponseWriter, r *http.Request, params gen.ListBusinessesParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	filter := db.BusinessFilter{
		JobID:      params.JobId,
		Category:   params.Category,
		State:      params.State,
		City:       params.City,
		Q:          params.Q,
		HasEmail:   params.HasEmail,
		Suppressed: params.Suppressed,
	}
	if params.EmailSource != nil {
		source := string(*params.EmailSource)
		filter.EmailSource = &source
	}
	if params.VerificationTag != nil {
		for _, tag := range *params.VerificationTag {
			filter.VerificationTags = append(filter.VerificationTags, string(tag))
		}
	}

	sort, err := resolveSortParam(params.Sort, db.BusinessSortKeys())
	if err != nil {
		WriteError(w, r, err)
		return
	}

	result, err := s.businesses.List(r.Context(), filter, sort, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.Business, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPIBusiness(row, result.Socials[row.ID]))
	}
	writeJSON(w, r, http.StatusOK, gen.BusinessList{
		Data: data,
		Meta: pageMeta(page, perPage, result.Total),
	})
}

// GetBusiness implements GET /businesses/{id}.
func (s *Server) GetBusiness(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	detail, err := s.businesses.Get(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIBusinessDetail(detail))
}

// UpdateBusiness implements PATCH /businesses/{id}.
func (s *Server) UpdateBusiness(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var req businessUpdateRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}

	in := business.UpdateInput{Suppressed: req.Suppressed}
	if len(req.Notes) > 0 {
		in.SetNotes = true
		var notes *string
		if err := json.Unmarshal(req.Notes, &notes); err != nil {
			WriteError(w, r, badRequestFromDecodeError(err))
			return
		}
		in.Notes = notes
	}

	detail, err := s.businesses.Update(r.Context(), id, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIBusinessDetail(detail))
}

// BulkUpdateBusinesses implements POST /businesses/bulk.
func (s *Server) BulkUpdateBusinesses(w http.ResponseWriter, r *http.Request) {
	var req businessBulkRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := validateStruct(req); err != nil {
		WriteError(w, r, err)
		return
	}

	updated, err := s.businesses.Bulk(r.Context(), req.IDs, business.BulkAction(req.Action))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, gen.BulkResult{Updated: int(updated)})
}

type businessRecrawlRequest struct {
	businessExportRequest
	Targets []string `json:"targets" validate:"required,min=1,max=2,dive,oneof=emails socials"`
}

// RecrawlBusinesses implements POST /businesses/recrawl: a re-crawl job for the
// listed ids, or for every business matching the filter when no ids are given.
func (s *Server) RecrawlBusinesses(w http.ResponseWriter, r *http.Request) {
	var req businessRecrawlRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := validateStruct(req); err != nil {
		WriteError(w, r, err)
		return
	}
	row, err := s.jobs.RecrawlBusinesses(r.Context(), req.filter(), req.Targets)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.writeJob(w, r, http.StatusCreated, row)
}

// filter is the business set a request selects: exactly its ids, or its filters.
func (req businessExportRequest) filter() db.BusinessFilter {
	return db.BusinessFilter{
		JobID:       req.JobID,
		Category:    req.Category,
		State:       req.State,
		City:        req.City,
		Q:           req.Q,
		HasEmail:    req.HasEmail,
		Suppressed:  req.Suppressed,
		EmailSource: req.EmailSource,
		IDs:         req.IDs,

		VerificationTags: req.VerificationTag,
	}
}

// ExportBusinesses implements POST /businesses/export. The CSV is streamed, so the
// response starts before the query has finished.
func (s *Server) ExportBusinesses(w http.ResponseWriter, r *http.Request) {
	var req businessExportRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := validateStruct(req); err != nil {
		WriteError(w, r, err)
		return
	}

	s.streamCSV(w, r, req.filter(), "karvon-businesses")
}

// ExportJobCsv implements GET /jobs/{id}/export.csv.
func (s *Server) ExportJobCsv(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	if _, err := s.jobs.Get(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	jobID := id
	s.streamCSV(w, r, db.BusinessFilter{JobID: &jobID}, "karvon-job-"+id.String())
}

// streamCSV writes an attachment. Because the body starts before the whole result set
// is known, a mid-stream failure can only be logged, not turned into an error response.
func (s *Server) streamCSV(w http.ResponseWriter, r *http.Request, filter db.BusinessFilter, basename string) {
	filename := fmt.Sprintf("%s-%s.csv", basename, time.Now().UTC().Format("20060102-150405"))

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	var flush func()
	if flusher, ok := w.(http.Flusher); ok {
		flush = flusher.Flush
	}

	rows, err := s.businesses.Export(r.Context(), filter, w, flush)
	if err != nil {
		s.log.Error("csv export failed mid-stream", "error", err, "rows_written", rows)
		return
	}
	s.log.Info("csv export finished", "rows", rows, "filename", filename)
}
