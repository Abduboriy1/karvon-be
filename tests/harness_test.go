// Package integration exercises the whole service against a real Postgres started
// with testcontainers: migrations, the queue, the scrape pipeline, SSE and the API.
//
// The provider and the crawler are replaced with deterministic doubles, so nothing in
// this suite touches the network.
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/bory/karvon-be/internal/app"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/ai"
	aifake "github.com/bory/karvon-be/internal/campaign/ai/fake"
	instantlyfake "github.com/bory/karvon-be/internal/campaign/provider/instantly/fake"
	mailchimpfake "github.com/bory/karvon-be/internal/campaign/provider/mailchimp/fake"
	"github.com/bory/karvon-be/internal/config"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/scraper"
	"github.com/bory/karvon-be/internal/scraper/provider"
	"github.com/bory/karvon-be/internal/scraper/provider/fake"
	"github.com/bory/karvon-be/internal/verify/verifier"
	fakeverifier "github.com/bory/karvon-be/internal/verify/verifier/fake"
)

const (
	testAPIKey        = "integration-test-api-key"
	testSecretKey     = "6ad91f1a9c7c4d6f8b3e2a1d0c9b8a7f6e5d4c3b2a1908070605040302010000"
	apifySourceID     = "0192f000-0000-7000-8000-000000000001"
	verifierSourceID  = "0192f000-0000-7000-8000-000000000003"
	instantlySourceID = "0192f000-0000-7000-8000-000000000004"
	mailchimpSourceID = "0192f000-0000-7000-8000-000000000005"
)

// harness is a running service instance plus the doubles it was built with.
type harness struct {
	t         *testing.T
	app       *app.App
	handler   http.Handler
	provider  *fake.Provider
	factory   *swappableFactory
	pages     *pageServer
	verifier  *fakeverifier.Verifier
	resolver  *stubResolver
	instantly *instantlyfake.Client
	mailchimp *mailchimpfake.Client
	ai        *aifake.Provider
	// now is the clock the campaign module reads, so a test can order events.
	now *testClock
}

// testClock is a settable clock shared by the campaign service and its workers.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func newTestClock() *testClock {
	return &testClock{at: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
}

// Now returns the current test time and is safe for concurrent workers.
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// Advance moves the clock forward, which is how a test orders a timeline.
func (c *testClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
	return c.at
}

// pageServer answers the crawler with canned HTML, keyed by host.
type pageServer struct {
	pages map[string]string
}

