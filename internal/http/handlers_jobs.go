package httpapi

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/http/gen"
	"github.com/bory/karvon-be/internal/scraper"
)

// jobCreateRequest is the wire shape of POST /jobs and POST /jobs/estimate.
type jobCreateRequest struct {
	Name     string           `json:"name" validate:"required,min=1,max=200"`
	SourceID uuid.UUID        `json:"source_id" validate:"required"`
	Config   jobConfigRequest `json:"config" validate:"required"`
}

type jobConfigRequest struct {
	Terms     []string          `json:"terms" validate:"required,min=1,max=20,dive,required,max=120"`
	Locations []locationRequest `json:"locations" validate:"omitempty,max=300,dive"`
	// -1 asks for every place in the area; the range is checked in the service, which
	// owns the meaning of that value.
	MaxPerQuery *int  `json:"max_per_query" validate:"omitempty,gte=-1,lte=1000"`
	CrawlEmails *bool `json:"crawl_emails"`
	Concurrency *int  `json:"concurrency" validate:"omitempty,gte=1,lte=32"`
}

// locationRequest allows both halves to be empty: an empty object (or an empty
// locations array) means "every state", which scraper.Config.Normalize expands.
type locationRequest struct {
	City  string `json:"city" validate:"omitempty,max=120"`
	State string `json:"state" validate:"omitempty,max=60"`
}

// toInput converts the wire shape into the service input, applying the documented
// defaults for omitted optional fields.
func (req jobCreateRequest) toInput() scraper.CreateInput {
	cfg := scraper.Config{
		Terms:       req.Config.Terms,
		MaxPerQuery: scraper.DefaultMaxPerQuery,
		Concurrency: scraper.DefaultConcurrency,
		CrawlEmails: true,
	}
	for _, loc := range req.Config.Locations {
		cfg.Locations = append(cfg.Locations, scraper.Location{City: loc.City, State: loc.State})
	}
	if req.Config.MaxPerQuery != nil {
		cfg.MaxPerQuery = *req.Config.MaxPerQuery
	}
	if req.Config.Concurrency != nil {
		cfg.Concurrency = *req.Config.Concurrency
	}
	if req.Config.CrawlEmails != nil {
		cfg.CrawlEmails = *req.Config.CrawlEmails
	}
	return scraper.CreateInput{Name: req.Name, SourceID: req.SourceID, Config: cfg}
}

func decodeJobRequest(r *http.Request) (scraper.CreateInput, error) {
	var req jobCreateRequest
	if err := decodeJSON(r, &req); err != nil {
		return scraper.CreateInput{}, err
	}
	if err := validateStruct(req); err != nil {
		return scraper.CreateInput{}, err
	}
	return req.toInput(), nil
}

// EstimateJob implements POST /jobs/estimate.
func (s *Server) EstimateJob(w http.ResponseWriter, r *http.Request) {
	in, err := decodeJobRequest(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	estimate, err := s.jobs.Estimate(r.Context(), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	costPer1k := estimate.CostPer1kCents
	unlimited := estimate.Unlimited
	writeJSON(w, r, http.StatusOK, gen.JobEstimate{
		Queries:        estimate.Queries,
		EstListings:    estimate.EstListings,
		EstCostCents:   estimate.EstCostCents,
		CostPer1kCents: &costPer1k,
		Unlimited:      &unlimited,
	})
}

// CreateJob implements POST /jobs.
func (s *Server) CreateJob(w http.ResponseWriter, r *http.Request) {
	in, err := decodeJobRequest(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	row, err := s.jobs.Create(r.Context(), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.writeJob(w, r, http.StatusCreated, row)
}

// ListJobs implements GET /jobs.
func (s *Server) ListJobs(w http.ResponseWriter, r *http.Request, params gen.ListJobsParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	filter := db.JobFilter{
		SourceID: params.SourceId,
		From:     params.From,
		To:       params.To,
		Q:        params.Q,
	}
	if params.Status != nil {
		for _, status := range *params.Status {
			filter.Statuses = append(filter.Statuses, string(status))
		}
	}

	sort, err := resolveSortParam(params.Sort, db.JobSortKeys())
	if err != nil {
		WriteError(w, r, err)
		return
	}

	result, err := s.jobs.List(r.Context(), filter, sort, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.Job, 0, len(result.Jobs))
	for _, row := range result.Jobs {
		job, err := toAPIJob(row)
		if err != nil {
			WriteError(w, r, apperr.Internal(err))
			return
		}
		data = append(data, job)
	}
	writeJSON(w, r, http.StatusOK, gen.JobList{Data: data, Meta: pageMeta(page, perPage, result.Total)})
}

// GetJob implements GET /jobs/{id}.
func (s *Server) GetJob(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	row, err := s.jobs.Get(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.writeJob(w, r, http.StatusOK, row)
}

// CancelJob implements POST /jobs/{id}/cancel.
func (s *Server) CancelJob(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	row, err := s.jobs.Cancel(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.writeJob(w, r, http.StatusOK, row)
}

// RerunJob implements POST /jobs/{id}/rerun.
func (s *Server) RerunJob(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	row, err := s.jobs.Rerun(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.writeJob(w, r, http.StatusCreated, row)
}

// recrawlRequest is the optional body of POST /jobs/{id}/recrawl.
type recrawlRequest struct {
	Targets []string `json:"targets" validate:"omitempty,max=2,dive,oneof=emails socials"`
}

// RecrawlJob implements POST /jobs/{id}/recrawl. The body is optional; without one
// the re-crawl looks for emails, as it always has.
func (s *Server) RecrawlJob(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var req recrawlRequest
	if err := decodeJSONIfPresent(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := validateStruct(req); err != nil {
		WriteError(w, r, err)
		return
	}
	row, err := s.jobs.Recrawl(r.Context(), id, req.Targets)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s.writeJob(w, r, http.StatusCreated, row)
}

// DeleteJob implements DELETE /jobs/{id}.
func (s *Server) DeleteJob(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	if err := s.jobs.Delete(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	writeNoContent(w)
}

func (s *Server) writeJob(w http.ResponseWriter, r *http.Request, status int, row db.JobRow) {
	job, err := toAPIJob(row)
	if err != nil {
		WriteError(w, r, apperr.Internal(err))
		return
	}
	writeJSON(w, r, status, job)
}
