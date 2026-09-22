package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/scraper/provider"
	"github.com/bory/karvon-be/internal/verify"
	"github.com/bory/karvon-be/internal/verify/verifier"
)

/* --------------------------------------------------------------- payloads */

type verificationPayload struct {
	ID               string `json:"id"`
	Email            string `json:"email"`
	Pass1Score       int    `json:"pass1_score"`
	Pass1HardFail    string `json:"pass1_hard_fail"`
	Pass1VerifiedAt  string `json:"pass1_verified_at"`
	FreeScore        int    `json:"free_score"`
	FreeScoredAt     string `json:"free_scored_at"`
	Pass2Score       *int   `json:"pass2_score"`
	Pass2Status      string `json:"pass2_status"`
	Pass2VerifiedAt  string `json:"pass2_verified_at"`
	ThirdPartySentAt string `json:"third_party_sent_at"`
	Pass2Credits     int    `json:"pass2_credits"`
	FinalScore       int    `json:"final_score"`
	VerificationTag  string `json:"verification_tag"`
	TagLabel         string `json:"tag_label"`
	TypoSuggestion   string `json:"typo_suggestion"`
	BusinessCount    int    `json:"business_count"`
	Pass1Checks      []struct {
		Key    string `json:"key"`
		Label  string `json:"label"`
		Status string `json:"status"`
		Points int    `json:"points"`
		Max    int    `json:"max"`
		Detail string `json:"detail"`
	} `json:"pass1_checks"`
	ProviderResults []providerResultPayload `json:"provider_results"`
	Businesses      []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"businesses"`
}

type providerResultPayload struct {
	Provider        string  `json:"provider"`
	Label           string  `json:"label"`
	Status          string  `json:"status"`
	Score           int     `json:"score"`
	Weight          int     `json:"weight"`
	EffectiveWeight int     `json:"effective_weight"`
	Contribution    float64 `json:"contribution"`
	Reason          string  `json:"reason"`
	Error           string  `json:"error"`
}

type settingsViewPayload struct {
	Settings struct {
		Weights       map[string]int  `json:"weights"`
		Enabled       map[string]bool `json:"enabled"`
		PaidEnabled   bool            `json:"paid_enabled"`
		PaidThreshold int             `json:"paid_threshold"`
		PaidMinScore  int             `json:"paid_min_score"`
	} `json:"settings"`
	Providers []struct {
		Provider    string `json:"provider"`
		Label       string `json:"label"`
		Stage       string `json:"stage"`
		Weighted    bool   `json:"weighted"`
		Toggleable  bool   `json:"toggleable"`
		Description string `json:"description"`
		Enabled     bool   `json:"enabled"`
		Weight      int    `json:"weight"`
		Healthy     *bool  `json:"healthy"`
		Error       string `json:"error"`
	} `json:"providers"`
	FreeMaxScore int `json:"free_max_score"`
	WeightTotal  int `json:"weight_total"`
}

type verificationListPayload struct {
	Data []verificationPayload `json:"data"`
	Meta struct {
		Total int64 `json:"total"`
	} `json:"meta"`
}

type runPayload struct {
	ID           string `json:"id"`
	Pass         string `json:"pass"`
	Status       string `json:"status"`
	Total        int    `json:"total"`
	Done         int    `json:"done"`
	Failed       int    `json:"failed"`
	Skipped      int    `json:"skipped"`
	CreditsUsed  int    `json:"credits_used"`
	EstCostCents int64  `json:"est_cost_cents"`
	Error        string `json:"error"`
}

type estimatePayload struct {
	Emails         int64  `json:"emails"`
	NeedsSelf      int64  `json:"needs_self"`
	Cached         int64  `json:"cached"`
	CreditsNeeded  int64  `json:"credits_needed"`
	CostPer1kCents int    `json:"cost_per_1k_cents"`
	EstCostCents   int64  `json:"est_cost_cents"`
	BalanceCredits *int64 `json:"balance_credits"`
}

type verificationStatsPayload struct {
	Total int64 `json:"total"`
	ByTag struct {
		Green      int64 `json:"green"`
		LightGreen int64 `json:"light_green"`
		Yellow     int64 `json:"yellow"`
		Orange     int64 `json:"orange"`
		Red        int64 `json:"red"`
	} `json:"by_tag"`
	SelfVerified            int64  `json:"self_verified"`
	ThirdPartyVerified      int64  `json:"third_party_verified"`
	NeedsSelf               int64  `json:"needs_self"`
	QualifyingForThirdParty int64  `json:"qualifying_for_third_party"`
	QualifyingEstCostCents  int64  `json:"qualifying_est_cost_cents"`
	CreditsUsedTotal        int64  `json:"credits_used_total"`
	BalanceCredits          *int64 `json:"balance_credits"`
	ActiveRuns              int64  `json:"active_runs"`
}

/* ----------------------------------------------------------------- helpers */

// verificationHarness seeds a scrape whose crawled pages hand us a deliberate mix:
// a clean personal address, a shared info@ mailbox, and a misspelled free-provider
// domain. Those three drive nearly every rule in the two passes.
func verificationHarness(t *testing.T) *harness {
	t.Helper()

	pages := map[string]string{
		"ironworksgym.com/":  `<html><body><a href="mailto:jane.doe@ironworksgym.com">Jane</a></body></html>`,
		"austinbarbell.com/": `<html><body><p>info@austinbarbell.com</p></body></html>`,
		"dallasiron.com/":    `<html><body><p>owner@gmial.com</p></body></html>`,
	}
	h := newHarness(t, pages)

	// Only the two real business domains resolve; the typo'd one does not, which is
	// exactly the situation the suggestion has to survive.
	h.resolver.healthy("ironworksgym.com", "austinbarbell.com")

	h.provider.ByQuery = map[string][]provider.Listing{
		"gyms in Austin, TX": {
			listingFor("place-iron-works", "Iron Works Gym", "Austin", "ironworksgym.com"),
			listingFor("place-barbell-club", "Austin Barbell Club", "Austin", "austinbarbell.com"),
		},
		"gyms in Dallas, TX": {
			listingFor("place-dallas-iron", "Dallas Iron Athletics", "Dallas", "dallasiron.com"),
		},
	}
	h.configureSource()

	job := h.waitForJob(h.createJob("Verify seed", []string{"gyms"}, []string{"Austin", "Dallas"}, true).ID)
	if job.Status != "done" {
		t.Fatalf("the seed job finished as %q (%s)", job.Status, job.Error)
	}
	if job.Stats.EmailsFound != 3 {
		t.Fatalf("the seed job found %d addresses, want 3", job.Stats.EmailsFound)
	}
	return h
}

// configureVerifier stores a key and enables the email verifier source.
func (h *harness) configureVerifier() {
	h.t.Helper()
	h.mustRequest(http.MethodPut, "/api/v1/sources/"+verifierSourceID,
		`{"api_key":"integration-verifier-key","enabled":true,"cost_per_1k_cents":500}`,
		http.StatusOK)
}

// runVerification starts a run and waits for it to reach a terminal status.
func (h *harness) runVerification(body string) runPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodPost, "/api/v1/verification/runs", body, http.StatusCreated)
	return h.waitForRun(decodeBody[runPayload](h.t, rec).ID)
}

func (h *harness) waitForRun(id string) runPayload {
	h.t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	var last runPayload
	for time.Now().Before(deadline) {
		rec := h.mustRequest(http.MethodGet, "/api/v1/verification/runs/"+id, "", http.StatusOK)
		last = decodeBody[runPayload](h.t, rec)
		switch last.Status {
		case verify.RunDone, verify.RunFailed, verify.RunCancelled:
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("verification run %s never finished, last state: %+v", id, last)
	return last
}

// verificationFor finds one address in the list.
func (h *harness) verificationFor(email string) verificationPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodGet,
		"/api/v1/verification/emails?q="+strings.ReplaceAll(email, "@", "%40"), "", http.StatusOK)
	list := decodeBody[verificationListPayload](h.t, rec)
	for _, row := range list.Data {
		if row.Email == email {
			return row
		}
	}
	h.t.Fatalf("no verification row for %q (%d rows returned)", email, len(list.Data))
	return verificationPayload{}
}

func (h *harness) verificationDetail(id string) verificationPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodGet, "/api/v1/verification/emails/"+id, "", http.StatusOK)
	return decodeBody[verificationPayload](h.t, rec)
}

func (h *harness) verificationStats() verificationStatsPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodGet, "/api/v1/verification/stats", "", http.StatusOK)
	return decodeBody[verificationStatsPayload](h.t, rec)
}

// settings reads the current verification settings.
func (h *harness) settings() settingsViewPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodGet, "/api/v1/verification/settings", "", http.StatusOK)
	return decodeBody[settingsViewPayload](h.t, rec)
}

// putSettings writes settings built from the current ones with one field changed.
func (h *harness) putSettings(body string) settingsViewPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodPut, "/api/v1/verification/settings", body, http.StatusOK)
	return decodeBody[settingsViewPayload](h.t, rec)
}

// openPaidCeiling pins the confidence threshold at 100 so the paid stage is gated
// only by its floor, whatever the shipped default happens to be. The tests below are
// about the floor, the cache and the cost approval;
// TestPaidStageSkipsConfidentAddresses covers the ceiling itself, and sets its own.
func (h *harness) openPaidCeiling() {
	h.t.Helper()
	h.putSettings(`{
		"weights": {"existing": 30, "mailchecker": 20, "reacher": 50},
		"enabled": {"mailchecker": true, "reacher": false, "paid": true},
		"paid_enabled": true,
		"paid_threshold": 100,
		"paid_min_score": 50
	}`)
}

// providerResult picks one provider out of a stored breakdown.
func providerResult(t *testing.T, row verificationPayload, key string) providerResultPayload {
	t.Helper()
	for _, entry := range row.ProviderResults {
		if entry.Provider == key {
			return entry
		}
	}
	t.Fatalf("provider %q is missing from the breakdown of %s", key, row.Email)
	return providerResultPayload{}
}

const runAllSelf = `{"pass":"self","filter":{"scope":"all"}}`

/* -------------------------------------------------------------------- tests */

// The local pass must score the whole master list without any network call, and the
// three seeded addresses must land in three different bands.
func TestSelfVerificationScoresTheMasterList(t *testing.T) {
	h := verificationHarness(t)

	before := h.verificationStats()
	if before.NeedsSelf != 3 {
		t.Fatalf("needs_self = %d before any run, want 3", before.NeedsSelf)
	}

	run := h.runVerification(runAllSelf)
	if run.Status != verify.RunDone {
		t.Fatalf("run finished as %q (%s)", run.Status, run.Error)
	}
	if run.Total != 3 || run.Done != 3 {
		t.Fatalf("run counters = %+v, want 3 of 3 done", run)
	}
	if run.CreditsUsed != 0 || run.EstCostCents != 0 {
		t.Fatalf("the free pass reported cost %+v", run)
	}

	// A clean personal address on a healthy domain: every local point.
	jane := h.verificationFor("jane.doe@ironworksgym.com")
	if jane.Pass1Score != verify.Pass1MaxScore-verify.PointsDomainAge {
		t.Errorf("jane pass1 = %d, want %d with RDAP disabled",
			jane.Pass1Score, verify.Pass1MaxScore-verify.PointsDomainAge)
	}
	if jane.VerificationTag != string(verify.TagLightGreen) {
		t.Errorf("jane tag = %q, want %q; only a third party may reach green",
			jane.VerificationTag, verify.TagLightGreen)
	}
	if jane.TagLabel != "Likely valid" {
		t.Errorf("jane label = %q, want the text that goes with the colour", jane.TagLabel)
	}

	// A shared mailbox: a hard fail in the default mode.
	info := h.verificationFor("info@austinbarbell.com")
	if info.Pass1Score != 0 || info.VerificationTag != string(verify.TagRed) {
		t.Errorf("info@ scored %d / %q, want 0 / red in hard role mode",
			info.Pass1Score, info.VerificationTag)
	}
	if info.Pass1HardFail != verify.CheckRoleAccount {
		t.Errorf("info@ hard fail = %q, want %q", info.Pass1HardFail, verify.CheckRoleAccount)
	}

	// A misspelled free provider that does not resolve: red, but the correction is
	// still offered, which is the whole point of computing it before the DNS check.
	typo := h.verificationFor("owner@gmial.com")
	if typo.VerificationTag != string(verify.TagRed) {
		t.Errorf("the typo'd address tags as %q, want red", typo.VerificationTag)
	}
	if typo.TypoSuggestion != "owner@gmail.com" {
		t.Errorf("suggestion = %q, want owner@gmail.com", typo.TypoSuggestion)
	}

	after := h.verificationStats()
	if after.NeedsSelf != 0 {
		t.Errorf("needs_self = %d after the run, want 0", after.NeedsSelf)
	}
	if after.SelfVerified != 3 || after.Total != 3 {
		t.Errorf("stats = %+v, want three scored addresses", after)
	}
	if after.ByTag.Red != 2 || after.ByTag.LightGreen != 1 {
		t.Errorf("tag distribution = %+v, want two red and one light green", after.ByTag)
	}
}

// The breakdown is what the UI renders to explain a score, so it must be complete
// and it must add up.
func TestSelfVerificationBreakdownExplainsTheScore(t *testing.T) {
	h := verificationHarness(t)
	h.runVerification(runAllSelf)

	detail := h.verificationDetail(h.verificationFor("jane.doe@ironworksgym.com").ID)

	if len(detail.Pass1Checks) == 0 {
		t.Fatal("the breakdown is empty")
	}
	total := 0
	seen := map[string]bool{}
	for _, check := range detail.Pass1Checks {
		total += check.Points
		seen[check.Key] = true
		if check.Label == "" {
			t.Errorf("check %q has no label", check.Key)
		}
		if check.Points > check.Max {
			t.Errorf("check %q awarded %d of a possible %d", check.Key, check.Points, check.Max)
		}
	}
	if total != detail.Pass1Score {
		t.Errorf("the breakdown sums to %d but the score is %d", total, detail.Pass1Score)
	}
	for _, key := range []string{verify.CheckSyntax, verify.CheckMX, verify.CheckSPF,
		verify.CheckDMARC, verify.CheckDomainAge, verify.CheckTypo} {
		if !seen[key] {
			t.Errorf("the breakdown is missing %q", key)
		}
	}
	if len(detail.Businesses) != 1 {
		t.Errorf("the address lists %d businesses, want 1", len(detail.Businesses))
	}
}

// The paid pass may only ever see addresses the free pass approved.
func TestThirdPartyVerificationOnlyBillsQualifyingAddresses(t *testing.T) {
	h := verificationHarness(t)
	h.configureVerifier()
	h.openPaidCeiling()
	h.runVerification(runAllSelf)

	jane := h.verificationFor("jane.doe@ironworksgym.com")
	h.verifier.ByEmail["jane.doe@ironworksgym.com"] = verifier.StatusDeliverable

	// The estimate has to match what is actually billed.
	rec := h.mustRequest(http.MethodPost, "/api/v1/verification/runs/estimate",
		`{"pass":"third_party","filter":{"scope":"all"}}`, http.StatusOK)
	estimate := decodeBody[estimatePayload](h.t, rec)

	if estimate.Emails != 1 {
		t.Fatalf("estimate covers %d addresses, want only the one that passed the gate", estimate.Emails)
	}
	if estimate.CreditsNeeded != 1 {
		t.Errorf("credits needed = %d, want 1", estimate.CreditsNeeded)
	}
	if estimate.EstCostCents != 0 {
		// One address at 500 cents per 1000 rounds down to nothing.
		t.Logf("one address at %d cents per 1000 costs %d cents", estimate.CostPer1kCents, estimate.EstCostCents)
	}
	if estimate.BalanceCredits == nil || *estimate.BalanceCredits <= 0 {
		t.Errorf("balance = %v, want the provider's reported credit", estimate.BalanceCredits)
	}

	run := h.runVerification(fmt.Sprintf(
		`{"pass":"third_party","filter":{"scope":"all"},"max_cost_cents":%d}`, estimate.EstCostCents))

	if run.Status != verify.RunDone {
		t.Fatalf("run finished as %q (%s)", run.Status, run.Error)
	}
	if run.Total != 1 || run.Done != 1 {
		t.Fatalf("run counters = %+v, want exactly one address processed", run)
	}
	if run.CreditsUsed != 1 {
		t.Errorf("credits used = %d, want 1", run.CreditsUsed)
	}

	// Only the qualifying address was ever sent.
	if h.verifier.Calls() != 1 {
		t.Errorf("the provider was called %d times, want once", h.verifier.Calls())
	}
	if h.verifier.CallsFor("info@austinbarbell.com") != 0 {
		t.Error("a role account that failed the local pass was sent to the paid provider")
	}
	if h.verifier.CallsFor("owner@gmial.com") != 0 {
		t.Error("a typo'd address was sent to the paid provider")
	}

	verified := h.verificationDetail(jane.ID)
	if verified.Pass2Status != string(verifier.StatusDeliverable) {
		t.Errorf("pass2 status = %q, want deliverable", verified.Pass2Status)
	}
	if verified.FinalScore != verify.ScoreDeliverable {
		t.Errorf("final score = %d, want %d", verified.FinalScore, verify.ScoreDeliverable)
	}
	if verified.VerificationTag != string(verify.TagGreen) {
		t.Errorf("tag = %q, want green once the provider confirmed it", verified.VerificationTag)
	}
	if verified.Pass2VerifiedAt == "" {
		t.Error("pass2_verified_at was not recorded, so the verdict was not stored")
	}
	if verified.ThirdPartySentAt == "" {
		t.Error("third_party_sent_at was not recorded, so the address could be sent a second time")
	}
}

// An address reaches a third party once in its life. Running the paid pass again
// must not call the provider, must not bill, and must say so rather than skipping
// quietly. Nothing expires this: there is no window to wait out.
func TestThirdPartyVerificationNeverSendsAnAddressTwice(t *testing.T) {
	h := verificationHarness(t)
	h.configureVerifier()
	h.openPaidCeiling()
	h.runVerification(runAllSelf)
	h.runVerification(`{"pass":"third_party","filter":{"scope":"all"},"max_cost_cents":1000}`)

	callsAfterFirst := h.verifier.Calls()
	if callsAfterFirst == 0 {
		t.Fatal("the first paid run called nothing")
	}

	rec := h.mustRequest(http.MethodPost, "/api/v1/verification/runs/estimate",
		`{"pass":"third_party","filter":{"scope":"all"}}`, http.StatusOK)
	estimate := decodeBody[estimatePayload](h.t, rec)
	if estimate.Emails != 0 {
		t.Errorf("the estimate still covers %d addresses after they were verified", estimate.Emails)
	}
	if estimate.Cached != 1 {
		t.Errorf("cached = %d, want the one address that has had its send", estimate.Cached)
	}

	second := h.runVerification(`{"pass":"third_party","filter":{"scope":"all"},"max_cost_cents":1000}`)
	if second.CreditsUsed != 0 {
		t.Errorf("the second run spent %d credits", second.CreditsUsed)
	}
	if h.verifier.Calls() != callsAfterFirst {
		t.Errorf("the provider was called %d more times for addresses that had already been sent",
			h.verifier.Calls()-callsAfterFirst)
	}

	// The single-address action explains the refusal rather than silently skipping.
	jane := h.verificationFor("jane.doe@ironworksgym.com")
	rec = h.request(http.MethodPost, "/api/v1/verification/emails/"+jane.ID+"/third-party", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-sending an address returned %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "never sent to one twice") {
		t.Errorf("the refusal does not explain the one-send rule: %s", rec.Body.String())
	}
}

// A paid run is confirmed against a cost. If the real cost has grown since the
// operator saw the dialog, the run must be refused rather than overspend.
func TestThirdPartyRunRefusesToExceedTheApprovedCost(t *testing.T) {
	h := verificationHarness(t)
	h.configureVerifier()
	h.openPaidCeiling()
	h.runVerification(runAllSelf)

	// A run costing anything at all cannot be approved for nothing, so price the
	// source high enough that one address is billable.
	h.mustRequest(http.MethodPut, "/api/v1/sources/"+verifierSourceID,
		`{"cost_per_1k_cents":1000000}`, http.StatusOK)

	rec := h.request(http.MethodPost, "/api/v1/verification/runs",
		`{"pass":"third_party","filter":{"scope":"all"},"max_cost_cents":1}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("an underfunded run returned %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if h.verifier.Calls() != 0 {
		t.Error("a refused run still called the provider")
	}

	// And the confirmation is mandatory.
	rec = h.request(http.MethodPost, "/api/v1/verification/runs",
		`{"pass":"third_party","filter":{"scope":"all"}}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an unconfirmed paid run returned %d, want 422: %s", rec.Code, rec.Body.String())
	}
}

// A provider outage must leave Pass 1 and the API intact.
//
// The run itself does not finish quickly, and that is the designed behaviour: a
// transient failure is retried with exponential backoff rather than thrown away.
// What matters is that nothing else degrades while those retries are pending.
func TestProviderOutageDoesNotBreakVerification(t *testing.T) {
	h := verificationHarness(t)
	h.configureVerifier()
	h.openPaidCeiling()
	h.runVerification(runAllSelf)

	h.verifier.Err = fmt.Errorf("connection refused")

	rec := h.mustRequest(http.MethodPost, "/api/v1/verification/runs",
		`{"pass":"third_party","filter":{"scope":"all"},"max_cost_cents":1000}`, http.StatusCreated)
	run := decodeBody[runPayload](h.t, rec)

	// Give the worker time to fail its first attempt and schedule a retry.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if h.verifier.Calls() > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if h.verifier.Calls() == 0 {
		t.Fatal("the paid worker never attempted a call")
	}

	// Nothing was billed for a call that failed.
	current := decodeBody[runPayload](h.t,
		h.mustRequest(http.MethodGet, "/api/v1/verification/runs/"+run.ID, "", http.StatusOK))
	if current.CreditsUsed != 0 {
		t.Errorf("a failing provider was billed %d credits", current.CreditsUsed)
	}

	// The free score stands and every read endpoint still answers.
	jane := h.verificationFor("jane.doe@ironworksgym.com")
	if jane.Pass1Score == 0 {
		t.Error("the local score was lost when the provider failed")
	}
	if jane.FreeScore == 0 {
		t.Error("the free score was lost when the paid provider failed")
	}
	if jane.FinalScore != jane.FreeScore {
		t.Errorf("final score = %d, want the free score %d to stand", jane.FinalScore, jane.FreeScore)
	}
	if jane.VerificationTag != string(verify.TagLightGreen) {
		t.Errorf("tag = %q, want the local tag to stand", jane.VerificationTag)
	}
	h.mustRequest(http.MethodGet, "/api/v1/verification/stats", "", http.StatusOK)
	h.mustRequest(http.MethodGet, "/api/v1/verification/emails", "", http.StatusOK)
	h.mustRequest(http.MethodGet, "/api/v1/businesses", "", http.StatusOK)

	// A stuck run can always be abandoned rather than waited out.
	rec = h.mustRequest(http.MethodPost, "/api/v1/verification/runs/"+run.ID+"/cancel", "", http.StatusOK)
	if cancelled := decodeBody[runPayload](h.t, rec); cancelled.Status != verify.RunCancelled {
		t.Errorf("run status = %q after cancelling, want %q", cancelled.Status, verify.RunCancelled)
	}

	// And the free pass still runs while the paid provider is down.
	if again := h.runVerification(runAllSelf); again.Status != verify.RunDone {
		t.Errorf("the local pass finished as %q during a provider outage", again.Status)
	}
}

// A rejected key stops the whole run instead of burning attempts on every address.
func TestRejectedKeyFailsTheRunImmediately(t *testing.T) {
	h := verificationHarness(t)
	h.configureVerifier()
	h.openPaidCeiling()
	h.runVerification(runAllSelf)

	h.verifier.Err = verifier.ErrAuth

	run := h.runVerification(`{"pass":"third_party","filter":{"scope":"all"},"max_cost_cents":1000}`)
	if run.Status != verify.RunFailed {
		t.Fatalf("run finished as %q, want failed on a rejected key", run.Status)
	}
	if !strings.Contains(run.Error, "key") {
		t.Errorf("run error = %q, want it to name the key", run.Error)
	}
}

// Our own failures must not spend an address's single send. A rejected key means the
// provider never looked at the address, so the claim is handed back and the address
// is still waiting for its one genuine check.
func TestARejectedKeyDoesNotSpendTheSend(t *testing.T) {
	h := verificationHarness(t)
	h.configureVerifier()
	h.openPaidCeiling()
	h.runVerification(runAllSelf)

	rec := h.mustRequest(http.MethodPost, "/api/v1/verification/runs/estimate",
		`{"pass":"third_party","filter":{"scope":"all"}}`, http.StatusOK)
	before := decodeBody[estimatePayload](h.t, rec).Emails
	if before == 0 {
		t.Fatal("nothing qualified for a paid run, so there is nothing to protect")
	}

	h.verifier.Err = verifier.ErrAuth
	h.runVerification(`{"pass":"third_party","filter":{"scope":"all"},"max_cost_cents":1000}`)

	jane := h.verificationFor("jane.doe@ironworksgym.com")
	if jane.ThirdPartySentAt != "" {
		t.Error("a rejected key burned the address's send; the provider never answered about it")
	}

	h.verifier.Err = nil
	rec = h.mustRequest(http.MethodPost, "/api/v1/verification/runs/estimate",
		`{"pass":"third_party","filter":{"scope":"all"}}`, http.StatusOK)
	if after := decodeBody[estimatePayload](h.t, rec).Emails; after != before {
		t.Errorf("%d addresses qualify after a rejected key, want the original %d", after, before)
	}
}

// The same, for a provider that is simply down. The claim is taken immediately
// before the call and handed straight back when the call fails, so the address is
// still unsent while the retries are pending — it must not be burned by an outage
// that never produced an answer about it.
func TestAProviderOutageDoesNotSpendTheSend(t *testing.T) {
	h := verificationHarness(t)
	h.configureVerifier()
	h.openPaidCeiling()
	h.runVerification(runAllSelf)

	h.verifier.Err = fmt.Errorf("connection refused")

	rec := h.mustRequest(http.MethodPost, "/api/v1/verification/runs",
		`{"pass":"third_party","filter":{"scope":"all"},"max_cost_cents":1000}`, http.StatusCreated)
	run := decodeBody[runPayload](h.t, rec)

	// A transient failure is retried with backoff, so the run stays open. What is
	// asserted here is the state it leaves behind between attempts.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && h.verifier.Calls() == 0 {
		time.Sleep(100 * time.Millisecond)
	}
	if h.verifier.Calls() == 0 {
		t.Fatal("the paid worker never attempted a call")
	}

	if jane := h.verificationFor("jane.doe@ironworksgym.com"); jane.ThirdPartySentAt != "" {
		t.Error("the address was marked as sent after a failed call; only an answer from the provider may do that")
	}

	h.mustRequest(http.MethodPost, "/api/v1/verification/runs/"+run.ID+"/cancel", "", http.StatusOK)
	h.verifier.Err = nil

	// Still eligible, because nothing ever answered about it.
	rec = h.mustRequest(http.MethodPost, "/api/v1/verification/runs/estimate",
		`{"pass":"third_party","filter":{"scope":"all"}}`, http.StatusOK)
	if estimate := decodeBody[estimatePayload](h.t, rec); estimate.Emails == 0 {
		t.Errorf("no address qualifies after an outage: %+v", estimate)
	}
}

// An address the provider cannot decide on keeps its free score and stores no
// verdict — but it has still been sent, and "unknown" is the provider's answer. It is
// never sent again, because a retry would be a second send.
func TestUnknownVerdictStillSpendsTheOneSend(t *testing.T) {
	h := verificationHarness(t)
	h.configureVerifier()
	h.openPaidCeiling()
	h.runVerification(runAllSelf)

	h.verifier.ByEmail["jane.doe@ironworksgym.com"] = verifier.StatusUnknown
	h.runVerification(`{"pass":"third_party","filter":{"scope":"all"},"max_cost_cents":1000}`)

	jane := h.verificationFor("jane.doe@ironworksgym.com")
	if jane.Pass2Status != string(verifier.StatusUnknown) {
		t.Errorf("pass2 status = %q, want unknown", jane.Pass2Status)
	}
	if jane.FinalScore != jane.FreeScore {
		t.Errorf("final score = %d, want the free score %d", jane.FinalScore, jane.FreeScore)
	}
	if jane.Pass2VerifiedAt != "" {
		t.Error("an unknown verdict was stored as a verdict; it decides nothing")
	}
	if jane.ThirdPartySentAt == "" {
		t.Fatal("the address was sent to the provider but not marked as sent")
	}

	// A second run must not pick it up: the provider answered, and that answer used
	// up the single send this address is allowed.
	rec := h.mustRequest(http.MethodPost, "/api/v1/verification/runs/estimate",
		`{"pass":"third_party","filter":{"scope":"all"}}`, http.StatusOK)
	if estimate := decodeBody[estimatePayload](h.t, rec); estimate.Emails != 0 {
		t.Errorf("the address is eligible again after an unknown verdict: %+v", estimate)
	}

	calls := h.verifier.Calls()
	h.runVerification(`{"pass":"third_party","filter":{"scope":"all"},"max_cost_cents":1000}`)
	if h.verifier.Calls() != calls {
		t.Errorf("the provider was called again for an address that had already been sent")
	}
}

// Applying a suggestion rewrites the businesses that hold the misspelled address.
func TestApplyTypoCorrectsTheMasterList(t *testing.T) {
	h := verificationHarness(t)
	h.runVerification(runAllSelf)

	typo := h.verificationFor("owner@gmial.com")
	if typo.TypoSuggestion != "owner@gmail.com" {
		t.Fatalf("no suggestion to apply: %+v", typo)
	}
	h.resolver.healthy("gmail.com")

	rec := h.mustRequest(http.MethodPost,
		"/api/v1/verification/emails/"+typo.ID+"/apply-typo", "", http.StatusOK)
	corrected := decodeBody[verificationPayload](h.t, rec)

	if corrected.Email != "owner@gmail.com" {
		t.Fatalf("the corrected row is %q", corrected.Email)
	}

	// The business now carries the corrected address and no longer the typo.
	businesses := decodeBody[businessListPayload](h.t,
		h.mustRequest(http.MethodGet, "/api/v1/businesses?q=dallas", "", http.StatusOK))
	if len(businesses.Data) != 1 {
		t.Fatalf("expected the one Dallas business, got %d", len(businesses.Data))
	}
	if businesses.Data[0].PrimaryEmail != "owner@gmail.com" {
		t.Errorf("the business still holds %q", businesses.Data[0].PrimaryEmail)
	}

	// Applying it twice is refused rather than repeated.
	rec = h.request(http.MethodPost, "/api/v1/verification/emails/"+typo.ID+"/apply-typo", "")
	if rec.Code != http.StatusConflict {
		t.Errorf("re-applying a correction returned %d, want 409", rec.Code)
	}
}

// Filtering and sorting are what make the list usable at scale.
func TestVerificationListFilteringAndSorting(t *testing.T) {
	h := verificationHarness(t)
	h.runVerification(runAllSelf)
	// "needs_third_party" is the paid band, and jane scores above the default
	// ceiling. Open it so the filter has something to find; the band itself is
	// covered by TestPaidStageSkipsConfidentAddresses.
	h.openPaidCeiling()

	tests := []struct {
		name  string
		query string
		want  int64
	}{
		{name: "all", query: "", want: 3},
		{name: "by tag", query: "?tag=red", want: 2},
		{name: "two tags", query: "?tag=red&tag=light_green", want: 3},
		{name: "by score floor", query: "?min_score=50", want: 1},
		{name: "by score ceiling", query: "?max_score=10", want: 2},
		{name: "with a typo", query: "?has_typo=true", want: 1},
		{name: "needing the paid pass", query: "?needs_third_party=true", want: 1},
		{name: "search", query: "?q=jane", want: 1},
		{name: "no match", query: "?q=zzzzzz", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := h.mustRequest(http.MethodGet, "/api/v1/verification/emails"+tt.query, "", http.StatusOK)
			if got := decodeBody[verificationListPayload](h.t, rec).Meta.Total; got != tt.want {
				t.Fatalf("total = %d, want %d", got, tt.want)
			}
		})
	}

	rec := h.mustRequest(http.MethodGet,
		"/api/v1/verification/emails?sort=final_score:desc", "", http.StatusOK)
	rows := decodeBody[verificationListPayload](h.t, rec).Data
	for i := 1; i < len(rows); i++ {
		if rows[i-1].FinalScore < rows[i].FinalScore {
			t.Fatalf("scores are not descending: %d before %d", rows[i-1].FinalScore, rows[i].FinalScore)
		}
	}

	// An unknown sort is a clear error rather than a silent fallback.
	if rec := h.request(http.MethodGet, "/api/v1/verification/emails?sort=nonsense", ""); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("an unknown sort returned %d, want 422", rec.Code)
	}
}

// The business list carries the primary address's tag, which is the column the
// Scraped emails page renders.
func TestBusinessListCarriesTheVerificationTag(t *testing.T) {
	h := verificationHarness(t)
	h.runVerification(runAllSelf)

	rec := h.mustRequest(http.MethodGet, "/api/v1/businesses?q=iron+works", "", http.StatusOK)
	payload := decodeBody[struct {
		Data []struct {
			Name                          string  `json:"name"`
			PrimaryEmailVerifiedStatus    *string `json:"primary_email_verified_status"`
			PrimaryEmailVerificationScore *int    `json:"primary_email_verification_score"`
		} `json:"data"`
	}](h.t, rec)

	if len(payload.Data) != 1 {
		t.Fatalf("expected one business, got %d", len(payload.Data))
	}
	row := payload.Data[0]
	if row.PrimaryEmailVerifiedStatus == nil || *row.PrimaryEmailVerifiedStatus != string(verify.TagLightGreen) {
		t.Errorf("tag = %v, want light_green", row.PrimaryEmailVerifiedStatus)
	}
	if row.PrimaryEmailVerificationScore == nil || *row.PrimaryEmailVerificationScore == 0 {
		t.Errorf("score = %v, want the verified score", row.PrimaryEmailVerificationScore)
	}

	// And the tag filter narrows the master list.
	rec = h.mustRequest(http.MethodGet, "/api/v1/businesses?verification_tag=light_green", "", http.StatusOK)
	if total := decodeBody[businessListPayload](h.t, rec).Meta.Total; total != 1 {
		t.Errorf("the tag filter matched %d businesses, want 1", total)
	}
}

// A scrape must not be able to select the email verifier as its data source.
func TestScrapeRejectsTheVerifierSource(t *testing.T) {
	h := newHarness(t, nil)
	h.configureVerifier()

	body := fmt.Sprintf(
		`{"name":"Wrong source","source_id":%q,"config":{"terms":["gyms"],"locations":[{"city":"Austin","state":"TX"}]}}`,
		verifierSourceID)
	rec := h.request(http.MethodPost, "/api/v1/jobs", body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("creating a job against the verifier returned %d, want 422: %s", rec.Code, rec.Body.String())
	}
}

// The DNS layer is shared by every address, so one domain is resolved once however
// many addresses sit on it.
func TestDNSLookupsAreCachedPerDomain(t *testing.T) {
	h := verificationHarness(t)
	h.runVerification(runAllSelf)

	var cached int
	err := h.app.Store().Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM verification_domains`).Scan(&cached)
	if err != nil {
		t.Fatal(err)
	}
	if cached == 0 {
		t.Fatal("no domain facts were cached")
	}

	var hasMX bool
	if err := h.app.Store().Pool().QueryRow(context.Background(),
		`SELECT has_mx FROM verification_domains WHERE domain = 'ironworksgym.com'`).Scan(&hasMX); err != nil {
		t.Fatal(err)
	}
	if !hasMX {
		t.Error("the cached record does not show the MX records the resolver returned")
	}
}
