package integration_test

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/scraper/provider"
)

// seededHarness runs one job whose listings cover several cities and categories.
func seededHarness(t *testing.T) (*harness, string) {
	t.Helper()

	pages := map[string]string{
		"ironworksgym.com/":  homepageWithMailto,
		"austinbarbell.com/": `<html><body><p>hi@austinbarbell.com</p></body></html>`,
		"dallasiron.com/":    `<html><body><p>no address</p></body></html>`,
	}
	h := newHarness(t, pages)
	h.provider.ByQuery = map[string][]provider.Listing{
		"gyms in Austin, TX": {
			listingFor("place-iron-works", "Iron Works Gym", "Austin", "ironworksgym.com"),
			listingFor("place-barbell-club", "Austin Barbell Club", "Austin", "austinbarbell.com"),
		},
		"gyms in Dallas, TX": {
			listingFor("place-dallas-iron", "Dallas Iron Athletics", "Dallas", "dallasiron.com"),
		},
	}
	h.configureSource()

	job := h.waitForJob(h.createJob("Seed", []string{"gyms"}, []string{"Austin", "Dallas"}, true).ID)
	if job.Status != "done" {
		t.Fatalf("seed job finished as %q (%s)", job.Status, job.Error)
	}
	return h, job.ID
}

type businessListPayload struct {
	Data []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		City         string `json:"city"`
		Domain       string `json:"domain"`
		PrimaryEmail string `json:"primary_email"`
		Suppressed   bool   `json:"suppressed"`
	} `json:"data"`
	Meta struct {
		Page    int   `json:"page"`
		PerPage int   `json:"per_page"`
		Total   int64 `json:"total"`
	} `json:"meta"`
}

func TestBusinessListFiltering(t *testing.T) {
	h, jobID := seededHarness(t)

	tests := []struct {
		name  string
		query string
		want  int64
	}{
		{name: "all", query: "", want: 3},
		{name: "by city", query: "?city=Austin", want: 2},
		{name: "by state", query: "?state=TX", want: 3},
		{name: "by category", query: "?category=Gym", want: 3},
		{name: "with an email", query: "?has_email=true", want: 2},
		{name: "without an email", query: "?has_email=false", want: 1},
		{name: "by job", query: "?job_id=" + jobID, want: 3},
		{name: "trigram on the name", query: "?q=iron", want: 2},
		{name: "trigram on the domain", query: "?q=barbell", want: 1},
		{name: "no match", query: "?q=zzzzz", want: 0},
		{name: "by email source", query: "?email_source=mailto", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := h.mustRequest(http.MethodGet, "/api/v1/businesses"+tt.query, "", http.StatusOK)
			payload := decodeBody[businessListPayload](t, rec)
			if payload.Meta.Total != tt.want {
				t.Fatalf("total = %d, want %d", payload.Meta.Total, tt.want)
			}
			if int64(len(payload.Data)) != tt.want {
				t.Fatalf("returned %d rows but reported %d", len(payload.Data), payload.Meta.Total)
			}
		})
	}
}

func TestBusinessListSortingAndPagination(t *testing.T) {
	h, _ := seededHarness(t)

	rec := h.mustRequest(http.MethodGet, "/api/v1/businesses?sort=name:asc&per_page=2&page=1", "", http.StatusOK)
	first := decodeBody[businessListPayload](t, rec)
	if len(first.Data) != 2 || first.Meta.Total != 3 {
		t.Fatalf("page 1 = %+v", first.Meta)
	}
	if first.Data[0].Name > first.Data[1].Name {
		t.Fatalf("name:asc is not sorted: %q then %q", first.Data[0].Name, first.Data[1].Name)
	}

	rec = h.mustRequest(http.MethodGet, "/api/v1/businesses?sort=name:asc&per_page=2&page=2", "", http.StatusOK)
	second := decodeBody[businessListPayload](t, rec)
	if len(second.Data) != 1 {
		t.Fatalf("page 2 returned %d rows, want 1", len(second.Data))
	}
	if second.Data[0].Name < first.Data[1].Name {
		t.Fatalf("pages overlap or are misordered: %q came after %q", second.Data[0].Name, first.Data[1].Name)
	}

	// Descending order must reverse the first page.
	rec = h.mustRequest(http.MethodGet, "/api/v1/businesses?sort=name:desc&per_page=1", "", http.StatusOK)
	desc := decodeBody[businessListPayload](t, rec)
	if desc.Data[0].Name != second.Data[0].Name {
		t.Fatalf("name:desc starts with %q, want %q", desc.Data[0].Name, second.Data[0].Name)
	}
}

