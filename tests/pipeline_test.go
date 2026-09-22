package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/scraper/provider"
)

// homepage with a mailto link; contactOnly hides the address behind /contact.
const (
	homepageWithMailto = `<html><body><a href="mailto:info@ironworksgym.com">Email us</a></body></html>`
	homepageNoEmail    = `<html><body><a href="/contact">Contact</a><p>Come train with us</p></body></html>`
	contactPage        = `<html><body><p>Reach the desk at hello@austinbarbell.com</p></body></html>`
)

func defaultPages() map[string]string {
	return map[string]string{
		"ironworksgym.com/":         homepageWithMailto,
		"austinbarbell.com/":        homepageNoEmail,
		"austinbarbell.com/contact": contactPage,
	}
}

func TestScrapePipelineEndToEnd(t *testing.T) {
	h := newHarness(t, defaultPages())
	h.provider.ByQuery = map[string][]provider.Listing{
		"gyms in Austin, TX": {
			listingFor("place-iron-works", "Iron Works Gym", "Austin", "ironworksgym.com"),
			listingFor("place-barbell-club", "Austin Barbell Club", "Austin", "austinbarbell.com"),
		},
	}
	// A high per-1k price makes the cost arithmetic observable with only two listings.
	h.mustRequest(http.MethodPut, "/api/v1/sources/"+apifySourceID,
		`{"api_key":"integration-provider-key","enabled":true,"cost_per_1k_cents":5000}`, http.StatusOK)

	created := h.createJob("Gyms Austin", []string{"gyms"}, []string{"Austin"}, true)
	if created.Status != "queued" {
		t.Fatalf("a new job should start queued, got %q", created.Status)
	}

	job := h.waitForJob(created.ID)
	if job.Status != "done" {
		t.Fatalf("job finished as %q (%s)", job.Status, job.Error)
	}

	t.Run("stats are recomputed from the tables", func(t *testing.T) {
		if job.Stats.QueriesTotal != 1 || job.Stats.QueriesDone != 1 || job.Stats.QueriesFailed != 0 {
			t.Errorf("query counters = %+v", job.Stats)
		}
		if job.Stats.ListingsFound != 2 {
			t.Errorf("listings_found = %d, want 2", job.Stats.ListingsFound)
		}
		if job.Stats.SitesTotal != 2 || job.Stats.SitesCrawled != 2 {
			t.Errorf("site counters = %d/%d, want 2/2", job.Stats.SitesCrawled, job.Stats.SitesTotal)
		}
		if job.Stats.EmailsFound != 2 {
			t.Errorf("emails_found = %d, want 2", job.Stats.EmailsFound)
		}
		// 2 listings at 5000 cents per 1000 listings = 10 cents.
		if job.Stats.CostCents != 10 {
			t.Errorf("cost_cents = %d, want 10", job.Stats.CostCents)
		}
	})

	t.Run("businesses are stored with their addresses", func(t *testing.T) {
		rec := h.mustRequest(http.MethodGet, "/api/v1/businesses?job_id="+job.ID, "", http.StatusOK)
		payload := decodeBody[struct {
			Data []struct {
				Name               string `json:"name"`
				Domain             string `json:"domain"`
				PrimaryEmail       string `json:"primary_email"`
				PrimaryEmailSource string `json:"primary_email_source"`
				EmailsCount        int    `json:"emails_count"`
				City               string `json:"city"`
			} `json:"data"`
			Meta struct {
				Total int64 `json:"total"`
			} `json:"meta"`
		}](t, rec)

		if payload.Meta.Total != 2 {
			t.Fatalf("total = %d, want 2", payload.Meta.Total)
		}

		byDomain := map[string]string{}
		sources := map[string]string{}
		for _, row := range payload.Data {
			byDomain[row.Domain] = row.PrimaryEmail
			sources[row.Domain] = row.PrimaryEmailSource
			if row.City != "Austin" {
				t.Errorf("%s city = %q", row.Domain, row.City)
			}
		}
		if byDomain["ironworksgym.com"] != "info@ironworksgym.com" {
			t.Errorf("homepage mailto was not stored: %v", byDomain)
		}
		if sources["ironworksgym.com"] != "mailto" {
			t.Errorf("email source = %q, want mailto", sources["ironworksgym.com"])
		}
		if byDomain["austinbarbell.com"] != "hello@austinbarbell.com" {
			t.Errorf("the contact page was not followed: %v", byDomain)
		}
		if sources["austinbarbell.com"] != "regex" {
			t.Errorf("email source = %q, want regex", sources["austinbarbell.com"])
		}
	})

	t.Run("the event stream replays the whole job", func(t *testing.T) {
		rec := h.mustRequest(http.MethodGet, "/api/v1/jobs/"+job.ID+"/events", "", http.StatusOK)
		body := rec.Body.String()

		// jsonb renders with spaces after the colon, so match on the value alone.
		for _, want := range []string{"event: status", "event: log", "event: progress", `"done"`} {
			if !strings.Contains(body, want) {
				t.Errorf("event stream is missing %q\n---\n%s", want, body)
			}
		}
		if !strings.Contains(body, "id: 1") {
			t.Errorf("events should carry monotonic ids:\n%s", body)
		}
	})

	t.Run("the per-job CSV export contains the addresses", func(t *testing.T) {
		rec := h.mustRequest(http.MethodGet, "/api/v1/jobs/"+job.ID+"/export.csv", "", http.StatusOK)
		body := rec.Body.String()

		if !strings.HasPrefix(body, "id,name,category") {
			t.Fatalf("CSV header = %q", strings.SplitN(body, "\n", 2)[0])
		}
		for _, want := range []string{"info@ironworksgym.com", "hello@austinbarbell.com", "Iron Works Gym"} {
			if !strings.Contains(body, want) {
				t.Errorf("CSV is missing %q", want)
			}
		}
		if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") {
			t.Errorf("Content-Disposition = %q", got)
		}
	})

	t.Run("the dashboard reflects the run", func(t *testing.T) {
		rec := h.mustRequest(http.MethodGet, "/api/v1/stats/scraper", "", http.StatusOK)
		payload := decodeBody[struct {
			Businesses   int64 `json:"businesses"`
			WithEmail    int64 `json:"with_email"`
			JobsTotal    int64 `json:"jobs_total"`
			EmailsPerJob []struct {
				JobID  string `json:"job_id"`
				Emails int    `json:"emails"`
			} `json:"emails_per_job"`
			LastJob *struct {
				ID string `json:"id"`
			} `json:"last_job"`
		}](t, rec)

		if payload.Businesses != 2 || payload.WithEmail != 2 || payload.JobsTotal != 1 {
			t.Fatalf("counters = %+v", payload)
		}
		if len(payload.EmailsPerJob) != 1 || payload.EmailsPerJob[0].Emails != 2 {
			t.Fatalf("chart = %+v", payload.EmailsPerJob)
		}
		if payload.LastJob == nil || payload.LastJob.ID != job.ID {
			t.Fatalf("last_job = %+v", payload.LastJob)
		}
	})
}

