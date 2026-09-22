package outscraper_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/scraper/provider"
	"github.com/bory/karvon-be/internal/scraper/provider/outscraper"
)

func loadFixture(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "tests", "fixtures", "outscraper_gyms_austin.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseSearchResponse(t *testing.T) {
	listings, err := outscraper.ParseSearchResponse(loadFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(listings) != 2 {
		t.Fatalf("got %d listings, want 2 (the unnamed row is dropped)", len(listings))
	}

	first := listings[0]
	if first.PlaceID != "ChIJ_outscraper_iron_works" || first.Name != "Iron Works Gym" {
		t.Fatalf("first listing = %+v", first)
	}
	if first.Category != "Gym" {
		t.Errorf("Category = %q, want the `type` field", first.Category)
	}
	if first.Address != "812 E 6th St, Austin, TX 78702" || first.Zip != "78702" {
		t.Errorf("address not mapped: %+v", first)
	}
	if first.Website != "https://www.ironworksgym.com/" {
		t.Errorf("Website = %q", first.Website)
	}
	if len(first.Emails) != 2 {
		t.Errorf("Emails = %v, want both provider addresses", first.Emails)
	}
	if first.RunID != "outscraper-run-8813" {
		t.Errorf("RunID = %q", first.RunID)
	}

	if listings[1].Category != "Fitness center" {
		t.Errorf("second listing should fall back to `category`: %q", listings[1].Category)
	}
}

func TestParseSearchResponseRejectsAFailedRun(t *testing.T) {
	_, err := outscraper.ParseSearchResponse([]byte(`{"id":"x","status":"Error","data":[]}`))
	if err == nil {
		t.Fatal("a failed run must be reported as an error")
	}
}

func TestSearchSendsTheKeyAsAHeader(t *testing.T) {
	fixture := loadFixture(t)

	var gotKey, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-KEY")
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	p := outscraper.New(provider.Options{APIKey: "outscraper-secret", BaseURL: server.URL, Timeout: 5 * time.Second})
	listings, err := p.Search(context.Background(), provider.SearchQuery{
		Term: "gyms", City: "Austin", State: "TX", Max: 25,
	})
	if err != nil {
		t.Fatal(err)
	}

	if gotKey != "outscraper-secret" {
		t.Errorf("X-API-KEY = %q", gotKey)
	}
	if strings.Contains(gotQuery, "outscraper-secret") {
		t.Errorf("the API key must never appear in the query string: %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "query=gyms+in+Austin%2C+TX") {
		t.Errorf("query string = %q, want the full search phrase", gotQuery)
	}
	if !strings.Contains(gotQuery, "limit=25") {
		t.Errorf("query string = %q, want the limit", gotQuery)
	}
	if len(listings) != 2 {
		t.Fatalf("got %d listings, want 2", len(listings))
	}
}

func TestSearchMapsAuthFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	p := outscraper.New(provider.Options{APIKey: "bad", BaseURL: server.URL, Timeout: 2 * time.Second})
	_, err := p.Search(context.Background(), provider.SearchQuery{Term: "gyms", City: "Austin", Max: 1})
	if !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestSearchWithoutAKeyFailsFast(t *testing.T) {
	p := outscraper.New(provider.Options{})
	_, err := p.Search(context.Background(), provider.SearchQuery{Term: "gyms", City: "Austin", Max: 1})
	if !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}
