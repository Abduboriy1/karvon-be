package httpapi_test

import (
	"encoding/csv"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/business"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

func sampleBusinessRow() db.BusinessRow {
	category := "Gym"
	city := "Austin"
	state := "TX"
	domain := "ironworksgym.com"
	email := "info@ironworksgym.com"
	emailSource := "mailto"
	reviews := int32(318)
	rating := 4.7

	return db.BusinessRow{
		ID:                 testBizID,
		Name:               "Iron Works Gym",
		Category:           &category,
		City:               &city,
		State:              &state,
		Domain:             &domain,
		Rating:             &rating,
		Reviews:            &reviews,
		PrimaryEmail:       &email,
		PrimaryEmailSource: &emailSource,
		EmailsCount:        2,
		CreatedAt:          time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
		UpdatedAt:          time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}
}

func TestListBusinessesReturnsThePrimaryEmailAndPagination(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.businesses.list = business.ListResult{Rows: []db.BusinessRow{sampleBusinessRow()}, Total: 51234}

	rec := do(t, handler, http.MethodGet, "/api/v1/businesses?per_page=25&page=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	payload := decodeJSONBody[struct {
		Data []struct {
			ID                 string `json:"id"`
			Name               string `json:"name"`
			PrimaryEmail       string `json:"primary_email"`
			PrimaryEmailSource string `json:"primary_email_source"`
			EmailsCount        int    `json:"emails_count"`
			Reviews            int    `json:"reviews"`
		} `json:"data"`
		Meta struct {
			Page    int   `json:"page"`
			PerPage int   `json:"per_page"`
			Total   int64 `json:"total"`
		} `json:"meta"`
	}](t, rec)

	if len(payload.Data) != 1 {
		t.Fatalf("data = %+v", payload.Data)
	}
	row := payload.Data[0]
	if row.PrimaryEmail != "info@ironworksgym.com" || row.PrimaryEmailSource != "mailto" {
		t.Errorf("email fields = %+v", row)
	}
	if row.EmailsCount != 2 || row.Reviews != 318 {
		t.Errorf("counters = %+v", row)
	}
	if payload.Meta.Total != 51234 || payload.Meta.Page != 2 || payload.Meta.PerPage != 25 {
		t.Errorf("meta = %+v", payload.Meta)
	}
}

