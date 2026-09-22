package httpapi_test

import (
	"net/http"
	"testing"
)

func TestAuthenticationIsRequiredOnEveryBusinessRoute(t *testing.T) {
	handler, _ := newTestServer(t)

	routes := []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/api/v1/jobs"},
		{method: http.MethodPost, path: "/api/v1/jobs", body: `{}`},
		{method: http.MethodGet, path: "/api/v1/jobs/" + testJobID.String()},
		{method: http.MethodPost, path: "/api/v1/jobs/" + testJobID.String() + "/cancel"},
		{method: http.MethodDelete, path: "/api/v1/jobs/" + testJobID.String()},
		{method: http.MethodGet, path: "/api/v1/jobs/" + testJobID.String() + "/export.csv"},
		{method: http.MethodGet, path: "/api/v1/jobs/" + testJobID.String() + "/events"},
		{method: http.MethodGet, path: "/api/v1/businesses"},
		{method: http.MethodPost, path: "/api/v1/businesses/bulk", body: `{}`},
		{method: http.MethodPost, path: "/api/v1/businesses/export", body: `{}`},
		{method: http.MethodGet, path: "/api/v1/sources"},
		{method: http.MethodPut, path: "/api/v1/sources/" + testSourceID.String(), body: `{}`},
		{method: http.MethodGet, path: "/api/v1/stats/scraper"},
	}

	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := do(t, handler, route.method, route.path, route.body, withoutKey)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := decodeError(t, rec).Code; got != "unauthorized" {
				t.Fatalf("error code = %q, want unauthorized", got)
			}
		})
	}
}

func TestAuthenticationRejectsAWrongKey(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodGet, "/api/v1/jobs", "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer not-the-key")
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := decodeError(t, rec).Message; got == "" {
		t.Fatal("a 401 should carry a message")
	}
}

func TestAuthenticationRejectsAMalformedHeader(t *testing.T) {
	handler, _ := newTestServer(t)

	for _, header := range []string{"Basic abc", testAPIKey, "Bearer", "Bearer "} {
		rec := do(t, handler, http.MethodGet, "/api/v1/jobs", "", func(r *http.Request) {
			r.Header.Set("Authorization", header)
		})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization: %q gave status %d, want 401", header, rec.Code)
		}
	}
}

func TestAuthenticationAcceptsAQueryParameterForTheEventStream(t *testing.T) {
	// EventSource cannot set headers, so the SSE route also accepts ?api_key=.
	handler, deps := newTestServer(t)
	deps.events.status = "done"
	deps.jobs.job.Status = "done"

	rec := do(t, handler,
		http.MethodGet,
		"/api/v1/jobs/"+testJobID.String()+"/events?api_key="+testAPIKey,
		"", withoutKey)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestPublicRoutesNeedNoKey(t *testing.T) {
	handler, _ := newTestServer(t)

	for _, path := range []string{"/healthz", "/api/v1/healthz", "/api/v1/openapi.json"} {
		rec := do(t, handler, http.MethodGet, path, "", withoutKey)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

func TestHealthzReports503WhenADependencyIsDown(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.health.queueErr = errQueueDown

	rec := do(t, handler, http.MethodGet, "/healthz", "", withoutKey)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	payload := decodeJSONBody[struct {
		Ok    bool `json:"ok"`
		Db    bool `json:"db"`
		Queue bool `json:"queue"`
	}](t, rec)
	if payload.Ok || payload.Queue {
		t.Fatalf("payload = %+v, want ok=false queue=false", payload)
	}
	if !payload.Db {
		t.Error("the database was healthy and should be reported as such")
	}
}

func TestUnknownRouteReturnsTheErrorEnvelope(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodGet, "/api/v1/does-not-exist", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := decodeError(t, rec).Code; got != "not_found" {
		t.Fatalf("code = %q, want not_found", got)
	}
}

func TestCORSPreflightAllowsTheDashboardOrigin(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodOptions, "/api/v1/jobs", "", func(r *http.Request) {
		r.Header.Set("Origin", "http://localhost:5173")
		r.Header.Set("Access-Control-Request-Method", "POST")
		r.Header.Set("Access-Control-Request-Headers", "Authorization,Content-Type")
	})

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("Access-Control-Allow-Origin = %q", got)
	}
}

func TestCORSRejectsAnUnknownOrigin(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodOptions, "/api/v1/jobs", "", func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("Access-Control-Request-Method", "POST")
	})

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want an empty header", got)
	}
}
