package verify

import (
	"encoding/json"
	"fmt"

	"github.com/bory/karvon-be/internal/verify/provider"
)

// ProviderScore is one provider's contribution to a free score, as stored on the row
// and rendered by the UI. It is the normalized result plus the arithmetic that was
// actually applied, so a score can always be reconstructed from what is persisted
// rather than recomputed from live settings that may have changed since.
type ProviderScore struct {
	Provider provider.Key    `json:"provider"`
	Label    string          `json:"label"`
	Status   provider.Status `json:"status"`
	// Score is the provider's normalized 0-100 answer. It is only meaningful when
	// Status is "scored".
	Score int `json:"score"`
	// Weight is the percentage configured for this provider at the time of the run.
	Weight int `json:"weight"`
	// EffectiveWeight is the weight after redistributing the weight of providers
	// that contributed nothing. It is 0 for a provider that did not contribute.
	EffectiveWeight int `json:"effective_weight"`
	// Contribution is Score × EffectiveWeight / 100, the points this provider put
	// into the final free score. The contributions sum to the free score before
	// the cap is applied.
	Contribution float64 `json:"contribution"`
	Reason       string  `json:"reason,omitempty"`
	// Disqualifying reports that this provider found a fact that zeroed the whole
	// free score rather than merely lowering it.
	Disqualifying bool   `json:"disqualifying,omitempty"`
	Error         string `json:"error,omitempty"`
	// Metadata is the small structured detail the provider chose to keep.
	Metadata map[string]any `json:"metadata,omitempty"`
	// DurationMs is how long the provider took, for the operator's benefit.
	DurationMs int64 `json:"duration_ms,omitempty"`
}

// Contributed reports whether this provider took part in the weighted score.
func (p ProviderScore) Contributed() bool { return p.Status.Contributes() }

// FreeScore is the outcome of the weighted free stage.
type FreeScore struct {
	// Score is the combined result, 0 to FreeMaxScore.
	Score int
	// Capped reports whether the cap actually bit, so the UI can explain why a
	// unanimous set of perfect scores did not produce 100.
	Capped bool
	// Disqualified reports that a provider found a fact that zeroed the score
	// outright, whatever the other providers said.
	Disqualified bool
	// Providers is the full breakdown, one entry per weighted provider, in
	// pipeline order. Every provider always appears, including the ones that were
	// switched off, so the breakdown is always complete.
	Providers []ProviderScore
	// Raw is the weighted average before the cap, for tests and for the API.
	Raw float64
}

// Contributors counts the providers that actually produced a score.
func (f FreeScore) Contributors() int {
	n := 0
	for _, p := range f.Providers {
		if p.Contributed() {
			n++
		}
	}
	return n
}