func TestListBusinessesForwardsEveryFilter(t *testing.T) {
	handler, deps := newTestServer(t)

	target := "/api/v1/businesses?" + strings.Join([]string{
		"job_id=" + testJobID.String(),
		"category=Gym",
		"state=TX",
		"city=Austin",
		"has_email=true",
		"suppressed=false",
		"q=iron",
		"email_source=mailto",
		"sort=name:asc",
	}, "&")

	if rec := do(t, handler, http.MethodGet, target, ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	filter := deps.businesses.lastFilter
	if filter.JobID == nil || *filter.JobID != testJobID {
		t.Errorf("job_id = %v", filter.JobID)
	}
	if filter.Category == nil || *filter.Category != "Gym" {
		t.Errorf("category = %v", filter.Category)
	}
	if filter.State == nil || *filter.State != "TX" {
		t.Errorf("state = %v", filter.State)
	}
	if filter.City == nil || *filter.City != "Austin" {
		t.Errorf("city = %v", filter.City)
	}
	if filter.HasEmail == nil || !*filter.HasEmail {
		t.Errorf("has_email = %v", filter.HasEmail)
	}
	if filter.Suppressed == nil || *filter.Suppressed {
		t.Errorf("suppressed = %v", filter.Suppressed)
	}
	if filter.Q == nil || *filter.Q != "iron" {
		t.Errorf("q = %v", filter.Q)
	}
	if filter.EmailSource == nil || *filter.EmailSource != "mailto" {
		t.Errorf("email_source = %v", filter.EmailSource)
	}
	if deps.businesses.lastSort != "name:asc" {
		t.Errorf("sort = %q", deps.businesses.lastSort)
	}
}

func TestGetBusinessReturnsEveryEmail(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.businesses.detail = business.Detail{
		Business: dbgen.GetBusinessRow{
			ID:                 testBizID,
			Name:               "Iron Works Gym",
			PrimaryEmail:       "info@ironworksgym.com",
			PrimaryEmailSource: "mailto",
			EmailsCount:        2,
			Raw:                []byte(`{"placeId":"abc"}`),
			CreatedAt:          time.Now().UTC(),
			UpdatedAt:          time.Now().UTC(),
		},
		Emails: []dbgen.ListBusinessEmailsWithVerificationRow{
			{
				ID: testBizID, Email: "info@ironworksgym.com", Source: "mailto",
				IsPrimary: true, FoundAt: time.Now().UTC(),
				VerificationTag: ptr("light_green"), VerificationScore: ptrInt32(85),
			},
			{ID: testJobID, Email: "amy@ironworksgym.com", Source: "regex", FoundAt: time.Now().UTC()},
		},
	}

	rec := do(t, handler, http.MethodGet, "/api/v1/businesses/"+testBizID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	payload := decodeJSONBody[struct {
		PrimaryEmail string `json:"primary_email"`
		Emails       []struct {
			Email     string `json:"email"`
			Source    string `json:"source"`
			IsPrimary bool   `json:"is_primary"`
		} `json:"emails"`
		Raw map[string]any `json:"raw"`
	}](t, rec)

	if len(payload.Emails) != 2 {
		t.Fatalf("emails = %+v", payload.Emails)
	}
	if !payload.Emails[0].IsPrimary {
		t.Error("the primary address should come first")
	}
	if payload.Raw["placeId"] != "abc" {
		t.Errorf("raw = %v, want the decoded provider payload", payload.Raw)
	}
}

func TestUpdateBusinessDistinguishesOmittedFromNullNotes(t *testing.T) {
	handler, deps := newTestServer(t)

	t.Run("notes omitted", func(t *testing.T) {
		rec := do(t, handler, http.MethodPatch, "/api/v1/businesses/"+testBizID.String(), `{"suppressed":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if deps.businesses.lastUpdate.SetNotes {
			t.Error("an omitted notes field must not clear the stored note")
		}
		if deps.businesses.lastUpdate.Suppressed == nil || !*deps.businesses.lastUpdate.Suppressed {
			t.Error("suppressed was not forwarded")
		}
	})

	t.Run("notes null", func(t *testing.T) {
		rec := do(t, handler, http.MethodPatch, "/api/v1/businesses/"+testBizID.String(), `{"notes":null}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		if !deps.businesses.lastUpdate.SetNotes || deps.businesses.lastUpdate.Notes != nil {
			t.Errorf("update = %+v, want an explicit clear", deps.businesses.lastUpdate)
		}
	})

	t.Run("notes set", func(t *testing.T) {
		rec := do(t, handler, http.MethodPatch, "/api/v1/businesses/"+testBizID.String(), `{"notes":"called them"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if !deps.businesses.lastUpdate.SetNotes ||
			deps.businesses.lastUpdate.Notes == nil ||
			*deps.businesses.lastUpdate.Notes != "called them" {
			t.Errorf("update = %+v", deps.businesses.lastUpdate)
		}
	})
}

func TestBulkUpdateValidation(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.businesses.bulk = 3

	t.Run("valid", func(t *testing.T) {
		body := `{"ids":["` + testBizID.String() + `"],"action":"suppress"}`
		rec := do(t, handler, http.MethodPost, "/api/v1/businesses/bulk", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		payload := decodeJSONBody[struct {
			Updated int `json:"updated"`
		}](t, rec)
		if payload.Updated != 3 {
			t.Fatalf("updated = %d, want 3", payload.Updated)
		}
	})

	t.Run("unknown action", func(t *testing.T) {
		body := `{"ids":["` + testBizID.String() + `"],"action":"delete"}`
		rec := do(t, handler, http.MethodPost, "/api/v1/businesses/bulk", body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
		if _, ok := decodeError(t, rec).Fields["action"]; !ok {
			t.Fatal("the error should name the action field")
		}
	})

	t.Run("no ids", func(t *testing.T) {
		rec := do(t, handler, http.MethodPost, "/api/v1/businesses/bulk", `{"ids":[],"action":"suppress"}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
	})
}

func TestExportBusinessesStreamsValidCSV(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.businesses.exportRows = []business.CSVRow{
		{ID: testBizID.String(), Name: "Iron Works Gym", PrimaryEmail: "info@ironworksgym.com", EmailsCount: 1},
		{ID: testJobID.String(), Name: "Austin Barbell, Inc", PrimaryEmail: "hi@austinbarbell.com", EmailsCount: 1},
	}

	rec := do(t, handler, http.MethodPost, "/api/v1/businesses/export",
		`{"state":"TX","has_email":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("export is not valid CSV: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d CSV rows, want header + 2", len(records))
	}
	if deps.businesses.lastFilter.State == nil || *deps.businesses.lastFilter.State != "TX" {
		t.Errorf("the filter did not reach the service: %+v", deps.businesses.lastFilter)
	}
}

func TestExportBusinessesRejectsAnUnknownEmailSource(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodPost, "/api/v1/businesses/export", `{"email_source":"telepathy"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestBusinessNotFound(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.businesses.err = apperr.NotFound("business")

	rec := do(t, handler, http.MethodGet, "/api/v1/businesses/"+testBizID.String(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestInternalErrorsDoNotLeakDetails(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.businesses.err = apperr.Internal(errQueueDown)

	rec := do(t, handler, http.MethodGet, "/api/v1/businesses", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	envelope := decodeError(t, rec)
	if envelope.Code != "internal" {
		t.Fatalf("code = %q", envelope.Code)
	}
	if strings.Contains(envelope.Message, "queue unavailable") {
		t.Fatalf("the underlying cause leaked to the client: %q", envelope.Message)
	}
}

func TestTimestampsAreServedInUTC(t *testing.T) {
	handler, deps := newTestServer(t)

	// A row whose timestamps arrive in a non-UTC zone, as pgx hands them over.
	zone := time.FixedZone("CDT", -5*60*60)
	row := sampleBusinessRow()
	row.CreatedAt = time.Date(2026, 9, 19, 11, 57, 49, 0, zone)
	row.UpdatedAt = row.CreatedAt
	deps.businesses.list = business.ListResult{Rows: []db.BusinessRow{row}, Total: 1}

	rec := do(t, handler, http.MethodGet, "/api/v1/businesses", "")
	body := rec.Body.String()

	if !strings.Contains(body, `"created_at":"2026-09-19T16:57:49Z"`) {
		t.Fatalf("timestamps must be RFC 3339 UTC, got: %s", body)
	}
	if strings.Contains(body, "-05:00") {
		t.Fatalf("a local offset leaked into the response: %s", body)
	}
}

func TestBusinessListCarriesTheFirstSeenJobName(t *testing.T) {
	handler, deps := newTestServer(t)

	name := "Gyms TX"
	row := sampleBusinessRow()
	row.FirstJobID = &testJobID
	row.FirstJobName = &name
	deps.businesses.list = business.ListResult{Rows: []db.BusinessRow{row}, Total: 1}

	rec := do(t, handler, http.MethodGet, "/api/v1/businesses", "")
	payload := decodeJSONBody[struct {
		Data []struct {
			FirstJobID   string `json:"first_job_id"`
			FirstJobName string `json:"first_job_name"`
		} `json:"data"`
	}](t, rec)

	if len(payload.Data) != 1 {
		t.Fatalf("data = %+v", payload.Data)
	}
	// The "first seen job" column needs a name, not only an id.
	if payload.Data[0].FirstJobName != "Gyms TX" || payload.Data[0].FirstJobID != testJobID.String() {
		t.Fatalf("row = %+v", payload.Data[0])
	}
}
