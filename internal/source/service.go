// Package source manages the provider credentials and their connection test.
// API keys are encrypted with AES-GCM before they reach the database and are never
// returned by the API or written to a log.
package source

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/crypto"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/registrar"
	"github.com/bory/karvon-be/internal/scraper"
	"github.com/bory/karvon-be/internal/scraper/provider"
	"github.com/bory/karvon-be/internal/verify"
	"github.com/bory/karvon-be/internal/verify/verifier"
	"github.com/bory/karvon-be/internal/workspace"
)

// MaxKeyLength bounds what a client may send as an API key.
const MaxKeyLength = 500

// probeQuery is the cheapest call that still proves a key works.
var probeQuery = provider.SearchQuery{Term: "coffee", City: "New York", State: "NY", Max: 1}

// RegistrarTester proves the stored Cloudflare token, which needs the account ID
// kept alongside it; the domains module owns that test.
type RegistrarTester interface {
	TestConnection(ctx context.Context) (registrar.TestResult, error)
}

// MailboxTester proves the stored Google service-account key, which needs the admin
// email kept alongside it; the mailboxes module owns that test.
type MailboxTester interface {
	TestConnection(ctx context.Context) (workspace.TestResult, error)
}

// Service implements the /sources endpoints.
type Service struct {
	store     *db.Store
	cipher    *crypto.Cipher
	providers scraper.ProviderFactory
	verifiers verify.VerifierFactory
	registrar RegistrarTester
	mailboxes MailboxTester
	log       *slog.Logger
}

// NewService builds the source service. It holds both factories because the sources
// table carries Maps providers and the email verifier, and each is tested its own way.
func NewService(store *db.Store, cipher *crypto.Cipher, providers scraper.ProviderFactory,
	verifiers verify.VerifierFactory, log *slog.Logger,
) *Service {
	return &Service{store: store, cipher: cipher, providers: providers, verifiers: verifiers, log: log}
}

// SetRegistrar wires in the domains module, which tests the registrar source.
func (s *Service) SetRegistrar(tester RegistrarTester) { s.registrar = tester }

// SetMailboxes wires in the mailboxes module, which tests the Google Workspace source.
func (s *Service) SetMailboxes(tester MailboxTester) { s.mailboxes = tester }

// List returns every configured provider.
func (s *Service) List(ctx context.Context) ([]dbgen.Source, error) {
	rows, err := s.store.ListSources(ctx)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	return rows, nil
}

// Get returns one provider or a 404.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (dbgen.Source, error) {
	row, err := s.store.GetSource(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.Source{}, apperr.NotFound("source")
	}
	if err != nil {
		return dbgen.Source{}, apperr.Internal(err)
	}
	return row, nil
}

// UpdateInput distinguishes "leave the key alone" (KeyPresent false) from
// "clear the key" (KeyPresent true, APIKey nil).
type UpdateInput struct {
	Name           *string
	CostPer1kCents *int
	Enabled        *bool
	KeyPresent     bool
	APIKey         *string
}

// Update stores new provider settings, encrypting a supplied key.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (dbgen.Source, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return dbgen.Source{}, err
	}
	if err := in.validate(); err != nil {
		return dbgen.Source{}, err
	}

	params := dbgen.UpdateSourceParams{ID: id}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		params.Name = &name
	}
	if in.CostPer1kCents != nil {
		// validate() has already bounded this to [0, 1_000_000].
		cost := int32(*in.CostPer1kCents) //nolint:gosec // G115: range-checked above
		params.CostPer1kCents = &cost
	}
	params.Enabled = in.Enabled

	if in.KeyPresent {
		params.SetKey = true
		if in.APIKey != nil && strings.TrimSpace(*in.APIKey) != "" {
			encrypted, err := s.cipher.EncryptString(strings.TrimSpace(*in.APIKey))
			if err != nil {
				return dbgen.Source{}, apperr.Internal(err)
			}
			params.ApiKeyEnc = encrypted
		}
	}

	row, err := s.store.UpdateSource(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.Source{}, apperr.NotFound("source")
	}
	if err != nil {
		return dbgen.Source{}, apperr.Internal(err)
	}
	return row, nil
}

func (in UpdateInput) validate() error {
	var fields []apperr.FieldError
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || len(name) > 100 {
			fields = append(fields, apperr.FieldError{Field: "name", Message: "must be between 1 and 100 characters"})
		}
	}
	if in.CostPer1kCents != nil && (*in.CostPer1kCents < 0 || *in.CostPer1kCents > 1_000_000) {
		fields = append(fields, apperr.FieldError{Field: "cost_per_1k_cents", Message: "must be between 0 and 1000000"})
	}
	if in.APIKey != nil && len(*in.APIKey) > MaxKeyLength {
		fields = append(fields, apperr.FieldError{
			Field:   "api_key",
			Message: fmt.Sprintf("must be at most %d characters", MaxKeyLength),
		})
	}
	if len(fields) > 0 {
		return apperr.Validation("source update is invalid", fields...)
	}
	return nil
}

