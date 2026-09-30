package cloudflare_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bory/karvon-be/internal/registrar/cloudflare"
)

const account = "0123456789abcdef0123456789abcdef"

func newClient(t *testing.T, handler http.HandlerFunc) *cloudflare.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return cloudflare.New(cloudflare.Config{BaseURL: srv.URL, AccountID: account, Token: "tok"})
}

func writeResult(w http.ResponseWriter, status int, result any, info map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{"success": status < 300, "errors": []any{}, "result": result}
	if info != nil {
		body["result_info"] = info
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeErrors(w http.ResponseWriter, status int, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": false,
		"errors":  []map[string]any{{"code": code, "message": message}},
	})
}

func TestSearchSendsTheQueryAndDecodesPrices(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/accounts/"+account+"/registrar/domain-search" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		q := r.URL.Query()
		if q.Get("q") != "acme corp" || q.Get("limit") != "5" {
			t.Errorf("query = %v", q)
		}
		if exts := q["extensions"]; len(exts) != 2 || exts[0] != "com" || exts[1] != "dev" {
			t.Errorf("extensions = %v", exts)
		}
		writeResult(w, http.StatusOK, map[string]any{"domains": []map[string]any{{
			"name": "acmecorp.dev", "registrable": true, "tier": "standard",
			"pricing": map[string]string{"currency": "USD", "registration_cost": "10.11", "renewal_cost": "12.5"},
		}}}, nil)
	})

	offers, err := client.Search(context.Background(), "acme corp", 5, []string{"com", "dev"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(offers) != 1 || offers[0].Name != "acmecorp.dev" || !offers[0].Registrable ||
		offers[0].Pricing == nil || offers[0].Pricing.RegistrationCost != "10.11" {
		t.Fatalf("offers = %+v", offers)
	}
}

func TestRegisterIsNeverRepeated(t *testing.T) {
	var calls atomic.Int32
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeErrors(w, http.StatusBadGateway, 10000, "upstream hiccup")
	})

	_, err := client.Register(context.Background(), cloudflare.RegisterInput{DomainName: "acmecorp.dev"})
	if err == nil {
		t.Fatal("Register succeeded against a failing server")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Register was sent %d times, want exactly 1", got)
	}
	if cloudflare.Definite(err) {
		t.Fatal("a 502 must not count as a definite refusal: the registration may have happened")
	}
}

