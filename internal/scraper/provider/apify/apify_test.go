package apify_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/scraper/provider"
	"github.com/bory/karvon-be/internal/scraper/provider/apify"
)

func loadFixture(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "tests", "fixtures", "apify_gyms_austin.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseDatasetItems(t *testing.T) {
	listings, err := apify.ParseDatasetItems(loadFixture(t), "run-123")
	if err != nil {
		t.Fatal(err)
	}
	// The unnamed place is dropped; the other three survive.
	if len(listings) != 3 {
		t.Fatalf("got %d listings, want 3", len(listings))
	}

	first := listings[0]
	if first.PlaceID != "ChIJ_apify_iron_works" || first.Name != "Iron Works Gym" {
		t.Fatalf("first listing = %+v", first)
	}
	if first.Category != "Gym" || first.City != "Austin" || first.State != "TX" || first.Zip != "78702" {
		t.Errorf("address fields not mapped: %+v", first)
	}
	if first.Rating == nil || *first.Rating != 4.7 {
		t.Errorf("Rating = %v, want 4.7", first.Rating)
	}
	if first.Reviews == nil || *first.Reviews != 318 {
		t.Errorf("Reviews = %v, want 318", first.Reviews)
	}
	if first.Lat == nil || first.Lng == nil {
		t.Errorf("coordinates not mapped: %+v", first)
	}
	if len(first.Emails) != 1 || first.Emails[0] != "info@ironworksgym.com" {
		t.Errorf("Emails = %v", first.Emails)
	}
	if first.RunID != "run-123" {
		t.Errorf("RunID = %q", first.RunID)
	}
	if len(first.Raw) == 0 {
		t.Error("Raw payload should be preserved")
	}

	// Zero values become nil so they are stored as NULL rather than as a real 0.
	third := listings[2]
	if third.Rating != nil || third.Reviews != nil || third.Lat != nil {
		t.Errorf("zero values should map to nil: %+v", third)
	}
}

func TestSearchSendsTheKeyInTheHeaderAndParsesTheResponse(t *testing.T) {
	fixture := loadFixture(t)

	var gotAuth, gotQuery, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)

		w.Header().Set("X-Apify-Run-Id", "run-from-header")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	p := apify.New(provider.Options{APIKey: "apify-secret", BaseURL: server.URL, Timeout: 5 * time.Second})
	listings, err := p.Search(context.Background(), provider.SearchQuery{
		Term: "gyms", City: "Austin", State: "TX", Max: 50,
	})
	if err != nil {
		t.Fatal(err)
	}

	if gotAuth != "Bearer apify-secret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if strings.Contains(gotQuery, "apify-secret") {
		t.Errorf("the API key must never appear in the query string: %q", gotQuery)
	}
	// The structured fields resolve to an exact administrative polygon, and the state
	// code is expanded because "TX" alone matches nothing in a geocoder.
	if !strings.Contains(gotBody, `"state":"Texas"`) || !strings.Contains(gotBody, `"city":"Austin"`) {
		t.Errorf("request body did not carry the structured location: %q", gotBody)
	}
	if !strings.Contains(gotBody, `"maxCrawledPlacesPerSearch":50`) {
		t.Errorf("request body did not carry the limit: %q", gotBody)
	}
	if len(listings) != 3 {
		t.Fatalf("got %d listings, want 3", len(listings))
	}
	if listings[0].RunID != "run-from-header" {
		t.Errorf("RunID = %q, want the value from the response header", listings[0].RunID)
	}
}

func TestSearchClassifiesProviderErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr error
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantErr: provider.ErrAuth},
		{name: "forbidden", status: http.StatusForbidden, wantErr: provider.ErrAuth},
		{name: "rate limited", status: http.StatusTooManyRequests, wantErr: provider.ErrRateLimited},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
			}))
			defer server.Close()

			p := apify.New(provider.Options{APIKey: "bad", BaseURL: server.URL, Timeout: 2 * time.Second})
			_, err := p.Search(context.Background(), provider.SearchQuery{Term: "gyms", City: "Austin", Max: 1})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestSearchWithoutAKeyFailsFast(t *testing.T) {
	p := apify.New(provider.Options{})
	_, err := p.Search(context.Background(), provider.SearchQuery{Term: "gyms", City: "Austin", Max: 1})
	if !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestSearchServerErrorIsNotAnAuthError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	p := apify.New(provider.Options{APIKey: "k", BaseURL: server.URL, Timeout: 2 * time.Second})
	_, err := p.Search(context.Background(), provider.SearchQuery{Term: "gyms", City: "Austin", Max: 1})
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, provider.ErrAuth) {
		t.Fatal("a 500 must not be reported as an authentication failure, or the job fails permanently")
	}
}

