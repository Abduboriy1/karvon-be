package integration_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/bory/karvon-be/internal/scraper/provider"
)

type businessSocialsPayload struct {
	Emails []struct {
		Email string `json:"email"`
	} `json:"emails"`
	Socials []struct {
		Network string `json:"network"`
		URL     string `json:"url"`
	} `json:"socials"`
}

func (h *harness) businessIDByDomain(domain string) string {
	h.t.Helper()
	rec := h.mustRequest(http.MethodGet, "/api/v1/businesses?q="+domain, "", http.StatusOK)
	list := decodeBody[businessListPayload](h.t, rec)
	for _, b := range list.Data {
		if b.Domain == domain {
			return b.ID
		}
	}
	h.t.Fatalf("no business with domain %s", domain)
	return ""
}

func TestRecrawlBusinessesForSocialsRevisitsSitesThatAlreadyHaveAnEmail(t *testing.T) {
	h, _ := seededHarness(t)

	// Iron Works got its email from the seed crawl; its site now also links to a
	// Facebook page. An email re-crawl would skip it; a social re-crawl must not.
	h.pages.pages["ironworksgym.com/"] = `<html><body><a href="mailto:info@ironworksgym.com">Email us</a>
<a href="https://www.facebook.com/IronWorksGym/">Facebook</a></body></html>`
	id := h.businessIDByDomain("ironworksgym.com")

	rec := h.mustRequest(http.MethodPost, "/api/v1/businesses/recrawl",
		fmt.Sprintf(`{"ids":[%q],"targets":["socials"]}`, id), http.StatusCreated)
	created := decodeBody[jobPayload](t, rec)

	final := h.waitForJob(created.ID)
	if final.Status != "done" {
		t.Fatalf("the re-crawl finished as %q (%s)", final.Status, final.Error)
	}
	if final.Stats.SitesTotal != 1 || final.Stats.SitesCrawled != 1 {
		t.Errorf("sites = %d/%d, want 1/1", final.Stats.SitesCrawled, final.Stats.SitesTotal)
	}

	rec = h.mustRequest(http.MethodGet, "/api/v1/businesses/"+id, "", http.StatusOK)
	detail := decodeBody[businessSocialsPayload](t, rec)
	if len(detail.Socials) != 1 || detail.Socials[0].URL != "https://www.facebook.com/ironworksgym" {
		t.Fatalf("socials = %+v, want the Facebook page", detail.Socials)
	}
	if len(detail.Emails) != 1 {
		t.Errorf("emails = %+v, want the original address kept once", detail.Emails)
	}
}

func TestRecrawlBusinessesForEmailsSkipsSitesThatHaveOne(t *testing.T) {
	h, _ := seededHarness(t)
	id := h.businessIDByDomain("ironworksgym.com")

	rec := h.mustRequest(http.MethodPost, "/api/v1/businesses/recrawl",
		fmt.Sprintf(`{"ids":[%q],"targets":["emails"]}`, id), http.StatusCreated)
	final := h.waitForJob(decodeBody[jobPayload](t, rec).ID)
	if final.Status != "done" {
		t.Fatalf("the re-crawl finished as %q (%s)", final.Status, final.Error)
	}
	if final.Stats.SitesTotal != 0 {
		t.Errorf("sites_total = %d, want 0: the only business already has an email", final.Stats.SitesTotal)
	}
}

func TestRecrawlBusinessesByFilterAndValidation(t *testing.T) {
	h, _ := seededHarness(t)

	rec := h.mustRequest(http.MethodPost, "/api/v1/businesses/recrawl",
		`{"city":"Dallas","targets":["emails","socials"]}`, http.StatusCreated)
	created := decodeBody[struct {
		Name  string `json:"name"`
		Stats struct {
			ListingsFound int `json:"listings_found"`
		} `json:"stats"`
		Config struct {
			RecrawlTargets []string `json:"recrawl_targets"`
		} `json:"config"`
	}](t, rec)
	if created.Stats.ListingsFound != 1 {
		t.Errorf("listings_found = %d, want the one Dallas business", created.Stats.ListingsFound)
	}
	if len(created.Config.RecrawlTargets) != 2 {
		t.Errorf("recrawl_targets = %v", created.Config.RecrawlTargets)
	}

	h.mustRequest(http.MethodPost, "/api/v1/businesses/recrawl", `{"targets":["phones"]}`, http.StatusUnprocessableEntity)
	h.mustRequest(http.MethodPost, "/api/v1/businesses/recrawl", `{"city":"Dallas"}`, http.StatusUnprocessableEntity)
	h.mustRequest(http.MethodPost, "/api/v1/businesses/recrawl", `{"city":"Nowhere","targets":["emails"]}`, http.StatusConflict)
}

func TestPagesOnASharedDomainAreNotReusedForEachOther(t *testing.T) {
	// A city site hosts one page per facility, each with its own address. Sharing a
	// domain must not hand one facility's address to another.
	pages := map[string]string{
		"cityofexample.gov/pool": `<html><body><a href="mailto:pool@cityofexample.gov">Pool</a></body></html>`,
		"cityofexample.gov/rink": `<html><body><a href="mailto:rink@cityofexample.gov">Rink</a></body></html>`,
	}
	h := newHarness(t, pages)
	h.provider.ByQuery = map[string][]provider.Listing{
		"parks in Austin, TX": {
			listingFor("place-city-pool", "City Pool", "Austin", "cityofexample.gov/pool"),
			listingFor("place-city-rink", "City Rink", "Austin", "cityofexample.gov/rink"),
		},
	}
	h.configureSource()

	job := h.waitForJob(h.createJob("City", []string{"parks"}, []string{"Austin"}, true).ID)
	if job.Status != "done" {
		t.Fatalf("job finished as %q (%s)", job.Status, job.Error)
	}

	rec := h.mustRequest(http.MethodGet, "/api/v1/businesses?q=cityofexample&per_page=50", "", http.StatusOK)
	list := decodeBody[businessListPayload](t, rec)
	want := map[string]string{"City Pool": "pool@cityofexample.gov", "City Rink": "rink@cityofexample.gov"}
	if len(list.Data) != 2 {
		t.Fatalf("businesses = %+v", list.Data)
	}
	for _, b := range list.Data {
		if b.PrimaryEmail != want[b.Name] {
			t.Errorf("%s primary_email = %q, want %q", b.Name, b.PrimaryEmail, want[b.Name])
		}
	}
}
