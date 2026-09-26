package reacher

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/verify/provider"
)

// newTestClient points a client at a test server with the breaker off, which is the
// right default for tests about a single call.
func newTestClient(t *testing.T, handler http.HandlerFunc, tweak func(*Config)) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	cfg := Config{
		BaseURL:    server.URL,
		Timeout:    2 * time.Second,
		Retries:    0,
		HTTPClient: server.Client(),
		Log:        discardLogger(),
	}
	if tweak != nil {
		tweak(&cfg)
	}
	return New(cfg), server
}

// respondWith serves one fixed CheckEmailOutput.
func respondWith(t *testing.T, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/check_email" {
			t.Errorf("request path = %q, want /v1/check_email", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

/* --------------------------------------------------------------- normalization */

func TestNormalize(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus provider.Status
		wantScore  int
		wantReason string
	}{
		{
			name:       "the mail server accepted the recipient",
			body:       `{"input":"a@b.com","is_reachable":"safe","smtp":{"can_connect_smtp":true,"is_deliverable":true},"mx":{"accepts_mail":true},"syntax":{"is_valid_syntax":true}}`,
			wantStatus: provider.StatusScored,
			wantScore:  ScoreSafe,
			wantReason: "accepted the recipient",
		},
		{
			name:       "a plain risky verdict",
			body:       `{"is_reachable":"risky","smtp":{"can_connect_smtp":true},"mx":{"accepts_mail":true},"syntax":{"is_valid_syntax":true}}`,
			wantStatus: provider.StatusScored,
			wantScore:  ScoreRisky,
		},
		{
			name:       "a catch-all domain is worth less than a plain risky verdict",
			body:       `{"is_reachable":"risky","smtp":{"can_connect_smtp":true,"is_catch_all":true},"mx":{"accepts_mail":true},"syntax":{"is_valid_syntax":true}}`,
			wantStatus: provider.StatusScored,
			wantScore:  ScoreCatchAll,
			wantReason: "accepts every address",
		},
		{
			name:       "a full inbox is a real mailbox that cannot receive",
			body:       `{"is_reachable":"risky","smtp":{"can_connect_smtp":true,"has_full_inbox":true},"mx":{"accepts_mail":true},"syntax":{"is_valid_syntax":true}}`,
			wantStatus: provider.StatusScored,
			wantScore:  ScoreFullInbox,
			wantReason: "inbox is full",
		},
		{
			name:       "a role account the server will not vouch for",
			body:       `{"is_reachable":"risky","smtp":{"can_connect_smtp":true},"misc":{"is_role_account":true},"mx":{"accepts_mail":true},"syntax":{"is_valid_syntax":true}}`,
			wantStatus: provider.StatusScored,
			wantScore:  ScoreRisky,
			wantReason: "role mailbox",
		},
		{
			name:       "the mailbox does not exist",
			body:       `{"is_reachable":"invalid","smtp":{"can_connect_smtp":true},"mx":{"accepts_mail":true},"syntax":{"is_valid_syntax":true}}`,
			wantStatus: provider.StatusScored,
			wantScore:  ScoreInvalid,
			wantReason: "rejected the recipient",
		},
		{
			name:       "the domain does not accept mail",
			body:       `{"is_reachable":"invalid","mx":{"accepts_mail":false},"syntax":{"is_valid_syntax":true}}`,
			wantStatus: provider.StatusScored,
			wantScore:  ScoreInvalid,
			wantReason: "does not accept mail",
		},
		{
			name:       "bad syntax",
			body:       `{"is_reachable":"invalid","mx":{"accepts_mail":false},"syntax":{"is_valid_syntax":false}}`,
			wantStatus: provider.StatusScored,
			wantScore:  ScoreInvalid,
			wantReason: "not valid syntax",
		},
		{
			name:       "a disabled mailbox",
			body:       `{"is_reachable":"invalid","smtp":{"can_connect_smtp":true,"is_disabled":true},"mx":{"accepts_mail":true},"syntax":{"is_valid_syntax":true}}`,
			wantStatus: provider.StatusScored,
			wantScore:  ScoreInvalid,
			wantReason: "disabled",
		},
		{
			name:       "a disposable domain is disqualifying whatever SMTP said",
			body:       `{"is_reachable":"safe","misc":{"is_disposable":true},"smtp":{"can_connect_smtp":true,"is_deliverable":true},"mx":{"accepts_mail":true},"syntax":{"is_valid_syntax":true}}`,
			wantStatus: provider.StatusScored,
			wantScore:  ScoreInvalid,
			wantReason: "disposable",
		},
		{
			name:       "unknown is a declined answer, not a bad one",
			body:       `{"is_reachable":"unknown","smtp":{"can_connect_smtp":false},"mx":{"accepts_mail":true},"syntax":{"is_valid_syntax":true}}`,
			wantStatus: provider.StatusInconclusive,
		},
		{
			name:       "a verdict we do not recognise is inconclusive rather than zero",
			body:       `{"is_reachable":"something_new","mx":{"accepts_mail":true},"syntax":{"is_valid_syntax":true}}`,
			wantStatus: provider.StatusInconclusive,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newTestClient(t, respondWith(t, tc.body), nil)
			result := client.Check(context.Background(), "someone@example.com")

			if result.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (reason: %q, error: %q)",
					result.Status, tc.wantStatus, result.Reason, result.Error)
			}
			if tc.wantStatus == provider.StatusScored && result.Score != tc.wantScore {
				t.Errorf("score = %d, want %d", result.Score, tc.wantScore)
			}
			if tc.wantReason != "" && !strings.Contains(result.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", result.Reason, tc.wantReason)
			}
			if result.Provider != provider.KeyReacher {
				t.Errorf("provider = %q, want %q", result.Provider, provider.KeyReacher)
			}
		})
	}
}

