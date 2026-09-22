package instantly

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// FactoryConfig holds everything a client needs except the key, which comes from
// the source row.
type FactoryConfig struct {
	BaseURL          string
	Timeout          time.Duration
	Retries          int
	Concurrency      int
	BreakerThreshold int
	BreakerCooldown  time.Duration
	HTTPClient       *http.Client
	Log              *slog.Logger
}

// DefaultFactory decrypts the stored API key and constructs the HTTP client.
//
// Clients are cached per (source, key) so the breaker and the concurrency cap
// survive across jobs; a rotated key simply yields a fresh client and the stale
// one is left for the garbage collector.
type DefaultFactory struct {
	cipher  *crypto.Cipher
	cfg     FactoryConfig
	clients sync.Map // cacheKey -> *HTTPClient
}

// NewFactory builds the production factory.
func NewFactory(cipher *crypto.Cipher, cfg FactoryConfig) *DefaultFactory {
	return &DefaultFactory{cipher: cipher, cfg: cfg}
}

// For implements Factory.
func (f *DefaultFactory) For(_ context.Context, source dbgen.Source) (Client, error) {
	if source.Role != campaign.RoleOutreach {
		return nil, apperr.Validation("source is not an outreach provider",
			apperr.FieldError{Field: "source_id", Message: "this source runs no outreach"})
	}
	if source.Kind != campaign.KindInstantly {
		return nil, apperr.Validation("source is not an Instantly workspace",
			apperr.FieldError{Field: "source_id", Message: "expected an instantly source, got " + source.Kind})
	}
	if !source.Enabled {
		return nil, apperr.Conflict("source %q is disabled", source.Name)
	}
	if len(source.ApiKeyEnc) == 0 {
		return nil, apperr.ProviderAuth("source %q has no API key configured", source.Name).
			WithCause(provider.ErrNotConfigured)
	}

	apiKey, err := f.cipher.DecryptString(source.ApiKeyEnc)
	if err != nil {
		// A key that cannot be decrypted means KARVON_SECRET_KEY changed.
		return nil, apperr.ProviderAuth("stored API key for %q cannot be decrypted", source.Name).WithCause(err)
	}

	key := cacheKey(source, apiKey)
	if cached, ok := f.clients.Load(key); ok {
		return cached.(*HTTPClient), nil //nolint:errcheck // the map only ever holds *HTTPClient
	}
	client := New(Config{
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
	actual, _ := f.clients.LoadOrStore(key, client)
	return actual.(*HTTPClient), nil //nolint:errcheck // the map only ever holds *HTTPClient
}

// cacheKey identifies a client by source and by a digest of its key, so the key
// itself is never held in a map key.
func cacheKey(source dbgen.Source, apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return source.ID.String() + ":" + hex.EncodeToString(sum[:])
}

// compile-time proof that the factory satisfies the interface.
var _ Factory = (*DefaultFactory)(nil)
