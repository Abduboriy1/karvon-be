package mailchimp

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/crypto"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// FactoryConfig is the part of Config that does not come from the source row.
type FactoryConfig struct {
	// BaseURL may contain {dc}; empty means DefaultBaseURL.
	BaseURL          string
	Timeout          time.Duration
	Retries          int
	Concurrency      int
	BreakerThreshold int
	BreakerCooldown  time.Duration
	HTTPClient       *http.Client
	Log              *slog.Logger
}

// DefaultFactory decrypts the stored key and builds an HTTPClient. Clients are
// cached per key so the breaker and the connection limit are shared by every job
// that talks to the same account.
type DefaultFactory struct {
	cipher *crypto.Cipher
	cfg    FactoryConfig

	mu      sync.Mutex
	clients map[[sha256.Size]byte]*HTTPClient
}

// NewFactory builds the production factory.
func NewFactory(cipher *crypto.Cipher, cfg FactoryConfig) *DefaultFactory {
	return &DefaultFactory{
		cipher:  cipher,
		cfg:     cfg,
		clients: make(map[[sha256.Size]byte]*HTTPClient),
	}
}

// For implements Factory.
func (f *DefaultFactory) For(_ context.Context, source dbgen.Source) (Client, error) {
	if source.Role != campaign.RoleNewsletter {
		return nil, apperr.Validation("source is not a newsletter provider",
			apperr.FieldError{Field: "source_id", Message: "this source sends no newsletters"})
	}
	if source.Kind != campaign.KindMailchimp {
		return nil, apperr.Validation("source is not a Mailchimp account",
			apperr.FieldError{Field: "source_id", Message: "expected kind " + campaign.KindMailchimp})
	}
	if !source.Enabled {
		return nil, apperr.Conflict("source %q is disabled", source.Name)
	}
	if len(source.ApiKeyEnc) == 0 {
		return nil, apperr.ProviderAuth("source %q has no API key configured", source.Name).
			WithCause(provider.ErrNotConfigured)
	}
	if f.cipher == nil {
		return nil, apperr.ProviderAuth("stored API key for %q cannot be decrypted", source.Name).
			WithCause(provider.ErrNotConfigured)
	}

	apiKey, err := f.cipher.DecryptString(source.ApiKeyEnc)
	if err != nil {
		// A key that cannot be decrypted means KARVON_SECRET_KEY changed.
		return nil, apperr.ProviderAuth("stored API key for %q cannot be decrypted", source.Name).WithCause(err)
	}
	if apiKey == "" {
		return nil, apperr.ProviderAuth("source %q has no API key configured", source.Name).
			WithCause(provider.ErrNotConfigured)
	}
	if DataCenter(apiKey) == "" {
		return nil, apperr.ProviderAuth("stored API key for %q has no datacenter suffix", source.Name).
			WithCause(provider.ErrAuth)
	}

	return f.client(apiKey), nil
}

// client returns the cached client for a key, building it on first use.
func (f *DefaultFactory) client(apiKey string) *HTTPClient {
	id := sha256.Sum256([]byte(apiKey))
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.clients[id]; ok {
		return c
	}
	c := New(Config{
		BaseURL:          f.cfg.BaseURL,
		APIKey:           apiKey,
		Timeout:          f.cfg.Timeout,
		Retries:          f.cfg.Retries,
		Concurrency:      f.cfg.Concurrency,
		BreakerThreshold: f.cfg.BreakerThreshold,
		BreakerCooldown:  f.cfg.BreakerCooldown,
		HTTPClient:       f.cfg.HTTPClient,
		Log:              f.cfg.Log,
	})
	f.clients[id] = c
	return c
}

// compile-time proof that the factory satisfies the interface.
var _ Factory = (*DefaultFactory)(nil)
