package integration_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/config"
)

// fastAutoSweep runs the automatic self-verify sweep every second, so a test can
// watch it rather than wait the default couple of minutes.
func fastAutoSweep() harnessOption {
	return withConfig(func(c *config.Config) { c.VerifyAutoInterval = time.Second })
}

// listRuns reads the newest verification runs.
func (h *harness) listRuns() []runPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodGet, "/api/v1/verification/runs?per_page=50", "", http.StatusOK)
	return decodeBody[struct {
		Data []runPayload `json:"data"`
	}](h.t, rec).Data
}

// Emails a scrape finds are self-verified without anyone starting a run, and the
// run that did it says it was automatic. Nothing is sent to the paid provider.
func TestAutoSelfVerifyScoresWhatAScrapeFinds(t *testing.T) {
	h := verificationHarness(t, fastAutoSweep())

	if !h.settings().Settings.AutoSelfVerify {
		t.Fatal("auto_self_verify is off on a fresh install, want on")
	}

	deadline := time.Now().Add(60 * time.Second)
	for h.verificationStats().NeedsSelf > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("addresses are still unscored after a minute: %+v", h.verificationStats())
		}
		time.Sleep(200 * time.Millisecond)
	}

	runs := h.listRuns()
	var auto int
	for _, run := range runs {
		if run.Pass != "self" {
			t.Errorf("the sweep started a %q run, want only self runs", run.Pass)
		}
		if !run.Auto {
			t.Errorf("run %s is not marked automatic, but nobody started it", run.ID)
		}
		auto++
	}
	if auto == 0 {
		t.Fatal("the addresses were scored but no automatic run is recorded")
	}

	// Scored addresses are not picked up again: the run history stops growing.
	time.Sleep(3 * time.Second)
	if again := h.listRuns(); len(again) != len(runs) {
		t.Errorf("the sweep kept starting runs with nothing new to score: %d, then %d", len(runs), len(again))
	}
}

// Switching the setting off leaves new emails for a manual run, and an update that
// leaves the field out keeps whatever was stored.
func TestAutoSelfVerifyCanBeSwitchedOff(t *testing.T) {
	h := bareVerificationHarness(t, fastAutoSweep())

	off := h.putSettings(`{
		"weights": {"existing": 30, "mailchecker": 20, "reacher": 50},
		"enabled": {"mailchecker": true, "reacher": false, "paid": true},
		"paid_enabled": true,
		"paid_threshold": 90,
		"paid_min_score": 50,
		"auto_self_verify": false
	}`)
	if off.Settings.AutoSelfVerify {
		t.Fatal("auto_self_verify is still on after switching it off")
	}

	// A client written before the field existed must not switch it back on.
	kept := h.putSettings(`{
		"weights": {"existing": 30, "mailchecker": 20, "reacher": 50},
		"enabled": {"mailchecker": true, "reacher": false, "paid": true},
		"paid_enabled": true,
		"paid_threshold": 90,
		"paid_min_score": 50
	}`)
	if kept.Settings.AutoSelfVerify {
		t.Fatal("an update without auto_self_verify switched it back on")
	}

	h.seedVerificationJob()
	time.Sleep(3 * time.Second)

	if got := h.verificationStats().NeedsSelf; got != 3 {
		t.Errorf("needs_self = %d with automatic verification off, want all 3 left alone", got)
	}
	if runs := h.listRuns(); len(runs) != 0 {
		t.Errorf("%d runs started with automatic verification off, want none", len(runs))
	}
}

// The run cap is a setting on the Maps source, bounded like the column.
func TestSourceRunCapIsASetting(t *testing.T) {
	h := newHarness(t, defaultPages())

	path := "/api/v1/sources/" + apifySourceID
	type sourcePayload struct {
		MaxActiveRuns int `json:"max_active_runs"`
	}

	if got := decodeBody[sourcePayload](t, h.mustRequest(http.MethodGet, path, "", http.StatusOK)); got.MaxActiveRuns != 6 {
		t.Fatalf("max_active_runs = %d on a fresh install, want 6", got.MaxActiveRuns)
	}

	saved := decodeBody[sourcePayload](t, h.mustRequest(http.MethodPut, path, `{"max_active_runs":2}`, http.StatusOK))
	if saved.MaxActiveRuns != 2 {
		t.Fatalf("max_active_runs = %d after saving 2", saved.MaxActiveRuns)
	}

	// Other updates leave it alone.
	other := decodeBody[sourcePayload](t, h.mustRequest(http.MethodPut, path, `{"cost_per_1k_cents":300}`, http.StatusOK))
	if other.MaxActiveRuns != 2 {
		t.Errorf("max_active_runs = %d after an unrelated update, want 2 kept", other.MaxActiveRuns)
	}

	for _, bad := range []string{`{"max_active_runs":0}`, `{"max_active_runs":65}`} {
		if rec := h.request(http.MethodPut, path, bad); rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("PUT %s answered %d, want a validation error", bad, rec.Code)
		}
	}
}
