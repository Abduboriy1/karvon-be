package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/business"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	httpapi "github.com/bory/karvon-be/internal/http"
	"github.com/bory/karvon-be/internal/scraper"
	"github.com/bory/karvon-be/internal/source"
	"github.com/bory/karvon-be/internal/stats"
	"github.com/bory/karvon-be/internal/verify"
)

const testAPIKey = "test-api-key"

var (
	testJobID    = uuid.MustParse("01a0ba9a-cd53-7ad9-b4f8-7a01755aac06")
	testSourceID = uuid.MustParse("0192f000-0000-7000-8000-000000000001")
	testBizID    = uuid.MustParse("01a0bb00-0000-7000-8000-00000000000b")
	testRunID    = uuid.MustParse("01a0bc00-0000-7000-8000-00000000000c")
	testVerifyID = uuid.MustParse("01a0bd00-0000-7000-8000-00000000000d")
)

// stubJobs records what the handler asked for and returns canned answers.
type stubJobs struct {
	job      db.JobRow
	list     scraper.ListResult
	estimate scraper.Estimate
	err      error

	recrawlTargets []string
	recrawlFilter  *db.BusinessFilter
	recrawled      []uuid.UUID

	lastCreate  scraper.CreateInput
	lastFilter  db.JobFilter
	lastSort    string
	lastPage    int
	lastPerPage int
	deleted     []uuid.UUID
}

func (s *stubJobs) Estimate(_ context.Context, in scraper.CreateInput) (scraper.Estimate, error) {
	s.lastCreate = in
	return s.estimate, s.err
}

func (s *stubJobs) Create(_ context.Context, in scraper.CreateInput) (db.JobRow, error) {
	s.lastCreate = in
	return s.job, s.err
}

func (s *stubJobs) Get(context.Context, uuid.UUID) (db.JobRow, error) { return s.job, s.err }

func (s *stubJobs) List(_ context.Context, filter db.JobFilter, sort string, page, perPage int) (scraper.ListResult, error) {
	s.lastFilter, s.lastSort, s.lastPage, s.lastPerPage = filter, sort, page, perPage
	return s.list, s.err
}

func (s *stubJobs) Cancel(context.Context, uuid.UUID) (db.JobRow, error) { return s.job, s.err }
func (s *stubJobs) Rerun(context.Context, uuid.UUID) (db.JobRow, error)  { return s.job, s.err }

func (s *stubJobs) Recrawl(_ context.Context, id uuid.UUID, targets []string) (db.JobRow, error) {
	s.recrawled = append(s.recrawled, id)
	s.recrawlTargets = targets
	return s.job, s.err
}

func (s *stubJobs) RecrawlBusinesses(_ context.Context, filter db.BusinessFilter, targets []string) (db.JobRow, error) {
	s.recrawlFilter = &filter
	s.recrawlTargets = targets
	return s.job, s.err
}

func (s *stubJobs) Delete(_ context.Context, id uuid.UUID) error {
	s.deleted = append(s.deleted, id)
	return s.err
}

type stubBusinesses struct {
	list   business.ListResult
	detail business.Detail
	bulk   int64
	err    error

	lastFilter  db.BusinessFilter
	lastSort    string
	lastPage    int
	lastPerPage int
	lastUpdate  business.UpdateInput
	exportRows  []business.CSVRow
}

func (s *stubBusinesses) List(_ context.Context, filter db.BusinessFilter, sort string, page, perPage int) (business.ListResult, error) {
	s.lastFilter, s.lastSort, s.lastPage, s.lastPerPage = filter, sort, page, perPage
	return s.list, s.err
}

func (s *stubBusinesses) Get(context.Context, uuid.UUID) (business.Detail, error) {
	return s.detail, s.err
}

func (s *stubBusinesses) Update(_ context.Context, _ uuid.UUID, in business.UpdateInput) (business.Detail, error) {
	s.lastUpdate = in
	return s.detail, s.err
}

