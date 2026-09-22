package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/http/gen"
	"github.com/bory/karvon-be/internal/verify"
)

func TestGetVerificationSettings(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodGet, "/api/v1/verification/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	body := decodeJSONBody[gen.VerificationSettingsView](t, rec)

	if body.Settings.Weights["existing"] == 0 {
		t.Error("the weights were not returned")
	}
	if body.WeightTotal != verify.WeightTotal {
		t.Errorf("weight_total = %d, want %d", body.WeightTotal, verify.WeightTotal)
	}
	if body.FreeMaxScore != verify.FreeMaxScore {
		t.Errorf("free_max_score = %d, want %d", body.FreeMaxScore, verify.FreeMaxScore)
	}
	if len(body.Providers) != 4 {
		t.Fatalf("providers = %d, want the whole catalogue", len(body.Providers))
	}
}

// The settings page draws its controls from the catalogue, so every entry has to
// carry enough to render one without the frontend knowing the provider list.
func TestGetVerificationSettingsDescribesEveryProvider(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodGet, "/api/v1/verification/settings", "")
	body := decodeJSONBody[gen.VerificationSettingsView](t, rec)

	for _, p := range body.Providers {
		if p.Label == "" || p.Description == "" {
			t.Errorf("provider %q has nothing to render: %+v", p.Provider, p)
		}
		if p.Stage != gen.Free && p.Stage != gen.Paid {
			t.Errorf("provider %q has stage %q", p.Provider, p.Stage)
		}
	}
}

// A provider that is down must say so, and the reason has to reach the operator.
func TestGetVerificationSettingsReportsAnUnhealthyProvider(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodGet, "/api/v1/verification/settings", "")
	body := decodeJSONBody[gen.VerificationSettingsView](t, rec)

	var reacher *gen.VerificationProviderHealth
	for i := range body.Providers {
		if body.Providers[i].Provider == "reacher" {
			reacher = &body.Providers[i]
		}
	}
	if reacher == nil {
		t.Fatal("Reacher is missing from the catalogue")
	}
	if reacher.Healthy == nil || *reacher.Healthy {
		t.Errorf("healthy = %v, want false", reacher.Healthy)
	}
	if reacher.Error == nil || *reacher.Error == "" {
		t.Error("the operator is not told why Reacher is unhealthy")
	}
}

// A provider that cannot be probed must report null rather than a green tick, or the
// UI would claim readiness nobody checked.
func TestGetVerificationSettingsLeavesUnprobedProvidersNull(t *testing.T) {
	handler, _ := newTestServer(t)

	rec := do(t, handler, http.MethodGet, "/api/v1/verification/settings", "")
	body := decodeJSONBody[gen.VerificationSettingsView](t, rec)

	for _, p := range body.Providers {
		if p.Provider == "existing" && p.Healthy != nil {
			t.Errorf("the local pipeline reported healthy = %v; it has no readiness to report", *p.Healthy)
		}
	}
}

func TestUpdateVerificationSettings(t *testing.T) {
	handler, deps := newTestServer(t)

	rec := do(t, handler, http.MethodPut, "/api/v1/verification/settings", `{
		"weights": {"existing": 40, "mailchecker": 10, "reacher": 50},
		"enabled": {"mailchecker": true, "reacher": true, "paid": true},
		"paid_enabled": true,
		"paid_threshold": 80,
		"paid_min_score": 45
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	if len(deps.verification.savedSettings) != 1 {
		t.Fatalf("the service was asked to save %d times, want 1", len(deps.verification.savedSettings))
	}
	saved := deps.verification.savedSettings[0]
	if saved.Weights["existing"] != 40 || saved.Weights["reacher"] != 50 {
		t.Errorf("weights were not forwarded: %+v", saved.Weights)
	}
	if !saved.Enabled["reacher"] {
		t.Error("the Reacher toggle was not forwarded")
	}
	if saved.PaidThreshold != 80 || saved.PaidMinScore != 45 {
		t.Errorf("the paid band was not forwarded: %d..%d", saved.PaidMinScore, saved.PaidThreshold)
	}

	// The response is the saved state, so the client needs one round trip.
	body := decodeJSONBody[gen.VerificationSettingsView](t, rec)
	if body.Settings.PaidThreshold != 80 {
		t.Errorf("the response does not reflect the save: %+v", body.Settings)
	}
}

// Validation lives in the domain, so the handler has to surface its field errors
// rather than a generic 500: the settings form needs to know which control is wrong.
func TestUpdateVerificationSettingsSurfacesValidationErrors(t *testing.T) {
	handler, deps := newTestServer(t)
	deps.verification.saveErr = apperr.Validation("the verification settings are invalid",
		apperr.FieldError{Field: "weights", Message: "the provider weights must total 100%"})

	rec := do(t, handler, http.MethodPut, "/api/v1/verification/settings", `{
		"weights": {"existing": 10, "mailchecker": 10, "reacher": 10},
		"enabled": {},
		"paid_enabled": true,
		"paid_threshold": 75,
		"paid_min_score": 50
	}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if field := decodeError(t, rec).Fields["weights"]; field == "" {
		t.Error("the error does not name the weights field")
	}
}

func TestUpdateVerificationSettingsRejectsAMalformedBody(t *testing.T) {
	handler, deps := newTestServer(t)

	rec := do(t, handler, http.MethodPut, "/api/v1/verification/settings", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(deps.verification.savedSettings) != 0 {
		t.Error("a malformed body reached the service")
	}
}

func TestVerificationSettingsRequireAuth(t *testing.T) {
	handler, _ := newTestServer(t)

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		rec := do(t, handler, method, "/api/v1/verification/settings", "{}",
			func(r *http.Request) { r.Header.Del("Authorization") })
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", method, rec.Code)
		}
	}
}
