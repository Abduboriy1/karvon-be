package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bory/karvon-be/internal/http/middleware"
)

const testKey = "test-api-key"

func newHandler(t *testing.T, auth *middleware.Authenticator) http.Handler {
	t.Helper()
	unauthorized := func(w http.ResponseWriter, _ *http.Request, _ string) {
		w.WriteHeader(http.StatusUnauthorized)
	}
	return auth.Middleware(unauthorized)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func call(t *testing.T, handler http.Handler, path, bearer string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code
}

func TestAnExactPublicPathDoesNotRequireTheApiKey(t *testing.T) {
	handler := newHandler(t, middleware.NewAuthenticator(testKey, "/healthz"))
	if code := call(t, handler, "/healthz", ""); code != http.StatusOK {
		t.Fatalf("GET /healthz without a key = %d, want 200", code)
	}
}

func TestWebhookPathsDoNotRequireTheApiKey(t *testing.T) {
	// Neither provider can send our API key, so the whole webhook subtree is
	// exempt and each handler verifies the provider's own credentials instead.
	auth := middleware.NewAuthenticator(testKey, "/healthz").PublicPrefix("/api/v1/webhooks/")
	handler := newHandler(t, auth)

	for _, path := range []string{
		"/api/v1/webhooks/instantly/abc123",
		"/api/v1/webhooks/mailchimp/def456",
	} {
		if code := call(t, handler, path, ""); code != http.StatusOK {
			t.Errorf("GET %s without a key = %d, want 200", path, code)
		}
	}
}

func TestAPathOutsideTheWebhookPrefixStillRequiresTheApiKey(t *testing.T) {
	auth := middleware.NewAuthenticator(testKey, "/healthz").PublicPrefix("/api/v1/webhooks/")
	handler := newHandler(t, auth)

	if code := call(t, handler, "/api/v1/campaigns", ""); code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/campaigns without a key = %d, want 401", code)
	}
	// A path that merely starts with the same letters is not inside the subtree.
	if code := call(t, handler, "/api/v1/webhooksomething", ""); code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/webhooksomething without a key = %d, want 401", code)
	}
	if code := call(t, handler, "/api/v1/campaigns", testKey); code != http.StatusOK {
		t.Fatalf("GET /api/v1/campaigns with the key = %d, want 200", code)
	}
}

func TestAWrongApiKeyIsRejected(t *testing.T) {
	handler := newHandler(t, middleware.NewAuthenticator(testKey))
	if code := call(t, handler, "/api/v1/campaigns", "not-the-key"); code != http.StatusUnauthorized {
		t.Fatalf("a wrong key = %d, want 401", code)
	}
}
