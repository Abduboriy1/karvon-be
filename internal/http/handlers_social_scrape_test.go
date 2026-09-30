package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/bory/karvon-be/internal/apperr"
)

func TestSocialScrapeBusinessesDefaultsToMissingEmailOnly(t *testing.T) {
	handler, deps := newTestServer(t)

	rec := do(t, handler, http.MethodPost, "/api/v1/businesses/social-scrape",
		`{"networks":["facebook"],"ids":["`+testBizID.String()+`"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	got := deps.jobs.socialScrape
	if got == nil {
		t.Fatal("the service was not called")
	}
	if len(got.Networks) != 1 || got.Networks[0] != "facebook" {
		t.Errorf("networks = %v", got.Networks)
	}
	if !got.MissingEmailOnly {
		t.Error("missing_email_only must default to true")
	}
	if len(got.Filter.IDs) != 1 || got.Filter.IDs[0] != testBizID {
		t.Errorf("filter ids = %v", got.Filter.IDs)
	}
}

func TestSocialScrapeBusinessesPassesFiltersAndAnExplicitFalse(t *testing.T) {
	handler, deps := newTestServer(t)

	rec := do(t, handler, http.MethodPost, "/api/v1/businesses/social-scrape",
		`{"networks":["facebook"],"missing_email_only":false,"city":"Austin","has_email":false}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	got := deps.jobs.socialScrape
	if got.MissingEmailOnly {
		t.Error("an explicit false was ignored")
	}
	if got.Filter.City == nil || *got.Filter.City != "Austin" || got.Filter.HasEmail == nil || *got.Filter.HasEmail {
		t.Errorf("filter = %+v", got.Filter)
	}
}

func TestSocialScrapeBusinessesValidatesNetworks(t *testing.T) {
	for name, body := range map[string]string{
		"none":    `{"city":"Austin"}`,
		"empty":   `{"networks":[]}`,
		"unknown": `{"networks":["myspace"]}`,
		"extra":   `{"networks":["facebook"],"targets":["emails"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			handler, deps := newTestServer(t)
			rec := do(t, handler, http.MethodPost, "/api/v1/businesses/social-scrape", body)
			if rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 422 or 400: %s", rec.Code, rec.Body.String())
			}
			if deps.jobs.socialScrape != nil {
				t.Error("an invalid request reached the service")
			}
		})
	}
}

func TestSocialScrapeBusinessesMapsConflictTo409(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.jobs.err = apperr.Conflict("the facebook scraper is not enabled")

	rec := do(t, handler, http.MethodPost, "/api/v1/businesses/social-scrape", `{"networks":["facebook"]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}