func TestReadsAreRetriedOnTransientFailures(t *testing.T) {
	var calls atomic.Int32
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			writeErrors(w, http.StatusServiceUnavailable, 10000, "busy")
			return
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"domains":["a.com"]}` {
			t.Errorf("body = %s", body)
		}
		writeResult(w, http.StatusOK, map[string]any{"domains": []map[string]any{{"name": "a.com", "registrable": false, "reason": "domain_unavailable"}}}, nil)
	})

	offers, err := client.Check(context.Background(), []string{"a.com"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if calls.Load() != 3 || len(offers) != 1 || offers[0].Reason != "domain_unavailable" {
		t.Fatalf("calls = %d, offers = %+v", calls.Load(), offers)
	}
}

func TestCheckRefusesMoreThanTwentyNames(t *testing.T) {
	client := cloudflare.New(cloudflare.Config{BaseURL: "http://127.0.0.1:1", AccountID: account, Token: "tok"})
	names := make([]string, cloudflare.MaxCheckDomains+1)
	for i := range names {
		names[i] = "a.com"
	}
	if _, err := client.Check(context.Background(), names); err == nil {
		t.Fatal("Check accepted 21 names")
	}
}

func TestErrorsMapToSentinels(t *testing.T) {
	cases := []struct {
		status   int
		sentinel error
		definite bool
	}{
		{http.StatusUnauthorized, cloudflare.ErrAuth, true},
		{http.StatusForbidden, cloudflare.ErrAuth, true},
		{http.StatusNotFound, cloudflare.ErrNotFound, true},
		{http.StatusTooManyRequests, cloudflare.ErrRateLimited, false},
		{http.StatusBadRequest, nil, true},
		{http.StatusInternalServerError, nil, false},
	}
	for _, tc := range cases {
		client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			writeErrors(w, tc.status, 1234, "nope")
		})
		_, err := client.Register(context.Background(), cloudflare.RegisterInput{DomainName: "a.com"})
		if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
			t.Errorf("status %d: err = %v, want %v", tc.status, err, tc.sentinel)
		}
		if got := cloudflare.Definite(err); got != tc.definite {
			t.Errorf("status %d: Definite = %v, want %v", tc.status, got, tc.definite)
		}
		var apiErr *cloudflare.APIError
		if !errors.As(err, &apiErr) || apiErr.Detail() != "nope" {
			t.Errorf("status %d: detail lost: %v", tc.status, err)
		}
	}
}

func TestRegisterDecodesTheWorkflow(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["domain_name"] != "acmecorp.dev" || body["auto_renew"] != true {
			t.Errorf("body = %v", body)
		}
		writeResult(w, http.StatusCreated, map[string]any{
			"domain_name": "acmecorp.dev", "state": "succeeded", "completed": true,
			"context": map[string]any{"registration": map[string]any{
				"domain_name": "acmecorp.dev", "status": "active", "auto_renew": true,
				"created_at": "2026-09-27T10:00:00Z", "expires_at": "2027-09-27T10:00:00Z",
			}},
		}, nil)
	})

	wf, err := client.Register(context.Background(), cloudflare.RegisterInput{DomainName: "acmecorp.dev", AutoRenew: true})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if wf.State != cloudflare.StateSucceeded || wf.Context.Registration == nil ||
		wf.Context.Registration.ExpiresAt == nil || wf.Context.Registration.ExpiresAt.Year() != 2027 {
		t.Fatalf("workflow = %+v", wf)
	}
}

func TestWorkflowErrorCodesMayBeStrings(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeResult(w, http.StatusOK, map[string]any{
			"state": "failed", "completed": true,
			"error": map[string]any{"code": "payment_declined", "message": "card declined"},
		}, nil)
	})
	wf, err := client.RegistrationStatus(context.Background(), "a.com")
	if err != nil {
		t.Fatalf("RegistrationStatus: %v", err)
	}
	if wf.Error == nil || wf.Error.Code.String() != "payment_declined" {
		t.Fatalf("workflow error = %+v", wf.Error)
	}
}

func TestListRegistrationsFollowsTheCursor(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			writeResult(w, http.StatusOK, []map[string]any{{"domain_name": "a.com", "status": "active"}},
				map[string]any{"cursor": "next-1", "count": 1, "per_page": 50})
			return
		}
		writeResult(w, http.StatusOK, []map[string]any{{"domain_name": "b.com", "status": "active"}},
			map[string]any{"cursor": "", "count": 1, "per_page": 50})
	})

	first, err := client.ListRegistrations(context.Background(), "", 0)
	if err != nil || first.NextCursor != "next-1" || first.Registrations[0].DomainName != "a.com" {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	second, err := client.ListRegistrations(context.Background(), first.NextCursor, 0)
	if err != nil || second.NextCursor != "" || second.Registrations[0].DomainName != "b.com" {
		t.Fatalf("second page = %+v, %v", second, err)
	}
}

func TestParseCents(t *testing.T) {
	good := map[string]int64{"10.11": 1011, "8": 800, "12.5": 1250, "0.99": 99, " 7.00 ": 700}
	for in, want := range good {
		got, err := cloudflare.ParseCents(in)
		if err != nil || got != want {
			t.Errorf("ParseCents(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-1", "1.234", "abc", "1.", ".5", "1e3", "1,000"} {
		if _, err := cloudflare.ParseCents(in); err == nil {
			t.Errorf("ParseCents(%q) accepted a malformed price", in)
		}
	}
}

func TestTXTValue(t *testing.T) {
	cases := map[string]string{
		`v=spf1 -all`:                        "v=spf1 -all",
		`"v=spf1 -all"`:                      "v=spf1 -all",
		`"v=DKIM1; k=rsa; " "p=MIIB" "IjAN"`: "v=DKIM1; k=rsa; p=MIIBIjAN",
		`"v=DKIM1; k=rsa; "  "p=MIIB"`:       "v=DKIM1; k=rsa; p=MIIB",
		`  google-site-verification=abc  `:   "google-site-verification=abc",
	}
	for in, want := range cases {
		if got := cloudflare.TXTValue(in); got != want {
			t.Errorf("TXTValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIdenticalRecordIsRecognised(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeErrors(w, http.StatusBadRequest, 81058, "An identical record already exists.")
	})
	_, err := client.CreateDNSRecord(context.Background(), "zone", cloudflare.DNSRecord{Type: "TXT", Name: "a.test", Content: "x"})
	if !cloudflare.IsIdenticalRecord(err) {
		t.Fatalf("err = %v, want an identical-record error", err)
	}
}
