package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/bory/karvon-be/internal/apperr"
)

func TestListScrapeCategories(t *testing.T) {
	handler, _ := newTestServer(t)
	rec := do(t, handler, http.MethodGet, "/api/v1/scrape-categories", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	payload := decodeJSONBody[[]struct {
		Name      string   `json:"name"`
		Terms     []string `json:"terms"`
		IsDefault bool     `json:"is_default"`
	}](t, rec)
	if len(payload) != 1 || payload[0].Name != "Gyms" || !payload[0].IsDefault || len(payload[0].Terms) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestCreateScrapeCategory(t *testing.T) {
	handler, deps := newTestServer(t)
	rec := do(t, handler, http.MethodPost, "/api/v1/scrape-categories", `{"name":"Cafes","terms":["cafe","coffee shop"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if deps.categories.lastInput.Name == nil || *deps.categories.lastInput.Name != "Cafes" ||
		len(deps.categories.lastInput.Terms) != 2 {
		t.Fatalf("input = %+v", deps.categories.lastInput)
	}
}

func TestUpdateScrapeCategoryOmittedTermsStayNil(t *testing.T) {
	handler, deps := newTestServer(t)
	path := "/api/v1/scrape-categories/" + testSourceID.String()
	rec := do(t, handler, http.MethodPut, path, `{"name":"Fitness"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if deps.categories.lastInput.Terms != nil {
		t.Fatalf("omitted terms should be nil, got %v", deps.categories.lastInput.Terms)
	}
}

func TestDeleteDefaultScrapeCategoryConflicts(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.categories.err = apperr.Conflict("default categories cannot be deleted")
	rec := do(t, handler, http.MethodDelete, "/api/v1/scrape-categories/"+testSourceID.String(), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
}
