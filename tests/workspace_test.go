package integration_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/config"
)

const workspaceAdmin = "admin@karvon-hq.test"

// fakeZones is just enough of Cloudflare's zone and DNS API to publish mail records
// against: the zones the account has and the records in each.
type fakeZones struct {
	mu      sync.Mutex
	zones   map[string]bool
	records []map[string]any
	next    int
}

func newFakeZones(names ...string) *fakeZones {
	z := &fakeZones{zones: map[string]bool{}}
	for _, name := range names {
		z.zones[name] = true
	}
	return z
}

func (z *fakeZones) add(recordType, name, content string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.next++
	z.records = append(z.records, map[string]any{"id": fmt.Sprintf("rec-%d", z.next), "type": recordType, "name": name, "content": content})
}

func (z *fakeZones) remove(recordType, name string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	kept := z.records[:0]
	for _, rec := range z.records {
		if rec["type"] != recordType || rec["name"] != name {
			kept = append(kept, rec)
		}
	}
	z.records = kept
}

func (z *fakeZones) find(recordType, name string) []string {
	z.mu.Lock()
	defer z.mu.Unlock()
	var out []string
	for _, rec := range z.records {
		if rec["type"] == recordType && rec["name"] == name {
			out = append(out, rec["content"].(string))
		}
	}
	return out
}

