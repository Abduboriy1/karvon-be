package instantly_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly/fake"
)

// newClient points a client with no retries and no breaker at the test server.
func newClient(t *testing.T, server *httptest.Server, cfg instantly.Config) *instantly.HTTPClient {
	t.Helper()
	cfg.BaseURL = server.URL
	if cfg.APIKey == "" {
		cfg.APIKey = "test-key"
	}
	if cfg.Retries == 0 {
		cfg.Retries = -1
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	return instantly.New(cfg)
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode request body %q: %v", raw, err)
	}
	return body
}

func TestClientRetriesA503ThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "upstream hiccup", http.StatusServiceUnavailable)
			return
		}
		writeJSON(t, w, http.StatusOK, instantly.Campaign{ID: "c1", Name: "Q3", Status: 1})
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{Retries: 3})
	campaign, err := client.GetCampaign(context.Background(), "c1")
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}
	if campaign.Name != "Q3" {
		t.Fatalf("campaign name = %q, want Q3", campaign.Name)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("server saw %d calls, want 2", got)
	}
}

func TestClientSnoozesOn429WithRetryAfter(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "7")
		http.Error(w, `{"error":"slow down"}`, http.StatusTooManyRequests)
	}))
	defer server.Close()

	// Retries are on, but a 7s wait exceeds the in-call backoff cap, so the
	// error surfaces at once and the job snoozes for the full wait.
	client := newClient(t, server, instantly.Config{Retries: 2})
	_, err := client.ListLeads(context.Background(), instantly.ListLeadsInput{CampaignID: "c1"})
	if !errors.Is(err, provider.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	after, ok := provider.RetryAfter(err)
	if !ok || after != 7*time.Second {
		t.Fatalf("RetryAfter = %v (%v), want 7s", after, ok)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("server saw %d calls, want 1", got)
	}
	if !provider.Retryable(err) {
		t.Fatal("a rate limit should still be retryable by the queue")
	}
}

func TestClientMapsA401ToErrAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{})
	_, err := client.Ping(context.Background())
	if !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if provider.Retryable(err) {
		t.Fatal("an auth failure must not be retried")
	}
	if !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("err = %q, want the response body in the message", err)
	}
}

func TestClientMapsA402ToPaymentRequired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no active plan", http.StatusPaymentRequired)
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{})
	_, err := client.Ping(context.Background())
	if !errors.Is(err, provider.ErrPaymentRequired) {
		t.Fatalf("err = %v, want ErrPaymentRequired", err)
	}
}

func TestClientMapsA404ToNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/leads/missing" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		http.Error(w, "", http.StatusNotFound)
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{})
	err := client.DeleteLead(context.Background(), "missing")
	if !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestClientMapsA400ToInvalidWithTheBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"email_list must not be empty"}`, http.StatusBadRequest)
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{})
	_, err := client.CreateCampaign(context.Background(), instantly.CreateCampaignInput{Name: "x"})
	if !errors.Is(err, provider.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "email_list must not be empty") {
		t.Fatalf("err = %q, want the response body in the message", err)
	}
}

func TestClientMapsAnUnknownStatusToStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "teapot", http.StatusTeapot)
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{})
	_, err := client.Ping(context.Background())
	var status *provider.StatusError
	if !errors.As(err, &status) {
		t.Fatalf("err = %v, want *StatusError", err)
	}
	if status.Provider != "instantly" || status.Code != http.StatusTeapot {
		t.Fatalf("status = %+v, want instantly/418", status)
	}
}

func TestClientCapsTheResponseBody(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// Valid JSON, just far too much of it: 300 * 4 KiB is past the 1 MiB cap.
		_, _ = io.WriteString(w, `{"items":[],"next_starting_after":"","padding":"`)
		chunk := strings.Repeat("a", 4096)
		for range 300 {
			_, _ = io.WriteString(w, chunk)
		}
		_, _ = io.WriteString(w, `"}`)
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{Retries: 3})
	_, err := client.ListAccounts(context.Background(), "")
	if !errors.Is(err, instantly.ErrResponseTooLarge) {
		t.Fatalf("err = %v, want ErrResponseTooLarge", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("server saw %d calls, want 1: an oversized body must not be re-fetched", got)
	}
}