// The asynchronous path: start, poll, drain.

func TestStartRunCarriesEveryTermAndTheRunSettings(t *testing.T) {
	var gotPath, gotQuery, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"id":"run-1","defaultDatasetId":"ds-1","status":"RUNNING"}}`))
	}))
	defer server.Close()

	p := apify.NewWithSettings(
		provider.Options{APIKey: "k", BaseURL: server.URL, Timeout: 5 * time.Second},
		apify.Settings{MemoryMB: 8192, MaxChargeUSD: 25, SkipClosedPlaces: true},
	)
	handle, err := p.StartRun(context.Background(), provider.SearchQuery{
		Terms: []string{"gyms", "crossfit"},
		State: "TX",
		Max:   -1,
	})
	if err != nil {
		t.Fatal(err)
	}

	if handle.RunID != "run-1" || handle.DatasetID != "ds-1" {
		t.Fatalf("handle = %+v", handle)
	}
	if !strings.HasSuffix(gotPath, "/runs") {
		t.Errorf("path = %q, want the run-start endpoint", gotPath)
	}
	for _, want := range []string{"memory=8192", "timeout=0", "maxTotalChargeUsd=25"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q is missing %q", gotQuery, want)
		}
	}
	// Every term in one run is what keeps a place matching two terms from being
	// scraped, and billed, twice.
	if !strings.Contains(gotBody, `"searchStringsArray":["gyms","crossfit"]`) {
		t.Errorf("request body did not carry every term: %q", gotBody)
	}
	// A negative maximum means "everything in the area", which the actor expresses
	// by the field being absent.
	if strings.Contains(gotBody, "maxCrawledPlacesPerSearch") {
		t.Errorf("an unlimited run must not send a per-search cap: %q", gotBody)
	}
}

func TestRunStateMapsVendorStatuses(t *testing.T) {
	tests := []struct {
		status       string
		wantTerminal bool
		wantOK       bool
	}{
		{status: "READY"},
		{status: "RUNNING"},
		{status: "ABORTING"},
		{status: "TIMING-OUT"},
		{status: "SUCCEEDED", wantTerminal: true, wantOK: true},
		{status: "FAILED", wantTerminal: true},
		{status: "ABORTED", wantTerminal: true},
		{status: "TIMED-OUT", wantTerminal: true},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"data":{"id":"run-1","status":"` + tt.status +
					`","defaultDatasetId":"ds-1","usageTotalUsd":1.25}}`))
			}))
			defer server.Close()

			p := apify.New(provider.Options{APIKey: "k", BaseURL: server.URL, Timeout: time.Second})
			state, err := p.RunState(context.Background(), "run-1")
			if err != nil {
				t.Fatal(err)
			}
			if state.Terminal != tt.wantTerminal || state.OK != tt.wantOK {
				t.Fatalf("%s: terminal=%v ok=%v, want %v/%v",
					tt.status, state.Terminal, state.OK, tt.wantTerminal, tt.wantOK)
			}
			if state.DatasetID != "ds-1" || state.CostUSD != 1.25 {
				t.Errorf("state = %+v", state)
			}
		})
	}
}

func TestFetchPageAsksForOneWindowOfTheDataset(t *testing.T) {
	fixture := loadFixture(t)
	var gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	p := apify.New(provider.Options{APIKey: "k", BaseURL: server.URL, Timeout: time.Second})
	page, err := p.FetchPage(context.Background(), "ds-1", 2000, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Listings) != 3 {
		t.Fatalf("got %d listings, want 3", len(page.Listings))
	}
	// The fixture holds four rows and one is unusable. Paging advances by what the
	// vendor returned, or a window of only unusable rows would be read forever.
	if page.Items != 4 {
		t.Errorf("Items = %d, want the 4 rows the vendor returned", page.Items)
	}
	if !strings.Contains(gotPath, "/datasets/ds-1/items") {
		t.Errorf("path = %q", gotPath)
	}
	for _, want := range []string{"offset=2000", "limit=500"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q is missing %q", gotQuery, want)
		}
	}
	// A server-side filter would return a shorter window than the range asked for,
	// and the offset is what a resumed drain counts on.
	if strings.Contains(gotQuery, "clean=true") {
		t.Errorf("the dataset window must not be filtered server-side: %q", gotQuery)
	}
}

