package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/config"
)

const (
	cloudflareAccount = "0123456789abcdef0123456789abcdef"
	cloudflareToken   = "cf-test-token"
)

// fakeCloudflare is just enough of the Registrar API to buy domains against: a price
// list, a set of names already taken, and the account's own registrations.
type fakeCloudflare struct {
	mu     sync.Mutex
	prices map[string]string
	taken  map[string]bool
	owned  map[string]map[string]any
	// slow names answer 202 and finish after this many status polls.
	slow  map[string]int
	polls map[string]int
	// flaky names are registered but answered with a 500, the case where only
	// asking Cloudflare afterwards tells the truth.
	flaky     map[string]bool
	registers map[string]int
}

func newFakeCloudflare() *fakeCloudflare {
	return &fakeCloudflare{
		prices: map[string]string{
			"karvon.com": "10.11", "karvon.dev": "12.00", "karvon.io": "45.00", "flaky.com": "9.99",
		},
		taken:     map[string]bool{"taken.com": true},
		owned:     map[string]map[string]any{},
		slow:      map[string]int{"karvon.dev": 2},
		polls:     map[string]int{},
		flaky:     map[string]bool{"flaky.com": true},
		registers: map[string]int{},
	}
}

func (f *fakeCloudflare) registerCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registers[name]
}

func (f *fakeCloudflare) own(name string, autoRenew bool) map[string]any {
	reg := map[string]any{
		"domain_name": name, "status": "active", "auto_renew": autoRenew, "locked": true,
		"privacy_mode": "redaction", "created_at": "2026-09-27T10:00:00Z", "expires_at": "2027-09-27T10:00:00Z",
	}
	f.owned[name] = reg
	return reg
}