func TestClientOpensTheBreakerAfterRepeatedFailures(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer server.Close()

	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	client := newClient(t, server, instantly.Config{
		BreakerThreshold: 2,
		BreakerCooldown:  time.Minute,
		Now:              func() time.Time { return now },
	})

	for i := range 2 {
		if _, err := client.Ping(context.Background()); err == nil {
			t.Fatalf("call %d: expected a failure", i+1)
		}
	}
	_, err := client.Ping(context.Background())
	if !errors.Is(err, provider.ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("server saw %d calls, want 2: the open circuit must short-circuit", got)
	}

	now = now.Add(2 * time.Minute)
	_, err = client.Ping(context.Background())
	if errors.Is(err, provider.ErrCircuitOpen) {
		t.Fatalf("err = %v: the circuit should have closed after the cooldown", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("server saw %d calls, want 3", got)
	}
}

func TestClientDoesNotOpenTheBreakerOnClientErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "", http.StatusNotFound)
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{BreakerThreshold: 1})
	for range 3 {
		err := client.DeleteLead(context.Background(), "gone")
		if !errors.Is(err, provider.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound every time", err)
		}
	}
}

func TestAddLeadsSendsSkipIfInCampaignAndCustomVariables(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/leads/add" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("Authorization = %q, want a bearer token", auth)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		got = decodeBody(t, r)
		writeJSON(t, w, http.StatusOK, map[string]any{
			"status": "success", "total_sent": 1, "leads_uploaded": 1,
			"created_leads": []map[string]any{{"id": "l1", "email": "ann@example.com"}},
		})
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{})
	result, err := client.AddLeads(context.Background(), instantly.AddLeadsInput{
		CampaignID:       "c1",
		SkipIfInCampaign: true,
		Leads: []instantly.LeadInput{{
			Email:           "ann@example.com",
			FirstName:       "Ann",
			CustomVariables: map[string]any{"city": "Berlin", "score": 42},
		}},
	})
	if err != nil {
		t.Fatalf("AddLeads: %v", err)
	}
	if len(result.CreatedLeads) != 1 || result.CreatedLeads[0].ID != "l1" {
		t.Fatalf("created = %+v, want l1", result.CreatedLeads)
	}

	if got["campaign_id"] != "c1" {
		t.Errorf("campaign_id = %v", got["campaign_id"])
	}
	if got["skip_if_in_campaign"] != true {
		t.Errorf("skip_if_in_campaign = %v, want true", got["skip_if_in_campaign"])
	}
	leads, _ := got["leads"].([]any)
	if len(leads) != 1 {
		t.Fatalf("leads = %v, want one", got["leads"])
	}
	lead, _ := leads[0].(map[string]any)
	if lead["first_name"] != "Ann" {
		t.Errorf("first_name = %v", lead["first_name"])
	}
	vars, _ := lead["custom_variables"].(map[string]any)
	if vars["city"] != "Berlin" || vars["score"] != float64(42) {
		t.Errorf("custom_variables = %v", lead["custom_variables"])
	}
	if _, present := lead["phone"]; present {
		t.Errorf("empty optional fields must be omitted, got %v", lead)
	}
}