func TestTwoJobsFindingTheSamePlaceShareOneBusiness(t *testing.T) {
	h := newHarness(t, defaultPages())
	h.provider.ByQuery = map[string][]provider.Listing{
		"gyms in Austin, TX": {listingFor("place-iron-works", "Iron Works Gym", "Austin", "ironworksgym.com")},
	}
	h.configureSource()

	first := h.waitForJob(h.createJob("First run", []string{"gyms"}, []string{"Austin"}, true).ID)
	second := h.waitForJob(h.createJob("Second run", []string{"gyms"}, []string{"Austin"}, true).ID)

	if first.Status != "done" || second.Status != "done" {
		t.Fatalf("jobs finished as %q and %q", first.Status, second.Status)
	}

	rec := h.mustRequest(http.MethodGet, "/api/v1/businesses", "", http.StatusOK)
	all := decodeBody[struct {
		Meta struct {
			Total int64 `json:"total"`
		} `json:"meta"`
	}](t, rec)
	if all.Meta.Total != 1 {
		t.Fatalf("the master list holds %d rows, want 1 deduplicated business", all.Meta.Total)
	}

	// Both jobs still list the business as one of their results.
	for _, id := range []string{first.ID, second.ID} {
		rec := h.mustRequest(http.MethodGet, "/api/v1/businesses?job_id="+id, "", http.StatusOK)
		scoped := decodeBody[struct {
			Meta struct {
				Total int64 `json:"total"`
			} `json:"meta"`
		}](t, rec)
		if scoped.Meta.Total != 1 {
			t.Fatalf("job %s sees %d businesses, want 1", id, scoped.Meta.Total)
		}
	}

	// The second job reuses the first crawl instead of fetching the site again.
	if second.Stats.EmailsFound == 0 {
		t.Error("the second job should still report the address it found")
	}
}

func TestJobWithoutCrawlingSkipsTheCrawlStage(t *testing.T) {
	h := newHarness(t, defaultPages())
	h.provider.ByQuery = map[string][]provider.Listing{
		"gyms in Austin, TX": {listingFor("place-iron-works", "Iron Works Gym", "Austin", "ironworksgym.com")},
	}
	h.configureSource()

	job := h.waitForJob(h.createJob("No crawl", []string{"gyms"}, []string{"Austin"}, false).ID)
	if job.Status != "done" {
		t.Fatalf("status = %q (%s)", job.Status, job.Error)
	}
	if job.Stats.EmailsFound != 0 {
		t.Fatalf("emails_found = %d, want 0 when crawling is off", job.Stats.EmailsFound)
	}
	if job.Stats.SitesCrawled != 0 {
		t.Fatalf("sites_crawled = %d, want 0", job.Stats.SitesCrawled)
	}
}

