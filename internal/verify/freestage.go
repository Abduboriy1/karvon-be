package verify

import (
	"context"
	"log/slog"
	"time"

	"github.com/bory/karvon-be/internal/verify/provider"
)

// ExistingProvider adapts the local pipeline onto the provider interface.
//
// It is the one provider that can never be switched off and can never fail: it is
// pure computation over DNS answers that are themselves cached, and every failure
// mode inside it already degrades to a skipped check rather than an error. That is
// what guarantees the weighted average always has at least one contributor.
type ExistingProvider struct {
	pipeline *Pipeline
}

// NewExistingProvider wraps a local pipeline.
func NewExistingProvider(pipeline *Pipeline) *ExistingProvider {
	return &ExistingProvider{pipeline: pipeline}
}

// Key implements provider.Provider.
func (*ExistingProvider) Key() provider.Key { return provider.KeyExisting }

// Check implements provider.Provider. Callers that also need the Pass 1 breakdown
// should use Run and normalize themselves; this exists so the local pipeline is a
// provider like any other.
func (p *ExistingProvider) Check(ctx context.Context, email string) provider.Result {
	return p.Normalize(p.pipeline.Run(ctx, email))
}

// Run exposes the full local result, which the free stage needs because the Pass 1
// check breakdown and the typo suggestion are stored in their own columns.
func (p *ExistingProvider) Run(ctx context.Context, email string) Pass1Result {
	return p.pipeline.Run(ctx, email)
}

// Normalize rescales a local result onto the shared 0-100 scale.
func (p *ExistingProvider) Normalize(result Pass1Result) provider.Result {
	meta := map[string]any{"raw_score": result.Score, "raw_max": p.pipeline.MaxScore()}
	if result.HardFail != "" {
		meta["hard_fail"] = result.HardFail
	}
	if result.TypoSuggestion != "" {
		meta["typo_suggestion"] = result.TypoSuggestion
	}

	if result.HardFail != "" {
		// A hard fail is a fact, not an opinion: the address is not valid syntax,
		// its mailbox is automated, its domain is a throwaway, or the domain has no
		// mail server. No other provider checks for these, so none of them may
		// average one away.
		return provider.Scored(provider.KeyExisting, 0, labelFor(result.HardFail)+" failed").
			Disqualify().
			WithMetadata(meta)
	}

	score := NormalizeExisting(result.Score, p.pipeline.MaxScore())
	reason := "passed the local checks"
	if score < 100 {
		reason = "some local checks did not pass"
	}
	return provider.Scored(provider.KeyExisting, score, reason).WithMetadata(meta)
}

// compile-time proof.
var _ provider.Provider = (*ExistingProvider)(nil)

/* ---------------------------------------------------------------- free stage */

// FreeStage runs the weighted providers for one address and combines their answers.
//
// The providers run in sequence rather than in parallel, cheapest first, because the
// expensive one is an SMTP conversation with a third party's mail server and there
// is no point having it about an address the local checks already disqualified.
type FreeStage struct {
	existing  *ExistingProvider
	providers []provider.Provider
	log       *slog.Logger
}

// FreeStageConfig wires the stage.
type FreeStageConfig struct {
	// Pipeline is the mandatory local scorer.
	Pipeline *Pipeline
	// Extra are the optional providers, in the order they should run.
	Extra []provider.Provider
	Log   *slog.Logger
}

// NewFreeStage builds the free stage.
func NewFreeStage(cfg FreeStageConfig) *FreeStage {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &FreeStage{
		existing:  NewExistingProvider(cfg.Pipeline),
		providers: cfg.Extra,
		log:       log,
	}
}

// FreeResult is everything one pass of the free stage produced.
type FreeResult struct {
	// Pass1 is the local pipeline's full breakdown, stored in its own columns.
	Pass1 Pass1Result
	// Score is the weighted combination.
	Score FreeScore
}

// Run scores one address with every enabled provider and combines the answers.
//
// It never returns an error. A provider that fails contributes nothing and its
// weight moves to the providers that answered, which is the whole point: a Reacher
// outage must degrade the confidence of a score, not the availability of the
// pipeline.
func (s *FreeStage) Run(ctx context.Context, settings Settings, email string) FreeResult {
	pass1 := s.existing.Run(ctx, email)
	results := []provider.Result{s.existing.Normalize(pass1)}

	// A hard fail means the address is not worth another provider's time: it is
	// not valid syntax, it is an automated mailbox, the domain is disposable, or
	// the domain does not resolve. Opening an SMTP conversation about it would be
	// slow, pointless, and mildly antisocial towards the mail server.
	shortCircuit := pass1.HardFail != ""

	for _, p := range s.providers {
		key := p.Key()
		switch {
		case !settings.IsEnabled(key):
			results = append(results, provider.Skipped(key, "switched off in the verification settings"))
		case shortCircuit && !cheapProvider(key):
			results = append(results, provider.Skipped(key,
				"not checked: "+labelFor(pass1.HardFail)+" already failed"))
		default:
			started := time.Now()
			result := p.Check(ctx, email)
			if result.Provider == "" {
				result.Provider = key
			}
			if result.Duration == 0 {
				result.Duration = time.Since(started)
			}
			if result.Status == provider.StatusError || result.Status == provider.StatusUnavailable {
				s.log.Warn("a verification provider did not answer",
					"provider", key, "email", email, "status", result.Status, "error", result.Error)
			}
			results = append(results, result)
		}
	}

	return FreeResult{Pass1: pass1, Score: CombineFree(settings, results)}
}

// cheapProvider reports whether a provider is free of network cost, and so worth
// running even for an address the local checks already rejected. Its answer is still
// useful: MailChecker naming the domain as disposable is a better explanation than
// "the domain does not resolve".
func cheapProvider(key provider.Key) bool {
	return key == provider.KeyMailChecker
}

// Health probes every provider that can be probed, keyed by provider.
func (s *FreeStage) Health(ctx context.Context) map[provider.Key]error {
	out := make(map[provider.Key]error, len(s.providers))
	for _, p := range s.providers {
		checker, ok := p.(provider.HealthChecker)
		if !ok {
			continue
		}
		out[p.Key()] = checker.Health(ctx)
	}
	return out
}