func TestSuppressionHidesBusinessesFromTheDefaultList(t *testing.T) {
	h, _ := seededHarness(t)

	rec := h.mustRequest(http.MethodGet, "/api/v1/businesses?q=iron%20works", "", http.StatusOK)
	list := decodeBody[businessListPayload](t, rec)
	if len(list.Data) != 1 {
		t.Fatalf("expected exactly one match, got %+v", list.Data)
	}
	target := list.Data[0].ID

	t.Run("patch", func(t *testing.T) {
		body := fmt.Sprintf(`{"suppressed":true,"notes":%q}`, "duplicate listing")
		rec := h.mustRequest(http.MethodPatch, "/api/v1/businesses/"+target, body, http.StatusOK)
		detail := decodeBody[struct {
			Suppressed bool   `json:"suppressed"`
			Notes      string `json:"notes"`
		}](t, rec)
		if !detail.Suppressed || detail.Notes != "duplicate listing" {
			t.Fatalf("detail = %+v", detail)
		}
	})

	t.Run("hidden by default", func(t *testing.T) {
		rec := h.mustRequest(http.MethodGet, "/api/v1/businesses", "", http.StatusOK)
		if got := decodeBody[businessListPayload](t, rec).Meta.Total; got != 2 {
			t.Fatalf("default list total = %d, want 2", got)
		}
	})

	t.Run("visible with the filter", func(t *testing.T) {
		rec := h.mustRequest(http.MethodGet, "/api/v1/businesses?suppressed=true", "", http.StatusOK)
		payload := decodeBody[businessListPayload](t, rec)
		if payload.Meta.Total != 1 || payload.Data[0].ID != target {
			t.Fatalf("suppressed list = %+v", payload)
		}
	})

	t.Run("bulk unsuppress", func(t *testing.T) {
		body := fmt.Sprintf(`{"ids":[%q],"action":"unsuppress"}`, target)
		rec := h.mustRequest(http.MethodPost, "/api/v1/businesses/bulk", body, http.StatusOK)
		result := decodeBody[struct {
			Updated int `json:"updated"`
		}](t, rec)
		if result.Updated != 1 {
			t.Fatalf("updated = %d, want 1", result.Updated)
		}

		rec = h.mustRequest(http.MethodGet, "/api/v1/businesses", "", http.StatusOK)
		if got := decodeBody[businessListPayload](t, rec).Meta.Total; got != 3 {
			t.Fatalf("total after unsuppress = %d, want 3", got)
		}
	})

	t.Run("bulk suppress is idempotent", func(t *testing.T) {
		body := fmt.Sprintf(`{"ids":[%q],"action":"suppress"}`, target)
		h.mustRequest(http.MethodPost, "/api/v1/businesses/bulk", body, http.StatusOK)

		rec := h.mustRequest(http.MethodPost, "/api/v1/businesses/bulk", body, http.StatusOK)
		result := decodeBody[struct {
			Updated int `json:"updated"`
		}](t, rec)
		if result.Updated != 0 {
			t.Fatalf("a repeated suppress reported %d changes, want 0", result.Updated)
		}
	})
}

func TestBusinessDetailListsEveryAddress(t *testing.T) {
	h, _ := seededHarness(t)

	rec := h.mustRequest(http.MethodGet, "/api/v1/businesses?q=iron%20works", "", http.StatusOK)
	target := decodeBody[businessListPayload](t, rec).Data[0].ID

	rec = h.mustRequest(http.MethodGet, "/api/v1/businesses/"+target, "", http.StatusOK)
	detail := decodeBody[struct {
		Name   string `json:"name"`
		Domain string `json:"domain"`
		Emails []struct {
			Email     string `json:"email"`
			Source    string `json:"source"`
			IsPrimary bool   `json:"is_primary"`
			PageURL   string `json:"page_url"`
		} `json:"emails"`
		Raw map[string]any `json:"raw"`
	}](t, rec)

	if len(detail.Emails) != 1 {
		t.Fatalf("emails = %+v", detail.Emails)
	}
	email := detail.Emails[0]
	if email.Email != "info@ironworksgym.com" || email.Source != "mailto" || !email.IsPrimary {
		t.Fatalf("email = %+v", email)
	}
	if !strings.Contains(email.PageURL, "ironworksgym.com") {
		t.Errorf("page_url = %q", email.PageURL)
	}
	if detail.Raw["placeId"] != "place-iron-works" {
		t.Errorf("raw = %v, want the stored provider payload", detail.Raw)
	}
}

