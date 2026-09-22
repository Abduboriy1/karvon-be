package verify

import (
	"errors"
	"math"
	"testing"

	"github.com/bory/karvon-be/internal/verify/provider"
)

// settings builds a settings value with the given weights and everything enabled, so
// each test states only what it is actually about.
func settingsWith(existing, mailchecker, reacher int) Settings {
	return Settings{
		Weights: map[Key]int{
			provider.KeyExisting:    existing,
			provider.KeyMailChecker: mailchecker,
			provider.KeyReacher:     reacher,
		},
		Enabled: map[Key]bool{
			provider.KeyMailChecker: true,
			provider.KeyReacher:     true,
			provider.KeyPaid:        true,
		},
		PaidEnabled:   true,
		PaidThreshold: 75,
		PaidMinScore:  50,
	}
}

func scoreOf(t *testing.T, result FreeScore, key Key) ProviderScore {
	t.Helper()
	for _, entry := range result.Providers {
		if entry.Provider == key {
			return entry
		}
	}
	t.Fatalf("provider %q is missing from the breakdown", key)
	return ProviderScore{}
}

// The worked example from the brief: 85% existing, 100% MailChecker, 72% Reacher at
// 30/20/50 is 25.5 + 20 + 36 = 81.5, which rounds to 82.
func TestCombineFreeWorkedExample(t *testing.T) {
	result := CombineFree(settingsWith(30, 20, 50), []provider.Result{
		provider.Scored(provider.KeyExisting, 85, ""),
		provider.Scored(provider.KeyMailChecker, 100, ""),
		provider.Scored(provider.KeyReacher, 72, ""),
	})

	if math.Abs(result.Raw-81.5) > 0.001 {
		t.Errorf("raw score = %v, want 81.5", result.Raw)
	}
	if result.Score != 82 {
		t.Errorf("score = %d, want 82", result.Score)
	}
	if result.Capped {
		t.Error("the score was capped, but 82 is below the ceiling")
	}
	if result.Contributors() != 3 {
		t.Errorf("contributors = %d, want 3", result.Contributors())
	}

	// The contributions must add up to the score, or the UI cannot explain it.
	total := 0.0
	for _, entry := range result.Providers {
		total += entry.Contribution
	}
	if math.Abs(total-result.Raw) > 0.001 {
		t.Errorf("contributions sum to %v, but the raw score is %v", total, result.Raw)
	}
}

// A unanimous set of perfect free scores must still stop below the Verified band:
// only a paid provider is contracted to stand behind a mailbox.
func TestCombineFreeCapsBelowVerified(t *testing.T) {
	result := CombineFree(settingsWith(30, 20, 50), []provider.Result{
		provider.Scored(provider.KeyExisting, 100, ""),
		provider.Scored(provider.KeyMailChecker, 100, ""),
		provider.Scored(provider.KeyReacher, 100, ""),
	})

	if result.Score != FreeMaxScore {
		t.Errorf("score = %d, want the ceiling %d", result.Score, FreeMaxScore)
	}
	if !result.Capped {
		t.Error("Capped is false, but the ceiling was applied")
	}
	if tag := TagFor(result.Score); tag == TagGreen {
		t.Errorf("a free-only score tagged %q; only the paid provider may award %q", tag, TagGreen)
	}
}

// The central promise of the design: a provider that does not answer must not drag
// the score down. Its weight moves to the providers that did.
func TestCombineFreeRedistributesAnOutage(t *testing.T) {
	down := provider.Unavailable(provider.KeyReacher, errors.New("connection refused"), "backend is down")

	result := CombineFree(settingsWith(30, 20, 50), []provider.Result{
		provider.Scored(provider.KeyExisting, 80, ""),
		provider.Scored(provider.KeyMailChecker, 100, ""),
		down,
	})

	// 30 and 20 of 50 active weight become 60% and 40%.
	existing := scoreOf(t, result, provider.KeyExisting)
	mail := scoreOf(t, result, provider.KeyMailChecker)
	reacher := scoreOf(t, result, provider.KeyReacher)

	if existing.EffectiveWeight != 60 {
		t.Errorf("existing effective weight = %d, want 60", existing.EffectiveWeight)
	}
	if mail.EffectiveWeight != 40 {
		t.Errorf("mailchecker effective weight = %d, want 40", mail.EffectiveWeight)
	}
	if reacher.EffectiveWeight != 0 {
		t.Errorf("an unavailable provider took effective weight %d, want 0", reacher.EffectiveWeight)
	}
	if reacher.Weight != 50 {
		t.Errorf("the configured weight was lost: %d, want 50", reacher.Weight)
	}
	if reacher.Error == "" {
		t.Error("the failure reason was not recorded")
	}

	// 80×0.6 + 100×0.4 = 88. Scoring the outage as 0 would have produced 40.
	if result.Score != 88 {
		t.Errorf("score = %d, want 88; an outage must not be scored as a failure", result.Score)
	}
}

