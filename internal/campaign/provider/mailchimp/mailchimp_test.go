package mailchimp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/provider/mailchimp"
	"github.com/bory/karvon-be/internal/campaign/provider/mailchimp/fake"
)

// Split so secret scanners don't flag this fake key as a Mailchimp API key.
const testKey = "0123456789abcdef" + "0123456789abcdef" + "-" + "us6"

// newClient points a client at a test server, with fast retries.
func newClient(t *testing.T, handler http.HandlerFunc, retries int) *mailchimp.HTTPClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return mailchimp.New(mailchimp.Config{
		BaseURL: srv.URL,
		APIKey:  testKey,
		Timeout: 5 * time.Second,
		Retries: retries,
	})
}

func problem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "https://mailchimp.com/developer/marketing/docs/errors/", "title": title,
		"status": status, "detail": detail, "instance": "abc",
	})
}

func TestClientDerivesTheDatacenterFromTheKey(t *testing.T) {
	if got := mailchimp.DataCenter("xxxx-us6"); got != "us6" {
		t.Fatalf("DataCenter = %q, want us6", got)
	}
	if got := mailchimp.DataCenter("nodash"); got != "" {
		t.Fatalf("DataCenter without a dash = %q, want empty", got)
	}
	c := mailchimp.New(mailchimp.Config{APIKey: "xxxx-us6"})
	if got := c.BaseURL(); got != "https://us6.api.mailchimp.com/3.0" {
		t.Fatalf("BaseURL = %q", got)
	}
	custom := mailchimp.New(mailchimp.Config{APIKey: "xxxx-us21", BaseURL: "http://{dc}.local/3.0/"})
	if got := custom.BaseURL(); got != "http://us21.local/3.0" {
		t.Fatalf("custom BaseURL = %q", got)
	}

	// A key without a datacenter cannot be used at all.
	broken := mailchimp.New(mailchimp.Config{APIKey: "nodash"})
	if _, err := broken.Ping(context.Background()); !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("Ping with a broken key = %v, want ErrAuth", err)
	}
}

