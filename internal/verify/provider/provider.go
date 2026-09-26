// Package provider is the vendor-neutral surface of one email verification
// provider. The scoring layer knows nothing about a provider beyond what is in this
// file, so adding a provider is a new package plus a registry entry rather than a
// change to the pipeline.
//
// This package deliberately depends on nothing else in the codebase: the local
// pipeline lives in package verify and implements Provider from there, which would
// be an import cycle if this package knew about it.
package provider

import (
	"context"
	"errors"
	"time"
)

// ErrPaused marks a provider that deliberately did not call its backend because a
// circuit breaker is open. The outage was already reported once when the breaker
// opened, so callers should not report it again for every address.
var ErrPaused = errors.New("backend is unavailable, circuit is open")

// Key identifies a provider. It is persisted inside provider_results and used as the
// settings weight key, so renaming one is a data migration rather than a refactor.
type Key = string

// The providers registered today.
const (
	// KeyExisting is Karvon's own local pipeline: syntax, blocklists, DNS, RDAP.
	KeyExisting Key = "existing"
	// KeyMailChecker is the MIT-licensed MailChecker library, compiled in.
	KeyMailChecker Key = "mailchecker"
	// KeyReacher is the Reacher (check-if-email-exists) HTTP backend.
	KeyReacher Key = "reacher"
	// KeyPaid is the paid third-party verifier. It is not part of the weighted
	// free stage, but its verdict is recorded in the same shape so the UI can
	// render one uniform breakdown.
	KeyPaid Key = "paid"
)

// Status says what a provider managed to determine. Only StatusScored carries a
// score; every other status means the provider contributed nothing and its weight is
// redistributed over the providers that did.
type Status string

// The five outcomes.
const (
	// StatusScored means the provider reached a verdict and Score is meaningful.
	StatusScored Status = "scored"
	// StatusInconclusive means the provider ran but could not decide. Reacher
	// answering "unknown" is the canonical case.
	StatusInconclusive Status = "inconclusive"
	// StatusSkipped means the provider was not asked: it is switched off in
	// settings, or an earlier provider hard-failed and asking would be pointless.
	StatusSkipped Status = "skipped"
	// StatusUnavailable means the provider could not be reached at all: the
	// service is down, the circuit breaker is open, or it is not configured.
	StatusUnavailable Status = "unavailable"
	// StatusError means the provider was reached and failed.
	StatusError Status = "error"
)

// Contributes reports whether this result takes part in the weighted score.
func (s Status) Contributes() bool { return s == StatusScored }

// Result is one provider's normalized answer.
//
// Metadata holds the few structured facts worth keeping — whether SMTP connected,
// which check hard-failed — and never a raw vendor payload: those are kilobytes per
// address and say nothing the normalized fields do not. The paid provider is the one
// exception and keeps its raw response in its own column, because a billed verdict
// can be disputed.
type Result struct {
	Provider Key            `json:"provider"`
	Score    int            `json:"score"`
	Status   Status         `json:"status"`
	Reason   string         `json:"reason,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	// Disqualifying marks a fact no other provider's opinion can override: the
	// address is not valid syntax, its domain is a throwaway, its mailbox is
	// automated, or its domain does not accept mail at all. A provider that did not
	// check for such a thing has nothing to say about it, so its score must not be
	// able to average one away.
	Disqualifying bool `json:"disqualifying,omitempty"`
	// Error is the failure text, set when Status is StatusError or
	// StatusUnavailable. The error value itself is in Err and is not serialized.
	Error string `json:"error,omitempty"`
	// Err is the underlying failure, for logging and errors.Is at the call site.
	Err error `json:"-"`
	// Duration is how long the provider took, for the operator's benefit.
	Duration time.Duration `json:"-"`
}

// Scored builds a result that takes part in the weighted score.
func Scored(key Key, score int, reason string) Result {
	return Result{Provider: key, Score: clamp(score), Status: StatusScored, Reason: reason}
}

// Inconclusive builds a result for a provider that ran but could not decide.
func Inconclusive(key Key, reason string) Result {
	return Result{Provider: key, Status: StatusInconclusive, Reason: reason}
}

// Skipped builds a result for a provider that was deliberately not asked.
func Skipped(key Key, reason string) Result {
	return Result{Provider: key, Status: StatusSkipped, Reason: reason}
}

// Unavailable builds a result for a provider that could not be reached.
func Unavailable(key Key, err error, reason string) Result {
	out := Result{Provider: key, Status: StatusUnavailable, Reason: reason, Err: err}
	if err != nil {
		out.Error = err.Error()
	}
	return out
}

// Failed builds a result for a provider that was reached and errored.
func Failed(key Key, err error) Result {
	out := Result{Provider: key, Status: StatusError, Err: err}
	if err != nil {
		out.Error = err.Error()
		out.Reason = err.Error()
	}
	return out
}

// Disqualify marks the result as a fact that zeroes the combined score. It is only
// ever set alongside a score of 0.
func (r Result) Disqualify() Result {
	r.Disqualifying = true
	r.Score = 0
	return r
}

// WithMetadata attaches structured detail, ignoring an empty map.
func (r Result) WithMetadata(meta map[string]any) Result {
	if len(meta) > 0 {
		r.Metadata = meta
	}
	return r
}

// Provider is one free verification provider.
//
// Check must never return an error: a provider that cannot answer returns a Result
// whose Status says so, because one provider being down must not fail the pipeline.
// Implementations must honour ctx.
type Provider interface {
	// Key is the stable identifier used for weights and storage.
	Key() Key
	// Check scores one already-normalized address.
	Check(ctx context.Context, email string) Result
}

// HealthChecker is implemented by providers that can be probed independently of a
// verification, so the settings page can show whether they are reachable.
type HealthChecker interface {
	// Health returns nil when the provider is ready to serve.
	Health(ctx context.Context) error
}

func clamp(score int) int {
	switch {
	case score < 0:
		return 0
	case score > 100:
		return 100
	default:
		return score
	}
}