func TestFilteredCSVExport(t *testing.T) {
	h, _ := seededHarness(t)

	rec := h.mustRequest(http.MethodPost, "/api/v1/businesses/export", `{"has_email":true}`, http.StatusOK)
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("export is not valid CSV: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d rows, want header + 2 businesses with an address", len(records))
	}

	body := rec.Body.String()
	if strings.Contains(body, "dallasiron.com") {
		t.Error("the has_email filter was ignored")
	}
	for _, want := range []string{"info@ironworksgym.com", "hi@austinbarbell.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("export is missing %q", want)
		}
	}
}

func TestRerunClonesTheConfigIntoANewJob(t *testing.T) {
	h, jobID := seededHarness(t)

	rec := h.mustRequest(http.MethodPost, "/api/v1/jobs/"+jobID+"/rerun", "", http.StatusCreated)
	clone := decodeBody[struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Config struct {
			Terms     []string `json:"terms"`
			Locations []struct {
				City string `json:"city"`
			} `json:"locations"`
		} `json:"config"`
	}](t, rec)

	if clone.ID == jobID {
		t.Fatal("a re-run must create a new job")
	}
	if !strings.Contains(clone.Name, "re-run") {
		t.Errorf("name = %q", clone.Name)
	}
	if len(clone.Config.Terms) != 1 || len(clone.Config.Locations) != 2 {
		t.Fatalf("the config was not cloned: %+v", clone.Config)
	}

	if final := h.waitForJob(clone.ID); final.Status != "done" {
		t.Fatalf("the cloned job finished as %q", final.Status)
	}
}

func TestRecrawlRevisitsWebsitesWithoutCallingTheProvider(t *testing.T) {
	h, jobID := seededHarness(t)

	// The seed crawl found nothing on dallasiron.com. The site now links to a contact
	// page that carries an address, which is exactly what a re-crawl is for.
	h.pages.pages["dallasiron.com/"] = `<html><body><a href="contact.html">Contact Us</a></body></html>`
	h.pages.pages["dallasiron.com/contact.html"] = `<html><body><p>owner@dallasiron.com</p></body></html>`
	providerCalls := len(h.provider.Calls())

	rec := h.mustRequest(http.MethodPost, "/api/v1/jobs/"+jobID+"/recrawl", "", http.StatusCreated)
	created := decodeBody[struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Config struct {
			Terms     []string `json:"terms"`
			RecrawlOf string   `json:"recrawl_of"`
		} `json:"config"`
	}](t, rec)

	if created.ID == jobID {
		t.Fatal("a re-crawl must create a new job")
	}
	if !strings.Contains(created.Name, "re-crawl") {
		t.Errorf("name = %q", created.Name)
	}
	if created.Config.RecrawlOf != jobID {
		t.Errorf("recrawl_of = %q, want %q", created.Config.RecrawlOf, jobID)
	}
	if len(created.Config.Terms) != 1 {
		t.Errorf("the config must be carried over: %+v", created.Config)
	}

	final := h.waitForJob(created.ID)
	if final.Status != "done" {
		t.Fatalf("the re-crawl finished as %q (%s)", final.Status, final.Error)
	}
	if got := len(h.provider.Calls()); got != providerCalls {
		t.Fatalf("provider calls = %d, want %d: a re-crawl must never search", got, providerCalls)
	}
	if final.Stats.QueriesTotal != 0 || final.Stats.CostCents != 0 {
		t.Errorf("stats = %+v, want no queries and no spend", final.Stats)
	}
	if final.Stats.ListingsFound != 3 {
		t.Errorf("listings_found = %d, want the 3 copied businesses", final.Stats.ListingsFound)
	}
	if final.Stats.EmailsFound != 3 {
		t.Errorf("emails_found = %d, want 3: the contact page address must be picked up", final.Stats.EmailsFound)
	}
	// Only dallasiron.com came up empty, so it is the re-crawl's whole workload: the
	// two sites that already had an address must not start the bar part-way done.
	if final.Stats.SitesTotal != 1 || final.Stats.SitesCrawled != 1 {
		t.Errorf("sites = %d/%d, want 1/1: only the site without an address is re-crawled",
			final.Stats.SitesCrawled, final.Stats.SitesTotal)
	}

	// The new job lists the same businesses, and the one that was empty now has an address.
	rec = h.mustRequest(http.MethodGet, "/api/v1/businesses?job_id="+created.ID+"&per_page=50", "", http.StatusOK)
	list := decodeBody[struct {
		Data []struct {
			Domain       string `json:"domain"`
			PrimaryEmail string `json:"primary_email"`
		} `json:"data"`
	}](t, rec)
	if len(list.Data) != 3 {
		t.Fatalf("the re-crawl job lists %d businesses, want 3", len(list.Data))
	}
	for _, b := range list.Data {
		if b.Domain == "dallasiron.com" && b.PrimaryEmail != "owner@dallasiron.com" {
			t.Errorf("dallasiron.com primary_email = %q, want owner@dallasiron.com", b.PrimaryEmail)
		}
	}

	// A re-crawl of a re-crawl still points at the original search.
	rec = h.mustRequest(http.MethodPost, "/api/v1/jobs/"+created.ID+"/recrawl", "", http.StatusCreated)
	second := decodeBody[struct {
		Name   string `json:"name"`
		Config struct {
			RecrawlOf string `json:"recrawl_of"`
		} `json:"config"`
	}](t, rec)
	if second.Config.RecrawlOf != jobID {
		t.Errorf("chained recrawl_of = %q, want the original %q", second.Config.RecrawlOf, jobID)
	}
	if strings.Count(second.Name, "(re-crawl)") != 1 {
		t.Errorf("chained name = %q, want a single marker", second.Name)
	}
}

