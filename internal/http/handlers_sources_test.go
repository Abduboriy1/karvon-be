package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/source"
	"github.com/bory/karvon-be/internal/stats"
)

func TestListSourcesNeverReturnsTheKey(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.sources.list = []dbgen.Source{sampleSource(), {
		ID:        testJobID,
		Kind:      "outscraper",
		Name:      "Outscraper",
		ApiKeyEnc: nil,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}}

	rec := do(t, handler, http.MethodGet, "/api/v1/sources", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	body := rec.Body.String()
	for _, forbidden := range []string{"api_key", "api_key_enc", "encrypted"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("response exposes %q: %s", forbidden, body)
		}
	}

	payload := decodeJSONBody[[]struct {
		Kind   string `json:"kind"`
		HasKey bool   `json:"has_key"`
	}](t, rec)
	if len(payload) != 2 {
		t.Fatalf("got %d sources", len(payload))
	}
	if !payload[0].HasKey {
		t.Error("the configured source should report has_key=true")
	}
	if payload[1].HasKey {
		t.Error("the unconfigured source should report has_key=false")
	}
}

func TestUpdateSourceDistinguishesOmittedFromNullKey(t *testing.T) {
	handler, deps := newTestServer(t)
	path := "/api/v1/sources/" + testSourceID.String()

	t.Run("key omitted keeps the stored key", func(t *testing.T) {
		rec := do(t, handler, http.MethodPut, path, `{"enabled":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if deps.sources.lastUpdate.KeyPresent {
			t.Error("an omitted api_key must leave the stored key untouched")
		}
	})

	t.Run("key null clears it", func(t *testing.T) {
		rec := do(t, handler, http.MethodPut, path, `{"api_key":null}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if !deps.sources.lastUpdate.KeyPresent || deps.sources.lastUpdate.APIKey != nil {
			t.Errorf("update = %+v, want an explicit clear", deps.sources.lastUpdate)
		}
	})

	t.Run("key set", func(t *testing.T) {
		rec := do(t, handler, http.MethodPut, path, `{"api_key":"apify_abc123","cost_per_1k_cents":450}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if deps.sources.lastUpdate.APIKey == nil || *deps.sources.lastUpdate.APIKey != "apify_abc123" {
			t.Errorf("update = %+v", deps.sources.lastUpdate)
		}
		if deps.sources.lastUpdate.CostPer1kCents == nil || *deps.sources.lastUpdate.CostPer1kCents != 450 {
			t.Errorf("cost = %v", deps.sources.lastUpdate.CostPer1kCents)
		}
	})
}

func TestUpdateSourceValidatesCost(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodPut, "/api/v1/sources/"+testSourceID.String(),
		`{"cost_per_1k_cents":-5}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if _, ok := decodeError(t, rec).Fields["cost_per_1k_cents"]; !ok {
		t.Fatal("the error should name the offending field")
	}
}

func TestTestSourceSuccess(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.sources.result = source.TestResult{OK: true, Kind: "apify", ListingsReturned: 1, TestedAt: time.Now().UTC()}

	rec := do(t, handler, http.MethodPost, "/api/v1/sources/"+testSourceID.String()+"/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	payload := decodeJSONBody[struct {
		Ok               bool   `json:"ok"`
		Kind             string `json:"kind"`
		ListingsReturned int    `json:"listings_returned"`
	}](t, rec)
	if !payload.Ok || payload.Kind != "apify" || payload.ListingsReturned != 1 {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestTestSourceReportsABadKeyAsProviderAuth(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.sources.err = apperr.ProviderAuth("the provider rejected the stored API key")

	rec := do(t, handler, http.MethodPost, "/api/v1/sources/"+testSourceID.String()+"/test", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if got := decodeError(t, rec).Code; got != "provider_auth" {
		t.Fatalf("code = %q, want provider_auth", got)
	}
}

func TestGetStatsReturnsTheDashboardPayload(t *testing.T) {
	handler, deps := newTestServer(t)
	job := sampleJobRow()
	deps.stats.value.Businesses = 1200
	deps.stats.value.WithEmail = 810
	deps.stats.value.EmailsTotal = 940
	deps.stats.value.JobsTotal = 17
	deps.stats.value.LastJob = &job
	deps.stats.value.EmailsPerJob = []stats.JobEmails{{JobID: testJobID, Name: "Gyms TX", Emails: 97}}

	rec := do(t, handler, http.MethodGet, "/api/v1/stats/scraper", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	payload := decodeJSONBody[struct {
		Businesses   int64 `json:"businesses"`
		WithEmail    int64 `json:"with_email"`
		JobsTotal    int64 `json:"jobs_total"`
		EmailsPerJob []struct {
			JobID  string `json:"job_id"`
			Name   string `json:"name"`
			Emails int    `json:"emails"`
		} `json:"emails_per_job"`
		LastJob *struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"last_job"`
	}](t, rec)

	if payload.Businesses != 1200 || payload.WithEmail != 810 || payload.JobsTotal != 17 {
		t.Fatalf("counters = %+v", payload)
	}
	if len(payload.EmailsPerJob) != 1 || payload.EmailsPerJob[0].Emails != 97 {
		t.Fatalf("chart = %+v", payload.EmailsPerJob)
	}
	if payload.LastJob == nil || payload.LastJob.ID != testJobID.String() {
		t.Fatalf("last_job = %+v", payload.LastJob)
	}
}

func TestGetStatsWithNoJobsYet(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodGet, "/api/v1/stats/scraper", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// An empty dashboard must still send arrays, not null, or the chart breaks.
	if !strings.Contains(rec.Body.String(), `"emails_per_job":[]`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"last_job":null`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestOpenAPISpecIsServedAsJSON(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodGet, "/api/v1/openapi.json", "", withoutKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q", got)
	}

	spec := decodeJSONBody[struct {
		OpenAPI string         `json:"openapi"`
		Paths   map[string]any `json:"paths"`
	}](t, rec)
	if spec.OpenAPI == "" {
		t.Fatal("the document has no openapi version")
	}
	for _, path := range []string{"/jobs", "/businesses", "/sources", "/stats/scraper"} {
		if _, ok := spec.Paths[path]; !ok {
			t.Errorf("the served spec is missing %s", path)
		}
	}
}
