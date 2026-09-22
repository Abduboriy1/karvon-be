package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/business"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/scraper"
)

const validJobBody = `{
  "name": "Gyms TX",
  "source_id": "0192f000-0000-7000-8000-000000000001",
  "config": {
    "terms": ["gyms", "crossfit"],
    "locations": [{"city": "Austin", "state": "TX"}],
    "max_per_query": 50,
    "crawl_emails": true,
    "concurrency": 4
  }
}`

func TestCreateJobReturns201AndTheJob(t *testing.T) {
	handler, deps := newTestServer(t)

	rec := do(t, handler, http.MethodPost, "/api/v1/jobs", validJobBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	payload := decodeJSONBody[struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Config struct {
			Terms       []string `json:"terms"`
			MaxPerQuery int      `json:"max_per_query"`
			CrawlEmails bool     `json:"crawl_emails"`
		} `json:"config"`
		Stats struct {
			QueriesTotal int `json:"queries_total"`
		} `json:"stats"`
		SourceName string `json:"source_name"`
	}](t, rec)

	if payload.ID != testJobID.String() || payload.Name != "Gyms TX" {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.SourceName != "Apify" {
		t.Errorf("source_name = %q, the frontend history table needs it", payload.SourceName)
	}
	if len(payload.Config.Terms) != 1 || payload.Config.MaxPerQuery != 50 {
		t.Errorf("config was not decoded from JSONB: %+v", payload.Config)
	}

	// The handler must pass the parsed input straight through to the service.
	if deps.jobs.lastCreate.Name != "Gyms TX" {
		t.Errorf("service received name %q", deps.jobs.lastCreate.Name)
	}
	if len(deps.jobs.lastCreate.Config.Terms) != 2 {
		t.Errorf("service received terms %v", deps.jobs.lastCreate.Config.Terms)
	}
	if deps.jobs.lastCreate.Config.MaxPerQuery != 50 || deps.jobs.lastCreate.Config.Concurrency != 4 {
		t.Errorf("service received config %+v", deps.jobs.lastCreate.Config)
	}
}

func TestCreateJobAppliesDefaultsForOmittedOptionalFields(t *testing.T) {
	handler, deps := newTestServer(t)

	body := `{"name":"Gyms","source_id":"0192f000-0000-7000-8000-000000000001","config":{"terms":["gyms"],"locations":[{"city":"Austin"}]}}`
	if rec := do(t, handler, http.MethodPost, "/api/v1/jobs", body); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	cfg := deps.jobs.lastCreate.Config
	if cfg.MaxPerQuery != scraper.DefaultMaxPerQuery {
		t.Errorf("MaxPerQuery = %d, want %d", cfg.MaxPerQuery, scraper.DefaultMaxPerQuery)
	}
	if cfg.Concurrency != scraper.DefaultConcurrency {
		t.Errorf("Concurrency = %d, want %d", cfg.Concurrency, scraper.DefaultConcurrency)
	}
	if !cfg.CrawlEmails {
		t.Error("crawl_emails should default to true")
	}
}

func TestCreateJobValidatesTheRequestBody(t *testing.T) {
	handler, _ := newTestServer(t)

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantField  string
	}{
		{
			name:       "missing name",
			body:       `{"source_id":"0192f000-0000-7000-8000-000000000001","config":{"terms":["a"],"locations":[{"city":"b"}]}}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantField:  "name",
		},
		{
			name:       "empty terms",
			body:       `{"name":"x","source_id":"0192f000-0000-7000-8000-000000000001","config":{"terms":[],"locations":[{"city":"b"}]}}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantField:  "config.terms",
		},
		{
			name:       "city over the length limit",
			body:       `{"name":"x","source_id":"0192f000-0000-7000-8000-000000000001","config":{"terms":["a"],"locations":[{"city":"` + strings.Repeat("a", 121) + `"}]}}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantField:  "config.locations[0].city",
		},
		{
			name:       "max_per_query out of range",
			body:       `{"name":"x","source_id":"0192f000-0000-7000-8000-000000000001","config":{"terms":["a"],"locations":[{"city":"b"}],"max_per_query":100000}}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantField:  "config.max_per_query",
		},
		{
			name:       "unknown field",
			body:       `{"name":"x","surprise":1,"source_id":"0192f000-0000-7000-8000-000000000001","config":{"terms":["a"],"locations":[{"city":"b"}]}}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantField:  "surprise",
		},
		{
			name:       "wrong type",
			body:       `{"name":123,"source_id":"0192f000-0000-7000-8000-000000000001","config":{"terms":["a"],"locations":[{"city":"b"}]}}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantField:  "name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, handler, http.MethodPost, "/api/v1/jobs", tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			envelope := decodeError(t, rec)
			if envelope.Code != "validation_failed" {
				t.Fatalf("code = %q", envelope.Code)
			}
			if _, ok := envelope.Fields[tt.wantField]; !ok {
				t.Fatalf("details = %v, want an entry for %q", envelope.Fields, tt.wantField)
			}
		})
	}
}