func TestClientUsesBasicAuth(t *testing.T) {
	var gotUser, gotPass, gotPath, gotQuery string
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"account_name":"Freddie's Jokes","email":"freddie@example.com"}`)
	}, 0)

	acct, err := c.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if gotUser != "anystring" || gotPass != testKey {
		t.Fatalf("basic auth = %q:%q", gotUser, gotPass)
	}
	if gotPath != "/" || gotQuery != "fields=account_name%2Cemail" {
		t.Fatalf("request = %s?%s", gotPath, gotQuery)
	}
	if acct.AccountName != "Freddie's Jokes" || acct.Email != "freddie@example.com" || acct.DC != "us6" {
		t.Fatalf("account = %+v", acct)
	}
}

func TestClientMapsMemberInComplianceStateToItsSentinel(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		problem(w, http.StatusBadRequest, "Member In Compliance State",
			"urist@example.com is in a compliance state due to unsubscribe, bounce, or compliance review and cannot be subscribed.")
	}, 2)

	_, err := c.UpsertMember(context.Background(), "list1", mailchimp.MemberInput{
		EmailAddress: "urist@example.com", StatusIfNew: mailchimp.StatusPending,
	})
	if !errors.Is(err, provider.ErrComplianceState) {
		t.Fatalf("err = %v, want ErrComplianceState", err)
	}
	if provider.Retryable(err) {
		t.Fatal("a compliance state must not be retried")
	}

	// The wording can also be only in the detail.
	c = newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		problem(w, http.StatusBadRequest, "Invalid Resource", "The member is in a Compliance State.")
	}, 0)
	_, err = c.UpsertMember(context.Background(), "list1", mailchimp.MemberInput{EmailAddress: "a@b.co"})
	if !errors.Is(err, provider.ErrComplianceState) {
		t.Fatalf("detail-only err = %v, want ErrComplianceState", err)
	}
}

func TestClientMapsMemberExists(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		problem(w, http.StatusBadRequest, "Member Exists", "urist@example.com is already a list member.")
	}, 0)
	_, err := c.UpsertMember(context.Background(), "list1", mailchimp.MemberInput{EmailAddress: "urist@example.com"})
	if !errors.Is(err, provider.ErrMemberExists) {
		t.Fatalf("err = %v, want ErrMemberExists", err)
	}

	c = newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		problem(w, http.StatusBadRequest, "Invalid Resource", "The resource submitted could not be validated.")
	}, 0)
	_, err = c.UpsertMember(context.Background(), "list1", mailchimp.MemberInput{EmailAddress: "bad"})
	if !errors.Is(err, provider.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}

	c = newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		problem(w, http.StatusUnauthorized, "API Key Invalid", "Your API key may be invalid.")
	}, 0)
	if _, err := c.Ping(context.Background()); !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("401 err = %v, want ErrAuth", err)
	}

	c = newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		problem(w, http.StatusNotFound, "Resource Not Found", "The requested resource could not be found.")
	}, 0)
	if err := c.ArchiveMember(context.Background(), "list1", "abc"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("404 err = %v, want ErrNotFound", err)
	}
}

func TestClientRetriesA503ThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"lists":[{"id":"l1","web_id":7,"name":"News","double_optin":true,"stats":{"member_count":3}}]}`)
	}, 2)

	lists, err := c.ListAudiences(context.Background())
	if err != nil {
		t.Fatalf("ListAudiences: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
	if len(lists) != 1 || lists[0].ID != "l1" || lists[0].Stats.MemberCount != 3 || !lists[0].DoubleOptin {
		t.Fatalf("lists = %+v", lists)
	}

	// With retries exhausted the status error surfaces and is retryable for the queue.
	c = newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}, 0)
	_, err = c.ListAudiences(context.Background())
	var status *provider.StatusError
	if !errors.As(err, &status) || status.Code != http.StatusBadGateway {
		t.Fatalf("err = %v, want StatusError 502", err)
	}
	if !provider.Retryable(err) {
		t.Fatal("a 5xx should stay retryable for the queue")
	}
}

func TestClientSnoozesOn429(t *testing.T) {
	var calls atomic.Int32
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "7")
		problem(w, http.StatusTooManyRequests, "Too Many Requests", "You have exceeded the limit of 10 simultaneous connections.")
	}, 3)

	_, err := c.GetMember(context.Background(), "list1", "abc")
	if !errors.Is(err, provider.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if after, ok := provider.RetryAfter(err); !ok || after != 7*time.Second {
		t.Fatalf("RetryAfter = %s, %v; want 7s", after, ok)
	}
	if calls.Load() != 1 {
		t.Fatalf("a 429 was retried %d times; the queue snoozes instead", calls.Load()-1)
	}

	// Without a header the fallback is 30s.
	c = newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		problem(w, http.StatusTooManyRequests, "Too Many Requests", "")
	}, 0)
	_, err = c.GetMember(context.Background(), "list1", "abc")
	if after, _ := provider.RetryAfter(err); after != 30*time.Second {
		t.Fatalf("fallback RetryAfter = %s, want 30s", after)
	}
}

func TestSubscriberHashIsMD5OfTheLowercasedAddress(t *testing.T) {
	const want = "62eeb292278cc15f5817cb78f7790b08"
	if got := mailchimp.SubscriberHash("Urist.McVankab@freddiesjokes.com"); got != want {
		t.Fatalf("SubscriberHash = %q, want %q", got, want)
	}
	if got := mailchimp.SubscriberHash("  urist.mcvankab@freddiesjokes.com "); got != want {
		t.Fatalf("SubscriberHash with whitespace = %q, want %q", got, want)
	}
}

