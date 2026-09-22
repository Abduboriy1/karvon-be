package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/scraper"
)

func event(id int64, eventType, data string) dbgen.JobEvent {
	return dbgen.JobEvent{
		ID:    id,
		JobID: testJobID,
		Type:  eventType,
		Ts:    time.Now().UTC(),
		Data:  []byte(data),
	}
}

// streamEvents runs the SSE handler until it finishes or the deadline passes.
func streamEvents(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the SSE handler did not finish before the deadline")
	}
	return rec
}

func TestStreamJobEventsReplaysHistoryThenClosesOnATerminalStatus(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.jobs.job.Status = scraper.StatusRunning
	deps.events.status = scraper.StatusRunning
	deps.events.events = []dbgen.JobEvent{
		event(1, "status", `{"status":"running"}`),
		event(2, "log", `{"level":"info","msg":"expanded 1 queries","ts":"2026-09-19T16:00:00Z"}`),
		event(3, "progress", `{"queries_done":1,"queries_total":1}`),
		event(4, "status", `{"status":"done","finished_at":"2026-09-19T16:01:00Z"}`),
	}

	rec := streamEvents(t, handler, "/api/v1/jobs/"+testJobID.String()+"/events")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-cache") {
		t.Errorf("Cache-Control = %q", got)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"event: status\nid: 1\ndata: {\"status\":\"running\"}",
		"event: log\nid: 2\n",
		"event: progress\nid: 3\n",
		"event: status\nid: 4\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream is missing %q\n---\n%s", want, body)
		}
	}
	// Events must arrive oldest first so the frontend's log buffer reads correctly.
	if strings.Index(body, "id: 1") > strings.Index(body, "id: 4") {
		t.Fatalf("events are out of order:\n%s", body)
	}
}

func TestStreamJobEventsResumesFromLastEventID(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.events.events = []dbgen.JobEvent{
		event(1, "log", `{"level":"info","msg":"one","ts":"2026-09-19T16:00:00Z"}`),
		event(2, "log", `{"level":"info","msg":"two","ts":"2026-09-19T16:00:01Z"}`),
		event(3, "status", `{"status":"done"}`),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+testJobID.String()+"/events", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Last-Event-ID", "2")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, `"msg":"one"`) || strings.Contains(body, `"msg":"two"`) {
		t.Fatalf("already-delivered events were replayed:\n%s", body)
	}
	if !strings.Contains(body, "id: 3") {
		t.Fatalf("the missed event was not delivered:\n%s", body)
	}
}

func TestStreamJobEventsResumesFromTheAfterParameter(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.events.events = []dbgen.JobEvent{
		event(10, "log", `{"level":"info","msg":"old","ts":"2026-09-19T16:00:00Z"}`),
		event(11, "status", `{"status":"cancelled"}`),
	}

	rec := streamEvents(t, handler, "/api/v1/jobs/"+testJobID.String()+"/events?after=10")

	body := rec.Body.String()
	if strings.Contains(body, `"msg":"old"`) {
		t.Fatalf("?after= was ignored:\n%s", body)
	}
	if !strings.Contains(body, "id: 11") {
		t.Fatalf("the newer event is missing:\n%s", body)
	}
}

func TestStreamJobEventsForAFinishedJobClosesImmediately(t *testing.T) {
	handler, deps := newTestServer(t)
	// A job that finished before this connection has no terminal event left to send.
	deps.jobs.job.Status = scraper.StatusDone
	deps.events.status = scraper.StatusDone
	deps.events.events = []dbgen.JobEvent{event(1, "progress", `{"queries_done":1}`)}

	rec := streamEvents(t, handler, "/api/v1/jobs/"+testJobID.String()+"/events")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "id: 1") {
		t.Fatalf("history was not replayed:\n%s", rec.Body.String())
	}
}

func TestStreamJobEventsSendsPingsWhileWaiting(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.jobs.job.Status = scraper.StatusRunning
	deps.events.status = scraper.StatusRunning
	deps.events.events = nil

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+testJobID.String()+"/events", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// The ping interval is 50ms in tests, so several comments should have gone out.
	if !strings.Contains(rec.Body.String(), ": ping") {
		t.Fatalf("no keep-alive ping was sent:\n%q", rec.Body.String())
	}
}

func TestStreamJobEventsUnknownJobIsA404(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.jobs.err = apperr.NotFound("job")

	rec := do(t, handler, http.MethodGet, "/api/v1/jobs/"+testJobID.String()+"/events", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("an error before the stream starts must be JSON, got %q", got)
	}
}