// CombineFree turns the providers' normalized results into one weighted score.
//
// The rule is a weighted average over the providers that produced a score, which is
// what makes a provider outage harmless: a provider that did not answer does not
// score 0, it simply stops being part of the average, and its weight is
// redistributed proportionally over the ones that did. Scoring a missing provider as
// 0 would mean a Reacher outage silently downgraded every address in the database.
//
// results may be in any order and may omit providers; the breakdown is always
// rendered in catalog order with an entry for every weighted provider.
func CombineFree(settings Settings, results []provider.Result) FreeScore {
	settings = settings.Normalize()
	byKey := make(map[provider.Key]provider.Result, len(results))
	for _, result := range results {
		byKey[result.Provider] = result
	}

	keys := FreeProviderKeys()
	scores := make([]ProviderScore, 0, len(keys))
	disqualified := false

	// First pass: render every provider and total the weight that will actually be
	// used as the denominator.
	activeWeight := 0
	contributors := 0
	for _, key := range keys {
		entry := ProviderScore{
			Provider: key,
			Label:    ProviderLabel(key),
			Weight:   settings.Weight(key),
			Status:   provider.StatusSkipped,
		}
		result, ran := byKey[key]
		switch {
		case !ran && !settings.IsEnabled(key):
			entry.Reason = "switched off in the verification settings"
		case !ran:
			entry.Reason = "the provider did not run"
		default:
			entry.Status = result.Status
			entry.Score = result.Score
			entry.Reason = result.Reason
			entry.Disqualifying = result.Disqualifying
			entry.Error = result.Error
			entry.Metadata = result.Metadata
			entry.DurationMs = result.Duration.Milliseconds()
		}
		if entry.Disqualifying {
			disqualified = true
		}
		if entry.Contributed() {
			activeWeight += entry.Weight
			contributors++
		}
		scores = append(scores, entry)
	}

	// A disqualifying fact settles it. Weighting exists to combine opinions about
	// how likely a mailbox is to accept mail; it has no business averaging away an
	// address that is not valid syntax, sits on a throwaway domain, or has no mail
	// server at all. The breakdown is still rendered in full so the UI can show
	// which provider found what, but nothing contributes.
	if disqualified {
		return FreeScore{Score: 0, Disqualified: true, Providers: scores}
	}

	// Second pass: distribute the weight. A provider that contributed nothing keeps
	// its configured Weight for display but takes an EffectiveWeight of 0.
	switch {
	case contributors == 0:
		// Cannot happen while the local pipeline is mandatory and never fails, but
		// a zero here must be an explicit decision rather than a division by zero.
		return FreeScore{Score: 0, Providers: scores}

	case activeWeight == 0:
		// Every provider that answered carries weight 0 — a configuration that
		// would otherwise divide by zero. An equal split is the only answer that
		// uses the information we have rather than throwing it away.
		share := 100 / contributors
		remainder := 100 - share*contributors
		for i := range scores {
			if !scores[i].Contributed() {
				continue
			}
			scores[i].EffectiveWeight = share
			if remainder > 0 {
				scores[i].EffectiveWeight++
				remainder--
			}
		}

	default:
		// Redistribute proportionally, then hand the rounding remainder to the
		// heaviest contributor so the effective weights total exactly 100.
		assigned := 0
		heaviest := -1
		for i := range scores {
			if !scores[i].Contributed() {
				continue
			}
			scores[i].EffectiveWeight = scores[i].Weight * 100 / activeWeight
			assigned += scores[i].EffectiveWeight
			if heaviest < 0 || scores[i].Weight > scores[heaviest].Weight {
				heaviest = i
			}
		}
		if heaviest >= 0 && assigned < 100 {
			scores[heaviest].EffectiveWeight += 100 - assigned
		}
	}

	raw := 0.0
	for i := range scores {
		if !scores[i].Contributed() {
			continue
		}
		scores[i].Contribution = float64(scores[i].Score) * float64(scores[i].EffectiveWeight) / 100
		raw += scores[i].Contribution
	}

	score := int(raw + 0.5)
	capped := false
	if score > FreeMaxScore {
		score, capped = FreeMaxScore, true
	}
	if score < 0 {
		score = 0
	}

	return FreeScore{Score: score, Capped: capped, Providers: scores, Raw: raw}
}

// NormalizeExisting rescales the local pipeline's raw score onto 0-100.
//
// The local pipeline tops out at 85, or 75 when the RDAP domain-age lookup is
// switched off, because those are the points it has to give. That ceiling used to
// double as the guarantee that a local-only address could never be tagged
// "Verified"; that job now belongs to FreeMaxScore, applied once to the combined
// score. So here the raw score is simply rescaled, and a pipeline that awarded
// everything it had reports 100 on its own scale.
func NormalizeExisting(score, maxScore int) int {
	if maxScore <= 0 {
		return 0
	}
	if score <= 0 {
		return 0
	}
	normalized := score * 100 / maxScore
	if normalized > 100 {
		return 100
	}
	return normalized
}

// EncodeProviderScores serializes a breakdown for storage.
func EncodeProviderScores(scores []ProviderScore) ([]byte, error) {
	if scores == nil {
		scores = []ProviderScore{}
	}
	raw, err := json.Marshal(scores)
	if err != nil {
		return nil, fmt.Errorf("verify: encode provider results: %w", err)
	}
	return raw, nil
}

// DecodeProviderScores parses a stored breakdown. An empty column is an empty
// breakdown, not an error: rows written before this column existed have none.
func DecodeProviderScores(raw []byte) ([]ProviderScore, error) {
	if len(raw) == 0 {
		return []ProviderScore{}, nil
	}
	var scores []ProviderScore
	if err := json.Unmarshal(raw, &scores); err != nil {
		return nil, fmt.Errorf("verify: decode provider results: %w", err)
	}
	if scores == nil {
		scores = []ProviderScore{}
	}
	return scores, nil
}

// PaidProviderScore renders a paid verdict in the same shape as the free ones, so
// the UI has a single uniform breakdown to draw. It carries no weight: the paid
// result replaces the free score rather than being averaged with it.
func PaidProviderScore(status Pass2Status, reason string) ProviderScore {
	out := ProviderScore{
		Provider: provider.KeyPaid,
		Label:    ProviderLabel(provider.KeyPaid),
		Reason:   reason,
		Status:   provider.StatusInconclusive,
	}
	if score, conclusive := ScoreFor(status); conclusive {
		out.Status = provider.StatusScored
		out.Score = score
	}
	if status == Pass2Error {
		out.Status = provider.StatusError
	}
	return out
}