func (s *stubBusinesses) Bulk(context.Context, []uuid.UUID, business.BulkAction) (int64, error) {
	return s.bulk, s.err
}

func (s *stubBusinesses) Export(_ context.Context, filter db.BusinessFilter, dst io.Writer, flush func()) (int, error) {
	s.lastFilter = filter
	if s.err != nil {
		return 0, s.err
	}
	writer, err := business.NewCSVWriter(dst, flush)
	if err != nil {
		return 0, err
	}
	for _, row := range s.exportRows {
		if err := writer.Write(row); err != nil {
			return 0, err
		}
	}
	return writer.Rows(), writer.Close()
}

type stubSources struct {
	list   []dbgen.Source
	source dbgen.Source
	result source.TestResult
	err    error

	lastUpdate source.UpdateInput
}

func (s *stubSources) List(context.Context) ([]dbgen.Source, error)         { return s.list, s.err }
func (s *stubSources) Get(context.Context, uuid.UUID) (dbgen.Source, error) { return s.source, s.err }
func (s *stubSources) Test(context.Context, uuid.UUID) (source.TestResult, error) {
	return s.result, s.err
}

func (s *stubSources) Update(_ context.Context, _ uuid.UUID, in source.UpdateInput) (dbgen.Source, error) {
	s.lastUpdate = in
	return s.source, s.err
}

// stubVerification answers the /verification endpoints with canned results.
type stubVerification struct {
	stats    verify.Stats
	list     verify.ListResult
	detail   verify.Detail
	run      dbgen.VerificationRun
	runs     verify.RunListResult
	estimate verify.Estimate
	cfg      verify.ServiceConfig
	settings verify.Settings
	view     verify.SettingsView
	err      error
	saveErr  error

	lastFilter    db.VerificationFilter
	lastSort      string
	lastPage      int
	lastPerPage   int
	lastRunInput  verify.CreateRunInput
	savedSettings []verify.Settings
	verifiedOne   []uuid.UUID
	verifiedPass  verify.Pass
	appliedTypoTo []uuid.UUID
	cancelled     []uuid.UUID
}

func (s *stubVerification) Config() verify.ServiceConfig { return s.cfg }

func (s *stubVerification) Settings(context.Context) verify.Settings { return s.settings }

func (s *stubVerification) SettingsView(context.Context) (verify.SettingsView, error) {
	return s.view, s.err
}

func (s *stubVerification) SaveSettings(_ context.Context, in verify.Settings) (verify.SettingsView, error) {
	s.savedSettings = append(s.savedSettings, in)
	if s.saveErr != nil {
		return verify.SettingsView{}, s.saveErr
	}
	view := s.view
	view.Settings = in
	return view, nil
}

func (s *stubVerification) Stats(context.Context) (verify.Stats, error) {
	return s.stats, s.err
}

func (s *stubVerification) List(_ context.Context, filter db.VerificationFilter, sort string, page, perPage int) (verify.ListResult, error) {
	s.lastFilter, s.lastSort, s.lastPage, s.lastPerPage = filter, sort, page, perPage
	return s.list, s.err
}

func (s *stubVerification) Get(context.Context, uuid.UUID) (verify.Detail, error) {
	return s.detail, s.err
}

func (s *stubVerification) VerifyOne(_ context.Context, id uuid.UUID, pass verify.Pass) (dbgen.VerificationRun, error) {
	s.verifiedOne = append(s.verifiedOne, id)
	s.verifiedPass = pass
	return s.run, s.err
}

func (s *stubVerification) ApplyTypo(_ context.Context, id uuid.UUID) (verify.Detail, error) {
	s.appliedTypoTo = append(s.appliedTypoTo, id)
	return s.detail, s.err
}

