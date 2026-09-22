package integration_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bory/karvon-be/internal/app"
	"github.com/bory/karvon-be/internal/config"
	"github.com/bory/karvon-be/internal/scraper/provider"
	"github.com/bory/karvon-be/internal/verify"
)

// providerHarness is verificationHarness with Reacher pointed at a test double, so
// the whole four-stage pipeline can be exercised without a container and without
// touching port 25.
func providerHarness(t *testing.T, reacher http.HandlerFunc) *harness {
	t.Helper()

	server := httptest.NewServer(reacher)
	t.Cleanup(server.Close)

	pages := map[string]string{
		"ironworksgym.com/":  `<html><body><a href="mailto:jane.doe@ironworksgym.com">Jane</a></body></html>`,
		"austinbarbell.com/": `<html><body><p>info@austinbarbell.com</p></body></html>`,
		"dallasiron.com/":    `<html><body><p>owner@gmial.com</p></body></html>`,
	}
	h := newHarness(t, pages,
		withConfig(func(cfg *config.Config) {
			cfg.VerifyReacherEnabled = true
			cfg.VerifyReacherURL = server.URL
			cfg.VerifyReacherSecret = "integration-secret"
			// One attempt, no breaker: these tests are about the pipeline's
			// behaviour, and the client's own retry and breaker logic is covered
			// by its unit tests.
			cfg.VerifyReacherRetries = 0
			cfg.VerifyReacherBreakerThreshold = 0
		}),
		withAppOption(app.WithReacherClient(server.Client())),
	)

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
	return h
}

// reacherAlways answers every verification with the same verdict.
func reacherAlways(verdict string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			_, _ = w.Write([]byte(`{"version":"0.11.7"}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"is_reachable":%q,"syntax":{"is_valid_syntax":true},`+
			`"mx":{"accepts_mail":true},"smtp":{"can_connect_smtp":true}}`, verdict)
	}
}

/* ------------------------------------------------------------ the breakdown */

// A stored row has to carry enough to reconstruct its score, or the UI cannot
// explain where a number came from.
func TestFreeStageStoresTheProviderBreakdown(t *testing.T) {
	h := providerHarness(t, reacherAlways("safe"))
	h.runVerification(runAllSelf)

	jane := h.verificationFor("jane.doe@ironworksgym.com")
	detail := h.verificationDetail(jane.ID)

	if len(detail.ProviderResults) != 3 {
		t.Fatalf("the breakdown has %d entries, want one per weighted provider: %+v",
			len(detail.ProviderResults), detail.ProviderResults)
	}

	total := 0.0
	for _, entry := range detail.ProviderResults {
		if entry.Label == "" {
			t.Errorf("provider %q has no label to render", entry.Provider)
		}
		if entry.Status == "scored" && entry.Reason == "" {
			t.Errorf("provider %q scored %d without saying why", entry.Provider, entry.Score)
		}
		total += entry.Contribution
	}

	// The contributions must account for the stored free score, up to the rounding
	// the integer column forces and the cap.
	rounded := int(total + 0.5)
	if rounded > verify.FreeMaxScore {
		rounded = verify.FreeMaxScore
	}
	if rounded != detail.FreeScore {
		t.Errorf("the contributions sum to %d but free_score is %d", rounded, detail.FreeScore)
	}
	if detail.FreeScoredAt == "" {
		t.Error("free_scored_at was not recorded, so the address looks unscored")
	}
}

// Every provider must appear even when it contributed nothing, so the UI draws the
// same rows whatever happened.
func TestFreeStageBreakdownListsSkippedProviders(t *testing.T) {
	h := providerHarness(t, reacherAlways("safe"))
	h.runVerification(runAllSelf)

	// This address hard-fails locally on its role account, so Reacher is never asked.
	info := h.verificationFor("info@austinbarbell.com")
	detail := h.verificationDetail(info.ID)

	reacher := providerResult(t, detail, "reacher")
	if reacher.Status != "skipped" {
		t.Errorf("Reacher status = %q, want skipped for an address that failed locally", reacher.Status)
	}
	if reacher.Reason == "" {
		t.Error("the skip was not explained")
	}
	if reacher.EffectiveWeight != 0 {
		t.Errorf("a skipped provider took effective weight %d", reacher.EffectiveWeight)
	}
	if detail.FreeScore != 0 {
		t.Errorf("free score = %d, want 0 for a hard local failure", detail.FreeScore)
	}
}

