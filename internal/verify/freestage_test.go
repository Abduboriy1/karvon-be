package verify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/verify/provider"
)

// fakeProvider records what it was asked and answers with a fixed result.
type fakeProvider struct {
	key     provider.Key
	result  provider.Result
	calls   int
	delay   time.Duration
	healthy error
	probed  bool
}

func (f *fakeProvider) Key() provider.Key { return f.key }

func (f *fakeProvider) Check(ctx context.Context, _ string) provider.Result {
	f.calls++
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
		}
	}
	return f.result
}

func (f *fakeProvider) Health(context.Context) error {
	f.probed = true
	return f.healthy
}

// testStage builds a stage over the real local pipeline with a stubbed resolver, so
// the tests exercise the orchestration rather than DNS.
func testStage(t *testing.T, extras ...provider.Provider) *FreeStage {
	t.Helper()
	lists, err := LoadLists("")
	if err != nil {
		t.Fatalf("load lists: %v", err)
	}
	pipeline := NewPipeline(PipelineConfig{
		Lists: lists,
		DNS:   stubLookup{facts: DomainFacts{HasMX: true, HasA: true, HasSPF: true, HasDMARC: true}},
		Age:   DisabledAgeLookup(),
	})
	return NewFreeStage(FreeStageConfig{
		Pipeline: pipeline,
		Extra:    extras,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// stubLookup answers every domain with the same facts.
type stubLookup struct {
	facts DomainFacts
	err   error
}

func (s stubLookup) Lookup(context.Context, string) (DomainFacts, error) { return s.facts, s.err }

func stageSettings() Settings {
	settings := DefaultSettings()
	settings.Enabled[provider.KeyReacher] = true
	return settings
}

func stageScore(t *testing.T, result FreeResult, key Key) ProviderScore {
	t.Helper()
	for _, entry := range result.Score.Providers {
		if entry.Provider == key {
			return entry
		}
	}
	t.Fatalf("provider %q is missing from the breakdown", key)
	return ProviderScore{}
}

// The local pipeline always runs and its full breakdown is preserved alongside the
// weighted score, because it is stored in its own columns.
func TestFreeStageKeepsTheLocalBreakdown(t *testing.T) {
	stage := testStage(t)
	result := stage.Run(context.Background(), stageSettings(), "jane.doe@karvon-example.com")

	if len(result.Pass1.Checks) == 0 {
		t.Fatal("the local breakdown was lost")
	}
	if result.Pass1.Score <= 0 {
		t.Errorf("local score = %d, want a positive score for a healthy domain", result.Pass1.Score)
	}
	if result.Score.Score <= 0 {
		t.Errorf("free score = %d, want a positive score", result.Score.Score)
	}
}

// A Reacher outage must still produce a usable score from the other providers. This
// is the failure mode the whole design exists to survive.
func TestFreeStageSurvivesAProviderOutage(t *testing.T) {
	mail := &fakeProvider{
		key:    provider.KeyMailChecker,
		result: provider.Scored(provider.KeyMailChecker, 100, "nothing against it"),
	}
	down := &fakeProvider{
		key: provider.KeyReacher,
		result: provider.Unavailable(provider.KeyReacher,
			errors.New("connection refused"), "the backend is unreachable"),
	}
	stage := testStage(t, mail, down)

	result := stage.Run(context.Background(), stageSettings(), "jane.doe@karvon-example.com")

	if result.Score.Score <= 0 {
		t.Fatalf("free score = %d; an outage must not zero the score", result.Score.Score)
	}
	entry := stageScore(t, result, provider.KeyReacher)
	if entry.Status != provider.StatusUnavailable {
		t.Errorf("status = %q, want %q", entry.Status, provider.StatusUnavailable)
	}
	if entry.EffectiveWeight != 0 {
		t.Errorf("an unavailable provider took weight %d, want 0", entry.EffectiveWeight)
	}
	if result.Score.Contributors() < 2 {
		t.Errorf("contributors = %d, want the remaining providers to carry the score",
			result.Score.Contributors())
	}
}

// There is no point opening an SMTP conversation about an address the local checks
// already hard-failed: it is slow, useless, and mildly antisocial.
func TestFreeStageSkipsTheNetworkAfterAHardFail(t *testing.T) {
	spy := &fakeProvider{
		key:    provider.KeyReacher,
		result: provider.Scored(provider.KeyReacher, 100, ""),
	}
	stage := testStage(t, spy)

	// Not a valid address at all, so the local pipeline hard-fails on syntax.
	result := stage.Run(context.Background(), stageSettings(), "definitely-not-an-address")

	if spy.calls != 0 {
		t.Errorf("Reacher was called %d times for an address that failed local syntax", spy.calls)
	}
	entry := stageScore(t, result, provider.KeyReacher)
	if entry.Status != provider.StatusSkipped {
		t.Errorf("status = %q, want %q", entry.Status, provider.StatusSkipped)
	}
	if entry.Reason == "" {
		t.Error("the skip was not explained")
	}
	if result.Score.Score != 0 {
		t.Errorf("free score = %d, want 0 for a hard local failure", result.Score.Score)
	}
}

// A hard local failure must survive contact with a provider that scored the address
// highly, because that provider never checked for what failed.
func TestFreeStageHardFailBeatsAHighScoreElsewhere(t *testing.T) {
	optimistic := &fakeProvider{
		key:    provider.KeyMailChecker,
		result: provider.Scored(provider.KeyMailChecker, 100, "nothing against it"),
	}
	stage := testStage(t, optimistic)

	// A shared role mailbox: a hard fail in the default role mode.
	result := stage.Run(context.Background(), stageSettings(), "info@karvon-example.com")

	if result.Pass1.HardFail == "" {
		t.Fatal("the local pipeline did not hard-fail a role mailbox")
	}
	if result.Score.Score != 0 {
		t.Errorf("free score = %d, want 0: a hard failure cannot be averaged away", result.Score.Score)
	}
	if !result.Score.Disqualified {
		t.Error("the result is not marked as disqualified")
	}
}

// MailChecker costs nothing, so it still runs after a hard fail: naming the domain as
// disposable is a better explanation than "the domain does not resolve".
func TestFreeStageStillRunsFreeProvidersAfterAHardFail(t *testing.T) {
	spy := &fakeProvider{
		key:    provider.KeyMailChecker,
		result: provider.Scored(provider.KeyMailChecker, 0, "the domain is a known disposable mail provider"),
	}
	stage := testStage(t, spy)

	stage.Run(context.Background(), stageSettings(), "someone@yopmail.com")
	if spy.calls != 1 {
		t.Errorf("MailChecker was called %d times, want 1: it costs nothing to ask", spy.calls)
	}
}

// A provider switched off must not be called at all, not merely ignored afterwards.
func TestFreeStageDoesNotCallDisabledProviders(t *testing.T) {
	spy := &fakeProvider{
		key:    provider.KeyReacher,
		result: provider.Scored(provider.KeyReacher, 100, ""),
	}
	stage := testStage(t, spy)

	settings := stageSettings()
	settings.Enabled[provider.KeyReacher] = false

	result := stage.Run(context.Background(), settings, "jane.doe@karvon-example.com")

	if spy.calls != 0 {
		t.Errorf("a disabled provider was called %d times", spy.calls)
	}
	if got := stageScore(t, result, provider.KeyReacher).Status; got != provider.StatusSkipped {
		t.Errorf("status = %q, want %q", got, provider.StatusSkipped)
	}
}

// A provider that forgets to set its own key must still be attributed correctly, or
// its result would vanish from the breakdown.
func TestFreeStageStampsTheProviderKey(t *testing.T) {
	sloppy := &fakeProvider{
		key:    provider.KeyReacher,
		result: provider.Result{Status: provider.StatusScored, Score: 80},
	}
	stage := testStage(t, sloppy)

	result := stage.Run(context.Background(), stageSettings(), "jane.doe@karvon-example.com")
	if got := stageScore(t, result, provider.KeyReacher).Score; got != 80 {
		t.Errorf("score = %d, want the provider's answer of 80", got)
	}
}

// A cancelled context must not turn into a panic or a wrong score.
func TestFreeStageWithACancelledContext(t *testing.T) {
	slow := &fakeProvider{
		key:    provider.KeyReacher,
		result: provider.Unavailable(provider.KeyReacher, context.DeadlineExceeded, "timed out"),
		delay:  50 * time.Millisecond,
	}
	stage := testStage(t, slow)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := stage.Run(ctx, stageSettings(), "jane.doe@karvon-example.com")
	if result.Score.Score < 0 || result.Score.Score > FreeMaxScore {
		t.Errorf("free score = %d, outside 0..%d", result.Score.Score, FreeMaxScore)
	}
}

// Health probes only the providers that can be probed; the compiled-in ones have no
// readiness to report and must be absent rather than reported as healthy.
func TestFreeStageHealth(t *testing.T) {
	probeable := &fakeProvider{key: provider.KeyReacher, healthy: errors.New("down")}
	stage := testStage(t, probeable)

	health := stage.Health(context.Background())
	if err, ok := health[provider.KeyReacher]; !ok || err == nil {
		t.Errorf("Reacher health = %v (probed: %v), want the reported failure", err, ok)
	}
	if _, ok := health[provider.KeyExisting]; ok {
		t.Error("the local pipeline reported readiness; it has none to report")
	}
}

// Normalizing the local result has to survive the pipeline's own maximum changing,
// which it does when RDAP is switched off.
func TestExistingProviderNormalizesAgainstTheLivePipelineMaximum(t *testing.T) {
	lists, err := LoadLists("")
	if err != nil {
		t.Fatalf("load lists: %v", err)
	}
	pipeline := NewPipeline(PipelineConfig{
		Lists: lists,
		DNS:   stubLookup{facts: DomainFacts{HasMX: true, HasA: true, HasSPF: true, HasDMARC: true}},
		Age:   DisabledAgeLookup(),
	})
	p := NewExistingProvider(pipeline)

	perfect := p.Normalize(Pass1Result{Score: pipeline.MaxScore()})
	if perfect.Score != 100 {
		t.Errorf("a perfect local result normalized to %d, want 100", perfect.Score)
	}

	hardFail := p.Normalize(Pass1Result{Score: 0, HardFail: CheckSyntax})
	if hardFail.Score != 0 {
		t.Errorf("a hard fail normalized to %d, want 0", hardFail.Score)
	}
	if hardFail.Metadata["hard_fail"] != CheckSyntax {
		t.Errorf("the hard fail key was not recorded: %+v", hardFail.Metadata)
	}
}