func (z *fakeZones) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reply := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
	}
	if r.Header.Get("Authorization") != "Bearer "+cloudflareToken {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"code": 10000, "message": "Authentication error"}}})
		return
	}
	if r.URL.Path == "/zones" {
		name := r.URL.Query().Get("name")
		if r.URL.Query().Get("account.id") != cloudflareAccount || !z.zones[name] {
			reply([]any{})
			return
		}
		reply([]map[string]string{{"id": "zone-" + name, "name": name, "status": "active"}})
		return
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		out := []map[string]any{}
		for _, rec := range z.records {
			if rec["type"] == r.URL.Query().Get("type") && rec["name"] == r.URL.Query().Get("name") {
				out = append(out, rec)
			}
		}
		reply(out)
	case http.MethodPost:
		var rec map[string]any
		_ = json.NewDecoder(r.Body).Decode(&rec)
		z.next++
		rec["id"] = fmt.Sprintf("rec-%d", z.next)
		z.records = append(z.records, rec)
		reply(rec)
	case http.MethodPatch:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		for _, rec := range z.records {
			if rec["id"] == id {
				rec["content"] = body["content"]
				reply(rec)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

// fakeWorkspace is the Directory and Site Verification APIs of one Workspace account.
// Verification looks at the fake zones, so it passes only once the record is there,
// and only after a couple of looks, the way DNS caches make Google wait.
type fakeWorkspace struct {
	mu       sync.Mutex
	zones    *fakeZones
	domains  map[string]bool // name → verified
	users    map[string]string
	inserts  map[string]int
	verifies map[string]int
	// lostAnswer addresses are created but answered with a 502.
	lostAnswer map[string]bool
	// verifyAfter is how many looks verification needs.
	verifyAfter int
}

func newFakeWorkspace(zones *fakeZones) *fakeWorkspace {
	return &fakeWorkspace{
		zones:       zones,
		domains:     map[string]bool{"karvon-hq.test": true},
		users:       map[string]string{workspaceAdmin: "admin-pw", "taken@shop.test": "someone-elses"},
		inserts:     map[string]int{},
		verifies:    map[string]int{},
		lostAnswer:  map[string]bool{"flaky@shop.test": true},
		verifyAfter: 2,
	}
}

func (f *fakeWorkspace) insertCount(email string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inserts[email]
}

func (f *fakeWorkspace) password(email string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[email]
}

func (f *fakeWorkspace) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fail := func(status int, message string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": status, "message": message}})
	}
	if r.URL.Path == "/token" {
		_ = r.ParseForm()
		if r.Form.Get("assertion") == "" {
			fail(http.StatusBadRequest, "no assertion")
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"google-at","expires_in":3600}`))
		return
	}
	if r.Header.Get("Authorization") != "Bearer google-at" {
		fail(http.StatusUnauthorized, "Invalid Credentials")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	domainOut := func(name string) map[string]any {
		return map[string]any{"domainName": name, "verified": f.domains[name], "isPrimary": name == "karvon-hq.test"}
	}
	const domains = "/directory/customer/my_customer/domains"
	switch path := r.URL.Path; {
	case path == domains && r.Method == http.MethodGet:
		out := []any{}
		for name := range f.domains {
			out = append(out, domainOut(name))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"domains": out})
	case path == domains && r.Method == http.MethodPost:
		name := body["domainName"].(string)
		if _, ok := f.domains[name]; ok {
			fail(http.StatusConflict, "Entity already exists.")
			return
		}
		f.domains[name] = false
		_ = json.NewEncoder(w).Encode(domainOut(name))
	case strings.HasPrefix(path, domains+"/"):
		name := strings.TrimPrefix(path, domains+"/")
		if _, ok := f.domains[name]; !ok {
			fail(http.StatusNotFound, "Domain not found.")
			return
		}
		_ = json.NewEncoder(w).Encode(domainOut(name))
	case path == "/siteverification/token":
		site := body["site"].(map[string]any)
		_ = json.NewEncoder(w).Encode(map[string]string{"method": "DNS_TXT",
			"token": "google-site-verification=" + site["identifier"].(string) + "-proof"})
	case path == "/siteverification/webResource":
		name := body["site"].(map[string]any)["identifier"].(string)
		f.verifies[name]++
		found := false
		for _, value := range f.zones.find("TXT", name) {
			found = found || value == "google-site-verification="+name+"-proof"
		}
		if !found || f.verifies[name] < f.verifyAfter {
			fail(http.StatusBadRequest, "The necessary verification token could not be found on your site.")
			return
		}
		f.domains[name] = true
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "dns://" + name})
	case path == "/directory/users" && r.Method == http.MethodPost:
		email := body["primaryEmail"].(string)
		f.inserts[email]++
		if _, exists := f.users[email]; exists {
			fail(http.StatusConflict, "Entity already exists.")
			return
		}
		_, domain, _ := strings.Cut(email, "@")
		if !f.domains[domain] {
			fail(http.StatusPreconditionFailed, "Domain not verified.")
			return
		}
		f.users[email] = body["password"].(string)
		if f.lostAnswer[email] && f.inserts[email] == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "u-" + email, "primaryEmail": email})
	case strings.HasPrefix(path, "/directory/users/"):
		email := strings.TrimPrefix(path, "/directory/users/")
		if _, ok := f.users[email]; !ok {
			fail(http.StatusNotFound, "Resource Not Found: userKey")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "u-" + email, "primaryEmail": email})
	default:
		fail(http.StatusNotFound, "no route "+r.Method+" "+path)
	}
}

func testServiceAccountKey(t *testing.T) string {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "karvon@proj.iam.gserviceaccount.com",
		"client_id": "109876543210", "private_key_id": "k1",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	})
	return string(raw)
}

type workspaceDomainPayload struct {
	ID              string                    `json:"id"`
	DomainName      string                    `json:"domain_name"`
	Status          string                    `json:"status"`
	NextStep        *string                   `json:"next_step"`
	ErrorCode       *string                   `json:"error_code"`
	ErrorMessage    *string                   `json:"error_message"`
	VerifiedAt      *string                   `json:"verified_at"`
	DkimPublishedAt *string                   `json:"dkim_published_at"`
	Mailboxes       []workspaceMailboxPayload `json:"mailboxes"`
}

type workspaceMailboxPayload struct {
	ID                       string  `json:"id"`
	Email                    string  `json:"email"`
	Status                   string  `json:"status"`
	ErrorCode                *string `json:"error_code"`
	InstantlyStatus          *string `json:"instantly_status"`
	InstantlyError           *string `json:"instantly_error"`
	InstantlyWarmupEnabledAt *string `json:"instantly_warmup_enabled_at"`
}

func newWorkspaceHarness(t *testing.T) (*harness, *fakeZones, *fakeWorkspace) {
	t.Helper()
	zones := newFakeZones("shop.test", "other.test")
	google := newFakeWorkspace(zones)
	cf := http.NewServeMux()
	cf.Handle("/zones", zones)
	cf.Handle("/zones/", zones)
	cf.Handle("/", newFakeCloudflare())
	cfSrv := httptest.NewServer(cf)
	t.Cleanup(cfSrv.Close)
	googleSrv := httptest.NewServer(google)
	t.Cleanup(googleSrv.Close)

	h := newHarness(t, nil, withConfig(func(cfg *config.Config) {
		cfg.CloudflareBaseURL = cfSrv.URL
		cfg.GoogleTokenURL = googleSrv.URL + "/token"
		cfg.GoogleDirectoryURL = googleSrv.URL + "/directory"
		cfg.GoogleSiteVerificationURL = googleSrv.URL + "/siteverification"
		cfg.WorkspacePollInterval = 100 * time.Millisecond
		cfg.WorkspaceInstantlyPollInterval = 100 * time.Millisecond
	}))
	h.mustRequest(http.MethodPut, "/api/v1/domains/settings",
		fmt.Sprintf(`{"account_id":%q,"api_token":%q,"enabled":true}`, cloudflareAccount, cloudflareToken), http.StatusOK)
	return h, zones, google
}

func (h *harness) connectWorkspace(key string) {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{"admin_email": workspaceAdmin, "service_account_key": key, "enabled": true})
	h.mustRequest(http.MethodPut, "/api/v1/workspace/settings", string(body), http.StatusOK)
}

func (h *harness) waitForWorkspaceDomain(domain string) workspaceDomainPayload {
	h.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last workspaceDomainPayload
	for time.Now().Before(deadline) {
		last = decodeBody[workspaceDomainPayload](h.t, h.mustRequest(http.MethodGet, "/api/v1/workspace/domains/"+domain, "", http.StatusOK))
		if last.Status != "provisioning" {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("setup of %s never settled: %+v", domain, last)
	return last
}

func setupBody(domain string, confirm bool, locals ...string) string {
	mailboxes := make([]string, len(locals))
	for i, local := range locals {
		mailboxes[i] = fmt.Sprintf(`{"local_part":%q,"given_name":"Jane","family_name":"Doe"}`, local)
	}
	return fmt.Sprintf(`{"domain":%q,"confirm":%t,"mailboxes":[%s]}`, domain, confirm, strings.Join(mailboxes, ","))
}

func TestWorkspaceSetupEndToEnd(t *testing.T) {
	h, zones, google := newWorkspaceHarness(t)

	// Nothing runs until the connection is set up.
	settings := decodeBody[map[string]any](t, h.mustRequest(http.MethodGet, "/api/v1/workspace/settings", "", http.StatusOK))
	if settings["ready"] != false || len(settings["scopes"].([]any)) != 3 || settings["max_mailboxes_per_domain"] != float64(5) {
		t.Fatalf("fresh settings = %v", settings)
	}
	h.mustRequest(http.MethodPost, "/api/v1/workspace/domains", setupBody("shop.test", true, "jane"), http.StatusConflict)

	// A file that is not a service-account key is refused, not stored.
	rec := h.mustRequest(http.MethodPut, "/api/v1/workspace/settings", `{"service_account_key":"{\"installed\":{}}"}`, http.StatusUnprocessableEntity)
	if got := decodeBody[errorPayload](t, rec); len(got.Error.Details) != 1 || got.Error.Details[0].Field != "service_account_key" {
		t.Fatalf("bad key = %+v", got)
	}
	h.connectWorkspace(testServiceAccountKey(t))
	settings = decodeBody[map[string]any](t, h.mustRequest(http.MethodGet, "/api/v1/workspace/settings", "", http.StatusOK))
	if settings["ready"] != true || settings["service_account_client_id"] != "109876543210" || settings["admin_email"] != workspaceAdmin {
		t.Fatalf("saved settings = %v", settings)
	}
	if _, leaked := settings["service_account_key"]; leaked {
		t.Fatal("the key was returned")
	}
	test := decodeBody[map[string]any](t, h.mustRequest(http.MethodPost, "/api/v1/workspace/settings/test", "", http.StatusOK))
	if test["primary_domain"] != "karvon-hq.test" {
		t.Fatalf("test = %v", test)
	}

	// The limits hold before anything is created.
	h.mustRequest(http.MethodPost, "/api/v1/workspace/domains", setupBody("shop.test", false, "jane"), http.StatusUnprocessableEntity)
	h.mustRequest(http.MethodPost, "/api/v1/workspace/domains",
		setupBody("shop.test", true, "a", "b", "c", "d", "e", "f"), http.StatusUnprocessableEntity)
	h.mustRequest(http.MethodPost, "/api/v1/workspace/domains", setupBody("nozone.test", true, "jane"), http.StatusConflict)

	// jane is created at once; flaky is created but its answer is lost.
	queued := decodeBody[workspaceDomainPayload](t, h.mustRequest(http.MethodPost, "/api/v1/workspace/domains",
		setupBody("Shop.TEST", true, "Jane", "flaky"), http.StatusAccepted))
	if queued.Status != "provisioning" || queued.DomainName != "shop.test" || len(queued.Mailboxes) != 2 {
		t.Fatalf("queued = %+v", queued)
	}
	h.mustRequest(http.MethodPost, "/api/v1/workspace/domains", setupBody("shop.test", true, "bob"), http.StatusConflict)

	done := h.waitForWorkspaceDomain("shop.test")
	if done.Status != "dkim_required" || done.VerifiedAt == nil || done.NextStep == nil {
		t.Fatalf("settled = %+v", done)
	}
	for _, mb := range done.Mailboxes {
		if mb.Status != "created" {
			t.Fatalf("mailbox = %+v", mb)
		}
	}
	if n := google.insertCount("jane@shop.test"); n != 1 {
		t.Fatalf("jane was sent %d times, want 1", n)
	}
	if n := google.insertCount("flaky@shop.test"); n != 2 {
		t.Fatalf("flaky was sent %d times; want the lost answer settled by one more send", n)
	}

	// The records Workspace mail needs are in the zone.
	if got := zones.find("MX", "shop.test"); len(got) != 1 || got[0] != "smtp.google.com" {
		t.Fatalf("MX = %v", got)
	}
	if got := zones.find("TXT", "shop.test"); len(got) != 2 {
		t.Fatalf("apex TXT = %v", got)
	}
	if got := zones.find("TXT", "_dmarc.shop.test"); len(got) != 1 {
		t.Fatalf("DMARC = %v", got)
	}

	// The stored password is the one Google has.
	rec = h.mustRequest(http.MethodGet, "/api/v1/workspace/mailboxes/"+done.Mailboxes[0].ID+"/credentials", "", http.StatusOK)
	creds := decodeBody[map[string]string](t, rec)
	if creds["email"] != "jane@shop.test" || creds["password"] == "" || creds["password"] != google.password("jane@shop.test") {
		t.Fatalf("credentials = %v", creds)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}

	// DKIM: the key from the Admin console is published and the domain is active.
	h.mustRequest(http.MethodPut, "/api/v1/workspace/domains/shop.test/dkim", `{"value":"k=rsa; p=abc"}`, http.StatusUnprocessableEntity)
	active := decodeBody[workspaceDomainPayload](t, h.mustRequest(http.MethodPut, "/api/v1/workspace/domains/shop.test/dkim",
		`{"value":"\"v=DKIM1; k=rsa; \" \"p=MIIBIjAN\""}`, http.StatusOK))
	if active.Status != "active" || active.DkimPublishedAt == nil {
		t.Fatalf("after DKIM = %+v", active)
	}
	if got := zones.find("TXT", "google._domainkey.shop.test"); len(got) != 1 || got[0] != "v=DKIM1; k=rsa; p=MIIBIjAN" {
		t.Fatalf("DKIM record = %v", got)
	}

	list := decodeBody[struct {
		Data []workspaceDomainPayload
		Meta struct{ Total int64 }
	}](t, h.mustRequest(http.MethodGet, "/api/v1/workspace/domains", "", http.StatusOK))
	if list.Meta.Total != 1 || len(list.Data[0].Mailboxes) != 2 {
		t.Fatalf("list = %+v", list)
	}
}

func TestWorkspaceSetupStopsForAPersonAndResumes(t *testing.T) {
	h, zones, google := newWorkspaceHarness(t)
	h.connectWorkspace(testServiceAccountKey(t))

	// Another mail service's MX is never overwritten.
	zones.add("MX", "other.test", "route1.mx.cloudflare.net")
	h.mustRequest(http.MethodPost, "/api/v1/workspace/domains", setupBody("other.test", true, "sales"), http.StatusAccepted)
	failed := h.waitForWorkspaceDomain("other.test")
	if failed.Status != "failed" || failed.ErrorCode == nil || *failed.ErrorCode != "dns_conflict" {
		t.Fatalf("failed = %+v", failed)
	}
	if n := google.insertCount("sales@other.test"); n != 0 {
		t.Fatalf("a mailbox was created on a domain that failed: %d", n)
	}
	h.mustRequest(http.MethodPut, "/api/v1/workspace/domains/other.test/dkim", `{"value":"v=DKIM1; p=abc"}`, http.StatusConflict)

	// Once the MX is removed, a retry finishes the job.
	zones.remove("MX", "other.test")
	h.mustRequest(http.MethodPost, "/api/v1/workspace/domains/other.test/retry", "", http.StatusAccepted)
	done := h.waitForWorkspaceDomain("other.test")
	if done.Status != "dkim_required" || done.Mailboxes[0].Status != "created" {
		t.Fatalf("retried = %+v", done)
	}
	h.mustRequest(http.MethodPost, "/api/v1/workspace/domains/other.test/retry", "", http.StatusConflict)

	// An address someone else already has is not claimed.
	h.mustRequest(http.MethodPost, "/api/v1/workspace/domains", setupBody("shop.test", true, "taken", "ok"), http.StatusAccepted)
	mixed := h.waitForWorkspaceDomain("shop.test")
	if mixed.Status != "dkim_required" {
		t.Fatalf("mixed = %+v", mixed)
	}
	taken, ok := mixed.Mailboxes[0], mixed.Mailboxes[1]
	if taken.Status != "failed" || taken.ErrorCode == nil || *taken.ErrorCode != "address_taken" || ok.Status != "created" {
		t.Fatalf("mailboxes = %+v", mixed.Mailboxes)
	}
	if google.password("taken@shop.test") != "someone-elses" {
		t.Fatal("someone else's mailbox was changed")
	}
	h.mustRequest(http.MethodGet, "/api/v1/workspace/mailboxes/"+taken.ID+"/credentials", "", http.StatusConflict)
}

func mailboxesBody(confirm bool, locals ...string) string {
	body := setupBody("unused", confirm, locals...)
	return strings.Replace(body, `"domain":"unused",`, "", 1)
}

func (h *harness) mailboxIn(domain, email string) workspaceMailboxPayload {
	h.t.Helper()
	setup := decodeBody[workspaceDomainPayload](h.t, h.mustRequest(http.MethodGet, "/api/v1/workspace/domains/"+domain, "", http.StatusOK))
	for _, mb := range setup.Mailboxes {
		if mb.Email == email {
			return mb
		}
	}
	h.t.Fatalf("%s has no mailbox %s: %+v", domain, email, setup.Mailboxes)
	return workspaceMailboxPayload{}
}

// waitForInstantly polls a mailbox until its Instantly connection leaves connecting.
func (h *harness) waitForInstantly(domain, email string) workspaceMailboxPayload {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		mb := h.mailboxIn(domain, email)
		if mb.InstantlyStatus != nil && *mb.InstantlyStatus != "connecting" {
			return mb
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%s never left connecting: %+v", email, mb)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// sessionOf reads the Instantly session id out of the Google sign-in URL.
func sessionOf(authURL string) string {
	_, state, _ := strings.Cut(authURL, "api_session%3A")
	session, _, _ := strings.Cut(state, "&")
	return session
}

func TestWorkspaceMailboxesCanBeAddedAndRemoved(t *testing.T) {
	h, _, google := newWorkspaceHarness(t)
	h.connectWorkspace(testServiceAccountKey(t))

	h.mustRequest(http.MethodPost, "/api/v1/workspace/domains", setupBody("shop.test", true, "jane", "taken"), http.StatusAccepted)
	// Nothing is added while the first setup runs.
	h.mustRequest(http.MethodPost, "/api/v1/workspace/domains/shop.test/mailboxes", mailboxesBody(true, "bob"), http.StatusConflict)
	done := h.waitForWorkspaceDomain("shop.test")
	if done.Status != "dkim_required" || len(done.Mailboxes) != 2 {
		t.Fatalf("setup = %+v", done)
	}

	// The limits: confirmation, repeats, five per domain with the failed one counted.
	h.mustRequest(http.MethodPost, "/api/v1/workspace/domains/shop.test/mailboxes", mailboxesBody(false, "bob"), http.StatusUnprocessableEntity)
	h.mustRequest(http.MethodPost, "/api/v1/workspace/domains/shop.test/mailboxes", mailboxesBody(true, "jane"), http.StatusUnprocessableEntity)
	h.mustRequest(http.MethodPost, "/api/v1/workspace/domains/shop.test/mailboxes",
		mailboxesBody(true, "a", "b", "c", "d"), http.StatusUnprocessableEntity)

	// A created mailbox is a Workspace user and stays; the failed one is just a record.
	jane, taken := h.mailboxIn("shop.test", "jane@shop.test"), h.mailboxIn("shop.test", "taken@shop.test")
	h.mustRequest(http.MethodDelete, "/api/v1/workspace/mailboxes/"+jane.ID, "", http.StatusConflict)
	h.mustRequest(http.MethodDelete, "/api/v1/workspace/mailboxes/"+taken.ID, "", http.StatusNoContent)

	added := decodeBody[workspaceDomainPayload](t, h.mustRequest(http.MethodPost, "/api/v1/workspace/domains/shop.test/mailboxes",
		mailboxesBody(true, "a", "b", "c", "d"), http.StatusAccepted))
	if added.Status != "provisioning" || len(added.Mailboxes) != 5 {
		t.Fatalf("added = %+v", added)
	}
	done = h.waitForWorkspaceDomain("shop.test")
	if done.Status != "dkim_required" {
		t.Fatalf("after adding = %+v", done)
	}
	for _, mb := range done.Mailboxes {
		if mb.Status != "created" {
			t.Fatalf("mailbox = %+v", mb)
		}
	}
	if n := google.insertCount("jane@shop.test"); n != 1 {
		t.Fatalf("jane was sent %d times after adding more, want 1", n)
	}
}

func TestWorkspaceMailboxesConnectToInstantly(t *testing.T) {
	h, _, _ := newWorkspaceHarness(t)
	h.connectWorkspace(testServiceAccountKey(t))
	h.configureInstantly()

	h.mustRequest(http.MethodPost, "/api/v1/workspace/domains", setupBody("shop.test", true, "jane", "sam", "taken"), http.StatusAccepted)
	h.waitForWorkspaceDomain("shop.test")
	jane, sam := h.mailboxIn("shop.test", "jane@shop.test"), h.mailboxIn("shop.test", "sam@shop.test")

	// Only a created mailbox can be connected.
	h.mustRequest(http.MethodPost, "/api/v1/workspace/mailboxes/"+h.mailboxIn("shop.test", "taken@shop.test").ID+"/instantly", "", http.StatusConflict)

	type connection struct {
		AuthURL   string                  `json:"auth_url"`
		ExpiresAt string                  `json:"expires_at"`
		Mailbox   workspaceMailboxPayload `json:"mailbox"`
	}
	conn := decodeBody[connection](t, h.mustRequest(http.MethodPost, "/api/v1/workspace/mailboxes/"+jane.ID+"/instantly",
		`{"warmup":true}`, http.StatusOK))
	if !strings.Contains(conn.AuthURL, "login_hint=jane%40shop.test") || conn.Mailbox.InstantlyStatus == nil ||
		*conn.Mailbox.InstantlyStatus != "connecting" {
		t.Fatalf("connection = %+v", conn)
	}
	// Asking again while the session is open hands back the same one.
	again := decodeBody[connection](t, h.mustRequest(http.MethodPost, "/api/v1/workspace/mailboxes/"+jane.ID+"/instantly", "", http.StatusOK))
	if again.AuthURL != conn.AuthURL || h.instantly.Calls("StartGoogleOAuth") != 1 {
		t.Fatalf("a second session was started: %q vs %q", again.AuthURL, conn.AuthURL)
	}

	// Someone signs in as jane: the account is recorded, warmed up and synced.
	h.instantly.CompleteOAuth(sessionOf(conn.AuthURL), "jane@shop.test")
	jane = h.waitForInstantly("shop.test", "jane@shop.test")
	if *jane.InstantlyStatus != "connected" || jane.InstantlyWarmupEnabledAt == nil {
		t.Fatalf("jane = %+v", jane)
	}
	h.mustRequest(http.MethodPost, "/api/v1/workspace/mailboxes/"+jane.ID+"/instantly", "", http.StatusConflict)
	deadline := time.Now().Add(20 * time.Second)
	for {
		accounts := decodeBody[[]map[string]any](t, h.mustRequest(http.MethodGet, "/api/v1/sending-accounts", "", http.StatusOK))
		found := false
		for _, account := range accounts {
			found = found || account["email"] == "jane@shop.test"
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("jane never reached the sending accounts: %v", accounts)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Instantly refuses sam: the reason is kept and a new session can be started.
	conn = decodeBody[connection](t, h.mustRequest(http.MethodPost, "/api/v1/workspace/mailboxes/"+sam.ID+"/instantly", "", http.StatusOK))
	h.instantly.FailOAuth(sessionOf(conn.AuthURL), "account_exists", "Account already exists in another workspace")
	sam = h.waitForInstantly("shop.test", "sam@shop.test")
	if *sam.InstantlyStatus != "failed" || sam.InstantlyError == nil || !strings.Contains(*sam.InstantlyError, "another workspace") {
		t.Fatalf("sam = %+v", sam)
	}
	retry := decodeBody[connection](t, h.mustRequest(http.MethodPost, "/api/v1/workspace/mailboxes/"+sam.ID+"/instantly", "", http.StatusOK))
	if retry.AuthURL == conn.AuthURL {
		t.Fatal("a failed connection handed back its old session")
	}
}