func TestUpsertMemberSendsStatusIfNew(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"id":"62eeb292278cc15f5817cb78f7790b08","email_address":"urist.mcvankab@freddiesjokes.com",
			"unique_email_id":"ue1","contact_id":"c1","web_id":42,"status":"pending","list_id":"list1",
			"last_changed":"2026-09-20T10:00:00+00:00"}`)
	}, 0)

	m, err := c.UpsertMember(context.Background(), "list1", mailchimp.MemberInput{
		EmailAddress: "Urist.McVankab@freddiesjokes.com",
		StatusIfNew:  mailchimp.StatusPending,
		MergeFields:  map[string]string{"FNAME": "Urist"},
		Tags:         []string{"karvon"},
	})
	if err != nil {
		t.Fatalf("UpsertMember: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/lists/list1/members/62eeb292278cc15f5817cb78f7790b08" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	if gotBody["status_if_new"] != "pending" {
		t.Fatalf("status_if_new = %v", gotBody["status_if_new"])
	}
	if _, present := gotBody["status"]; present {
		t.Fatal("an empty status must be omitted so an existing member is not changed")
	}
	if _, present := gotBody["email_type"]; present {
		t.Fatal("an empty email_type must be omitted")
	}
	if m.Status != mailchimp.StatusPending || m.WebID != 42 || m.LastChanged == nil {
		t.Fatalf("member = %+v", m)
	}

	// Tags go to a separate endpoint and answer 204.
	c = newClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}, 0)
	err = c.AddTags(context.Background(), "list1", "abc", []mailchimp.Tag{{Name: "karvon", Status: "active"}})
	if err != nil {
		t.Fatalf("AddTags: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/lists/list1/members/abc/tags" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}
	tags, _ := gotBody["tags"].([]any)
	if len(tags) != 1 {
		t.Fatalf("tags body = %v", gotBody)
	}
}

func TestClientListsMembersWithTheDocumentedQuery(t *testing.T) {
	var gotQuery string
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"members":[{"id":"a","status":"subscribed"}],"total_items":1}`)
	}, 0)
	since := time.Date(2026, 9, 1, 12, 0, 0, 0, time.FixedZone("x", 3600))
	page, err := c.ListMembers(context.Background(), "l1", mailchimp.ListMembersInput{
		Count: 500, Offset: 1000, SinceLastChanged: &since, Status: "subscribed",
	})
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	want := "count=500&offset=1000&since_last_changed=2026-09-01T11%3A00%3A00Z&status=subscribed"
	if gotQuery != want {
		t.Fatalf("query = %q, want %q", gotQuery, want)
	}
	if page.TotalItems != 1 || len(page.Members) != 1 {
		t.Fatalf("page = %+v", page)
	}
}

func TestClientDecodesTheWebhookSigningSecret(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			_, _ = io.WriteString(w, `{"id":"wh1","url":"https://k/hook","list_id":"l1","events":{"subscribe":true},"sources":{"api":true},"signing_secret":"s3cr3t"}`)
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"webhooks":[{"id":"wh1","url":"https://k/hook","list_id":"l1"}]}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}, 0)
	ctx := context.Background()
	w, err := c.CreateWebhook(ctx, "l1", mailchimp.WebhookInput{
		URL:     "https://k/hook",
		Events:  map[string]bool{"subscribe": true},
		Sources: map[string]bool{"api": true},
	})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if w.SigningSecret != "s3cr3t" || !w.Events["subscribe"] {
		t.Fatalf("webhook = %+v", w)
	}
	hooks, err := c.ListWebhooks(ctx, "l1")
	if err != nil || len(hooks) != 1 || hooks[0].ID != "wh1" {
		t.Fatalf("ListWebhooks = %+v, %v", hooks, err)
	}
	if err := c.DeleteWebhook(ctx, "l1", "wh1"); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}
}

