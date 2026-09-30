package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bory/karvon-be/internal/config"
	"github.com/bory/karvon-be/internal/scraper/provider"
)

// fakeFacebook stands in for services/fb-scrape: it answers POST /scrape from a fixed
// table of page results and records which pages were asked for.
type fakeFacebook struct {
	mu        sync.Mutex
	results   map[string]string
	requested []string
}

func newFakeFacebook(t *testing.T, results map[string]string) (*fakeFacebook, string) {
	t.Helper()
	fake := &fakeFacebook{results: results}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/scrape" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Pages []string `json:"pages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Pages) != 1 {
			http.Error(w, "bad request", http.StatusUnprocessableEntity)
			return
		}
		page := body.Pages[0]
		fake.mu.Lock()
		fake.requested = append(fake.requested, page)
		result, ok := fake.results[page]
		fake.mu.Unlock()
		if !ok {
			result = fmt.Sprintf(`{"url":%q,"name":null,"error":"Details section not found"}`, page)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"results":[%s]}`, result)
	}))
	t.Cleanup(server.Close)
	return fake, server.URL
}

func (f *fakeFacebook) pages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requested...)
}

type socialScrapeDetail struct {
	Phone  string `json:"phone"`
	Emails []struct {
		Email     string `json:"email"`
		Source    string `json:"source"`
		IsPrimary bool   `json:"is_primary"`
	} `json:"emails"`
}

func TestSocialScrapeReadsFacebookPagesForEmailsAndPhones(t *testing.T) {
	const dallasPage = "https://www.facebook.com/dallasiron"
	fake, fbURL := newFakeFacebook(t, map[string]string{
		dallasPage: `{"url":"https://www.facebook.com/dallasiron","name":"Dallas Iron Athletics",
			"category":"Gym","email":"coach@dallasiron.com","phone":"(214) 555-0199","website":null,"lines":[]}`,
	})

	// Both websites link to a Facebook Page; only Iron Works shows an address.
	pages := map[string]string{
		"ironworksgym.com/": `<html><body><a href="mailto:info@ironworksgym.com">Email us</a>
<a href="https://www.facebook.com/IronWorksGym/">Facebook</a></body></html>`,
		"dallasiron.com/": `<html><body><a href="https://www.facebook.com/DallasIron/">Facebook</a></body></html>`,
	}
	h := newHarness(t, pages, withConfig(func(c *config.Config) {
		c.FBScrapeEnabled = true
		c.FBScrapeURL = fbURL
		c.FBScrapeConcurrency = 2
	}))
	dallas := listingFor("place-dallas-iron", "Dallas Iron Athletics", "Dallas", "dallasiron.com")
	dallas.Phone = ""
	h.provider.ByQuery = map[string][]provider.Listing{
		"gyms in Austin, TX": {listingFor("place-iron-works", "Iron Works Gym", "Austin", "ironworksgym.com")},
		"gyms in Dallas, TX": {dallas},
	}
	h.configureSource()
	if seed := h.waitForJob(h.createJob("Seed", []string{"gyms"}, []string{"Austin", "Dallas"}, true).ID); seed.Status != "done" {
		t.Fatalf("seed job finished as %q (%s)", seed.Status, seed.Error)
	}
	dallasID := h.businessIDByDomain("dallasiron.com")
	ironID := h.businessIDByDomain("ironworksgym.com")

	// By default only businesses without an email are read: Dallas, not Iron Works.
	rec := h.mustRequest(http.MethodPost, "/api/v1/businesses/social-scrape", `{"networks":["facebook"]}`, http.StatusCreated)
	created := decodeBody[struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Config struct {
			SocialNetworks         []string `json:"social_networks"`
			SocialMissingEmailOnly bool     `json:"social_missing_email_only"`
		} `json:"config"`
	}](t, rec)
	if created.Name != "Facebook scrape of 2 business(es)" {
		t.Errorf("name = %q", created.Name)
	}
	if len(created.Config.SocialNetworks) != 1 || !created.Config.SocialMissingEmailOnly {
		t.Errorf("config = %+v", created.Config)
	}
	final := h.waitForJob(created.ID)
	if final.Status != "done" {
		t.Fatalf("the scrape finished as %q (%s)", final.Status, final.Error)
	}
	if final.Stats.SitesTotal != 1 || final.Stats.SitesCrawled != 1 {
		t.Errorf("pages = %d/%d, want 1/1", final.Stats.SitesCrawled, final.Stats.SitesTotal)
	}
	if got := fake.pages(); len(got) != 1 || got[0] != dallasPage {
		t.Fatalf("pages read = %v, want only %s", got, dallasPage)
	}

	detail := decodeBody[socialScrapeDetail](t, h.mustRequest(http.MethodGet, "/api/v1/businesses/"+dallasID, "", http.StatusOK))
	if len(detail.Emails) != 1 || detail.Emails[0].Email != "coach@dallasiron.com" ||
		detail.Emails[0].Source != "facebook" || !detail.Emails[0].IsPrimary {
		t.Errorf("emails = %+v, want the Facebook address as primary", detail.Emails)
	}
	if detail.Phone != "(214) 555-0199" {
		t.Errorf("phone = %q, want the number from the page", detail.Phone)
	}

	// Asked for explicitly, a business that already has an address is read too. Its
	// page shows nothing, which leaves the business as it was.
	rec = h.mustRequest(http.MethodPost, "/api/v1/businesses/social-scrape",
		fmt.Sprintf(`{"networks":["facebook"],"missing_email_only":false,"ids":[%q]}`, ironID), http.StatusCreated)
	final = h.waitForJob(decodeBody[jobPayload](t, rec).ID)
	if final.Status != "done" || final.Stats.SitesTotal != 1 || final.Stats.SitesCrawled != 1 {
		t.Fatalf("second scrape = %q, pages %d/%d", final.Status, final.Stats.SitesCrawled, final.Stats.SitesTotal)
	}
	iron := decodeBody[socialScrapeDetail](t, h.mustRequest(http.MethodGet, "/api/v1/businesses/"+ironID, "", http.StatusOK))
	if len(iron.Emails) != 1 || iron.Emails[0].Source != "mailto" {
		t.Errorf("emails = %+v, want the website's address untouched", iron.Emails)
	}
	if want := listingFor("place-iron-works", "", "", "").Phone; iron.Phone != want {
		t.Errorf("phone = %q, want the provider's %q kept", iron.Phone, want)
	}

	// A social media scrape has no search behind it.
	h.mustRequest(http.MethodPost, "/api/v1/jobs/"+created.ID+"/rerun", "", http.StatusConflict)
	h.mustRequest(http.MethodPost, "/api/v1/businesses/social-scrape", `{"networks":["facebook"],"city":"Nowhere"}`, http.StatusConflict)
}