func TestCrawlerHonoursRobotsDisallow(t *testing.T) {
	pages := defaultPages()
	pages["ironworksgym.com/robots.txt"] = "User-agent: *\nDisallow: /\n"

	h := newHarness(t, pages)
	h.provider.ByQuery = map[string][]provider.Listing{
		"gyms in Austin, TX": {listingFor("place-iron-works", "Iron Works Gym", "Austin", "ironworksgym.com")},
	}
	h.configureSource()

	job := h.waitForJob(h.createJob("Robots", []string{"gyms"}, []string{"Austin"}, true).ID)
	if job.Status != "done" {
		t.Fatalf("status = %q (%s)", job.Status, job.Error)
	}
	if job.Stats.EmailsFound != 0 {
		t.Fatalf("emails_found = %d: a disallowed site must not be crawled", job.Stats.EmailsFound)
	}
}

func TestProviderAuthFailureFailsTheWholeJobImmediately(t *testing.T) {
	h := newHarness(t, defaultPages())
	h.provider.Err = provider.ErrAuth
	h.configureSource()

	job := h.waitForJob(h.createJob("Bad key", []string{"gyms", "crossfit"}, []string{"Austin", "Dallas"}, true).ID)
	if job.Status != "failed" {
		t.Fatalf("status = %q, want failed", job.Status)
	}
	if !strings.Contains(job.Error, "provider_auth") {
		t.Fatalf("error = %q, want it to name provider_auth", job.Error)
	}

	// Every remaining query is cancelled rather than retried against a bad key.
	store := h.app.Store()
	queries, err := store.ListJobQueries(context.Background(), mustUUID(t, job.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range queries {
		if q.Status == "queued" || q.Status == "running" {
			t.Errorf("query %s is still %q after an auth failure", q.ID, q.Status)
		}
	}
}

func TestCancelStopsARunningJob(t *testing.T) {
	h := newHarness(t, defaultPages())
	h.configureSource()

	// A provider that blocks lets the test cancel while the job is genuinely running.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.provider.ByQuery = nil
	h.provider.Generate = false

	blocking := &blockingProvider{release: release, started: make(chan struct{}, 16)}
	h.swapProvider(blocking)

	created := h.createJob("Cancel me", []string{"gyms"}, []string{"Austin", "Dallas", "Houston"}, true)

	select {
	case <-blocking.started:
	case <-time.After(20 * time.Second):
		t.Fatal("the provider was never called")
	}

	rec := h.mustRequest(http.MethodPost, "/api/v1/jobs/"+created.ID+"/cancel", "", http.StatusOK)
	cancelled := decodeBody[jobPayload](t, rec)
	if cancelled.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", cancelled.Status)
	}

	// Cancelling twice is a conflict, not a second cancellation.
	conflict := h.request(http.MethodPost, "/api/v1/jobs/"+created.ID+"/cancel", "")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("second cancel = %d, want 409", conflict.Code)
	}

	final := h.waitForJob(created.ID)
	if final.Status != "cancelled" {
		t.Fatalf("the job went back to %q after cancellation", final.Status)
	}
}

func TestDeleteJobKeepsTheBusinesses(t *testing.T) {
	h := newHarness(t, defaultPages())
	h.provider.ByQuery = map[string][]provider.Listing{
		"gyms in Austin, TX": {listingFor("place-iron-works", "Iron Works Gym", "Austin", "ironworksgym.com")},
	}
	h.configureSource()

	job := h.waitForJob(h.createJob("Delete me", []string{"gyms"}, []string{"Austin"}, true).ID)
	jobID := mustUUID(t, job.ID)

	h.mustRequest(http.MethodDelete, "/api/v1/jobs/"+job.ID, "", http.StatusNoContent)
	h.mustRequest(http.MethodGet, "/api/v1/jobs/"+job.ID, "", http.StatusNotFound)

	ctx := context.Background()
	store := h.app.Store()

	queries, err := store.ListJobQueries(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 0 {
		t.Errorf("%d job_queries rows survived the delete", len(queries))
	}

	results, err := store.CountJobResults(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if results != 0 {
		t.Errorf("%d job_results rows survived the delete", results)
	}

	total, err := store.CountBusinesses(ctx, db.BusinessFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("businesses = %d, want the master list to keep the row", total)
	}
}
