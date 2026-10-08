package verify

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/verify/provider"
)

// FreeMaxScore is the most the combined free stage can award.
//
// It is deliberately below MinScoreGreen: an address that only free providers have
// seen can never be tagged "Verified", because only a paid third party is contracted
// to stand behind the claim that a mailbox accepts mail. This replaces the old
// Pass1MaxScore of 85 as the invariant that protects the meaning of the green tag;
// Pass1MaxScore still bounds the local pipeline's own raw score.
const FreeMaxScore = 89

// Weight bounds. A weight is a percentage.
const (
	MinWeight = 0
	MaxWeight = 100
	// WeightTotal is what the free providers' weights must add up to. Disabled and
	// unavailable providers are redistributed at scoring time rather than requiring
	// the operator to re-balance, so this holds regardless of what is switched on.
	WeightTotal = 100
)

// ProviderInfo describes one registered provider to the API, so the settings page
// can render controls without knowing the provider list at compile time.
type ProviderInfo struct {
	Key Key
	// Label is the human-readable name.
	Label string
	// Description says what the provider actually determines.
	Description string
	// Stage is "free" or "paid".
	Stage string
	// Weighted reports whether this provider takes part in the weighted score.
	Weighted bool
	// Toggleable reports whether it can be switched off. The local pipeline cannot:
	// it is what stops every other provider being asked about rubbish.
	Toggleable bool
}

// Key aliases the provider key type so callers need only import verify.
type Key = provider.Key

// Stages.
const (
	StageFree = "free"
	StagePaid = "paid"
)

// Catalog is every provider the scoring system knows about, in pipeline order.
//
// Adding a provider is an entry here, an implementation of provider.Provider, and a
// weight in settings. Nothing in the scoring, storage or API layers is keyed to a
// specific provider.
var Catalog = []ProviderInfo{
	{
		Key:         provider.KeyExisting,
		Label:       "Karvon local checks",
		Description: "Syntax, blocklists, DNS records, SPF, DMARC and domain age. Free, runs on every address.",
		Stage:       StageFree,
		Weighted:    true,
		Toggleable:  false,
	},
	{
		Key:         provider.KeyMailChecker,
		Label:       "MailChecker",
		Description: "Syntax validation and a list of ~56 000 disposable mail providers, compiled into the binary.",
		Stage:       StageFree,
		Weighted:    true,
		Toggleable:  true,
	},
	{
		Key:         provider.KeyReacher,
		Label:       "Reacher",
		Description: "A self-hosted SMTP probe: connects to the recipient's mail server and asks whether the mailbox exists.",
		Stage:       StageFree,
		Weighted:    true,
		Toggleable:  true,
	},
	{
		Key:         provider.KeyPaid,
		Label:       "Paid verifier",
		Description: "The contracted third-party API. Costs credits, and is the only provider that can tag an address Verified.",
		Stage:       StagePaid,
		Weighted:    false,
		Toggleable:  true,
	},
}

// FreeProviderKeys lists the weighted providers in pipeline order.
func FreeProviderKeys() []Key {
	out := make([]Key, 0, len(Catalog))
	for _, info := range Catalog {
		if info.Weighted {
			out = append(out, info.Key)
		}
	}
	return out
}

// ProviderLabel returns the human name for a key, falling back to the key itself.
func ProviderLabel(key Key) string {
	for _, info := range Catalog {
		if info.Key == key {
			return info.Label
		}
	}
	return key
}

// Settings is the operator-editable policy for the whole pipeline. It is persisted
// as one row and cached in the service; every field has an environment default so an
// existing deployment keeps behaving exactly as it did before this table existed.
type Settings struct {
	// Weights is the percentage each weighted provider contributes, keyed by
	// provider key. Every weighted provider in the catalog must be present and the
	// values must total WeightTotal.
	Weights map[Key]int
	// Enabled switches a provider off without changing its weight; the weight is
	// redistributed over the providers that did run. The local pipeline is always
	// enabled and is not listed here.
	Enabled map[Key]bool
	// PaidEnabled turns the paid stage off entirely.
	PaidEnabled bool
	// PaidThreshold is the ceiling: an address whose free score reaches it is
	// already confident enough and is never billed. A value above FreeMaxScore
	// disables the ceiling, which is the default: see DefaultSettings.
	PaidThreshold int
	// PaidMinScore is the floor: an address below it is rubbish and is never
	// billed. This is the pre-existing Pass 2 gate and defaults to the same value.
	PaidMinScore int
	// AutoSelfVerify runs the free self pass on its own: a periodic sweep starts a
	// self run over every address that has never been scored, so emails are verified
	// as scrapes find them. The paid pass is never started automatically.
	AutoSelfVerify bool
}

// DefaultSettings is the policy a fresh install starts from. The weights follow the
// brief; Reacher is off because of its licence, not because of its quality.
//
// The paid band runs from the floor to FreeMaxScore inclusive: everything the free
// stage does not rule out is paid for once. The free checks read the domain — MX,
// SPF, DMARC, domain age, the shape of the local part — and none of them prove a
// mailbox accepts mail, so a high free score is where false confidence collects
// (a catch-all domain, or a departed employee at a well-run one) rather than where
// it is safe to skip the check. The floor is where the saving is: a typo caps at
// TypoScoreCap and is never billed.
func DefaultSettings() Settings {
	return Settings{
		Weights: map[Key]int{
			provider.KeyExisting:    30,
			provider.KeyMailChecker: 20,
			provider.KeyReacher:     50,
		},
		Enabled: map[Key]bool{
			provider.KeyMailChecker: true,
			provider.KeyReacher:     false,
			provider.KeyPaid:        true,
		},
		PaidEnabled:    true,
		PaidThreshold:  FreeMaxScore + 1,
		PaidMinScore:   MinScoreYellow,
		AutoSelfVerify: true,
	}
}