// Reacher answering "unknown" is a declined answer, not a bad one, and must be
// treated exactly like an outage.
func TestCombineFreeIgnoresInconclusive(t *testing.T) {
	result := CombineFree(settingsWith(30, 20, 50), []provider.Result{
		provider.Scored(provider.KeyExisting, 60, ""),
		provider.Scored(provider.KeyMailChecker, 100, ""),
		provider.Inconclusive(provider.KeyReacher, "the mail server gave no usable answer"),
	})

	if got := scoreOf(t, result, provider.KeyReacher).EffectiveWeight; got != 0 {
		t.Errorf("an inconclusive provider took effective weight %d, want 0", got)
	}
	// 60×0.6 + 100×0.4 = 76.
	if result.Score != 76 {
		t.Errorf("score = %d, want 76", result.Score)
	}
}

// A provider switched off in settings behaves like one that is unavailable, and the
// breakdown still lists it so the UI can say why it contributed nothing.
func TestCombineFreeRendersDisabledProviders(t *testing.T) {
	settings := settingsWith(30, 20, 50)
	settings.Enabled[provider.KeyReacher] = false

	result := CombineFree(settings, []provider.Result{
		provider.Scored(provider.KeyExisting, 50, ""),
		provider.Scored(provider.KeyMailChecker, 100, ""),
	})

	if len(result.Providers) != len(FreeProviderKeys()) {
		t.Fatalf("breakdown has %d entries, want one per weighted provider", len(result.Providers))
	}
	reacher := scoreOf(t, result, provider.KeyReacher)
	if reacher.Status != provider.StatusSkipped {
		t.Errorf("status = %q, want %q", reacher.Status, provider.StatusSkipped)
	}
	if reacher.Reason == "" {
		t.Error("a skipped provider must say why it was skipped")
	}
	// 50×0.6 + 100×0.4 = 70.
	if result.Score != 70 {
		t.Errorf("score = %d, want 70", result.Score)
	}
}

// Every provider carrying weight 0 is a configuration that would otherwise divide by
// zero. It must fall back to an equal split rather than crashing or scoring 0.
func TestCombineFreeHandlesZeroActiveWeight(t *testing.T) {
	settings := settingsWith(0, 0, 100)
	settings.Enabled[provider.KeyReacher] = false

	result := CombineFree(settings, []provider.Result{
		provider.Scored(provider.KeyExisting, 40, ""),
		provider.Scored(provider.KeyMailChecker, 100, ""),
	})

	existing := scoreOf(t, result, provider.KeyExisting)
	mail := scoreOf(t, result, provider.KeyMailChecker)
	if existing.EffectiveWeight+mail.EffectiveWeight != 100 {
		t.Errorf("effective weights total %d, want 100",
			existing.EffectiveWeight+mail.EffectiveWeight)
	}
	if result.Score != 70 {
		t.Errorf("score = %d, want an equal split of 40 and 100", result.Score)
	}
}

// Effective weights must always total exactly 100, whatever the rounding, or the
// breakdown will not add up to the stored score.
func TestCombineFreeEffectiveWeightsAlwaysTotal100(t *testing.T) {
	// 33/33/34 does not divide evenly once one provider drops out.
	settings := settingsWith(33, 33, 34)
	result := CombineFree(settings, []provider.Result{
		provider.Scored(provider.KeyExisting, 90, ""),
		provider.Scored(provider.KeyMailChecker, 90, ""),
		provider.Failed(provider.KeyReacher, errors.New("boom")),
	})

	total := 0
	for _, entry := range result.Providers {
		total += entry.EffectiveWeight
	}
	if total != 100 {
		t.Errorf("effective weights total %d, want 100", total)
	}
}

// The local pipeline never fails, so there is always a contributor. The guard exists
// anyway, and must produce 0 rather than panic.
func TestCombineFreeWithNoContributors(t *testing.T) {
	result := CombineFree(settingsWith(30, 20, 50), []provider.Result{
		provider.Failed(provider.KeyExisting, errors.New("impossible")),
		provider.Failed(provider.KeyMailChecker, errors.New("impossible")),
		provider.Failed(provider.KeyReacher, errors.New("impossible")),
	})
	if result.Score != 0 {
		t.Errorf("score = %d, want 0 when nothing contributed", result.Score)
	}
	if result.Contributors() != 0 {
		t.Errorf("contributors = %d, want 0", result.Contributors())
	}
}

// A provider that was never asked at all still has to appear, so the UI always draws
// the same rows.
func TestCombineFreeRendersMissingProviders(t *testing.T) {
	result := CombineFree(settingsWith(30, 20, 50), []provider.Result{
		provider.Scored(provider.KeyExisting, 85, ""),
	})
	if len(result.Providers) != len(FreeProviderKeys()) {
		t.Fatalf("breakdown has %d entries, want %d", len(result.Providers), len(FreeProviderKeys()))
	}
	if result.Score != 85 {
		t.Errorf("score = %d, want the single contributor's score", result.Score)
	}
}