func TestCreateJobAcceptsLocationsWithoutACity(t *testing.T) {
	for name, locations := range map[string]string{
		"state only":       `[{"state":"TX"}]`,
		"empty object":     `[{}]`,
		"empty array":      `[]`,
		"omitted entirely": ``,
	} {
		t.Run(name, func(t *testing.T) {
			handler, deps := newTestServer(t)

			config := `{"terms":["gyms"]`
			if locations != "" {
				config += `,"locations":` + locations
			}
			config += `}`
			body := `{"name":"x","source_id":"0192f000-0000-7000-8000-000000000001","config":` + config + `}`

			rec := do(t, handler, http.MethodPost, "/api/v1/jobs", body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
			}
			if got := deps.jobs.lastCreate.Config.Locations; len(got) > 1 {
				t.Fatalf("service received locations %+v, want the request passed through untouched", got)
			}
		})
	}
}

func TestCreateJobRejectsAMalformedBody(t *testing.T) {
	handler, _ := newTestServer(t)

	for _, body := range []string{`not json`, `{"name":`, `{}{}`} {
		rec := do(t, handler, http.MethodPost, "/api/v1/jobs", body)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("body %q gave status %d, want 400 or 422", body, rec.Code)
		}
	}
}

func TestCreateJobSurfacesServiceErrorsWithTheirStatus(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.jobs.err = apperr.Validation("job configuration is invalid",
		apperr.FieldError{Field: "source_id", Message: "source has no API key configured"})

	rec := do(t, handler, http.MethodPost, "/api/v1/jobs", validJobBody)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if got := decodeError(t, rec).Fields["source_id"]; got == "" {
		t.Fatal("the service's field error should reach the client")
	}
}