func TestJobHistoryFilters(t *testing.T) {
	h, jobID := seededHarness(t)

	rec := h.mustRequest(http.MethodGet, "/api/v1/jobs?status=done", "", http.StatusOK)
	done := decodeBody[struct {
		Data []struct {
			ID         string `json:"id"`
			SourceName string `json:"source_name"`
			SourceKind string `json:"source_kind"`
		} `json:"data"`
		Meta struct {
			Total int64 `json:"total"`
		} `json:"meta"`
	}](t, rec)

	if done.Meta.Total != 1 || done.Data[0].ID != jobID {
		t.Fatalf("status filter = %+v", done)
	}
	if done.Data[0].SourceName == "" || done.Data[0].SourceKind != "apify" {
		t.Errorf("the job list must carry the provider for the history table: %+v", done.Data[0])
	}

	rec = h.mustRequest(http.MethodGet, "/api/v1/jobs?status=failed", "", http.StatusOK)
	if got := decodeBody[struct {
		Meta struct {
			Total int64 `json:"total"`
		} `json:"meta"`
	}](t, rec).Meta.Total; got != 0 {
		t.Fatalf("failed jobs = %d, want 0", got)
	}

	rec = h.mustRequest(http.MethodGet, "/api/v1/jobs?q=see", "", http.StatusOK)
	if got := decodeBody[struct {
		Meta struct {
			Total int64 `json:"total"`
		} `json:"meta"`
	}](t, rec).Meta.Total; got != 1 {
		t.Fatalf("name search = %d, want 1", got)
	}
}