func TestCreateCampaignOverlaysSettingsAtTopLevel(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/campaigns" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		got = decodeBody(t, r)
		writeJSON(t, w, http.StatusOK, map[string]any{
			"id": "c9", "name": got["name"], "status": 0,
			"timestamp_created": "2026-09-20T10:00:00Z", "timestamp_updated": "2026-09-20T10:00:00Z",
		})
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{})
	campaign, err := client.CreateCampaign(context.Background(), instantly.CreateCampaignInput{
		Name: "Launch",
		Schedule: instantly.Schedule{Schedules: []instantly.ScheduleWindow{{
			Name:     "weekdays",
			Timing:   map[string]any{"from": "09:00", "to": "17:00"},
			Days:     map[string]bool{"1": true, "2": true},
			Timezone: "Europe/Berlin",
		}}},
		Sequences: []instantly.Sequence{{Steps: []instantly.Step{{
			Type: "email", Delay: 0, Variants: []instantly.Variant{{Subject: "Hi", Body: "Hello"}},
		}}}},
		EmailList: []string{"me@example.com"},
		Settings:  map[string]any{"daily_limit": 50, "stop_on_reply": true, "name": "Overridden"},
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if campaign.ID != "c9" || campaign.TimestampCreated.IsZero() {
		t.Fatalf("campaign = %+v", campaign)
	}

	if got["daily_limit"] != float64(50) {
		t.Errorf("daily_limit = %v, want 50 at the top level", got["daily_limit"])
	}
	if got["stop_on_reply"] != true {
		t.Errorf("stop_on_reply = %v, want true", got["stop_on_reply"])
	}
	if got["name"] != "Overridden" {
		t.Errorf("name = %v: settings win over struct fields", got["name"])
	}
	if _, present := got["Settings"]; present {
		t.Error("the Settings map itself must not be serialised")
	}
	schedule, _ := got["campaign_schedule"].(map[string]any)
	windows, _ := schedule["schedules"].([]any)
	if len(windows) != 1 {
		t.Errorf("campaign_schedule = %v", got["campaign_schedule"])
	}
	if _, present := schedule["start_date"]; present {
		t.Error("a nil start_date must be omitted")
	}
	list, _ := got["email_list"].([]any)
	if len(list) != 1 || list[0] != "me@example.com" {
		t.Errorf("email_list = %v", got["email_list"])
	}
}

func TestUpdateCampaignPatchesOnlyWhatIsSet(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/campaigns/c1" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		got = decodeBody(t, r)
		writeJSON(t, w, http.StatusOK, map[string]any{"id": "c1", "status": 2})
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{})
	_, err := client.UpdateCampaign(context.Background(), "c1", instantly.UpdateCampaignInput{
		Settings: map[string]any{"daily_limit": 10},
	})
	if err != nil {
		t.Fatalf("UpdateCampaign: %v", err)
	}
	if len(got) != 1 || got["daily_limit"] != float64(10) {
		t.Fatalf("body = %v, want only daily_limit", got)
	}
}

func TestListAccountsPaginatesWithStartingAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if limit := r.URL.Query().Get("limit"); limit != "100" {
			t.Errorf("limit = %q, want 100", limit)
		}
		switch r.URL.Query().Get("starting_after") {
		case "":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"items": []map[string]any{{
					"email": "a@example.com", "status": 1, "provider_code": 2,
					"warmup_status": 1, "daily_limit": 30, "stat_warmup_score": 98,
					"timestamp_created":  "2026-01-02T03:04:05Z",
					"undocumented_field": "kept",
				}},
				"next_starting_after": "a@example.com",
			})
		case "a@example.com":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"items":               []map[string]any{{"email": "b@example.com", "status": -1}},
				"next_starting_after": nil,
			})
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("starting_after"))
		}
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{})
	first, err := client.ListAccounts(context.Background(), "")
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(first.Items) != 1 || first.NextStartingAfter != "a@example.com" {
		t.Fatalf("first page = %+v", first)
	}
	account := first.Items[0]
	if account.ProviderCode == nil || *account.ProviderCode != 2 || account.WarmupScore == nil || *account.WarmupScore != 98 {
		t.Errorf("account = %+v: typed fields not decoded", account)
	}
	if account.TimestampCreated == nil || account.TimestampCreated.Year() != 2026 {
		t.Errorf("timestamp_created = %v", account.TimestampCreated)
	}
	if account.Raw["undocumented_field"] != "kept" {
		t.Errorf("Raw = %v, want the whole object", account.Raw)
	}

	second, err := client.ListAccounts(context.Background(), first.NextStartingAfter)
	if err != nil {
		t.Fatalf("ListAccounts(page 2): %v", err)
	}
	if len(second.Items) != 1 || second.Items[0].Email != "b@example.com" || second.NextStartingAfter != "" {
		t.Fatalf("second page = %+v", second)
	}
}

func TestPingListsOneAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts" || r.URL.Query().Get("limit") != "1" {
			t.Errorf("unexpected request %s", r.URL.String())
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"items": []map[string]any{{"email": "a@example.com"}}})
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{})
	workspace, err := client.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if workspace.AccountsCount != 1 {
		t.Fatalf("AccountsCount = %d, want 1", workspace.AccountsCount)
	}
}

func TestAnalyticsEncodeRepeatedQueryKeysAndDates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/campaigns/analytics":
			if ids := q["ids"]; len(ids) != 2 || ids[0] != "c1" || ids[1] != "c2" {
				t.Errorf("ids = %v", ids)
			}
			writeJSON(t, w, http.StatusOK, []map[string]any{{"campaign_id": "c1"}, {"campaign_id": "c2"}})
		case "/accounts/analytics/daily":
			if q.Get("start_date") != "2026-09-01" || q.Get("end_date") != "2026-09-20" {
				t.Errorf("dates = %v", q)
			}
			if emails := q["emails"]; len(emails) != 2 {
				t.Errorf("emails = %v", emails)
			}
			writeJSON(t, w, http.StatusOK, []map[string]any{{"date": "2026-09-01", "sent": 3}})
		case "/campaigns/analytics/steps":
			if q.Get("campaign_id") != "c1" {
				t.Errorf("campaign_id = %q", q.Get("campaign_id"))
			}
			writeJSON(t, w, http.StatusOK, []map[string]any{{"step": "1", "variant": "A", "sent": 5}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{})
	ctx := context.Background()
	rows, err := client.CampaignAnalytics(ctx, []string{"c1", "c2"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("CampaignAnalytics = %v, %v", rows, err)
	}
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	daily, err := client.AccountDailyAnalytics(ctx, from, to, []string{"a@example.com", "b@example.com"})
	if err != nil || len(daily) != 1 || daily[0].Sent != 3 {
		t.Fatalf("AccountDailyAnalytics = %v, %v", daily, err)
	}
	steps, err := client.CampaignStepAnalytics(ctx, "c1")
	if err != nil || len(steps) != 1 || steps[0].Variant != "A" {
		t.Fatalf("CampaignStepAnalytics = %v, %v", steps, err)
	}
}

func TestSendingStatusReadsTheSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/campaigns/c1/sending-status" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"diagnostics": map[string]any{"anything": true},
			"summary": map[string]any{
				"status": "not_sending", "status_message": "no healthy accounts",
				"issue_started_at": "2026-09-20T08:00:00Z",
			},
		})
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{})
	status, err := client.SendingStatus(context.Background(), "c1")
	if err != nil {
		t.Fatalf("SendingStatus: %v", err)
	}
	if status.Status != "not_sending" || status.Message != "no healthy accounts" || status.IssueStartedAt == nil {
		t.Fatalf("status = %+v", status)
	}
}

func TestWebhookEventsEncodeTheSuccessFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("success") != "false" || q.Get("from") != "2026-09-01" || q.Get("limit") != "50" {
			t.Errorf("query = %v", q)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"items": []map[string]any{{"id": "e1", "success": false, "status_code": 500, "will_retry": true}},
		})
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{})
	failed := false
	page, err := client.ListWebhookEvents(context.Background(), instantly.WebhookEventsInput{
		Success: &failed, From: "2026-09-01", Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListWebhookEvents: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].StatusCode == nil || *page.Items[0].StatusCode != 500 {
		t.Fatalf("page = %+v", page)
	}
}

func TestClientHonoursTheCallerContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	client := newClient(t, server, instantly.Config{Retries: 3, Timeout: 100 * time.Millisecond})
	_, err := client.Ping(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

/* --------------------------------------------------------------------- fake */

func TestFakeAddLeadsSkipsDuplicatesAndInvalidEmails(t *testing.T) {
	f := fake.New()
	f.InvalidEmails["bad@"] = true
	f.Blocklisted["blocked@example.com"] = true
	ctx := context.Background()

	campaign, err := f.CreateCampaign(ctx, instantly.CreateCampaignInput{Name: "x"})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if !strings.HasPrefix(campaign.ID, "ic_") {
		t.Fatalf("campaign id = %q", campaign.ID)
	}

	first, err := f.AddLeads(ctx, instantly.AddLeadsInput{
		CampaignID:       campaign.ID,
		SkipIfInCampaign: true,
		Leads: []instantly.LeadInput{
			{Email: "ann@example.com", FirstName: "Ann"},
			{Email: "bad@"},
			{Email: "blocked@example.com"},
		},
	})
	if err != nil {
		t.Fatalf("AddLeads: %v", err)
	}
	if first.LeadsUploaded != 1 || len(first.CreatedLeads) != 1 || first.CreatedLeads[0].Email != "ann@example.com" {
		t.Fatalf("first = %+v", first)
	}
	if first.InvalidEmailCount != 1 || len(first.InvalidEmails) != 1 || first.InvalidEmails[0] != "bad@" {
		t.Fatalf("invalid = %d %v", first.InvalidEmailCount, first.InvalidEmails)
	}
	if first.InBlocklist != 1 || first.TotalSent != 3 {
		t.Fatalf("first = %+v", first)
	}
	if !strings.HasPrefix(first.CreatedLeads[0].ID, "il_") {
		t.Fatalf("lead id = %q", first.CreatedLeads[0].ID)
	}

	second, err := f.AddLeads(ctx, instantly.AddLeadsInput{
		CampaignID:       campaign.ID,
		SkipIfInCampaign: true,
		Leads:            []instantly.LeadInput{{Email: "Ann@Example.com"}, {Email: "bob@example.com"}},
	})
	if err != nil {
		t.Fatalf("AddLeads: %v", err)
	}
	if second.DuplicatedLeads != 1 || second.LeadsUploaded != 1 || second.CreatedLeads[0].Email != "bob@example.com" {
		t.Fatalf("second = %+v", second)
	}

	// Without the flag the duplicate is created again, as Instantly does.
	third, err := f.AddLeads(ctx, instantly.AddLeadsInput{
		CampaignID: campaign.ID,
		Leads:      []instantly.LeadInput{{Email: "ann@example.com"}},
	})
	if err != nil {
		t.Fatalf("AddLeads: %v", err)
	}
	if third.DuplicatedLeads != 0 || third.LeadsUploaded != 1 {
		t.Fatalf("third = %+v", third)
	}

	page, err := f.ListLeads(ctx, instantly.ListLeadsInput{CampaignID: campaign.ID, Limit: 2})
	if err != nil {
		t.Fatalf("ListLeads: %v", err)
	}
	if len(page.Items) != 2 || page.NextStartingAfter != page.Items[1].ID {
		t.Fatalf("page = %+v", page)
	}
	rest, err := f.ListLeads(ctx, instantly.ListLeadsInput{CampaignID: campaign.ID, Limit: 2, StartingAfter: page.NextStartingAfter})
	if err != nil {
		t.Fatalf("ListLeads: %v", err)
	}
	if len(rest.Items) != 1 || rest.NextStartingAfter != "" {
		t.Fatalf("rest = %+v", rest)
	}
	if rest.Items[0].Campaign != campaign.ID || rest.Items[0].Status != instantly.LeadStatusActive {
		t.Fatalf("lead = %+v", rest.Items[0])
	}

	only, err := f.ListLeads(ctx, instantly.ListLeadsInput{Contacts: []string{"bob@example.com"}})
	if err != nil || len(only.Items) != 1 || only.Items[0].Email != "bob@example.com" {
		t.Fatalf("contacts filter = %+v, %v", only, err)
	}

	if err := f.DeleteLead(ctx, "il_nope"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("DeleteLead(missing) = %v, want ErrNotFound", err)
	}
	if err := f.ActivateCampaign(ctx, campaign.ID); err != nil {
		t.Fatalf("ActivateCampaign: %v", err)
	}
	if f.Campaigns[campaign.ID].Status != instantly.CampaignStatusActive {
		t.Fatalf("status after activate = %d", f.Campaigns[campaign.ID].Status)
	}
	if err := f.PauseCampaign(ctx, campaign.ID); err != nil {
		t.Fatalf("PauseCampaign: %v", err)
	}
	if f.Campaigns[campaign.ID].Status != instantly.CampaignStatusPaused {
		t.Fatalf("status after pause = %d", f.Campaigns[campaign.ID].Status)
	}
}

func TestFakeFailQueueIsConsumedInOrder(t *testing.T) {
	f := fake.New()
	boom := errors.New("boom")
	f.Fail["AddLeads"] = []error{boom, provider.ErrRateLimited}
	ctx := context.Background()
	in := instantly.AddLeadsInput{CampaignID: "c", Leads: []instantly.LeadInput{{Email: "a@example.com"}}}

	if _, err := f.AddLeads(ctx, in); !errors.Is(err, boom) {
		t.Fatalf("first call err = %v, want boom", err)
	}
	if _, err := f.AddLeads(ctx, in); !errors.Is(err, provider.ErrRateLimited) {
		t.Fatalf("second call err = %v, want ErrRateLimited", err)
	}
	result, err := f.AddLeads(ctx, in)
	if err != nil || result.LeadsUploaded != 1 {
		t.Fatalf("third call = %+v, %v; want success", result, err)
	}
	if len(f.Fail["AddLeads"]) != 0 {
		t.Fatalf("queue not drained: %v", f.Fail["AddLeads"])
	}

	requests := f.Requests()
	if len(requests) != 3 {
		t.Fatalf("recorded %d requests, want 3 (failures are recorded too)", len(requests))
	}
	for i, r := range requests {
		if r.Method != "AddLeads" {
			t.Errorf("request %d method = %q", i, r.Method)
		}
		if got, ok := r.Input.(instantly.AddLeadsInput); !ok || got.CampaignID != "c" {
			t.Errorf("request %d input = %#v", i, r.Input)
		}
	}
	if f.Calls("AddLeads") != 3 || f.Calls("Ping") != 0 {
		t.Fatalf("Calls = %d / %d", f.Calls("AddLeads"), f.Calls("Ping"))
	}

	factory := fake.StaticFactory{Client: f}
	if client, err := factory.For(ctx, dbgenSource()); err != nil || client != f {
		t.Fatalf("StaticFactory.For = %v, %v", client, err)
	}
	factory.Err = boom
	if _, err := factory.For(ctx, dbgenSource()); !errors.Is(err, boom) {
		t.Fatalf("StaticFactory.For with Err = %v", err)
	}
}

func TestGoogleOAuthSessionAndWarmup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/google/init":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"session_id": "s-1",
				"auth_url":   "https://accounts.google.com/o/oauth2/v2/auth?state=api_session:s-1",
				"expires_at": "2026-09-28T10:30:00.000Z",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/oauth/session/status/s-1":
			writeJSON(t, w, http.StatusOK, map[string]any{
				"status": "error", "error": "account_exists", "error_description": "Account already exists in another workspace",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/accounts/warmup/enable":
			body := decodeBody(t, r)
			if emails, _ := body["emails"].([]any); len(emails) != 1 || emails[0] != "jane@shop.test" {
				t.Errorf("warmup body = %v", body)
			}
			writeJSON(t, w, http.StatusOK, map[string]any{"id": "job-1", "status": "pending", "type": "warmup_enable"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client := newClient(t, server, instantly.Config{})

	session, err := client.StartGoogleOAuth(context.Background())
	if err != nil || session.SessionID != "s-1" || !strings.HasPrefix(session.AuthURL, "https://accounts.google.com/") ||
		session.ExpiresAt.IsZero() {
		t.Fatalf("StartGoogleOAuth = %+v, %v", session, err)
	}
	status, err := client.OAuthSessionStatus(context.Background(), "s-1")
	if err != nil || status.Status != instantly.OAuthError || status.Error != "account_exists" {
		t.Fatalf("OAuthSessionStatus = %+v, %v", status, err)
	}
	job, err := client.EnableWarmup(context.Background(), []string{"jane@shop.test"})
	if err != nil || job.ID != "job-1" {
		t.Fatalf("EnableWarmup = %+v, %v", job, err)
	}
}
