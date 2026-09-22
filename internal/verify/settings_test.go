package verify

import (
	"errors"
	"strings"
	"testing"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/verify/provider"
)

// The defaults are the contract with an existing deployment: they must be valid, and
// the free ceiling must stay below the Verified band.
func TestDefaultSettingsAreValid(t *testing.T) {
	settings := DefaultSettings()
	if err := settings.Validate(); err != nil {
		t.Fatalf("the shipped defaults are invalid: %v", err)
	}
	if FreeMaxScore >= MinScoreGreen {
		t.Fatalf("FreeMaxScore %d reaches the green band (%d); only the paid provider may",
			FreeMaxScore, MinScoreGreen)
	}
	if TagFor(FreeMaxScore) != TagLightGreen {
		t.Fatalf("a perfect free score tags as %q, want %q", TagFor(FreeMaxScore), TagLightGreen)
	}
	// Reacher is off by default because of its licence. If that ever changes it
	// should be a deliberate decision, not a drifting default.
	if settings.Enabled[provider.KeyReacher] {
		t.Error("Reacher is enabled by default; its AGPL/commercial licence has to be chosen first")
	}
}

// Every weighted provider in the catalogue must have a default weight, or a fresh
// install would fail its own validation.
func TestCatalogAndDefaultsAgree(t *testing.T) {
	defaults := DefaultSettings()
	for _, key := range FreeProviderKeys() {
		if _, ok := defaults.Weights[key]; !ok {
			t.Errorf("weighted provider %q has no default weight", key)
		}
	}
	seen := map[Key]bool{}
	for _, info := range Catalog {
		if seen[info.Key] {
			t.Errorf("provider %q appears twice in the catalogue", info.Key)
		}
		seen[info.Key] = true
		if info.Label == "" || info.Description == "" {
			t.Errorf("provider %q has no label or description for the settings page", info.Key)
		}
		if info.Stage != StageFree && info.Stage != StagePaid {
			t.Errorf("provider %q has stage %q", info.Key, info.Stage)
		}
	}
}

func TestValidate(t *testing.T) {
	valid := DefaultSettings()

	tests := []struct {
		name      string
		mutate    func(*Settings)
		wantField string
		wantSub   string
	}{
		{
			name:   "the shipped defaults",
			mutate: func(*Settings) {},
		},
		{
			name:      "weights that do not total 100",
			mutate:    func(s *Settings) { s.Weights[provider.KeyReacher] = 40 },
			wantField: "weights",
			wantSub:   "must total 100%",
		},
		{
			name:      "weights that total more than 100",
			mutate:    func(s *Settings) { s.Weights[provider.KeyExisting] = 90 },
			wantField: "weights",
			wantSub:   "must total 100%",
		},
		{
			name:      "a negative weight",
			mutate:    func(s *Settings) { s.Weights[provider.KeyExisting] = -10 },
			wantField: "weights." + provider.KeyExisting,
			wantSub:   "between 0 and 100",
		},
		{
			name:      "a weight above 100",
			mutate:    func(s *Settings) { s.Weights[provider.KeyExisting] = 400 },
			wantField: "weights." + provider.KeyExisting,
			wantSub:   "between 0 and 100",
		},
		{
			name:      "a threshold out of range",
			mutate:    func(s *Settings) { s.PaidThreshold = 140 },
			wantField: "paid_threshold",
			wantSub:   "between 0 and 100",
		},
		{
			name:      "a floor out of range",
			mutate:    func(s *Settings) { s.PaidMinScore = -1 },
			wantField: "paid_min_score",
			wantSub:   "between 0 and 100",
		},
		{
			name: "a floor above the ceiling, which would match nothing",
			mutate: func(s *Settings) {
				s.PaidMinScore = 80
				s.PaidThreshold = 60
			},
			wantField: "paid_min_score",
			wantSub:   "above the paid threshold",
		},
		{
			name: "a floor equal to the ceiling is allowed and simply never matches",
			mutate: func(s *Settings) {
				s.PaidMinScore = 60
				s.PaidThreshold = 60
			},
		},
		{
			name: "all the weight on one provider is unusual but legal",
			mutate: func(s *Settings) {
				s.Weights = map[Key]int{provider.KeyExisting: 100, provider.KeyMailChecker: 0, provider.KeyReacher: 0}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			settings := valid.Clone()
			tc.mutate(&settings)
			err := settings.Validate()

			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate() = nil, want a validation error")
			}

			var appErr *apperr.Error
			if !errors.As(err, &appErr) {
				t.Fatalf("error %v is not an application error", err)
			}
			if appErr.Code != apperr.CodeValidationFailed {
				t.Errorf("code = %q, want %q", appErr.Code, apperr.CodeValidationFailed)
			}
			found := false
			for _, field := range appErr.Fields {
				if field.Field == tc.wantField && strings.Contains(field.Message, tc.wantSub) {
					found = true
				}
			}
			if !found {
				t.Errorf("no field error on %q mentioning %q; got %+v",
					tc.wantField, tc.wantSub, appErr.Fields)
			}
		})
	}
}

