package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/exclusion"
)

func TestListExclusionsForwardsFiltersAndCounts(t *testing.T) {
	handler, deps := newTestServer(t)
	rec := do(t, handler, http.MethodGet, "/api/v1/exclusions?q=exam&kind=domain&kind=email&status=all", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	f := deps.exclusions.lastFilter
	if f.Q == nil || *f.Q != "exam" || len(f.Kinds) != 2 || f.Status != "all" {
		t.Fatalf("filter = %+v", f)
	}
	payload := decodeJSONBody[struct {
		Data []struct {
			Value        string `json:"value"`
			DisplayValue string `json:"display_value"`
			Affected     struct {
				Businesses int64 `json:"businesses"`
				Emails     int64 `json:"emails"`
			} `json:"affected"`
		} `json:"data"`
	}](t, rec)
	if len(payload.Data) != 1 || payload.Data[0].Value != "example.com" || payload.Data[0].Affected.Emails != 5 {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestListExclusionsRejectsAnUnknownKind(t *testing.T) {
	handler, _ := newTestServer(t)
	rec := do(t, handler, http.MethodGet, "/api/v1/exclusions?kind=planet", "")
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestCreateExclusionForwardsTheRule(t *testing.T) {
	handler, deps := newTestServer(t)
	rec := do(t, handler, http.MethodPost, "/api/v1/exclusions",
		`{"kind":"company","value":"Planet Fitness","match_mode":"prefix","reason":"chain","source":"business","source_ref_id":"`+testBizID.String()+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	in := deps.exclusions.lastInput
	if in.Kind != exclusion.KindCompany || in.Value != "Planet Fitness" || in.MatchMode != exclusion.MatchPrefix ||
		in.Source != exclusion.SourceBusiness || in.SourceRefID == nil || *in.SourceRefID != testBizID ||
		in.Reason == nil || *in.Reason != "chain" {
		t.Fatalf("input = %+v", in)
	}
}

func TestCreateExclusionDuplicateConflicts(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.exclusions.err = apperr.Conflict("domain \"example.com\" is already excluded")
	rec := do(t, handler, http.MethodPost, "/api/v1/exclusions", `{"kind":"domain","value":"example.com"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestCheckExclusionReportsTheRule(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.exclusions.match = &db.ExclusionRef{ID: testSourceID, Kind: exclusion.KindDomain, Value: "example.com", DisplayValue: "example.com"}
	rec := do(t, handler, http.MethodPost, "/api/v1/exclusions/check", `{"email":"a@example.com","company":"Acme"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if deps.exclusions.lastSubject.Email != "a@example.com" || deps.exclusions.lastSubject.Company != "Acme" {
		t.Fatalf("subject = %+v", deps.exclusions.lastSubject)
	}
	payload := decodeJSONBody[struct {
		Excluded  bool `json:"excluded"`
		Exclusion *struct {
			Kind string `json:"kind"`
		} `json:"exclusion"`
	}](t, rec)
	if !payload.Excluded || payload.Exclusion == nil || payload.Exclusion.Kind != "domain" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestRemoveExclusionPassesTheNote(t *testing.T) {
	handler, deps := newTestServer(t)
	rec := do(t, handler, http.MethodDelete, "/api/v1/exclusions/"+testSourceID.String()+"?note=added+by+mistake", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if deps.exclusions.lastNote != "added by mistake" {
		t.Fatalf("note = %q", deps.exclusions.lastNote)
	}
}

func TestPreviewExclusionAlwaysReturnsSampleArrays(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.exclusions.preview = exclusion.Preview{Kind: exclusion.KindEmail, Value: "a@b.com", DisplayValue: "A@B.com", MatchMode: exclusion.MatchExact}
	rec := do(t, handler, http.MethodPost, "/api/v1/exclusions/preview", `{"kind":"email","value":"A@B.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"sample_emails":[]`, `"sample_businesses":[]`, `"existing_id":null`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %s is missing %s", body, want)
		}
	}
}

func TestScanBrandsForwardsFiltersAndRendersSuggestions(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.exclusions.scan = exclusion.BrandScanPage{Total: 1, Rows: []exclusion.BrandCandidate{{
		BrandCandidate: db.BrandCandidate{Key: "hotworx.net", DisplayName: "HOTWORX - Austin", Locations: 12},
		GroupBy:        db.BrandGroupDomain,
		Suggested:      exclusion.Input{Kind: exclusion.KindDomain, Value: "hotworx.net", MatchMode: exclusion.MatchExact, Source: exclusion.SourceManual},
	}}}
	rec := do(t, handler, http.MethodGet,
		"/api/v1/exclusions/brand-scan?group_by=name_prefix&min_locations=5&min_states=2&state=TX&state=OK&include_dismissed=true&sort=cities:desc", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	in := deps.exclusions.lastScan
	if in.GroupBy != "name_prefix" || in.MinLocations != 5 || in.MinStates != 2 || len(in.States) != 2 ||
		!in.IncludeDismissed || in.IncludeExcluded || in.Sort != "cities:desc" {
		t.Fatalf("input = %+v", in)
	}
	payload := decodeJSONBody[struct {
		Data []struct {
			Key           string   `json:"key"`
			StateList     []string `json:"state_list"`
			SuggestedRule struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			} `json:"suggested_rule"`
		} `json:"data"`
	}](t, rec)
	if len(payload.Data) != 1 || payload.Data[0].SuggestedRule.Value != "hotworx.net" || payload.Data[0].StateList == nil {
		t.Fatalf("payload = %+v", payload)
	}

	rec = do(t, handler, http.MethodGet, "/api/v1/exclusions/brand-scan?sort=value:asc", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("an unknown sort returned %d", rec.Code)
	}
}

func TestBulkCreateExclusionsReportsEachItem(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.exclusions.bulk = []exclusion.BulkResult{
		{Rule: &deps.exclusions.rule},
		{Err: apperr.Conflict("domain %q is already excluded", "example.com")},
		{Err: apperr.Validation("exclusion is invalid")},
	}
	rec := do(t, handler, http.MethodPost, "/api/v1/exclusions/bulk",
		`{"items":[{"kind":"domain","value":"a.com"},{"kind":"domain","value":"b.com"},{"kind":"domain","value":"c"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(deps.exclusions.lastBulk) != 3 || deps.exclusions.lastBulk[1].Value != "b.com" {
		t.Fatalf("input = %+v", deps.exclusions.lastBulk)
	}
	body := rec.Body.String()
	for _, want := range []string{`"created":1`, `"status":"created"`, `"status":"duplicate"`, `"status":"invalid"`, `"code":"conflict"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s: %s", want, body)
		}
	}
}