// An inconclusive result must never carry a score, or the scoring layer would be
// tempted to use it.
func TestNormalizeInconclusiveHasNoScore(t *testing.T) {
	client, _ := newTestClient(t, respondWith(t, `{"is_reachable":"unknown"}`), nil)
	result := client.Check(context.Background(), "a@example.com")
	if result.Score != 0 {
		t.Errorf("an inconclusive result carries score %d, want 0", result.Score)
	}
	if result.Status.Contributes() {
		t.Error("an inconclusive result claims to contribute to the weighted score")
	}
}

/* ------------------------------------------------------------------- the wire */

func TestCheckSendsTheSecretHeader(t *testing.T) {
	var got string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(SecretHeader)
		_, _ = io.WriteString(w, `{"is_reachable":"safe"}`)
	}, func(c *Config) { c.Secret = "s3cret" })

	client.Check(context.Background(), "a@example.com")
	if got != "s3cret" {
		t.Errorf("%s header = %q, want the configured secret", SecretHeader, got)
	}
}

func TestCheckOmitsTheSecretHeaderWhenUnset(t *testing.T) {
	var present bool
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header[http.CanonicalHeaderKey(SecretHeader)]
		_, _ = io.WriteString(w, `{"is_reachable":"safe"}`)
	}, nil)

	client.Check(context.Background(), "a@example.com")
	if present {
		t.Error("the secret header was sent although no secret is configured")
	}
}

func TestCheckSendsTheAddress(t *testing.T) {
	var body checkRequest
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"is_reachable":"safe"}`)
	}, func(c *Config) {
		c.HelloName = "karvon.example"
		c.FromEmail = "hello@karvon.example"
	})

	client.Check(context.Background(), "someone@example.com")
	if body.ToEmail != "someone@example.com" {
		t.Errorf("to_email = %q, want the address under test", body.ToEmail)
	}
	if body.HelloName != "karvon.example" || body.FromEmail != "hello@karvon.example" {
		t.Errorf("the SMTP identity was not forwarded: %+v", body)
	}
}

/* ------------------------------------------------------------------- failures */

// The contract of the whole design: nothing Reacher does may produce an error the
// pipeline has to handle. Every failure is a result that contributes nothing.
func TestCheckNeverFailsThePipeline(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantStatus provider.Status
	}{
		{
			name: "a server error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":"boom"}`)
			},
			wantStatus: provider.StatusError,
		},
		{
			name: "throttled",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":"too many requests"}`)
			},
			wantStatus: provider.StatusUnavailable,
		},
		{
			name: "rejected for a bad secret",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
			},
			wantStatus: provider.StatusError,
		},
		{
			name: "a body that is not JSON",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `<html>not json</html>`)
			},
			wantStatus: provider.StatusError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newTestClient(t, tc.handler, nil)
			result := client.Check(context.Background(), "a@example.com")

			if result.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", result.Status, tc.wantStatus)
			}
			if result.Status.Contributes() {
				t.Error("a failure claims to contribute to the weighted score")
			}
			if result.Error == "" {
				t.Error("the failure was not explained")
			}
		})
	}
}