func (p *pageServer) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	if path == "" {
		path = "/"
	}
	key := req.URL.Host + path
	body, ok := p.pages[key]
	status := http.StatusOK
	if !ok {
		status = http.StatusNotFound
		body = "not found"
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

// startPostgres boots a throwaway database for one test.
func startPostgres(t *testing.T) string {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("karvon"),
		tcpostgres.WithUsername("karvon"),
		tcpostgres.WithPassword("karvon"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		if os.Getenv("KARVON_INTEGRATION") == "1" {
			t.Fatalf("could not start postgres: %v", err)
		}
		t.Skipf("skipping integration test, Docker is unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("could not terminate postgres container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("could not build connection string: %v", err)
	}
	return dsn
}

// harnessTuning collects the per-test adjustments to the application graph.
type harnessTuning struct {
	cfg  *config.Config
	opts []app.Option
}

// harnessOption customises one harness. Most tests need none.
type harnessOption func(*harnessTuning)

// withConfig adjusts the configuration the application is built from.
func withConfig(fn func(*config.Config)) harnessOption {
	return func(h *harnessTuning) { fn(h.cfg) }
}

// withAppOption adds an application option, for replacing a network-facing piece.
func withAppOption(opt app.Option) harnessOption {
	return func(h *harnessTuning) { h.opts = append(h.opts, opt) }
}

// newHarness starts Postgres, wires the app with test doubles and runs the workers.
func newHarness(t *testing.T, pages map[string]string, options ...harnessOption) *harness {
	t.Helper()

	dsn := startPostgres(t)

	// Start from the shipped defaults so a new setting does not silently differ
	// between the suite and production.
	cfg := config.Defaults()
	cfg.Env = "test"
	cfg.Addr = "127.0.0.1:0"
	cfg.LogLevel = "error"
	cfg.Version = "test"
	cfg.DatabaseURL = dsn
	cfg.DBMaxConns = 8
	cfg.MigrateOnBoot = true
	cfg.APIKey = testAPIKey
	cfg.SecretKey = testSecretKey
	cfg.QueryConcurrency = 2
	cfg.CrawlConcurrency = 4
	cfg.CrawlTimeout = 2 * time.Second
	cfg.CrawlUserAgent = "KarvonBot/test"
	cfg.CrawlMaxBodyBytes = 1 << 20
	cfg.ProviderTimeout = 5 * time.Second
	cfg.SSEPingInterval = time.Second
	// Verification: RDAP is off and the rate limit is lifted, so the suite makes no
	// outbound calls and does not sleep between them.
	cfg.VerifyRDAPEnabled = false
	cfg.VerifyThirdPartyRPS = 1000
	cfg.VerifySelfConcurrency = 4
	cfg.VerifyThirdPartyConcurrency = 2
	// Campaigns: a public URL so webhook registration is exercised, and no pacing
	// or batching delay, so a run finishes in seconds.
	cfg.PublicBaseURL = "https://karvon.test"
	cfg.InstantlyRPS = 100
	cfg.InstantlyLeadBatchGap = 0
	cfg.CampaignPushConcurrency = 2
	cfg.CampaignEventConcurrency = 4

	tuning := harnessTuning{cfg: &cfg}
	for _, option := range options {
		option(&tuning)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("test configuration is invalid: %v", err)
	}

	fakeProvider := fake.New(0)
	factory := newSwappableFactory(fakeProvider)
	crawlTransport := &pageServer{pages: pages}
	fakeVerifier := fakeverifier.New(10_000)
	resolver := newStubResolver()
	// The campaign doubles: nothing in this suite reaches Instantly, Mailchimp or
	// OpenAI, and the clock is fixed so a timeline can be asserted in order.
	clock := newTestClock()
	fakeInstantly := instantlyfake.New()
	fakeInstantly.Now = clock.Now
	fakeMailchimp := mailchimpfake.New()
	fakeMailchimp.Now = clock.Now
	fakeAI := aifake.New(sampleGeneration())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	appOptions := append([]app.Option{
		app.WithProviderFactory(factory),
		app.WithCrawlTransport(crawlTransport),
		app.WithVerifierFactory(staticVerifierFactory{verifier: fakeVerifier}),
		app.WithResolver(resolver),
		app.WithInstantlyFactory(instantlyfake.StaticFactory{Client: fakeInstantly}),
		app.WithMailchimpFactory(mailchimpfake.StaticFactory{Client: fakeMailchimp}),
		app.WithAIProvider(fakeAI),
		app.WithCampaignClock(clock.Now),
	}, tuning.opts...)

	application, err := app.New(ctx, cfg, testLogger(), appOptions...)
	if err != nil {
		t.Fatalf("could not build the application: %v", err)
	}
	t.Cleanup(application.Close)

	if err := application.Queue().Start(ctx); err != nil {
		t.Fatalf("could not start the queue: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = application.Queue().Stop(stopCtx)
	})

	return &harness{
		t:         t,
		app:       application,
		handler:   application.Handler(),
		provider:  fakeProvider,
		factory:   factory,
		pages:     crawlTransport,
		verifier:  fakeVerifier,
		resolver:  resolver,
		instantly: fakeInstantly,
		mailchimp: fakeMailchimp,
		ai:        fakeAI,
		now:       clock,
	}
}

// sampleGeneration is the canned AI output the fake generator returns.
func sampleGeneration() ai.Output {
	hook := 1
	cta := 2
	return ai.Output{
		Components: []ai.Component{
			{Type: campaign.ComponentSubject, Name: "Quick question, {{company|there}}", Body: "Quick question about {{company|your gym}}"},
			{Type: campaign.ComponentHook, Name: "Noticed the schedule", Body: "Hi {{first_name|there}}, I noticed your class schedule fills up most weeks."},
			{Type: campaign.ComponentCTA, Name: "Worth 15 minutes", Body: "Worth a 15-minute call next week?"},
		},
		Variants: []ai.VariantRef{{Name: "Schedule angle", SubjectIndex: 0, HookIndex: &hook, CTAIndex: &cta}},
	}
}

// staticVerifierFactory hands every source the same verifier double.
type staticVerifierFactory struct {
	verifier verifier.Verifier
}

func (f staticVerifierFactory) For(context.Context, dbgen.Source) (verifier.Verifier, error) {
	return f.verifier, nil
}

// stubResolver answers DNS from a map. Unknown domains resolve to nothing, which is
// a hard fail, and the default entry makes every *.test domain healthy.
type stubResolver struct {
	mu   sync.Mutex
	mx   map[string][]*net.MX
	ips  map[string][]net.IP
	txt  map[string][]string
	fail map[string]error
}

func newStubResolver() *stubResolver {
	return &stubResolver{
		mx:   map[string][]*net.MX{},
		ips:  map[string][]net.IP{},
		txt:  map[string][]string{},
		fail: map[string]error{},
	}
}

// healthy makes a domain look like a well-configured mail domain.
func (r *stubResolver) healthy(domains ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, domain := range domains {
		r.mx[domain] = []*net.MX{{Host: "mx." + domain, Pref: 10}}
		r.ips[domain] = []net.IP{net.ParseIP("93.184.216.34")}
		r.txt[domain] = []string{"v=spf1 include:_spf.example.com ~all"}
		r.txt["_dmarc."+domain] = []string{"v=DMARC1; p=none"}
	}
}

func (r *stubResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err, ok := r.fail[name]; ok {
		return nil, err
	}
	records, ok := r.mx[name]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return records, nil
}

func (r *stubResolver) LookupIP(_ context.Context, _, host string) ([]net.IP, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err, ok := r.fail[host]; ok {
		return nil, err
	}
	ips, ok := r.ips[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return ips, nil
}

func (r *stubResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	records, ok := r.txt[name]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return records, nil
}

// swappableFactory hands every source the same provider double, which a test can
// replace before it creates a job.
type swappableFactory struct {
	current atomic.Pointer[provider.Provider]
}

func newSwappableFactory(p provider.Provider) *swappableFactory {
	f := &swappableFactory{}
	f.current.Store(&p)
	return f
}

func (f *swappableFactory) For(context.Context, dbgen.Source) (provider.Provider, error) {
	return *f.current.Load(), nil
}

// request issues an authenticated API call and returns the recorder.
func (h *harness) request(method, target, body string) *httptest.ResponseRecorder {
	h.t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// mustRequest fails the test unless the call returned the expected status.
func (h *harness) mustRequest(method, target, body string, wantStatus int) *httptest.ResponseRecorder {
	h.t.Helper()
	rec := h.request(method, target, body)
	if rec.Code != wantStatus {
		h.t.Fatalf("%s %s = %d, want %d: %s", method, target, rec.Code, wantStatus, rec.Body.String())
	}
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("could not decode response: %v (body: %s)", err, rec.Body.String())
	}
	return out
}

// configureSource stores a key and enables the Apify source.
func (h *harness) configureSource() {
	h.t.Helper()
	h.mustRequest(http.MethodPut, "/api/v1/sources/"+apifySourceID,
		`{"api_key":"integration-provider-key","enabled":true,"cost_per_1k_cents":400}`,
		http.StatusOK)
}

type jobPayload struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error"`
	Stats  struct {
		QueriesTotal  int   `json:"queries_total"`
		QueriesDone   int   `json:"queries_done"`
		QueriesFailed int   `json:"queries_failed"`
		ListingsFound int   `json:"listings_found"`
		SitesTotal    int   `json:"sites_total"`
		SitesCrawled  int   `json:"sites_crawled"`
		EmailsFound   int   `json:"emails_found"`
		CostCents     int64 `json:"cost_cents"`
	} `json:"stats"`
}

// createJob posts a job and returns it.
func (h *harness) createJob(name string, terms, cities []string, crawl bool) jobPayload {
	h.t.Helper()

	locations := make([]string, 0, len(cities))
	for _, city := range cities {
		locations = append(locations, fmt.Sprintf(`{"city":%q,"state":"TX"}`, city))
	}
	quotedTerms := make([]string, 0, len(terms))
	for _, term := range terms {
		quotedTerms = append(quotedTerms, fmt.Sprintf("%q", term))
	}

	body := fmt.Sprintf(`{"name":%q,"source_id":%q,"config":{"terms":[%s],"locations":[%s],"max_per_query":10,"crawl_emails":%t,"concurrency":2}}`,
		name, apifySourceID, strings.Join(quotedTerms, ","), strings.Join(locations, ","), crawl)

	rec := h.mustRequest(http.MethodPost, "/api/v1/jobs", body, http.StatusCreated)
	return decodeBody[jobPayload](h.t, rec)
}

// waitForJob polls until the job reaches a terminal status.
func (h *harness) waitForJob(id string) jobPayload {
	h.t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	var last jobPayload
	for time.Now().Before(deadline) {
		rec := h.mustRequest(http.MethodGet, "/api/v1/jobs/"+id, "", http.StatusOK)
		last = decodeBody[jobPayload](h.t, rec)
		switch last.Status {
		case scraper.StatusDone, scraper.StatusFailed, scraper.StatusCancelled:
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("job %s never finished, last state: %+v", id, last)
	return last
}

// listingFor builds a provider listing for a business with its own website.
func listingFor(placeID, name, city, domain string) provider.Listing {
	rating := 4.5
	reviews := int32(120)
	lat, lng := 30.26, -97.74
	return provider.Listing{
		PlaceID:  placeID,
		Name:     name,
		Category: "Gym",
		Address:  "1 Main St, " + city,
		City:     city,
		State:    "TX",
		Zip:      "78701",
		Phone:    "+1512555" + placeID[len(placeID)-4:],
		Website:  "https://" + domain,
		Rating:   &rating,
		Reviews:  &reviews,
		Lat:      &lat,
		Lng:      &lng,
		RunID:    "integration-run",
		Raw:      json.RawMessage(fmt.Sprintf(`{"placeId":%q}`, placeID)),
	}
}

func mustUUID(t *testing.T, raw string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(raw)
	if err != nil {
		t.Fatalf("invalid uuid %q: %v", raw, err)
	}
	return id
}

// swapProvider replaces the provider double after the app was built. It is only safe
// before a job is created.
func (h *harness) swapProvider(p provider.Provider) {
	h.t.Helper()
	h.factory.current.Store(&p)
}

// blockingProvider blocks inside Search until released, which lets a test cancel a job
// while it is genuinely running.
type blockingProvider struct {
	release chan struct{}
	started chan struct{}
}

func (b *blockingProvider) Name() string { return "blocking" }

func (b *blockingProvider) Search(ctx context.Context, _ provider.SearchQuery) ([]provider.Listing, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.release:
		return nil, nil
	}
}

// dbgenListParams builds the parameters for reading a job's whole event history.
func dbgenListParams(t *testing.T, jobID string) dbgen.ListJobEventsAfterParams {
	t.Helper()
	return dbgen.ListJobEventsAfterParams{JobID: mustUUID(t, jobID), AfterID: 0, Lim: 1000}
}

// testLogger discards the service's own logs so a passing run stays readable.
// Set KARVON_TEST_LOG=1 to send them to stderr while chasing a failure.
func testLogger() *slog.Logger {
	if os.Getenv("KARVON_TEST_LOG") == "1" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
