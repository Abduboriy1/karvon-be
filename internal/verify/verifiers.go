package verify

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/crypto"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/verify/verifier"
	"github.com/bory/karvon-be/internal/verify/verifier/emailable"
)

// Source roles. A source is either a Maps data provider or an email verifier; they
// share the encrypted-key storage, the cost field and the connection test.
const (
	RoleMaps     = "maps"
	RoleVerifier = "verifier"
)

// KindEmailable is the only verifier implementation shipped today.
const KindEmailable = "emailable"

// VerifierFactory builds a ready-to-use verifier client from a stored source row.
// Tests swap in their own implementation so no test makes a paid call.
type VerifierFactory interface {
	For(ctx context.Context, source dbgen.Source) (verifier.Verifier, error)
}

// VerifierConfig holds the endpoints and timeouts used when building clients.
type VerifierConfig struct {
	EmailableBaseURL string
	Timeout          time.Duration
	HTTPClient       *http.Client
}

// DefaultVerifierFactory decrypts the stored API key and constructs the vendor client.
type DefaultVerifierFactory struct {
	cipher *crypto.Cipher
	cfg    VerifierConfig
}

// NewVerifierFactory builds the production factory.
func NewVerifierFactory(cipher *crypto.Cipher, cfg VerifierConfig) *DefaultVerifierFactory {
	return &DefaultVerifierFactory{cipher: cipher, cfg: cfg}
}

// For implements VerifierFactory.
func (f *DefaultVerifierFactory) For(_ context.Context, source dbgen.Source) (verifier.Verifier, error) {
	if source.Role != RoleVerifier {
		return nil, apperr.Validation("source is not an email verifier",
			apperr.FieldError{Field: "source_id", Message: "this source verifies nothing"})
	}
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

	opts := verifier.Options{
		APIKey:     apiKey,
		Timeout:    f.cfg.Timeout,
		HTTPClient: f.cfg.HTTPClient,
	}
	switch source.Kind {
	case KindEmailable:
		opts.BaseURL = f.cfg.EmailableBaseURL
		return emailable.New(opts), nil
	default:
		return nil, fmt.Errorf("verify: unknown verifier kind %q", source.Kind)
	}
}