// The error message has to tell the operator what the weights currently add up to,
// or "must total 100%" is useless in a form.
func TestValidateExplainsTheTotal(t *testing.T) {
	settings := DefaultSettings()
	settings.Weights[provider.KeyReacher] = 10

	err := settings.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error")
	}

	var appErr *apperr.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("error %v is not an application error", err)
	}
	message := ""
	for _, field := range appErr.Fields {
		if field.Field == "weights" {
			message = field.Message
		}
	}
	if !strings.Contains(message, "60%") {
		t.Errorf("message %q does not report the actual total of 60%%", message)
	}
	// It must also show the operator which weight to change.
	for _, key := range FreeProviderKeys() {
		if !strings.Contains(message, key) {
			t.Errorf("message %q does not mention provider %q", message, key)
		}
	}
}

// A settings row written before a provider existed must still load, which is the
// point of storing weights as an object rather than as columns.
func TestNormalizeFillsMissingProviders(t *testing.T) {
	partial := Settings{
		Weights:       map[Key]int{provider.KeyExisting: 30},
		Enabled:       map[Key]bool{},
		PaidThreshold: 75,
		PaidMinScore:  50,
	}

	filled := partial.Normalize()
	for _, key := range FreeProviderKeys() {
		if _, ok := filled.Weights[key]; !ok {
			t.Errorf("provider %q was not filled in from the defaults", key)
		}
	}
	if filled.Weights[provider.KeyExisting] != 30 {
		t.Errorf("the stored weight was overwritten: %d, want 30", filled.Weights[provider.KeyExisting])
	}
}

// A weight for a provider nobody knows about would silently distort the total, so it
// is dropped rather than counted.
func TestNormalizeDropsUnknownProviders(t *testing.T) {
	settings := DefaultSettings()
	settings.Weights["a-provider-we-removed"] = 40

	filled := settings.Normalize()
	if _, ok := filled.Weights["a-provider-we-removed"]; ok {
		t.Error("an unknown provider kept its weight")
	}
	if err := filled.Validate(); err != nil {
		t.Errorf("the normalized settings are invalid: %v", err)
	}
}

// Clone must be deep, or a cached Settings could be mutated by whoever read it.
func TestCloneIsDeep(t *testing.T) {
	original := DefaultSettings()
	copied := original.Clone()
	copied.Weights[provider.KeyExisting] = 99
	copied.Enabled[provider.KeyReacher] = true

	if original.Weights[provider.KeyExisting] == 99 {
		t.Error("mutating the copy changed the original's weights")
	}
	if original.Enabled[provider.KeyReacher] {
		t.Error("mutating the copy changed the original's flags")
	}
}

// The local pipeline cannot be switched off: it is what stops every other provider
// being asked about rubbish.
func TestExistingProviderIsAlwaysEnabled(t *testing.T) {
	settings := DefaultSettings()
	settings.Enabled[provider.KeyExisting] = false
	if !settings.IsEnabled(provider.KeyExisting) {
		t.Error("the local pipeline can be switched off")
	}
}

func TestQualifiesForPaid(t *testing.T) {
	settings := DefaultSettings() // band: 50 inclusive to 90 exclusive

	tests := []struct {
		score int
		want  bool
	}{
		{0, false},
		{49, false},
		{50, true},
		{60, true},
		{74, true},
		{75, true},
		{FreeMaxScore, true},
		{90, false},
		{100, false},
	}
	for _, tc := range tests {
		if got := settings.QualifiesForPaid(tc.score); got != tc.want {
			t.Errorf("QualifiesForPaid(%d) = %v, want %v", tc.score, got, tc.want)
		}
	}

	off := settings.Clone()
	off.PaidEnabled = false
	if off.QualifiesForPaid(60) {
		t.Error("an address qualified for paid verification while the paid stage is off")
	}
}