func (f *fakeCloudflare) offer(name string) map[string]any {
	switch price, priced := f.prices[name]; {
	case f.taken[name] || f.owned[name] != nil:
		return map[string]any{"name": name, "registrable": false, "reason": "domain_unavailable"}
	case priced:
		return map[string]any{"name": name, "registrable": true, "tier": "standard",
			"pricing": map[string]string{"currency": "USD", "registration_cost": price, "renewal_cost": price}}
	default:
		return map[string]any{"name": name, "registrable": false, "reason": "extension_not_supported_via_api"}
	}
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reply := func(status int, result any, info map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		body := map[string]any{"success": status < 300, "errors": []any{}, "messages": []any{}, "result": result}
		if info != nil {
			body["result_info"] = info
		}
		_ = json.NewEncoder(w).Encode(body)
	}
	fail := func(status int, message string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false,
			"errors": []map[string]any{{"code": 1000, "message": message}}})
	}
	if r.Header.Get("Authorization") != "Bearer "+cloudflareToken {
		fail(http.StatusForbidden, "Authentication error")
		return
	}
	prefix := "/accounts/" + cloudflareAccount + "/registrar/"
	path, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		fail(http.StatusNotFound, "account not found")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && path == "domain-search":
		q := r.URL.Query().Get("q")
		reply(http.StatusOK, map[string]any{"domains": []any{f.offer(q + ".com"), f.offer(q + ".dev")}}, nil)

	case r.Method == http.MethodPost && path == "domain-check":
		var body struct {
			Domains []string `json:"domains"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		out := make([]any, 0, len(body.Domains))
		for _, name := range body.Domains {
			out = append(out, f.offer(name))
		}
		reply(http.StatusOK, map[string]any{"domains": out}, nil)

	case r.Method == http.MethodPost && path == "registrations":
		var body struct {
			DomainName string `json:"domain_name"`
			AutoRenew  bool   `json:"auto_renew"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := body.DomainName
		f.registers[name]++
		if f.owned[name] != nil || f.taken[name] {
			fail(http.StatusConflict, name+" is not available")
			return
		}
		if f.flaky[name] {
			f.own(name, body.AutoRenew)
			fail(http.StatusBadGateway, "gateway timeout")
			return
		}
		if _, slow := f.slow[name]; slow {
			f.polls[name] = 0
			reply(http.StatusAccepted, map[string]any{"domain_name": name, "state": "in_progress", "completed": false}, nil)
			return
		}
		reg := f.own(name, body.AutoRenew)
		reply(http.StatusCreated, map[string]any{"domain_name": name, "state": "succeeded", "completed": true,
			"context": map[string]any{"domain_name": name, "registration": reg}}, nil)

	case r.Method == http.MethodGet && strings.HasSuffix(path, "/registration-status"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "registrations/"), "/registration-status")
		if need, slow := f.slow[name]; slow && f.owned[name] == nil {
			if _, started := f.polls[name]; !started {
				fail(http.StatusNotFound, "no registration")
				return
			}
			f.polls[name]++
			if f.polls[name] < need {
				reply(http.StatusOK, map[string]any{"domain_name": name, "state": "in_progress"}, nil)
				return
			}
			f.own(name, false)
		}
		if reg := f.owned[name]; reg != nil {
			reply(http.StatusOK, map[string]any{"domain_name": name, "state": "succeeded", "completed": true,
				"context": map[string]any{"registration": reg}}, nil)
			return
		}
		fail(http.StatusNotFound, "no registration")

	case r.Method == http.MethodGet && path == "registrations":
		out := make([]any, 0, len(f.owned))
		for _, reg := range f.owned {
			out = append(out, reg)
		}
		reply(http.StatusOK, out, map[string]any{"cursor": "", "count": len(out), "per_page": 50})

	case strings.HasPrefix(path, "registrations/"):
		name := strings.TrimPrefix(path, "registrations/")
		reg := f.owned[name]
		if reg == nil {
			fail(http.StatusNotFound, "domain not found")
			return
		}
		if r.Method == http.MethodPatch {
			var body struct {
				AutoRenew bool `json:"auto_renew"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			reg["auto_renew"] = body.AutoRenew
			reply(http.StatusOK, map[string]any{"domain_name": name, "state": "succeeded", "completed": true,
				"context": map[string]any{"registration": reg}}, nil)
			return
		}
		reply(http.StatusOK, reg, nil)

	default:
		fail(http.StatusNotFound, "no route "+r.Method+" "+path)
	}
}

type domainOfferPayload struct {
	Name        string  `json:"name"`
	Registrable bool    `json:"registrable"`
	Purchasable bool    `json:"purchasable"`
	Reason      *string `json:"reason"`
	Pricing     *struct {
		RegistrationCostCents int64 `json:"registration_cost_cents"`
	} `json:"pricing"`
}

type domainPurchasePayload struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	ItemCount         int    `json:"item_count"`
	QuotedTotalCents  int64  `json:"quoted_total_cents"`
	ChargedTotalCents int64  `json:"charged_total_cents"`
	SucceededCount    int    `json:"succeeded_count"`
	InFlightCount     int    `json:"in_flight_count"`
	Items             []struct {
		DomainName string  `json:"domain_name"`
		Status     string  `json:"status"`
		ExpiresAt  *string `json:"expires_at"`
	} `json:"items"`
}

type errorPayload struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"details"`
	} `json:"error"`
}

func newDomainsHarness(t *testing.T) (*harness, *fakeCloudflare) {
	t.Helper()
	fake := newFakeCloudflare()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	h := newHarness(t, nil, withConfig(func(cfg *config.Config) {
		cfg.CloudflareBaseURL = srv.URL
		cfg.DomainPollInterval = 100 * time.Millisecond
	}))
	return h, fake
}