// A backend that never answers must be given up on at the timeout rather than
// holding a worker open.
func TestCheckTimesOut(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	client, _ := newTestClient(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}, func(c *Config) { c.Timeout = 150 * time.Millisecond })

	started := time.Now()
	result := client.Check(context.Background(), "a@example.com")
	elapsed := time.Since(started)

	if result.Status != provider.StatusUnavailable {
		t.Fatalf("status = %q, want %q", result.Status, provider.StatusUnavailable)
	}
	if !strings.Contains(result.Reason, "timeout") {
		t.Errorf("reason = %q, want it to mention the timeout", result.Reason)
	}
	if elapsed > 2*time.Second {
		t.Errorf("the call took %s; the timeout is 150ms", elapsed)
	}
}

// A connection that is refused outright — the container is not running — is the most
// common outage and must behave like any other.
func TestCheckWithNoBackend(t *testing.T) {
	client := New(Config{
		BaseURL: "http://127.0.0.1:1", // a port nothing listens on
		Timeout: time.Second,
		Log:     discardLogger(),
	})
	result := client.Check(context.Background(), "a@example.com")
	if result.Status.Contributes() {
		t.Fatalf("status = %q; an unreachable backend must contribute nothing", result.Status)
	}
	if result.Error == "" {
		t.Error("the connection failure was not explained")
	}
}

/* -------------------------------------------------------------------- retries */

func TestCheckRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `{"is_reachable":"safe"}`)
	}, func(c *Config) { c.Retries = 2 })

	result := client.Check(context.Background(), "a@example.com")
	if result.Status != provider.StatusScored || result.Score != ScoreSafe {
		t.Fatalf("status = %q score = %d, want a successful retry", result.Status, result.Score)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("the backend was called %d times, want 3", got)
	}
}

// A 4xx that is not a throttle will fail identically every time, so retrying it only
// wastes the address's timeout budget.
func TestCheckDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}, func(c *Config) { c.Retries = 3 })

	client.Check(context.Background(), "a@example.com")
	if got := calls.Load(); got != 1 {
		t.Errorf("a 400 was attempted %d times, want 1", got)
	}
}

// A 429 means the backend's per-minute quota is spent. Retrying within the backoff
// only spends more of it, so the address is given up on at once.
func TestCheckDoesNotRetryAThrottle(t *testing.T) {
	var calls atomic.Int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}, func(c *Config) { c.Retries = 3 })

	result := client.Check(context.Background(), "a@example.com")
	if result.Status != provider.StatusUnavailable {
		t.Errorf("status = %q, want %q", result.Status, provider.StatusUnavailable)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("a 429 was attempted %d times, want 1", got)
	}
}