func TestSourceLifecycle(t *testing.T) {
	h := newHarness(t, defaultPages())

	t.Run("starts unconfigured", func(t *testing.T) {
		rec := h.mustRequest(http.MethodGet, "/api/v1/sources", "", http.StatusOK)
		sources := decodeBody[[]struct {
			ID      string `json:"id"`
			Kind    string `json:"kind"`
			Role    string `json:"role"`
			HasKey  bool   `json:"has_key"`
			Enabled bool   `json:"enabled"`
		}](t, rec)

		// Two Google Maps providers, the email verifier, and the two campaign
		// providers: they all share the table, the encrypted key storage and the
		// connection test.
		byKind := make(map[string]string, len(sources))
		for _, source := range sources {
			byKind[source.Kind] = source.Role
			if source.HasKey || source.Enabled {
				t.Errorf("source %s should start unconfigured: %+v", source.Kind, source)
			}
		}
		want := map[string]string{
			"apify": "maps", "outscraper": "maps", "emailable": "verifier",
			"instantly": "outreach", "mailchimp": "newsletter",
		}
		for kind, role := range want {
			got, ok := byKind[kind]
			if !ok {
				t.Errorf("the %s source was not seeded", kind)
				continue
			}
			if got != role {
				t.Errorf("source %s has role %q, want %q", kind, got, role)
			}
		}
		if len(sources) != len(want) {
			t.Fatalf("got %d seeded sources, want %d", len(sources), len(want))
		}
	})

	t.Run("a job cannot run on an unconfigured source", func(t *testing.T) {
		body := fmt.Sprintf(`{"name":"No key","source_id":%q,"config":{"terms":["gyms"],"locations":[{"city":"Austin"}]}}`, apifySourceID)
		rec := h.mustRequest(http.MethodPost, "/api/v1/jobs", body, http.StatusUnprocessableEntity)
		if !strings.Contains(rec.Body.String(), "source_id") {
			t.Fatalf("error = %s", rec.Body.String())
		}
	})

	t.Run("the key is stored encrypted and never returned", func(t *testing.T) {
		h.configureSource()

		rec := h.mustRequest(http.MethodGet, "/api/v1/sources/"+apifySourceID, "", http.StatusOK)
		if strings.Contains(rec.Body.String(), "integration-provider-key") {
			t.Fatalf("the API returned the key: %s", rec.Body.String())
		}

		row, err := h.app.Store().GetSource(context.Background(), mustUUID(t, apifySourceID))
		if err != nil {
			t.Fatal(err)
		}
		if len(row.ApiKeyEnc) == 0 {
			t.Fatal("no key was stored")
		}
		if strings.Contains(string(row.ApiKeyEnc), "integration-provider-key") {
			t.Fatal("the key was stored in plaintext")
		}
	})

	t.Run("test connection", func(t *testing.T) {
		rec := h.mustRequest(http.MethodPost, "/api/v1/sources/"+apifySourceID+"/test", "", http.StatusOK)
		result := decodeBody[struct {
			Ok   bool   `json:"ok"`
			Kind string `json:"kind"`
		}](t, rec)
		if !result.Ok || result.Kind != "apify" {
			t.Fatalf("result = %+v", result)
		}

		rec = h.mustRequest(http.MethodGet, "/api/v1/sources/"+apifySourceID, "", http.StatusOK)
		source := decodeBody[struct {
			LastTestedAt *string `json:"last_tested_at"`
			LastTestOk   *bool   `json:"last_test_ok"`
		}](t, rec)
		if source.LastTestedAt == nil || source.LastTestOk == nil || !*source.LastTestOk {
			t.Fatalf("the test result was not recorded: %+v", source)
		}
	})

	t.Run("a bad key is reported as provider_auth", func(t *testing.T) {
		h.provider.Err = provider.ErrAuth
		t.Cleanup(func() { h.provider.Err = nil })

		rec := h.mustRequest(http.MethodPost, "/api/v1/sources/"+apifySourceID+"/test", "", http.StatusBadGateway)
		if !strings.Contains(rec.Body.String(), "provider_auth") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})
}

func TestEventRetentionPrunesOldRows(t *testing.T) {
	h, jobID := seededHarness(t)
	ctx := context.Background()
	store := h.app.Store()

	// Age every stored event past the retention window.
	if _, err := store.Pool().Exec(ctx,
		`UPDATE job_events SET ts = now() - interval '45 days' WHERE job_id = $1`,
		mustUUID(t, jobID)); err != nil {
		t.Fatal(err)
	}

	deleted, err := store.PruneJobEvents(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	if deleted == 0 {
		t.Fatal("nothing was pruned")
	}

	remaining, err := store.ListJobEventsAfter(ctx, dbgenListParams(t, jobID))
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("%d events survived pruning", len(remaining))
	}
}

func TestBusinessCountIsUnaffectedBySuppressionFilterInExport(t *testing.T) {
	h, _ := seededHarness(t)

	// Suppress everything, then confirm the default export is empty and the
	// suppressed export is not.
	rec := h.mustRequest(http.MethodGet, "/api/v1/businesses", "", http.StatusOK)
	list := decodeBody[businessListPayload](t, rec)

	ids := make([]string, 0, len(list.Data))
	for _, row := range list.Data {
		ids = append(ids, fmt.Sprintf("%q", row.ID))
	}
	body := fmt.Sprintf(`{"ids":[%s],"action":"suppress"}`, strings.Join(ids, ","))
	h.mustRequest(http.MethodPost, "/api/v1/businesses/bulk", body, http.StatusOK)

	rec = h.mustRequest(http.MethodPost, "/api/v1/businesses/export", `{}`, http.StatusOK)
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("the default export returned %d rows, want only the header", len(records))
	}

	rec = h.mustRequest(http.MethodPost, "/api/v1/businesses/export", `{"suppressed":true}`, http.StatusOK)
	records, err = csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 {
		t.Fatalf("the suppressed export returned %d rows, want header + 3", len(records))
	}

	total, err := h.app.Store().CountBusinesses(context.Background(), db.BusinessFilter{Suppressed: boolPtr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("suppressed count = %d, want 3", total)
	}
}

func boolPtr(b bool) *bool { return &b }