// Clone returns a deep copy, so a cached Settings cannot be mutated by a caller.
func (s Settings) Clone() Settings {
	out := s
	out.Weights = make(map[Key]int, len(s.Weights))
	for key, weight := range s.Weights {
		out.Weights[key] = weight
	}
	out.Enabled = make(map[Key]bool, len(s.Enabled))
	for key, on := range s.Enabled {
		out.Enabled[key] = on
	}
	return out
}

// IsEnabled reports whether a provider should run. The local pipeline is always on;
// anything absent from the map is off.
func (s Settings) IsEnabled(key Key) bool {
	if key == provider.KeyExisting {
		return true
	}
	if key == provider.KeyPaid {
		return s.PaidEnabled && s.Enabled[key]
	}
	return s.Enabled[key]
}

// Weight returns the configured weight for a provider, zero when it has none.
func (s Settings) Weight(key Key) int { return s.Weights[key] }

// WeightTotalOf sums every weighted provider's weight, which validation requires to
// be exactly WeightTotal.
func (s Settings) WeightTotalOf() int {
	total := 0
	for _, key := range FreeProviderKeys() {
		total += s.Weights[key]
	}
	return total
}

// Normalize fills in any missing map entries from the defaults, so a settings row
// written before a provider existed still loads. It does not fix invalid values;
// that is Validate's job.
func (s Settings) Normalize() Settings {
	out := s.Clone()
	defaults := DefaultSettings()

	if out.Weights == nil {
		out.Weights = map[Key]int{}
	}
	if out.Enabled == nil {
		out.Enabled = map[Key]bool{}
	}
	for _, key := range FreeProviderKeys() {
		if _, ok := out.Weights[key]; !ok {
			out.Weights[key] = defaults.Weights[key]
		}
	}
	for _, info := range Catalog {
		if !info.Toggleable {
			continue
		}
		if _, ok := out.Enabled[info.Key]; !ok {
			out.Enabled[info.Key] = defaults.Enabled[info.Key]
		}
	}
	// A weight for a provider nobody knows about would silently distort the total.
	for key := range out.Weights {
		if !knownWeighted(key) {
			delete(out.Weights, key)
		}
	}
	return out
}

func knownWeighted(key Key) bool {
	for _, info := range Catalog {
		if info.Key == key && info.Weighted {
			return true
		}
	}
	return false
}

// Validate returns a validation error describing every problem at once, in the
// shared field-error envelope the rest of the API uses.
func (s Settings) Validate() error {
	var fields []apperr.FieldError

	for _, key := range FreeProviderKeys() {
		weight, ok := s.Weights[key]
		if !ok {
			fields = append(fields, apperr.FieldError{
				Field:   "weights." + key,
				Message: "a weight is required for every verification provider",
			})
			continue
		}
		if weight < MinWeight || weight > MaxWeight {
			fields = append(fields, apperr.FieldError{
				Field:   "weights." + key,
				Message: fmt.Sprintf("must be between %d and %d", MinWeight, MaxWeight),
			})
		}
	}

	if total := s.WeightTotalOf(); total != WeightTotal {
		fields = append(fields, apperr.FieldError{
			Field: "weights",
			Message: fmt.Sprintf(
				"the provider weights must total %d%%, but %s total %d%%",
				WeightTotal, strings.Join(weightSummary(s), " + "), total),
		})
	}

	if s.PaidThreshold < 0 || s.PaidThreshold > 100 {
		fields = append(fields, apperr.FieldError{
			Field: "paid_threshold", Message: "must be between 0 and 100",
		})
	}
	if s.PaidMinScore < 0 || s.PaidMinScore > 100 {
		fields = append(fields, apperr.FieldError{
			Field: "paid_min_score", Message: "must be between 0 and 100",
		})
	}
	// A floor above the ceiling matches nothing, so the paid stage would silently
	// never run. That is a configuration mistake, not a policy.
	if s.PaidMinScore >= 0 && s.PaidThreshold >= 0 && s.PaidMinScore > s.PaidThreshold {
		fields = append(fields, apperr.FieldError{
			Field: "paid_min_score",
			Message: fmt.Sprintf(
				"must not be above the paid threshold (%d), or no address would ever qualify",
				s.PaidThreshold),
		})
	}

	if len(fields) > 0 {
		return apperr.Validation("the verification settings are invalid", fields...)
	}
	return nil
}

// weightSummary renders "existing 30% + mailchecker 20%" for the error message, in a
// stable order so the text does not change between requests.
func weightSummary(s Settings) []string {
	keys := FreeProviderKeys()
	sort.SliceStable(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, fmt.Sprintf("%s %d%%", key, s.Weights[key]))
	}
	return out
}

// QualifiesForPaid reports whether an address with this free score should be sent to
// the paid provider.
//
// Both bounds matter and they mean different things. The floor is the pre-existing
// rule: nobody should ever pay to be told that gmial.com is a typo. The ceiling is
// the new one: an address the free providers already agree on is not worth paying to
// confirm.
func (s Settings) QualifiesForPaid(freeScore int) bool {
	if !s.PaidEnabled {
		return false
	}
	return freeScore >= s.PaidMinScore && freeScore < s.PaidThreshold
}