func TestEstimateJobReturnsTheCost(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.jobs.estimate = scraper.Estimate{Queries: 4, EstListings: 200, EstCostCents: 90, CostPer1kCents: 450}

	rec := do(t, handler, http.MethodPost, "/api/v1/jobs/estimate", validJobBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	payload := decodeJSONBody[struct {
		Queries      int   `json:"queries"`
		EstListings  int   `json:"est_listings"`
		EstCostCents int64 `json:"est_cost_cents"`
	}](t, rec)
	if payload.Queries != 4 || payload.EstListings != 200 || payload.EstCostCents != 90 {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestListJobsPaginationAndFilters(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.jobs.list = scraper.ListResult{Jobs: []db.JobRow{sampleJobRow()}, Total: 137}

	rec := do(t, handler, http.MethodGet,
		"/api/v1/jobs?status=running&status=queued&q=gym&sort=name:asc&page=3&per_page=25"+
			"&source_id=0192f000-0000-7000-8000-000000000001"+
			"&from=2026-09-01T00:00:00Z&to=2026-09-30T00:00:00Z", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	payload := decodeJSONBody[struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Meta struct {
			Page    int   `json:"page"`
			PerPage int   `json:"per_page"`
			Total   int64 `json:"total"`
		} `json:"meta"`
	}](t, rec)

	if payload.Meta.Page != 3 || payload.Meta.PerPage != 25 || payload.Meta.Total != 137 {
		t.Fatalf("meta = %+v", payload.Meta)
	}
	if len(payload.Data) != 1 {
		t.Fatalf("data has %d rows", len(payload.Data))
	}

	if len(deps.jobs.lastFilter.Statuses) != 2 {
		t.Errorf("statuses = %v, want both values", deps.jobs.lastFilter.Statuses)
	}
	if deps.jobs.lastFilter.SourceID == nil || *deps.jobs.lastFilter.SourceID != testSourceID {
		t.Errorf("source_id filter = %v", deps.jobs.lastFilter.SourceID)
	}
	if deps.jobs.lastFilter.From == nil || !deps.jobs.lastFilter.From.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("from filter = %v", deps.jobs.lastFilter.From)
	}
	if deps.jobs.lastSort != "name:asc" {
		t.Errorf("sort = %q", deps.jobs.lastSort)
	}
	if deps.jobs.lastPage != 3 || deps.jobs.lastPerPage != 25 {
		t.Errorf("page/per_page = %d/%d", deps.jobs.lastPage, deps.jobs.lastPerPage)
	}
}

func TestListJobsAppliesPaginationDefaultsAndCaps(t *testing.T) {
	handler, deps := newTestServer(t)

	do(t, handler, http.MethodGet, "/api/v1/jobs", "")
	if deps.jobs.lastPage != 1 || deps.jobs.lastPerPage != 50 {
		t.Fatalf("defaults = %d/%d, want 1/50", deps.jobs.lastPage, deps.jobs.lastPerPage)
	}

	do(t, handler, http.MethodGet, "/api/v1/jobs?per_page=100000", "")
	if deps.jobs.lastPerPage != 200 {
		t.Fatalf("per_page = %d, want it capped at 200", deps.jobs.lastPerPage)
	}
}

func TestListJobsRejectsAnUnknownSortValue(t *testing.T) {
	handler, _ := newTestServer(t)

	for _, sort := range []string{"bogus:asc", "name", "name%3Basc"} {
		rec := do(t, handler, http.MethodGet, "/api/v1/jobs?sort="+sort, "")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("sort=%s gave status %d, want 422", sort, rec.Code)
		}
		if _, ok := decodeError(t, rec).Fields["sort"]; !ok {
			t.Fatalf("sort=%s: the error should name the sort parameter", sort)
		}
	}
}

func TestGetJobNotFound(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.jobs.err = apperr.NotFound("job")

	rec := do(t, handler, http.MethodGet, "/api/v1/jobs/"+testJobID.String(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := decodeError(t, rec).Code; got != "not_found" {
		t.Fatalf("code = %q", got)
	}
}

func TestGetJobRejectsAMalformedID(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodGet, "/api/v1/jobs/not-a-uuid", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	envelope := decodeError(t, rec)
	if envelope.Code != "validation_failed" {
		t.Fatalf("code = %q", envelope.Code)
	}
	if _, ok := envelope.Fields["id"]; !ok {
		t.Fatalf("details = %v, want the offending parameter named", envelope.Fields)
	}
}

func TestCancelJobConflictWhenAlreadyFinished(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.jobs.err = apperr.Conflict("job is already done")

	rec := do(t, handler, http.MethodPost, "/api/v1/jobs/"+testJobID.String()+"/cancel", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if got := decodeError(t, rec).Code; got != "conflict" {
		t.Fatalf("code = %q", got)
	}
}

func TestRerunJobReturns201(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodPost, "/api/v1/jobs/"+testJobID.String()+"/rerun", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
}

func TestRecrawlJobReturns201(t *testing.T) {
	handler, deps := newTestServer(t)

	rec := do(t, handler, http.MethodPost, "/api/v1/jobs/"+testJobID.String()+"/recrawl", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if len(deps.jobs.recrawled) != 1 || deps.jobs.recrawled[0] != testJobID {
		t.Fatalf("recrawled = %v", deps.jobs.recrawled)
	}
}

func TestRecrawlJobMapsConflictTo409(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.jobs.err = apperr.Conflict("job is still running")

	rec := do(t, handler, http.MethodPost, "/api/v1/jobs/"+testJobID.String()+"/recrawl", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteJobReturns204WithNoBody(t *testing.T) {
	handler, deps := newTestServer(t)

	rec := do(t, handler, http.MethodDelete, "/api/v1/jobs/"+testJobID.String(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("204 must have an empty body, got %q", rec.Body.String())
	}
	if len(deps.jobs.deleted) != 1 || deps.jobs.deleted[0] != testJobID {
		t.Fatalf("deleted = %v", deps.jobs.deleted)
	}
}

func TestExportJobCsvStreamsAnAttachment(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.businesses.exportRows = []business.CSVRow{
		{ID: testBizID.String(), Name: "Iron Works Gym", PrimaryEmail: "info@ironworksgym.com"},
	}

	rec := do(t, handler, http.MethodGet, "/api/v1/jobs/"+testJobID.String()+"/export.csv", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/csv") {
		t.Errorf("Content-Type = %q", got)
	}
	disposition := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment;") || !strings.Contains(disposition, testJobID.String()) {
		t.Errorf("Content-Disposition = %q", disposition)
	}
	if !strings.Contains(rec.Body.String(), "info@ironworksgym.com") {
		t.Errorf("body = %q", rec.Body.String())
	}

	// The export must be scoped to the job, never to the whole master list.
	if deps.businesses.lastFilter.JobID == nil || *deps.businesses.lastFilter.JobID != testJobID {
		t.Fatalf("export filter = %+v, want it scoped to the job", deps.businesses.lastFilter)
	}
}