func TestNormalizeExisting(t *testing.T) {
	tests := []struct {
		name       string
		score, max int
		want       int
	}{
		{"a perfect local score is 100 on its own scale", 85, 85, 100},
		{"a perfect score with RDAP disabled is still 100", 75, 75, 100},
		{"half the points is half the scale", 42, 85, 49},
		{"a hard fail is zero", 0, 85, 0},
		{"a negative score cannot happen but must not underflow", -5, 85, 0},
		{"a score above the maximum is clamped", 120, 85, 100},
		{"a zero maximum cannot divide", 10, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeExisting(tc.score, tc.max); got != tc.want {
				t.Errorf("NormalizeExisting(%d, %d) = %d, want %d", tc.score, tc.max, got, tc.want)
			}
		})
	}
}

func TestProviderScoreRoundTrip(t *testing.T) {
	original := []ProviderScore{{
		Provider:        provider.KeyReacher,
		Label:           "Reacher",
		Status:          provider.StatusScored,
		Score:           70,
		Weight:          50,
		EffectiveWeight: 50,
		Contribution:    35,
		Reason:          "the mail server would not confirm the recipient",
		Metadata:        map[string]any{"is_catch_all": true},
	}}

	raw, err := EncodeProviderScores(original)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeProviderScores(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 1 || decoded[0].Provider != provider.KeyReacher || decoded[0].Score != 70 {
		t.Fatalf("round trip lost data: %+v", decoded)
	}

	// Rows written before this column existed have no breakdown, which is not an
	// error.
	empty, err := DecodeProviderScores(nil)
	if err != nil {
		t.Fatalf("decoding an empty column failed: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("an empty column decoded to %d entries, want 0", len(empty))
	}
}

func TestPaidProviderScore(t *testing.T) {
	tests := []struct {
		status     Pass2Status
		wantStatus provider.Status
		wantScore  int
	}{
		{Pass2Deliverable, provider.StatusScored, ScoreDeliverable},
		{Pass2Risky, provider.StatusScored, ScoreRisky},
		{Pass2Undeliverable, provider.StatusScored, ScoreUndeliverable},
		{Pass2Unknown, provider.StatusInconclusive, 0},
		{Pass2Error, provider.StatusError, 0},
	}
	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			got := PaidProviderScore(tc.status, "")
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.Score != tc.wantScore {
				t.Errorf("score = %d, want %d", got.Score, tc.wantScore)
			}
			if got.Weight != 0 {
				t.Errorf("the paid provider carries weight %d; it replaces the free score rather than being averaged with it", got.Weight)
			}
		})
	}
}

// A disqualifying fact must settle the score outright. MailChecker saying "nothing
// against it" is not evidence that a role mailbox is reachable — it never checked —
// so it must not be able to average a hard failure back up into a usable band.
func TestCombineFreeHonoursADisqualifyingFact(t *testing.T) {
	disqualified := provider.Scored(provider.KeyExisting, 0, "Not a role account failed").Disqualify()

	result := CombineFree(settingsWith(30, 20, 50), []provider.Result{
		disqualified,
		provider.Scored(provider.KeyMailChecker, 100, "nothing against it"),
		provider.Scored(provider.KeyReacher, 100, "the mail server accepted the recipient"),
	})

	if result.Score != 0 {
		t.Errorf("score = %d, want 0: a disqualifying fact cannot be averaged away", result.Score)
	}
	if !result.Disqualified {
		t.Error("Disqualified is false, but a provider disqualified the address")
	}
	if TagFor(result.Score) != TagRed {
		t.Errorf("tag = %q, want red", TagFor(result.Score))
	}

	// The breakdown is still complete, so the UI can show which provider found what.
	if len(result.Providers) != len(FreeProviderKeys()) {
		t.Fatalf("breakdown has %d entries, want %d", len(result.Providers), len(FreeProviderKeys()))
	}
	if !scoreOf(t, result, provider.KeyExisting).Disqualifying {
		t.Error("the breakdown does not say which provider disqualified the address")
	}
	for _, entry := range result.Providers {
		if entry.Contribution != 0 {
			t.Errorf("provider %q contributed %v to a disqualified address",
				entry.Provider, entry.Contribution)
		}
	}
}

// Disqualify zeroes the score it is attached to, so a caller cannot leave a
// contradictory score behind.
func TestDisqualifyZeroesTheScore(t *testing.T) {
	result := provider.Scored(provider.KeyMailChecker, 100, "").Disqualify()
	if result.Score != 0 {
		t.Errorf("score = %d, want 0", result.Score)
	}
	if !result.Disqualifying {
		t.Error("Disqualifying was not set")
	}
}
