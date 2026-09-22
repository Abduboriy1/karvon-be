package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsStreamingSkipsLongLivedRoutes(t *testing.T) {
	streaming := []string{
		"/api/v1/jobs/01a0/events",
		"/api/v1/jobs/01a0/export.csv",
		"/api/v1/businesses/export",
	}
	for _, path := range streaming {
		if !IsStreaming(path) {
			t.Errorf("IsStreaming(%q) = false, want true", path)
		}
	}

	ordinary := []string{"/api/v1/jobs", "/api/v1/businesses", "/healthz"}
	for _, path := range ordinary {
		if IsStreaming(path) {
			t.Errorf("IsStreaming(%q) = true, want false", path)
		}
	}
}

func TestTimeoutBoundsOrdinaryRequests(t *testing.T) {
	var gotDeadline bool
	handler := Timeout(50 * time.Millisecond)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, gotDeadline = r.Context().Deadline()
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !gotDeadline {
		t.Fatal("an ordinary request should carry a deadline")
	}
}

func TestTimeoutLeavesStreamingRoutesAlone(t *testing.T) {
	var gotDeadline bool
	handler := Timeout(50 * time.Millisecond)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, gotDeadline = r.Context().Deadline()
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/01a0/events", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotDeadline {
		t.Fatal("an SSE stream must not inherit the request timeout")
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
		query  string
		want   string
		ok     bool
	}{
		{name: "bearer header", header: "Bearer abc", want: "abc", ok: true},
		{name: "case insensitive scheme", header: "bearer abc", want: "abc", ok: true},
		{name: "query fallback", query: "abc", want: "abc", ok: true},
		{name: "header wins over query", header: "Bearer from-header", query: "from-query", want: "from-header", ok: true},
		{name: "wrong scheme", header: "Basic abc", ok: false},
		{name: "no credentials", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := "/api/v1/jobs"
			if tt.query != "" {
				target += "?api_key=" + tt.query
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}

			token, ok := bearerToken(req)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && token != tt.want {
				t.Fatalf("token = %q, want %q", token, tt.want)
			}
		})
	}
}

func TestLoggerFromFallsBackToTheDefaultLogger(t *testing.T) {
	if LoggerFrom(context.Background()) == nil {
		t.Fatal("LoggerFrom should never return nil")
	}
}
