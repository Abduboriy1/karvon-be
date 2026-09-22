// Package mailchecker adapts FGRibreau/mailchecker onto the verification provider
// interface.
//
// MailChecker is MIT-licensed, pure Go, and ships its blocklist of ~56 000
// throwaway providers as a compiled-in map, so it is a library dependency rather
// than a service: a network hop would add latency and a failure mode in exchange for
// nothing. The cost is binary size, about a megabyte, and the fact that refreshing
// the list means bumping the module.
package mailchecker

import (
	"context"

	mc "github.com/FGRibreau/mailchecker/v6/platform/go"

	"github.com/bory/karvon-be/internal/verify/provider"
)

// Scores. MailChecker exposes exactly two booleans, so its normalized score is
// honestly binary: it can tell us an address is definitely not worth having, or that
// it found nothing against it. It never confirms a mailbox exists, which is why the
// weighting layer exists at all.
const (
	// ScoreClean is awarded when the syntax parses and the domain is not a known
	// throwaway provider.
	ScoreClean = 100
	// ScoreRejected is a definite negative: bad syntax or a disposable domain.
	ScoreRejected = 0
)

// Provider is the MailChecker adapter. It holds no state and is safe for concurrent
// use.
type Provider struct{}

// New builds the adapter.
func New() *Provider { return &Provider{} }

// Key implements provider.Provider.
func (*Provider) Key() provider.Key { return provider.KeyMailChecker }

// Check implements provider.Provider. It never touches the network, so it never
// fails and never honours a cancelled context in any observable way.
func (*Provider) Check(_ context.Context, email string) provider.Result {
	// Order matters: IsValid folds the blocklist into its answer, so asking about
	// the blocklist first is the only way to tell a disposable domain apart from a
	// syntax failure, and the two mean very different things to an operator.
	// Both negatives are facts rather than opinions — a throwaway domain and an
	// unparseable address are disqualifying however confident the other providers
	// are — so they zero the combined score rather than merely lowering it.
	if mc.IsBlacklisted(email) {
		return provider.Scored(provider.KeyMailChecker, ScoreRejected,
			"the domain is a known disposable mail provider").
			Disqualify().
			WithMetadata(map[string]any{"disposable": true})
	}
	if !mc.IsValid(email) {
		return provider.Scored(provider.KeyMailChecker, ScoreRejected,
			"the address does not parse as an email address").
			Disqualify().
			WithMetadata(map[string]any{"valid_syntax": false})
	}
	return provider.Scored(provider.KeyMailChecker, ScoreClean,
		"valid syntax, and the domain is not a known disposable provider").
		WithMetadata(map[string]any{"valid_syntax": true, "disposable": false})
}