// The client must pace itself under the backend's throttle rather than discover it
// through 429s.
func TestCheckPacesRequests(t *testing.T) {
	var calls atomic.Int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"is_reachable":"safe"}`)
	}, func(c *Config) {
		c.RatePerMinute = 600 // one every 100ms
		c.Concurrency = 8
	})

	started := time.Now()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { client.Check(context.Background(), "a@example.com") })
	}
	wg.Wait()

	if got := calls.Load(); got != 4 {
		t.Fatalf("the backend was called %d times, want 4", got)
	}
	// The first request goes at once, the other three wait 100ms each.
	if elapsed := time.Since(started); elapsed < 250*time.Millisecond {
		t.Errorf("4 requests at 600/min finished in %s, want at least 300ms", elapsed)
	}
}

// Timing out while waiting on our own rate limit says nothing about the backend's
// health, so it must not open the breaker.
func TestRateLimitWaitDoesNotTripTheBreaker(t *testing.T) {
	var calls atomic.Int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"is_reachable":"safe"}`)
	}, func(c *Config) {
		c.RatePerMinute = 1 // one a minute: every call after the first must wait
		c.Timeout = 50 * time.Millisecond
		c.BreakerThreshold = 2
		c.BreakerCooldown = time.Hour
	})

	client.Check(context.Background(), "a@example.com")
	for i := range 5 {
		result := client.Check(context.Background(), "a@example.com")
		if result.Status != provider.StatusUnavailable {
			t.Fatalf("call %d: status = %q, want %q", i, result.Status, provider.StatusUnavailable)
		}
		if errors.Is(result.Err, provider.ErrPaused) {
			t.Fatalf("call %d: waiting on the local rate limit opened the breaker", i)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("the backend was called %d times, want 1", got)
	}
}

/* -------------------------------------------------------------------- breaker */

// A backend that is down must cost one connection attempt per cooldown, not one per
// address: a bulk run of fifty thousand addresses against a dead Reacher would
// otherwise take its timeout fifty thousand times.
func TestBreakerStopsCallingADeadBackend(t *testing.T) {
	var calls atomic.Int32
	now := time.Now()
	var mu sync.Mutex

	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}, func(c *Config) {
		c.BreakerThreshold = 3
		c.BreakerCooldown = time.Minute
		c.Now = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}
	})

	for i := range 10 {
		result := client.Check(context.Background(), "a@example.com")
		if result.Status.Contributes() {
			t.Fatalf("call %d contributed to the score although the backend is failing", i)
		}
		if paused := errors.Is(result.Err, provider.ErrPaused); paused != (i >= 3) {
			t.Errorf("call %d: paused = %v, want %v", i, paused, i >= 3)
		}
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("the dead backend was called %d times, want 3 before the circuit opened", got)
	}

	// Once the cooldown has passed, it is worth trying again.
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()

	client.Check(context.Background(), "a@example.com")
	if got := calls.Load(); got != 4 {
		t.Errorf("the backend was called %d times after the cooldown, want 4", got)
	}
}

// A success must clear the failure count, or a backend that fails intermittently
// would eventually trip the breaker on unrelated failures.
func TestBreakerResetsOnSuccess(t *testing.T) {
	var calls atomic.Int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		// Fail, fail, succeed, repeated.
		if calls.Add(1)%3 != 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"is_reachable":"safe"}`)
	}, func(c *Config) {
		c.BreakerThreshold = 3
		c.BreakerCooldown = time.Hour
	})

	for range 9 {
		client.Check(context.Background(), "a@example.com")
	}
	if got := calls.Load(); got != 9 {
		t.Errorf("the backend was called %d times, want 9: a success must reset the breaker", got)
	}
}

/* -------------------------------------------------------------- concurrency */

// The semaphore is what stops us outrunning the backend's own throttle.
func TestCheckBoundsConcurrency(t *testing.T) {
	var inFlight, peak atomic.Int32

	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		current := inFlight.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		_, _ = io.WriteString(w, `{"is_reachable":"safe"}`)
	}, func(c *Config) {
		c.Concurrency = 2
		c.Timeout = 5 * time.Second
	})

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client.Check(context.Background(), "a@example.com")
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > 2 {
		t.Errorf("peak in-flight requests = %d, want at most the configured 2", got)
	}
}

/* ----------------------------------------------------------------- health */

func TestHealth(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			t.Errorf("health path = %q, want /version", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"version":"0.11.7"}`)
	}, nil)

	if err := client.Health(context.Background()); err != nil {
		t.Errorf("Health() = %v, want nil", err)
	}
}

func TestHealthReportsAFailure(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}, nil)

	if err := client.Health(context.Background()); err == nil {
		t.Error("Health() = nil, want an error for a 503")
	}
}

// discardLogger keeps the provider's warnings out of the test output; the tests
// assert on results, not on logs.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
