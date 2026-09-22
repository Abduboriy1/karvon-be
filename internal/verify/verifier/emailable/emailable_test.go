package emailable_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/verify/verifier"
	"github.com/bory/karvon-be/internal/verify/verifier/emailable"
)

// newClient wires a client to a test server, so no test reaches the real vendor.
func newClient(t *testing.T, handler http.HandlerFunc) *emailable.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return emailable.New(verifier.Options{
		APIKey:     "test-key",
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
	})
}

func TestVerifyMapsEveryVendorState(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantStatus  verifier.Status
		wantCredits int
	}{
		{
			name:        "deliverable",
			body:        `{"state":"deliverable","reason":"accepted_email","accept_all":false}`,
			wantStatus:  verifier.StatusDeliverable,
			wantCredits: 1,
		},
		{
			name:        "catch-all is risky",
			body:        `{"state":"deliverable","reason":"accepted_email","accept_all":true}`,
			wantStatus:  verifier.StatusRisky,
			wantCredits: 1,
		},
		{
			name:        "risky",
			body:        `{"state":"risky","reason":"low_quality"}`,
			wantStatus:  verifier.StatusRisky,
			wantCredits: 1,
		},
		{
			name:        "undeliverable",
			body:        `{"state":"undeliverable","reason":"rejected_email"}`,
			wantStatus:  verifier.StatusUndeliverable,
			wantCredits: 1,
		},
		{
			name:        "unknown is never billed",
			body:        `{"state":"unknown","reason":"timeout"}`,
			wantStatus:  verifier.StatusUnknown,
			wantCredits: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Query().Get("email"); got != "jane@ironworksgym.com" {
					t.Errorf("email parameter = %q", got)
				}
				if got := r.URL.Query().Get("api_key"); got != "test-key" {
					t.Errorf("api_key parameter = %q", got)
				}
				_, _ = w.Write([]byte(tt.body))
			})

			result, err := client.Verify(context.Background(), "jane@ironworksgym.com")
			if err != nil {
				t.Fatalf("Verify returned %v", err)
			}
			if result.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", result.Status, tt.wantStatus)
			}
			if result.CreditsUsed != tt.wantCredits {
				t.Errorf("credits = %d, want %d", result.CreditsUsed, tt.wantCredits)
			}
			if result.Reason == "" {
				t.Error("the vendor reason was dropped")
			}
			// The raw payload is kept verbatim so a disputed verdict is auditable.
			if string(result.Raw) != tt.body {
				t.Errorf("raw payload = %s, want the original body %s", result.Raw, tt.body)
			}
		})
	}
}

func TestVerifyClassifiesErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		headers map[string]string
		wantErr error
	}{
		{name: "bad key", status: http.StatusUnauthorized, wantErr: verifier.ErrAuth},
		{name: "forbidden", status: http.StatusForbidden, wantErr: verifier.ErrAuth},
		{name: "out of credit", status: http.StatusPaymentRequired, wantErr: verifier.ErrInsufficientCredits},
		{
			name:   "out of credit reported as a message",
			status: http.StatusBadRequest,
			body:   `{"message":"Insufficient credits available"}`, wantErr: verifier.ErrInsufficientCredits,
		},
		{
			name:    "throttled",
			status:  http.StatusTooManyRequests,
			headers: map[string]string{"Retry-After": "12"},
			wantErr: verifier.ErrRateLimited,
		},
		{name: "not ready yet", status: 249, body: `{"message":"pending"}`, wantErr: verifier.ErrPending},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
				for key, value := range tt.headers {
					w.Header().Set(key, value)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})

			_, err := client.Verify(context.Background(), "jane@ironworksgym.com")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// A throttle carries the vendor's own wait time, so the queue snoozes for exactly
// as long as it was asked to.
func TestRateLimitCarriesRetryAfter(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := client.Verify(context.Background(), "jane@ironworksgym.com")
	wait, ok := verifier.RetryAfter(err)
	if !ok {
		t.Fatalf("no retry hint on %v", err)
	}
	if wait != 45*time.Second {
		t.Fatalf("retry after = %s, want 45s", wait)
	}
}

func TestBalanceReadsRemainingCredits(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/account" {
			t.Errorf("path = %q, want /v1/account", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"available_credits":4321}`))
	})

	balance, err := client.Balance(context.Background())
	if err != nil {
		t.Fatalf("Balance returned %v", err)
	}
	if balance.Credits != 4321 {
		t.Fatalf("credits = %d, want 4321", balance.Credits)
	}
}

func TestBalanceRejectsABadKey(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	if _, err := client.Balance(context.Background()); !errors.Is(err, verifier.ErrAuth) {
		t.Fatalf("error = %v, want %v", err, verifier.ErrAuth)
	}
}