/* ---------------------------------------------------------------- outages */

// The promise the whole design rests on: Reacher being down degrades confidence, not
// availability.
func TestReacherOutageDoesNotBreakTheFreeStage(t *testing.T) {
	down := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"backend is on fire"}`))
	}
	h := providerHarness(t, down)

	run := h.runVerification(runAllSelf)
	if run.Status != verify.RunDone {
		t.Fatalf("the run finished as %q (%s); a provider outage must not fail the run",
			run.Status, run.Error)
	}
	if run.Failed != 0 {
		t.Errorf("%d addresses failed although only one provider was down", run.Failed)
	}

	jane := h.verificationFor("jane.doe@ironworksgym.com")
	if jane.FreeScore <= 0 {
		t.Fatalf("free score = %d; an outage must not zero the score", jane.FreeScore)
	}
	if jane.VerificationTag == string(verify.TagRed) {
		t.Errorf("a healthy address tagged red because one provider was down")
	}

	detail := h.verificationDetail(jane.ID)
	reacher := providerResult(t, detail, "reacher")
	if reacher.Status == "scored" {
		t.Error("a failing provider contributed a score")
	}
	if reacher.Error == "" {
		t.Error("the outage was not recorded on the row")
	}
	if reacher.EffectiveWeight != 0 {
		t.Errorf("a failing provider took effective weight %d", reacher.EffectiveWeight)
	}

	// Its weight went to the providers that answered, and they still total 100.
	assigned := 0
	for _, entry := range detail.ProviderResults {
		assigned += entry.EffectiveWeight
	}
	if assigned != 100 {
		t.Errorf("effective weights total %d, want 100", assigned)
	}
}

// "unknown" is Reacher declining to answer — greylisting, a blocked port 25 — and
// must be treated as an absent opinion rather than a bad one.
func TestReacherUnknownIsNotAFailure(t *testing.T) {
	h := providerHarness(t, reacherAlways("unknown"))
	h.runVerification(runAllSelf)

	jane := h.verificationFor("jane.doe@ironworksgym.com")
	detail := h.verificationDetail(jane.ID)

	reacher := providerResult(t, detail, "reacher")
	if reacher.Status != "inconclusive" {
		t.Errorf("status = %q, want inconclusive", reacher.Status)
	}
	if jane.FreeScore <= 0 {
		t.Errorf("free score = %d; an undecided provider must not drag the score down", jane.FreeScore)
	}
}

// A verdict of "invalid" is a real negative and must pull the score down, which is
// the other half of the same contract.
func TestReacherInvalidLowersTheScore(t *testing.T) {
	strict := providerHarness(t, reacherAlways("invalid"))
	strict.runVerification(runAllSelf)
	withInvalid := strict.verificationFor("jane.doe@ironworksgym.com")

	lenient := providerHarness(t, reacherAlways("safe"))
	lenient.runVerification(runAllSelf)
	withSafe := lenient.verificationFor("jane.doe@ironworksgym.com")

	if withInvalid.FreeScore >= withSafe.FreeScore {
		t.Errorf("an invalid verdict scored %d and a safe one %d; the verdict is being ignored",
			withInvalid.FreeScore, withSafe.FreeScore)
	}
}

/* --------------------------------------------------------------- settings */

func TestVerificationSettingsRoundTrip(t *testing.T) {
	h := providerHarness(t, reacherAlways("safe"))

	before := h.settings()
	if before.WeightTotal != verify.WeightTotal {
		t.Errorf("weight_total = %d, want %d", before.WeightTotal, verify.WeightTotal)
	}
	if before.FreeMaxScore != verify.FreeMaxScore {
		t.Errorf("free_max_score = %d, want %d", before.FreeMaxScore, verify.FreeMaxScore)
	}
	if len(before.Providers) != len(verify.Catalog) {
		t.Fatalf("the catalogue has %d entries, want %d", len(before.Providers), len(verify.Catalog))
	}

	// Reacher is enabled in this harness and its double is up, so it must report
	// healthy: this is the readiness the settings page shows.
	for _, p := range before.Providers {
		if p.Provider == "reacher" {
			if p.Healthy == nil || !*p.Healthy {
				t.Errorf("Reacher healthy = %v (%s), want true", p.Healthy, p.Error)
			}
		}
	}

	after := h.putSettings(`{
		"weights": {"existing": 50, "mailchecker": 25, "reacher": 25},
		"enabled": {"mailchecker": true, "reacher": true, "paid": true},
		"paid_enabled": true,
		"paid_threshold": 70,
		"paid_min_score": 40
	}`)
	if after.Settings.Weights["existing"] != 50 || after.Settings.PaidThreshold != 70 {
		t.Fatalf("the settings did not round trip: %+v", after.Settings)
	}

	// And they survive a re-read, which is the point of persisting them.
	reread := h.settings()
	if reread.Settings.Weights["existing"] != 50 || reread.Settings.PaidMinScore != 40 {
		t.Errorf("the settings were not persisted: %+v", reread.Settings)
	}
}

// The weights are what the whole score is built on, so a total that is not 100 has to
// be refused with a field error the form can point at.
func TestVerificationSettingsRejectInvalidWeights(t *testing.T) {
	h := providerHarness(t, reacherAlways("safe"))

	rec := h.request(http.MethodPut, "/api/v1/verification/settings", `{
		"weights": {"existing": 10, "mailchecker": 10, "reacher": 10},
		"enabled": {"mailchecker": true, "reacher": true, "paid": true},
		"paid_enabled": true,
		"paid_threshold": 75,
		"paid_min_score": 50
	}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "weights") {
		t.Errorf("the error does not name the weights: %s", rec.Body.String())
	}

	// A floor above the ceiling would match nothing, so it is a mistake too.
	rec = h.request(http.MethodPut, "/api/v1/verification/settings", `{
		"weights": {"existing": 30, "mailchecker": 20, "reacher": 50},
		"enabled": {"mailchecker": true, "reacher": true, "paid": true},
		"paid_enabled": true,
		"paid_threshold": 40,
		"paid_min_score": 80
	}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an impossible band returned %d, want 422: %s", rec.Code, rec.Body.String())
	}

	// Nothing was written.
	if h.settings().Settings.PaidThreshold == 40 {
		t.Error("a rejected update was persisted anyway")
	}
}

// Changing the weights must change the next score, or the settings page is decorative.
func TestWeightsChangeTheScore(t *testing.T) {
	h := providerHarness(t, reacherAlways("invalid"))

	// Reacher carries almost nothing, so its "invalid" barely dents the score.
	h.putSettings(`{
		"weights": {"existing": 90, "mailchecker": 5, "reacher": 5},
		"enabled": {"mailchecker": true, "reacher": true, "paid": true},
		"paid_enabled": true,
		"paid_threshold": 100,
		"paid_min_score": 50
	}`)
	h.runVerification(runAllSelf)
	lightlyWeighted := h.verificationFor("jane.doe@ironworksgym.com").FreeScore

	// Now Reacher dominates, and the same verdict should cost far more.
	h.putSettings(`{
		"weights": {"existing": 5, "mailchecker": 5, "reacher": 90},
		"enabled": {"mailchecker": true, "reacher": true, "paid": true},
		"paid_enabled": true,
		"paid_threshold": 100,
		"paid_min_score": 50
	}`)
	h.runVerification(runAllSelf)
	heavilyWeighted := h.verificationFor("jane.doe@ironworksgym.com").FreeScore

	if heavilyWeighted >= lightlyWeighted {
		t.Errorf("the score was %d at 5%% Reacher weight and %d at 90%%; the weights are not being applied",
			lightlyWeighted, heavilyWeighted)
	}
}

// Switching a provider off must stop it being consulted at all.
func TestDisablingAProviderTakesEffect(t *testing.T) {
	var calls int
	h := providerHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/check_email" {
			calls++
		}
		_, _ = w.Write([]byte(`{"is_reachable":"safe","syntax":{"is_valid_syntax":true},` +
			`"mx":{"accepts_mail":true},"smtp":{"can_connect_smtp":true}}`))
	})

	h.putSettings(`{
		"weights": {"existing": 30, "mailchecker": 20, "reacher": 50},
		"enabled": {"mailchecker": true, "reacher": false, "paid": true},
		"paid_enabled": true,
		"paid_threshold": 75,
		"paid_min_score": 50
	}`)

	h.runVerification(runAllSelf)
	if calls != 0 {
		t.Errorf("Reacher was called %d times although it is switched off", calls)
	}

	detail := h.verificationDetail(h.verificationFor("jane.doe@ironworksgym.com").ID)
	if got := providerResult(t, detail, "reacher").Status; got != "skipped" {
		t.Errorf("Reacher status = %q, want skipped", got)
	}
}

/* ------------------------------------------------------------- paid gating */

// The ceiling is the opt-in spend control: once an operator lowers it, an address the
// free providers already agree on must never be sent to a paid provider. It ships
// above the free ceiling, so this test sets one rather than relying on the default.
func TestPaidStageSkipsConfidentAddresses(t *testing.T) {
	h := providerHarness(t, reacherAlways("safe"))
	h.configureVerifier()
	h.putSettings(`{
		"weights": {"existing": 30, "mailchecker": 20, "reacher": 50},
		"enabled": {"mailchecker": true, "reacher": true, "paid": true},
		"paid_enabled": true,
		"paid_threshold": 75,
		"paid_min_score": 50
	}`)
	h.runVerification(runAllSelf)

	jane := h.verificationFor("jane.doe@ironworksgym.com")
	if jane.FreeScore < 75 {
		t.Fatalf("free score = %d; this test needs an address above the ceiling it set", jane.FreeScore)
	}

	rec := h.mustRequest(http.MethodPost, "/api/v1/verification/runs/estimate",
		`{"pass":"third_party","filter":{"scope":"all"}}`, http.StatusOK)
	if estimate := decodeBody[estimatePayload](h.t, rec); estimate.Emails != 0 {
		t.Errorf("the estimate covers %d addresses; an address above the threshold must not be billed",
			estimate.Emails)
	}

	// The single-address action must explain the refusal rather than silently
	// queueing a run that skips its only item.
	rec = h.request(http.MethodPost, "/api/v1/verification/emails/"+jane.ID+"/third-party", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "threshold") {
		t.Errorf("the refusal does not mention the threshold: %s", rec.Body.String())
	}
	if h.verifier.Calls() != 0 {
		t.Error("a confident address was sent to the paid provider anyway")
	}
}

// Raising the threshold must bring those addresses back into the paid band, which is
// how an operator buys more certainty.
func TestRaisingTheThresholdWidensThePaidBand(t *testing.T) {
	h := providerHarness(t, reacherAlways("safe"))
	h.configureVerifier()
	h.runVerification(runAllSelf)

	h.openPaidCeiling()

	rec := h.mustRequest(http.MethodPost, "/api/v1/verification/runs/estimate",
		`{"pass":"third_party","filter":{"scope":"all"}}`, http.StatusOK)
	if estimate := decodeBody[estimatePayload](h.t, rec); estimate.Emails != 1 {
		t.Errorf("the estimate covers %d addresses, want the one inside the widened band",
			estimate.Emails)
	}
}

// Switching the paid stage off has to stop it everywhere, not just in bulk runs.
func TestPaidStageCanBeSwitchedOff(t *testing.T) {
	h := providerHarness(t, reacherAlways("risky"))
	h.configureVerifier()
	h.runVerification(runAllSelf)

	h.putSettings(`{
		"weights": {"existing": 30, "mailchecker": 20, "reacher": 50},
		"enabled": {"mailchecker": true, "reacher": true, "paid": false},
		"paid_enabled": false,
		"paid_threshold": 100,
		"paid_min_score": 0
	}`)

	rec := h.mustRequest(http.MethodPost, "/api/v1/verification/runs/estimate",
		`{"pass":"third_party","filter":{"scope":"all"}}`, http.StatusOK)
	if estimate := decodeBody[estimatePayload](h.t, rec); estimate.Emails != 0 {
		t.Errorf("the estimate covers %d addresses although the paid stage is off", estimate.Emails)
	}

	jane := h.verificationFor("jane.doe@ironworksgym.com")
	rec = h.request(http.MethodPost, "/api/v1/verification/emails/"+jane.ID+"/third-party", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if h.verifier.Calls() != 0 {
		t.Error("the paid provider was called while it is switched off")
	}
}