func (s *stubVerification) EstimateRun(_ context.Context, pass verify.Pass, filter verify.RunFilter) (verify.Estimate, error) {
	s.lastRunInput = verify.CreateRunInput{Pass: pass, Filter: filter}
	return s.estimate, s.err
}

func (s *stubVerification) CreateRun(_ context.Context, in verify.CreateRunInput) (dbgen.VerificationRun, error) {
	s.lastRunInput = in
	return s.run, s.err
}

func (s *stubVerification) GetRun(context.Context, uuid.UUID) (dbgen.VerificationRun, error) {
	return s.run, s.err
}

func (s *stubVerification) ListRuns(_ context.Context, _, _ *string, page, perPage int) (verify.RunListResult, error) {
	s.lastPage, s.lastPerPage = page, perPage
	return s.runs, s.err
}

func (s *stubVerification) CancelRun(_ context.Context, id uuid.UUID) (dbgen.VerificationRun, error) {
	s.cancelled = append(s.cancelled, id)
	return s.run, s.err
}

type stubStats struct {
	value stats.Scraper
	err   error
}

func (s *stubStats) Scraper(context.Context) (stats.Scraper, error) { return s.value, s.err }

// stubEvents serves stored events to the SSE handler without a database.
type stubEvents struct {
	events []dbgen.JobEvent
	status string
}

func (s *stubEvents) ListRecentJobEvents(_ context.Context, arg dbgen.ListRecentJobEventsParams) ([]dbgen.JobEvent, error) {
	out := append([]dbgen.JobEvent(nil), s.events...)
	// Newest first, as the real query returns them.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if int(arg.Lim) < len(out) {
		out = out[:arg.Lim]
	}
	return out, nil
}