func TestClientOpensTheBreakerAfterRepeatedFailures(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := mailchimp.New(mailchimp.Config{
		BaseURL: srv.URL, APIKey: testKey, Timeout: time.Second,
		BreakerThreshold: 2, BreakerCooldown: time.Minute,
		Now: func() time.Time { return now },
	})
	ctx := context.Background()
	for range 2 {
		if _, err := c.Ping(ctx); err == nil {
			t.Fatal("expected a failure")
		}
	}
	_, err := c.Ping(ctx)
	if !errors.Is(err, provider.ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d; an open circuit must not hit the server", calls.Load())
	}
	now = now.Add(2 * time.Minute)
	_, err = c.Ping(ctx)
	if errors.Is(err, provider.ErrCircuitOpen) {
		t.Fatal("the circuit should close after the cooldown")
	}
}

func TestFakeAutoConfirmTurnsPendingIntoSubscribed(t *testing.T) {
	f := fake.New()
	f.AutoConfirmPending = true
	ctx := context.Background()

	m, err := f.UpsertMember(ctx, "l1", mailchimp.MemberInput{
		EmailAddress: "Urist.McVankab@freddiesjokes.com", StatusIfNew: mailchimp.StatusPending,
	})
	if err != nil {
		t.Fatalf("UpsertMember: %v", err)
	}
	if m.Status != mailchimp.StatusSubscribed {
		t.Fatalf("status = %q, want subscribed", m.Status)
	}
	if m.ID != "62eeb292278cc15f5817cb78f7790b08" || m.WebID != 1 || m.UniqueEmailID != "ue_1" || m.ContactID != "c_1" {
		t.Fatalf("member = %+v", m)
	}
	if m.LastChanged == nil {
		t.Fatal("LastChanged must be set")
	}

	// Without auto-confirm a pending member stays pending, and a later Status
	// update is applied to the existing member.
	f.AutoConfirmPending = false
	m, err = f.UpsertMember(ctx, "l1", mailchimp.MemberInput{EmailAddress: "second@example.com", StatusIfNew: mailchimp.StatusPending})
	if err != nil || m.Status != mailchimp.StatusPending {
		t.Fatalf("member = %+v, %v", m, err)
	}
	m, err = f.UpsertMember(ctx, "l1", mailchimp.MemberInput{EmailAddress: "second@example.com", Status: mailchimp.StatusUnsubscribed})
	if err != nil || m.Status != mailchimp.StatusUnsubscribed || m.WebID != 2 {
		t.Fatalf("updated member = %+v, %v", m, err)
	}

	// Compliance addresses are refused, and failure queues pop per call.
	f.ComplianceEmails["blocked@example.com"] = true
	if _, err := f.UpsertMember(ctx, "l1", mailchimp.MemberInput{EmailAddress: "Blocked@example.com"}); !errors.Is(err, provider.ErrComplianceState) {
		t.Fatalf("err = %v, want ErrComplianceState", err)
	}
	f.Fail["Ping"] = []error{provider.ErrAuth}
	if _, err := f.Ping(ctx); !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("queued failure = %v", err)
	}
	if _, err := f.Ping(ctx); err != nil {
		t.Fatalf("second Ping = %v, want nil", err)
	}

	reqs := f.Requests()
	if len(reqs) != 6 || reqs[0].Method != "UpsertMember" || reqs[5].Method != "Ping" {
		t.Fatalf("requests = %+v", reqs)
	}

	page, err := f.ListMembers(ctx, "l1", mailchimp.ListMembersInput{Status: mailchimp.StatusSubscribed})
	if err != nil || page.TotalItems != 1 {
		t.Fatalf("page = %+v, %v", page, err)
	}
}

func TestStatusLabelIsHumanReadable(t *testing.T) {
	if got := mailchimp.StatusLabel("cleaned"); got != "Cleaned (bounced)" {
		t.Fatalf("StatusLabel = %q", got)
	}
	if got := mailchimp.StatusLabel("weird"); got != "weird" {
		t.Fatalf("unknown StatusLabel = %q", got)
	}
}
