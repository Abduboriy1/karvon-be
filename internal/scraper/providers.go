package scraper

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/crypto"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/scraper/provider"
	"github.com/bory/karvon-be/internal/scraper/provider/apify"
	"github.com/bory/karvon-be/internal/scraper/provider/outscraper"
)

// ProviderFactory builds a ready-to-use provider client for a stored source row.
// Tests swap in their own implementation to avoid network calls.
type ProviderFactory interface {
	For(ctx context.Context, source dbgen.Source) (provider.Provider, error)
}

// ProviderConfig holds the endpoints, timeouts and run settings used when building
// clients.
type ProviderConfig struct {
	ApifyBaseURL      string
	OutscraperBaseURL string
	Timeout           time.Duration
	HTTPClient        *http.Client
	// Apify carries the run-level settings that decide a run's speed and its bill.
	Apify apify.Settings
}

// DefaultProviderFactory decrypts the stored API key and constructs the vendor client.
type DefaultProviderFactory struct {
	cipher *crypto.Cipher
	cfg    ProviderConfig
}

// NewProviderFactory builds the production factory.
func NewProviderFactory(cipher *crypto.Cipher, cfg ProviderConfig) *DefaultProviderFactory {
	return &DefaultProviderFactory{cipher: cipher, cfg: cfg}
}

// For implements ProviderFactory.
func (f *DefaultProviderFactory) For(_ context.Context, source dbgen.Source) (provider.Provider, error) {
	if !source.Enabled {
		return nil, apperr.Conflict("source %q is disabled", source.Name)
	}
	if len(source.ApiKeyEnc) == 0 {
		return nil, apperr.ProviderAuth("source %q has no API key configured", source.Name)
	}

	apiKey, err := f.cipher.DecryptString(source.ApiKeyEnc)
	if err != nil {
		// A key that cannot be decrypted means KARVON_SECRET_KEY changed.
		return nil, apperr.ProviderAuth("stored API key for %q cannot be decrypted", source.Name).WithCause(err)
	}

	opts := provider.Options{
		APIKey:     apiKey,
		Timeout:    f.cfg.Timeout,
		HTTPClient: f.cfg.HTTPClient,
	}
	switch provider.Kind(source.Kind) {
	case provider.KindApify:
		opts.BaseURL = f.cfg.ApifyBaseURL
		return apify.NewWithSettings(opts, f.cfg.Apify), nil
	case provider.KindOutscraper:
		opts.BaseURL = f.cfg.OutscraperBaseURL
		return outscraper.New(opts), nil
	default:
		return nil, fmt.Errorf("scraper: unknown provider kind %q", source.Kind)
	}
}