func (s *stubEvents) ListJobEventsAfter(_ context.Context, arg dbgen.ListJobEventsAfterParams) ([]dbgen.JobEvent, error) {
	var out []dbgen.JobEvent
	for _, e := range s.events {
		if e.ID > arg.AfterID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *stubEvents) GetJobStatus(context.Context, uuid.UUID) (string, error) {
	return s.status, nil
}

// testDeps builds a server with all stubs wired in.
type testDeps struct {
	jobs         *stubJobs
	businesses   *stubBusinesses
	sources      *stubSources
	verification *stubVerification
	stats        *stubStats
	events       *stubEvents
	health       *stubHealth
}

type stubHealth struct {
	dbErr    error
	queueErr error
}

func (s *stubHealth) CheckDB(context.Context) error    { return s.dbErr }
func (s *stubHealth) CheckQueue(context.Context) error { return s.queueErr }

func newTestServer(t *testing.T) (http.Handler, *testDeps) {
	t.Helper()

	deps := &testDeps{
		jobs:       &stubJobs{job: sampleJobRow()},
		businesses: &stubBusinesses{},
		sources:    &stubSources{source: sampleSource()},
		verification: &stubVerification{
			cfg:      verify.ServiceConfig{MaxRunEmails: 50_000},
			settings: verify.DefaultSettings(),
			view:     sampleSettingsView(),
			run:      sampleVerificationRun(),
		},
		stats:  &stubStats{},
		events: &stubEvents{status: scraper.StatusRunning},
		health: &stubHealth{},
	}

	server := httpapi.NewServer(httpapi.Deps{
		Jobs:         deps.jobs,
		Businesses:   deps.businesses,
		Sources:      deps.sources,
		Verification: deps.verification,
		Stats:        deps.stats,
		Store:        deps.events,
		Health:       deps.health,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config:       httpapi.Config{Version: "test", SSEPingInterval: 50 * time.Millisecond},
	})

	handler := httpapi.NewRouter(server, httpapi.RouterConfig{
		APIKey:         testAPIKey,
		CORSOrigins:    []string{"http://localhost:5173"},
		RequestTimeout: 5 * time.Second,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return handler, deps
}

func sampleJobRow() db.JobRow {
	return db.JobRow{
		ID:         testJobID,
		Name:       "Gyms TX",
		Status:     scraper.StatusRunning,
		SourceID:   testSourceID,
		SourceName: "Apify",
		SourceKind: "apify",
		Config:     []byte(`{"terms":["gyms"],"locations":[{"city":"Austin","state":"TX"}],"max_per_query":50,"crawl_emails":true,"concurrency":8}`),
		Stats:      []byte(`{"queries_total":1,"queries_done":0}`),
		CreatedAt:  time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}
}

// sampleSettingsView is what the settings endpoint returns when nothing special is
// being tested: the shipped defaults with Reacher reported as down, which is the
// state a deployment that has not enabled it is actually in.
func sampleSettingsView() verify.SettingsView {
	settings := verify.DefaultSettings()
	reacherDown := false
	return verify.SettingsView{
		Settings:     settings,
		FreeMaxScore: verify.FreeMaxScore,
		WeightTotal:  verify.WeightTotal,
		Providers: []verify.ProviderHealth{
			{
				Provider: "existing", Label: "Karvon local checks", Stage: "free",
				Weighted: true, Toggleable: false, Description: "Local checks",
				Enabled: true, Weight: settings.Weights["existing"],
			},
			{
				Provider: "mailchecker", Label: "MailChecker", Stage: "free",
				Weighted: true, Toggleable: true, Description: "Disposable domains",
				Enabled: true, Weight: settings.Weights["mailchecker"],
			},
			{
				Provider: "reacher", Label: "Reacher", Stage: "free",
				Weighted: true, Toggleable: true, Description: "SMTP probe",
				Enabled: false, Weight: settings.Weights["reacher"],
				Healthy: &reacherDown, Error: "connection refused",
			},
			{
				Provider: "paid", Label: "Paid verifier", Stage: "paid",
				Weighted: false, Toggleable: true, Description: "Contracted API",
				Enabled: true,
			},
		},
	}
}

func sampleVerificationRun() dbgen.VerificationRun {
	return dbgen.VerificationRun{
		ID:        testRunID,
		Pass:      string(verify.PassSelf),
		Status:    verify.RunQueued,
		Filter:    []byte(`{"scope":"all"}`),
		Total:     3,
		CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}
}

// ptr and ptrInt32 keep the fixtures readable.
func ptr(v string) *string { return &v }

func ptrInt32(v int32) *int32 { return &v }

func sampleSource() dbgen.Source {
	return dbgen.Source{
		ID:             testSourceID,
		Kind:           "apify",
		Role:           "maps",
		Name:           "Apify",
		ApiKeyEnc:      []byte("encrypted"),
		CostPer1kCents: 400,
		Enabled:        true,
		CreatedAt:      time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
	}
}

// do issues an authenticated request unless withoutKey is set.
func do(t *testing.T, handler http.Handler, method, target, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	for _, opt := range opts {
		opt(req)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func withoutKey(r *http.Request) { r.Header.Del("Authorization") }

// decodeError reads the shared error envelope from a response.
func decodeError(t *testing.T, rec *httptest.ResponseRecorder) struct {
	Code    string
	Message string
	Fields  map[string]string
} {
	t.Helper()

	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details []struct {
				Field   string `json:"field"`
				Message string `json:"message"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("response is not an error envelope: %s", rec.Body.String())
	}
	if payload.Error.Code == "" {
		t.Fatalf("response has no error code: %s", rec.Body.String())
	}

	fields := map[string]string{}
	for _, d := range payload.Error.Details {
		fields[d.Field] = d.Message
	}
	return struct {
		Code    string
		Message string
		Fields  map[string]string
	}{Code: payload.Error.Code, Message: payload.Error.Message, Fields: fields}
}

func decodeJSONBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("could not decode response: %v (body: %s)", err, rec.Body.String())
	}
	return out
}

// errQueueDown stands in for a queue outage in health tests.
var errQueueDown = errors.New("queue unavailable")