// TestResult reports the outcome of a connection test.
type TestResult struct {
	OK               bool
	Kind             string
	ListingsReturned int
	// Credits is the verifier's remaining balance. It is nil for a Maps provider.
	Credits  *int64
	TestedAt time.Time
}

// Test performs one minimal provider call to prove the stored key works. The result is
// recorded on the row so the Sources page can show when a key was last verified.
func (s *Service) Test(ctx context.Context, id uuid.UUID) (TestResult, error) {
	row, err := s.Get(ctx, id)
	if err != nil {
		return TestResult{}, err
	}
	if len(row.ApiKeyEnc) == 0 {
		return TestResult{}, apperr.Validation("source is not configured",
			apperr.FieldError{Field: "api_key", Message: "no API key is stored for this source"})
	}

	// The email verifier proves its key by reading the account balance, which costs
	// nothing and is the number the Verification page wants anyway.
	if row.Role == verify.RoleVerifier {
		return s.testVerifier(ctx, row)
	}
	if row.Role == registrar.RoleRegistrar {
		return s.testRegistrar(ctx, row)
	}
	if row.Role == workspace.RoleMailboxes {
		return s.testMailboxes(ctx, row)
	}

	// A disabled source can still be tested: that is how a key is verified before
	// enabling it. Build the client directly rather than through the enabled check.
	prov, err := s.providers.For(ctx, enabledCopy(row))
	if err != nil {
		s.recordTest(ctx, id, false)
		return TestResult{}, err
	}

	listings, err := prov.Search(ctx, probeQuery)
	if err != nil {
		s.recordTest(ctx, id, false)
		if errors.Is(err, provider.ErrAuth) {
			return TestResult{}, apperr.ProviderAuth("the provider rejected the stored API key")
		}
		// Provider errors are logged in full; the client gets a safe summary.
		s.log.Warn("source test failed", "source_id", id, "kind", row.Kind, "error", err)
		return TestResult{}, apperr.ProviderError("the provider could not be reached")
	}

	s.recordTest(ctx, id, true)
	return TestResult{
		OK:               true,
		Kind:             row.Kind,
		ListingsReturned: len(listings),
		TestedAt:         time.Now().UTC(),
	}, nil
}

// testVerifier proves an email verifier's key and reports its remaining credit.
func (s *Service) testVerifier(ctx context.Context, row dbgen.Source) (TestResult, error) {
	if s.verifiers == nil {
		return TestResult{}, apperr.Internal(errors.New("source: no verifier factory is configured"))
	}

	client, err := s.verifiers.For(ctx, enabledCopy(row))
	if err != nil {
		s.recordTest(ctx, row.ID, false)
		return TestResult{}, err
	}

	balance, err := client.Balance(ctx)
	if err != nil {
		s.recordTest(ctx, row.ID, false)
		if errors.Is(err, verifier.ErrAuth) {
			return TestResult{}, apperr.ProviderAuth("the verifier rejected the stored API key")
		}
		s.log.Warn("verifier test failed", "source_id", row.ID, "kind", row.Kind, "error", err)
		return TestResult{}, apperr.ProviderError("the verifier could not be reached")
	}

	s.recordTest(ctx, row.ID, true)
	credits := balance.Credits
	return TestResult{OK: true, Kind: row.Kind, Credits: &credits, TestedAt: time.Now().UTC()}, nil
}

// testRegistrar proves the Cloudflare token through the domains module, which also
// records the result on the row.
func (s *Service) testRegistrar(ctx context.Context, row dbgen.Source) (TestResult, error) {
	if s.registrar == nil {
		return TestResult{}, apperr.Internal(errors.New("source: no registrar is configured"))
	}
	result, err := s.registrar.TestConnection(ctx)
	if err != nil {
		return TestResult{}, err
	}
	return TestResult{OK: result.OK, Kind: row.Kind, TestedAt: result.TestedAt}, nil
}

// testMailboxes proves the service-account key through the mailboxes module, which
// also records the result on the row.
func (s *Service) testMailboxes(ctx context.Context, row dbgen.Source) (TestResult, error) {
	if s.mailboxes == nil {
		return TestResult{}, apperr.Internal(errors.New("source: no mailboxes module is configured"))
	}
	result, err := s.mailboxes.TestConnection(ctx)
	if err != nil {
		return TestResult{}, err
	}
	return TestResult{OK: result.OK, Kind: row.Kind, TestedAt: result.TestedAt}, nil
}

// enabledCopy lets a disabled source be tested without enabling it first.
func enabledCopy(row dbgen.Source) dbgen.Source {
	row.Enabled = true
	return row
}

func (s *Service) recordTest(ctx context.Context, id uuid.UUID, ok bool) {
	if err := s.store.SetSourceTestResult(ctx, dbgen.SetSourceTestResultParams{ID: id, Ok: &ok}); err != nil {
		s.log.Warn("could not record source test result", "source_id", id, "error", err)
	}
}