func (h *harness) waitForPurchase(id string) domainPurchasePayload {
	h.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last domainPurchasePayload
	for time.Now().Before(deadline) {
		last = decodeBody[domainPurchasePayload](h.t, h.mustRequest(http.MethodGet, "/api/v1/domains/purchases/"+id, "", http.StatusOK))
		if last.Status != "queued" && last.Status != "processing" {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("purchase %s never settled: %+v", id, last)
	return last
}

func purchaseBody(confirm bool, lines ...string) string {
	return fmt.Sprintf(`{"confirm":%t,"domains":[%s]}`, confirm, strings.Join(lines, ","))
}

func line(name string, cents int64) string {
	return fmt.Sprintf(`{"name":%q,"expected_cost_cents":%d}`, name, cents)
}

func TestDomainsSearchCheckAndBuy(t *testing.T) {
	h, fake := newDomainsHarness(t)

	// Nothing works until the connection is set up.
	settings := decodeBody[map[string]any](t, h.mustRequest(http.MethodGet, "/api/v1/domains/settings", "", http.StatusOK))
	if settings["ready"] != false || settings["max_domains_per_purchase"] != float64(10) {
		t.Fatalf("fresh settings = %v", settings)
	}
	h.mustRequest(http.MethodGet, "/api/v1/domains/search?q=karvon", "", http.StatusConflict)

	settings = decodeBody[map[string]any](t, h.mustRequest(http.MethodPut, "/api/v1/domains/settings",
		fmt.Sprintf(`{"account_id":%q,"api_token":%q,"enabled":true}`, strings.ToUpper(cloudflareAccount), cloudflareToken),
		http.StatusOK))
	if settings["ready"] != true || settings["has_token"] != true || settings["account_id"] != cloudflareAccount {
		t.Fatalf("saved settings = %v", settings)
	}
	if _, leaked := settings["api_token"]; leaked {
		t.Fatal("the API token was returned")
	}
	h.mustRequest(http.MethodPost, "/api/v1/domains/settings/test", "", http.StatusOK)

	// Search, then check.
	search := decodeBody[struct{ Data []domainOfferPayload }](t,
		h.mustRequest(http.MethodGet, "/api/v1/domains/search?q=karvon&limit=5", "", http.StatusOK))
	if len(search.Data) != 2 || !search.Data[0].Purchasable || search.Data[0].Pricing.RegistrationCostCents != 1011 {
		t.Fatalf("search = %+v", search.Data)
	}
	check := decodeBody[struct{ Data []domainOfferPayload }](t, h.mustRequest(http.MethodPost, "/api/v1/domains/check",
		`{"domains":["karvon.com","https://www.KARVON.dev/","taken.com","karvon.com"]}`, http.StatusOK))
	if len(check.Data) != 3 || check.Data[1].Name != "karvon.dev" || !check.Data[1].Purchasable ||
		check.Data[2].Purchasable || check.Data[2].Reason == nil || *check.Data[2].Reason != "domain_unavailable" {
		t.Fatalf("check = %+v", check.Data)
	}

	// The limits hold before Cloudflare is asked anything.
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = line(fmt.Sprintf("shop%d.com", i), 1000)
	}
	h.mustRequest(http.MethodPost, "/api/v1/domains/purchases", purchaseBody(true, eleven...), http.StatusUnprocessableEntity)
	h.mustRequest(http.MethodPost, "/api/v1/domains/purchases", purchaseBody(false, line("karvon.com", 1011)), http.StatusUnprocessableEntity)

	// A price above the confirmed one buys nothing and says which domain moved.
	rec := h.mustRequest(http.MethodPost, "/api/v1/domains/purchases",
		purchaseBody(true, line("karvon.com", 1011), line("karvon.io", 4000)), http.StatusConflict)
	conflict := decodeBody[errorPayload](t, rec)
	if len(conflict.Error.Details) != 1 || conflict.Error.Details[0].Field != "domains[1]" {
		t.Fatalf("price conflict = %+v", conflict)
	}
	if fake.registerCount("karvon.com") != 0 {
		t.Fatal("a refused purchase registered something")
	}

	// A good purchase: one registers at once, one takes a few polls.
	purchase := decodeBody[domainPurchasePayload](t, h.mustRequest(http.MethodPost, "/api/v1/domains/purchases",
		purchaseBody(true, line("karvon.com", 1011), line("karvon.dev", 1200)), http.StatusAccepted))
	if purchase.ItemCount != 2 || purchase.QuotedTotalCents != 2211 {
		t.Fatalf("queued purchase = %+v", purchase)
	}
	// Only one purchase runs at a time.
	h.mustRequest(http.MethodPost, "/api/v1/domains/purchases", purchaseBody(true, line("karvon.io", 4500)), http.StatusConflict)

	done := h.waitForPurchase(purchase.ID)
	if done.Status != "succeeded" || done.SucceededCount != 2 || done.ChargedTotalCents != 2211 || done.InFlightCount != 0 {
		t.Fatalf("settled purchase = %+v", done)
	}
	for _, item := range done.Items {
		if item.Status != "succeeded" || item.ExpiresAt == nil {
			t.Fatalf("item = %+v", item)
		}
	}
	for _, name := range []string{"karvon.com", "karvon.dev"} {
		if n := fake.registerCount(name); n != 1 {
			t.Fatalf("%s was sent to Cloudflare %d times, want 1", name, n)
		}
	}

	// Manage what was bought.
	regs := decodeBody[struct {
		Data []struct {
			DomainName string `json:"domain_name"`
		}
	}](t, h.mustRequest(http.MethodGet, "/api/v1/domains/registrations", "", http.StatusOK))
	if len(regs.Data) != 2 {
		t.Fatalf("registrations = %+v", regs.Data)
	}
	updated := decodeBody[map[string]any](t, h.mustRequest(http.MethodPatch, "/api/v1/domains/registrations/karvon.com",
		`{"auto_renew":true}`, http.StatusOK))
	if updated["auto_renew"] != true {
		t.Fatalf("updated = %v", updated)
	}
	h.mustRequest(http.MethodGet, "/api/v1/domains/registrations/nothere.com", "", http.StatusNotFound)

	list := decodeBody[struct {
		Data []domainPurchasePayload
		Meta struct{ Total int64 }
	}](t, h.mustRequest(http.MethodGet, "/api/v1/domains/purchases", "", http.StatusOK))
	if list.Meta.Total != 1 || len(list.Data[0].Items) != 2 {
		t.Fatalf("purchases = %+v", list)
	}
}

func TestADomainPurchaseWithALostAnswerIsConfirmedNotResent(t *testing.T) {
	h, fake := newDomainsHarness(t)
	h.mustRequest(http.MethodPut, "/api/v1/domains/settings",
		fmt.Sprintf(`{"account_id":%q,"api_token":%q,"enabled":true}`, cloudflareAccount, cloudflareToken), http.StatusOK)

	// One unavailable domain refuses the whole purchase; nothing is sent.
	h.mustRequest(http.MethodPost, "/api/v1/domains/purchases",
		purchaseBody(true, line("flaky.com", 999), line("taken.com", 999)), http.StatusConflict)

	if n := fake.registerCount("flaky.com"); n != 0 {
		t.Fatalf("a refused purchase sent flaky.com %d times", n)
	}

	// flaky.com is registered, but Cloudflare's answer is lost to a 502.
	purchase := decodeBody[domainPurchasePayload](t, h.mustRequest(http.MethodPost, "/api/v1/domains/purchases",
		purchaseBody(true, line("flaky.com", 999)), http.StatusAccepted))
	done := h.waitForPurchase(purchase.ID)
	if done.Status != "succeeded" || done.Items[0].Status != "succeeded" {
		t.Fatalf("settled purchase = %+v", done)
	}
	if n := fake.registerCount("flaky.com"); n != 1 {
		t.Fatalf("flaky.com was sent %d times; a lost answer must be confirmed, not re-sent", n)
	}
}

func TestDomainPurchasesStopWhenTheConnectionIsSwitchedOff(t *testing.T) {
	h, _ := newDomainsHarness(t)
	h.mustRequest(http.MethodPut, "/api/v1/domains/settings",
		fmt.Sprintf(`{"account_id":%q,"api_token":"wrong-token","enabled":true}`, cloudflareAccount), http.StatusOK)

	// A rejected token is a provider_auth failure, not a purchase.
	rec := h.mustRequest(http.MethodPost, "/api/v1/domains/settings/test", "", http.StatusBadGateway)
	if got := decodeBody[errorPayload](t, rec).Error.Code; got != "provider_auth" {
		t.Fatalf("test error code = %q", got)
	}
	h.mustRequest(http.MethodPost, "/api/v1/domains/purchases", purchaseBody(true, line("karvon.com", 1011)), http.StatusBadGateway)

	h.mustRequest(http.MethodPut, "/api/v1/domains/settings", `{"enabled":false}`, http.StatusOK)
	h.mustRequest(http.MethodPost, "/api/v1/domains/check", `{"domains":["karvon.com"]}`, http.StatusConflict)

	// Clearing the token is reported as not ready.
	settings := decodeBody[map[string]any](t, h.mustRequest(http.MethodPut, "/api/v1/domains/settings",
		`{"api_token":null,"enabled":true}`, http.StatusOK))
	if settings["has_token"] != false || settings["ready"] != false {
		t.Fatalf("settings = %v", settings)
	}
}