func TestFetchPageWithoutADatasetFails(t *testing.T) {
	p := apify.New(provider.Options{APIKey: "k", BaseURL: "http://unused.invalid"})
	if _, err := p.FetchPage(context.Background(), "", 0, 10); err == nil {
		t.Fatal("expected an error when there is no dataset to read")
	}
}

func TestResurrectAndAbortHitTheRunEndpoints(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{"data":{"id":"run-1","status":"RUNNING"}}`))
	}))
	defer server.Close()

	p := apify.New(provider.Options{APIKey: "k", BaseURL: server.URL, Timeout: time.Second})
	if err := p.Resurrect(context.Background(), "run-1"); err != nil {
		t.Fatal(err)
	}
	if err := p.AbortRun(context.Background(), "run-1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /v2/actor-runs/run-1/resurrect", "POST /v2/actor-runs/run-1/abort"}
	for i := range want {
		if i >= len(paths) || paths[i] != want[i] {
			t.Fatalf("calls = %v, want %v", paths, want)
		}
	}
}

func TestFindOrphanRunOnlyAdoptsAnUnambiguousMatch(t *testing.T) {
	started := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	runsJSON := func(runs ...string) string {
		return `{"data":{"items":[` + strings.Join(runs, ",") + `]}}`
	}
	run := func(id string, at time.Time) string {
		return `{"id":"` + id + `","status":"RUNNING","defaultDatasetId":"ds-` + id +
			`","startedAt":"` + at.Format(time.RFC3339) + `"}`
	}

	tests := []struct {
		name        string
		payload     string
		known       map[string]struct{}
		wantMatches int
		wantRunID   string
	}{
		{
			name:        "one unrecorded run in the window",
			payload:     runsJSON(run("orphan", started.Add(2*time.Second))),
			wantMatches: 1,
			wantRunID:   "orphan",
		},
		{
			name:        "runs we already know about are not orphans",
			payload:     runsJSON(run("recorded", started)),
			known:       map[string]struct{}{"recorded": {}},
			wantMatches: 0,
		},
		{
			name:        "a run from another hour is not ours",
			payload:     runsJSON(run("elsewhere", started.Add(-2*time.Hour))),
			wantMatches: 0,
		},
		{
			// Guessing here would file one location's places under another, and
			// starting a fresh run would add a third bill.
			name:        "two candidates are ambiguous",
			payload:     runsJSON(run("a", started), run("b", started.Add(time.Second))),
			wantMatches: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.payload))
			}))
			defer server.Close()

			p := apify.New(provider.Options{APIKey: "k", BaseURL: server.URL, Timeout: time.Second})
			handle, matches, err := p.FindOrphanRun(context.Background(), started, tt.known)
			if err != nil {
				t.Fatal(err)
			}
			if matches != tt.wantMatches {
				t.Fatalf("matches = %d, want %d", matches, tt.wantMatches)
			}
			if tt.wantRunID != "" && handle.RunID != tt.wantRunID {
				t.Errorf("run id = %q, want %q", handle.RunID, tt.wantRunID)
			}
		})
	}
}

func TestAsyncCallsNeedAKey(t *testing.T) {
	p := apify.New(provider.Options{BaseURL: "http://unused.invalid"})
	if _, err := p.StartRun(context.Background(), provider.SearchQuery{Terms: []string{"gyms"}}); !errors.Is(err, provider.ErrAuth) {
		t.Errorf("StartRun err = %v, want ErrAuth", err)
	}
	if _, err := p.RunState(context.Background(), "run-1"); !errors.Is(err, provider.ErrAuth) {
		t.Errorf("RunState err = %v, want ErrAuth", err)
	}
	if _, err := p.FetchPage(context.Background(), "ds-1", 0, 10); !errors.Is(err, provider.ErrAuth) {
		t.Errorf("FetchPage err = %v, want ErrAuth", err)
	}
}

// ProviderImplementsAsync is a compile-time check that the Apify client can be used on
// the long-run path at all; the job pipeline picks the path by this assertion.
var _ provider.AsyncProvider = (*apify.Provider)(nil)
