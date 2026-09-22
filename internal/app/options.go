package app

import (
	"net/http"
	"time"

	"github.com/bory/karvon-be/internal/campaign/ai"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/campaign/provider/mailchimp"
	"github.com/bory/karvon-be/internal/scraper"
	"github.com/bory/karvon-be/internal/verify"
)

// Option customises the application graph. Production uses none of these; tests use
// them to replace the network-facing pieces with deterministic doubles.
type Option func(*options)

type options struct {
	providerFactory scraper.ProviderFactory
	crawlTransport  http.RoundTripper
	verifierFactory verify.VerifierFactory
	resolver        verify.Resolver
	ageLookup       verify.AgeLookup
	reacherClient   *http.Client

	instantlyFactory instantly.Factory
	mailchimpFactory mailchimp.Factory
	aiProvider       ai.Provider
	campaignClock    func() time.Time
}

// WithProviderFactory replaces the vendor client factory.
func WithProviderFactory(factory scraper.ProviderFactory) Option {
	return func(o *options) { o.providerFactory = factory }
}

// WithCrawlTransport replaces the crawler's HTTP transport.
func WithCrawlTransport(transport http.RoundTripper) Option {
	return func(o *options) { o.crawlTransport = transport }
}

// WithVerifierFactory replaces the email verification vendor client factory, so no
// test ever spends a credit.
func WithVerifierFactory(factory verify.VerifierFactory) Option {
	return func(o *options) { o.verifierFactory = factory }
}

// WithResolver replaces the DNS resolver used by the local verification pass.
func WithResolver(resolver verify.Resolver) Option {
	return func(o *options) { o.resolver = resolver }
}

// WithAgeLookup replaces the domain registration lookup.
func WithAgeLookup(lookup verify.AgeLookup) Option {
	return func(o *options) { o.ageLookup = lookup }
}

// WithReacherClient replaces the HTTP client the Reacher provider uses, so an
// integration test can point it at a httptest server instead of a container.
func WithReacherClient(client *http.Client) Option {
	return func(o *options) { o.reacherClient = client }
}

// WithInstantlyFactory replaces the Instantly client factory, so no test sends
// a cold email.
func WithInstantlyFactory(factory instantly.Factory) Option {
	return func(o *options) { o.instantlyFactory = factory }
}

// WithMailchimpFactory replaces the Mailchimp client factory, so no test
// subscribes a real address.
func WithMailchimpFactory(factory mailchimp.Factory) Option {
	return func(o *options) { o.mailchimpFactory = factory }
}

// WithAIProvider replaces the email generator.
func WithAIProvider(provider ai.Provider) Option {
	return func(o *options) { o.aiProvider = provider }
}

// WithCampaignClock replaces the campaign module's clock.
func WithCampaignClock(now func() time.Time) Option {
	return func(o *options) { o.campaignClock = now }
}
