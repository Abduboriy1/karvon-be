package registrar

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/crypto"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
	"github.com/bory/karvon-be/internal/registrar/cloudflare"
)

// Search bounds.
const (
	DefaultSearchLimit = 20
	maxQueryLength     = 100
	maxExtensions      = 20
	// maxListPages caps how many pages of registrations one listing reads. Cloudflare
	// allows 500 domains per account, which is ten pages of fifty.
	maxListPages = 20
)

// accountIDPattern is what a Cloudflare account ID looks like.
var accountIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Enqueuer is the queue surface the service needs.
type Enqueuer interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// Config holds everything the service needs besides the stored credentials.
type Config struct {
	BaseURL string
	// Timeout bounds one Cloudflare call. Registration alone can wait ten seconds.
	Timeout    time.Duration
	HTTPClient *http.Client
	// PollInterval is how often a registration Cloudflare is still working on is
	// asked about again.
	PollInterval time.Duration
	// MaxWait is how long a purchase keeps polling before handing the unfinished
	// registrations to a person.
	MaxWait time.Duration
}

// Service implements the /domains endpoints and the purchase worker.
type Service struct {
	store  *db.Store
	cipher *crypto.Cipher
	cfg    Config
	queue  Enqueuer
	log    *slog.Logger
	now    func() time.Time
}

// NewService builds the domains service. The queue is set later with SetQueue,
// because the River client is built after the services.
func NewService(store *db.Store, cipher *crypto.Cipher, cfg Config, log *slog.Logger) *Service {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 15 * time.Second
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 30 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: store, cipher: cipher, cfg: cfg, log: log, now: time.Now}
}

// SetQueue wires the River client in once it exists.
func (s *Service) SetQueue(queue Enqueuer) { s.queue = queue }

// ---------------------------------------------------------------------------
// Settings

// Settings is the Cloudflare connection as the Domains page shows it. The token is
// never returned, only whether one is stored.
type Settings struct {
	SourceID     uuid.UUID
	AccountID    *string
	HasToken     bool
	Enabled      bool
	LastTestedAt *time.Time
	LastTestOK   *bool
}

// Ready reports whether purchases and searches can run.
func (s Settings) Ready() bool {
	return s.AccountID != nil && s.HasToken && s.Enabled
}

// SettingsInput distinguishes "leave it alone" (Present false) from "clear it"
// (Present true, value nil) for the account ID and the token.
type SettingsInput struct {
	AccountIDPresent bool
	AccountID        *string
	TokenPresent     bool
	Token            *string
	Enabled          *bool
}

// Settings returns the current connection.
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	settings, _, err := s.loadSettings(ctx)
	return settings, err
}

// SaveSettings stores the account ID, the token (encrypted) and the enabled flag.
func (s *Service) SaveSettings(ctx context.Context, in SettingsInput) (Settings, error) {
	var fields []apperr.FieldError
	var accountID *string
	if in.AccountIDPresent && in.AccountID != nil {
		value := strings.ToLower(strings.TrimSpace(*in.AccountID))
		if value != "" {
			if !accountIDPattern.MatchString(value) {
				fields = append(fields, apperr.FieldError{Field: "account_id",
					Message: "must be the 32-character account ID shown in the Cloudflare dashboard"})
			}
			accountID = &value
		}
	}
	var tokenEnc []byte
	if in.TokenPresent && in.Token != nil {
		token := strings.TrimSpace(*in.Token)
		if len(token) > MaxTokenLength {
			fields = append(fields, apperr.FieldError{Field: "api_token",
				Message: fmt.Sprintf("must be at most %d characters", MaxTokenLength)})
		} else if token != "" {
			encrypted, err := s.cipher.EncryptString(token)
			if err != nil {
				return Settings{}, apperr.Internal(err)
			}
			tokenEnc = encrypted
		}
	}
	if len(fields) > 0 {
		return Settings{}, apperr.Validation("domain settings are invalid", fields...)
	}

	source, err := s.source(ctx)
	if err != nil {
		return Settings{}, err
	}
	err = s.store.InTx(ctx, func(q *dbgen.Queries) error {
		if in.AccountIDPresent {
			if _, err := q.SetRegistrarAccountID(ctx, accountID); err != nil {
				return err
			}
		}
		if in.TokenPresent || in.Enabled != nil {
			_, err := q.UpdateSource(ctx, dbgen.UpdateSourceParams{
				ID:        source.ID,
				Enabled:   in.Enabled,
				SetKey:    in.TokenPresent,
				ApiKeyEnc: tokenEnc,
			})
			return err
		}
		return nil
	})
	if err != nil {
		return Settings{}, apperr.Internal(err)
	}
	return s.Settings(ctx)
}

// TestResult is the outcome of a connection test.
type TestResult struct {
	OK       bool
	TestedAt time.Time
}

// TestConnection proves the token, the account ID and the registrar permission with
// one read that costs nothing. A disabled connection can be tested: that is how a
// token is checked before switching purchases on. The result is recorded on the
// source row.
func (s *Service) TestConnection(ctx context.Context) (TestResult, error) {
	client, settings, err := s.client(ctx, false)
	if err != nil {
		return TestResult{}, err
	}
	_, err = client.ListRegistrations(ctx, "", 1)
	s.recordTest(ctx, settings.SourceID, err == nil)
	if err != nil {
		return TestResult{}, s.providerError(err, "test the connection")
	}
	return TestResult{OK: true, TestedAt: s.now().UTC()}, nil
}

func (s *Service) recordTest(ctx context.Context, sourceID uuid.UUID, ok bool) {
	if err := s.store.SetSourceTestResult(ctx, dbgen.SetSourceTestResultParams{ID: sourceID, Ok: &ok}); err != nil {
		s.log.Warn("could not record the Cloudflare test result", "error", err)
	}
}

func (s *Service) source(ctx context.Context) (dbgen.Source, error) {
	row, err := s.store.GetSourceByKind(ctx, KindCloudflare)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.Source{}, apperr.Internal(errors.New("registrar: the cloudflare source row is missing; run the migrations"))
	}
	if err != nil {
		return dbgen.Source{}, apperr.Internal(err)
	}
	return row, nil
}

func (s *Service) loadSettings(ctx context.Context) (Settings, dbgen.Source, error) {
	source, err := s.source(ctx)
	if err != nil {
		return Settings{}, dbgen.Source{}, err
	}
	row, err := s.store.GetRegistrarSettings(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Settings{}, dbgen.Source{}, apperr.Internal(err)
	}
	return Settings{
		SourceID:     source.ID,
		AccountID:    row.CloudflareAccountID,
		HasToken:     len(source.ApiKeyEnc) > 0,
		Enabled:      source.Enabled,
		LastTestedAt: source.LastTestedAt,
		LastTestOK:   source.LastTestOk,
	}, source, nil
}

// client builds a Cloudflare client from the stored settings. requireEnabled is
// false only for the connection test.
func (s *Service) client(ctx context.Context, requireEnabled bool) (*cloudflare.Client, Settings, error) {
	settings, source, err := s.loadSettings(ctx)
	if err != nil {
		return nil, Settings{}, err
	}
	switch {
	case settings.AccountID == nil:
		return nil, settings, apperr.Conflict("the Cloudflare account ID is not set; add it under Settings → Domains")
	case !settings.HasToken:
		return nil, settings, apperr.Conflict("no Cloudflare API token is stored; add one under Settings → Domains")
	case requireEnabled && !settings.Enabled:
		return nil, settings, apperr.Conflict("the Cloudflare connection is switched off; enable it under Settings → Domains")
	}
	token, err := s.cipher.DecryptString(source.ApiKeyEnc)
	if err != nil {
		// A token that cannot be decrypted means KARVON_SECRET_KEY changed.
		return nil, settings, apperr.ProviderAuth("the stored Cloudflare token cannot be decrypted; save it again").WithCause(err)
	}
	return cloudflare.New(cloudflare.Config{
		BaseURL:    s.cfg.BaseURL,
		AccountID:  *settings.AccountID,
		Token:      token,
		Timeout:    s.cfg.Timeout,
		HTTPClient: s.cfg.HTTPClient,
	}), settings, nil
}

// DNSClient is the Cloudflare client for the account's zones, built from the same
// stored token. The workspace module publishes mail records through it, which needs
// the token to carry Zone → Zone → Read and Zone → DNS → Edit as well.
func (s *Service) DNSClient(ctx context.Context) (*cloudflare.Client, error) {
	client, _, err := s.client(ctx, true)
	return client, err
}

// providerError turns a Cloudflare failure into the error the client sees. Details
// are logged in full; the client gets Cloudflare's own message only where it is about
// the request rather than about the account.
func (s *Service) providerError(err error, action string) error {
	var apiErr *cloudflare.APIError
	switch {
	case errors.Is(err, cloudflare.ErrAuth):
		return apperr.ProviderAuth("Cloudflare rejected the API token; check it has the Registrar permission for this account").WithCause(err)
	case errors.Is(err, cloudflare.ErrNotFound):
		// Every call is scoped to the account, so an unknown account is a 404 too.
		return apperr.ProviderAuth("Cloudflare did not find the account; check the account ID and that the token can see it").WithCause(err)
	case errors.Is(err, cloudflare.ErrRateLimited):
		return apperr.ProviderError("Cloudflare is rate limiting this account; try again in a minute").WithCause(err)
	case errors.As(err, &apiErr) && cloudflare.Definite(err):
		detail := apiErr.Detail()
		if detail == "" {
			detail = "Cloudflare rejected the request"
		}
		return apperr.Validation(detail).WithCause(err)
	}
	s.log.Warn("cloudflare call failed", "action", action, "error", err)
	return apperr.ProviderError("Cloudflare could not %s", action).WithCause(err)
}

// ---------------------------------------------------------------------------
// Search and check

// SearchInput is a keyword search.
type SearchInput struct {
	Query      string
	Limit      int
	Extensions []string
}

// Search suggests domains for a keyword. Results come from Cloudflare's cache and
// are for discovery; Check is what a purchase trusts.
func (s *Service) Search(ctx context.Context, in SearchInput) ([]Offer, error) {
	query := strings.Join(strings.Fields(in.Query), " ")
	var fields []apperr.FieldError
	if query == "" || len(query) > maxQueryLength {
		fields = append(fields, apperr.FieldError{Field: "q", Message: fmt.Sprintf("must be between 1 and %d characters", maxQueryLength)})
	}
	limit := in.Limit
	if limit == 0 {
		limit = DefaultSearchLimit
	}
	if limit < 1 || limit > cloudflare.MaxSearchLimit {
		fields = append(fields, apperr.FieldError{Field: "limit", Message: fmt.Sprintf("must be between 1 and %d", cloudflare.MaxSearchLimit)})
	}
	if len(in.Extensions) > maxExtensions {
		fields = append(fields, apperr.FieldError{Field: "extensions", Message: fmt.Sprintf("must have at most %d entries", maxExtensions)})
	}
	extensions := make([]string, 0, len(in.Extensions))
	for i, raw := range in.Extensions {
		ext, err := NormalizeExtension(raw)
		if err != nil {
			fields = append(fields, apperr.FieldError{Field: fmt.Sprintf("extensions[%d]", i), Message: err.Error()})
			continue
		}
		extensions = append(extensions, ext)
	}
	if len(fields) > 0 {
		return nil, apperr.Validation("search parameters are invalid", fields...)
	}

	client, _, err := s.client(ctx, true)
	if err != nil {
		return nil, err
	}
	offers, err := client.Search(ctx, query, limit, extensions)
	if err != nil {
		return nil, s.providerError(err, "search for domains")
	}
	out := make([]Offer, 0, len(offers))
	for _, offer := range offers {
		out = append(out, toOffer(offer))
	}
	return out, nil
}

// Check asks the registries whether each name can be bought now, and for how much.
// Names are normalised and de-duplicated; the answer keeps the request's order.
func (s *Service) Check(ctx context.Context, names []string) ([]Offer, error) {
	if len(names) == 0 || len(names) > cloudflare.MaxCheckDomains {
		return nil, apperr.Validation("request body is invalid", apperr.FieldError{
			Field: "domains", Message: fmt.Sprintf("must hold between 1 and %d domains", cloudflare.MaxCheckDomains),
		})
	}
	normalized, fields := normalizeNames(names, "domains")
	if len(fields) > 0 {
		return nil, apperr.Validation("request body is invalid", fields...)
	}
	unique := dedupe(normalized)

	client, _, err := s.client(ctx, true)
	if err != nil {
		return nil, err
	}
	offers, err := s.checkAll(ctx, client, unique)
	if err != nil {
		return nil, s.providerError(err, "check domain availability")
	}
	out := make([]Offer, 0, len(unique))
	for _, name := range unique {
		offer, ok := offers[name]
		if !ok {
			reason := CodeNotReturned
			offer = Offer{Name: name, Reason: &reason}
		}
		out = append(out, offer)
	}
	return out, nil
}

// checkAll runs one authoritative check and indexes the answer by name.
func (s *Service) checkAll(ctx context.Context, client *cloudflare.Client, names []string) (map[string]Offer, error) {
	offers, err := client.Check(ctx, names)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Offer, len(offers))
	for _, offer := range offers {
		converted := toOffer(offer)
		key := strings.ToLower(strings.TrimSuffix(converted.Name, "."))
		out[key] = converted
	}
	return out, nil
}

func normalizeNames(names []string, field string) ([]string, []apperr.FieldError) {
	out := make([]string, len(names))
	var fields []apperr.FieldError
	for i, raw := range names {
		name, err := NormalizeDomain(raw)
		if err != nil {
			fields = append(fields, apperr.FieldError{Field: fmt.Sprintf("%s[%d]", field, i), Message: err.Error()})
			continue
		}
		out[i] = name
	}
	return out, fields
}

func dedupe(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// ---------------------------------------------------------------------------
// Purchases

// PurchaseDomain is one line of a confirmed purchase: the domain and the
// registration price the operator was shown and agreed to.
type PurchaseDomain struct {
	Name              string
	ExpectedCostCents int64
}

// PurchaseInput is a confirmed purchase.
type PurchaseInput struct {
	Domains   []PurchaseDomain
	AutoRenew bool
	// Confirm must be true. It is the API's half of the confirm button: a request
	// without it is refused, so nothing is ever bought by a stray call.
	Confirm bool
}

// Purchase is a purchase with its domains.
type Purchase struct {
	dbgen.DomainPurchase
	Items []dbgen.DomainPurchaseItem
}

// PurchasePage is one page of purchases.
type PurchasePage struct {
	Rows  []Purchase
	Total int64
}

// CreatePurchase checks every domain against Cloudflare one last time and, when each
// is still available at no more than the confirmed price, queues the purchase. It is
// all or nothing: if any domain changed, nothing is queued and the answer says which.
func (s *Service) CreatePurchase(ctx context.Context, in PurchaseInput) (Purchase, error) {
	names, err := validatePurchase(in)
	if err != nil {
		return Purchase{}, err
	}
	if s.queue == nil {
		return Purchase{}, apperr.Internal(errors.New("registrar: no queue is configured"))
	}

	if active, err := s.store.GetActiveDomainPurchase(ctx); err == nil {
		return Purchase{}, apperr.Conflict("another domain purchase (%s) is still in progress; wait for it to finish", active.ID)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Purchase{}, apperr.Internal(err)
	}

	client, _, err := s.client(ctx, true)
	if err != nil {
		return Purchase{}, err
	}
	offers, err := s.checkAll(ctx, client, names)
	if err != nil {
		return Purchase{}, s.providerError(err, "confirm domain availability")
	}

	currency := ""
	var total int64
	var fields []apperr.FieldError
	prices := make([]*Pricing, len(names))
	for i, name := range names {
		field := fmt.Sprintf("domains[%d]", i)
		offer, ok := offers[name]
		expected := in.Domains[i].ExpectedCostCents
		code, message := refusal(name, offer, ok, expected)
		if code == "" && currency != "" && offer.Pricing.Currency != currency {
			message = fmt.Sprintf("%s is priced in %s while the rest are in %s; buy it separately", name, offer.Pricing.Currency, currency)
		}
		if message != "" {
			fields = append(fields, apperr.FieldError{Field: field, Message: message})
			continue
		}
		currency = offer.Pricing.Currency
		prices[i] = offer.Pricing
		total += expected
	}
	if len(fields) > 0 {
		return Purchase{}, &apperr.Error{
			Code:    apperr.CodeConflict,
			Status:  http.StatusConflict,
			Message: "some domains can no longer be bought as confirmed; nothing was purchased. Check them again and confirm the new list",
			Fields:  fields,
		}
	}

	purchaseID := ids.New()
	err = s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.CreateDomainPurchase(ctx, dbgen.CreateDomainPurchaseParams{
			ID:               purchaseID,
			AutoRenew:        in.AutoRenew,
			Currency:         currency,
			QuotedTotalCents: total,
			ItemCount:        int16(len(names)), //nolint:gosec // G115: at most MaxDomainsPerPurchase
		}); err != nil {
			return err
		}
		for i, name := range names {
			cost, renewal := prices[i].RegistrationCostCents, prices[i].RenewalCostCents
			if _, err := q.CreateDomainPurchaseItem(ctx, dbgen.CreateDomainPurchaseItemParams{
				ID:               ids.New(),
				PurchaseID:       purchaseID,
				Position:         int16(i), //nolint:gosec // G115: at most MaxDomainsPerPurchase
				DomainName:       name,
				QuotedCostCents:  in.Domains[i].ExpectedCostCents,
				CostCents:        &cost,
				RenewalCostCents: &renewal,
			}); err != nil {
				return err
			}
		}
		_, err := s.queue.InsertTx(ctx, tx, PurchaseArgs{PurchaseID: purchaseID}, nil)
		return err
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "domain_purchases_one_active_idx" {
			return Purchase{}, apperr.Conflict("another domain purchase is still in progress; wait for it to finish")
		}
		return Purchase{}, apperr.Internal(err)
	}

	s.log.Info("domain purchase queued", "purchase_id", purchaseID, "domains", len(names),
		"quoted_total_cents", total, "currency", currency)
	return s.GetPurchase(ctx, purchaseID)
}

// validatePurchase applies the rules that need no network: the confirmation, the
// ten-domain cap, well-formed and distinct names, sane prices.
func validatePurchase(in PurchaseInput) ([]string, error) {
	var fields []apperr.FieldError
	if !in.Confirm {
		fields = append(fields, apperr.FieldError{Field: "confirm", Message: "must be true to buy domains"})
	}
	switch n := len(in.Domains); {
	case n == 0:
		fields = append(fields, apperr.FieldError{Field: "domains", Message: "must hold at least one domain"})
	case n > MaxDomainsPerPurchase:
		fields = append(fields, apperr.FieldError{Field: "domains",
			Message: fmt.Sprintf("at most %d domains can be bought at a time, got %d", MaxDomainsPerPurchase, n)})
	}
	if len(fields) > 0 {
		return nil, apperr.Validation("purchase is invalid", fields...)
	}

	raw := make([]string, len(in.Domains))
	for i, d := range in.Domains {
		raw[i] = d.Name
	}
	names, fields := normalizeNames(raw, "domains")
	seen := make(map[string]int, len(names))
	for i, name := range names {
		if name == "" {
			continue // already reported
		}
		field := fmt.Sprintf("domains[%d]", i)
		if first, ok := seen[name]; ok {
			fields = append(fields, apperr.FieldError{Field: field, Message: fmt.Sprintf("repeats domains[%d]", first)})
		}
		seen[name] = i
		if in.Domains[i].ExpectedCostCents < 0 {
			fields = append(fields, apperr.FieldError{Field: field + ".expected_cost_cents", Message: "must not be negative"})
		}
	}
	if len(fields) > 0 {
		return nil, apperr.Validation("purchase is invalid", fields...)
	}
	return names, nil
}

// refusal says why a domain cannot be bought at the confirmed price, as a code for
// the record and a sentence for a person. An empty code means it can.
func refusal(name string, offer Offer, found bool, confirmedCents int64) (string, string) {
	switch {
	case !found:
		return CodeNotReturned, fmt.Sprintf("Cloudflare did not return %s; check it again", name)
	case offer.Tier == cloudflare.TierPremium:
		return CodePremium, fmt.Sprintf("%s is a premium domain, which cannot be bought through the API", name)
	case !offer.Registrable:
		code := CodeUnavailable
		if offer.Reason != nil {
			code = *offer.Reason
		}
		return code, fmt.Sprintf("%s cannot be registered (%s)", name, strings.ReplaceAll(code, "_", " "))
	case offer.Pricing == nil:
		return CodeNoPrice, fmt.Sprintf("Cloudflare returned no usable price for %s", name)
	case offer.Pricing.RegistrationCostCents > confirmedCents:
		return CodePriceChanged, fmt.Sprintf("%s now costs %s %s, more than the %s confirmed", name,
			offer.Pricing.RegistrationCost, offer.Pricing.Currency, formatCents(confirmedCents))
	}
	return "", ""
}

func formatCents(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// GetPurchase returns one purchase with its domains.
func (s *Service) GetPurchase(ctx context.Context, id uuid.UUID) (Purchase, error) {
	row, err := s.store.GetDomainPurchase(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Purchase{}, apperr.NotFound("purchase")
	}
	if err != nil {
		return Purchase{}, apperr.Internal(err)
	}
	items, err := s.store.ListDomainPurchaseItems(ctx, id)
	if err != nil {
		return Purchase{}, apperr.Internal(err)
	}
	return Purchase{DomainPurchase: row, Items: items}, nil
}

// ListPurchases returns one page of purchases, newest first, with their domains.
func (s *Service) ListPurchases(ctx context.Context, page, perPage int) (PurchasePage, error) {
	rows, err := s.store.ListDomainPurchases(ctx, dbgen.ListDomainPurchasesParams{
		Limit:  int32(perPage),              //nolint:gosec // G115: bounded by the HTTP layer
		Offset: int32((page - 1) * perPage), //nolint:gosec // G115: bounded by the HTTP layer
	})
	if err != nil {
		return PurchasePage{}, apperr.Internal(err)
	}
	total, err := s.store.CountDomainPurchases(ctx)
	if err != nil {
		return PurchasePage{}, apperr.Internal(err)
	}
	purchaseIDs := make([]uuid.UUID, len(rows))
	for i, row := range rows {
		purchaseIDs[i] = row.ID
	}
	items, err := s.store.ListDomainPurchaseItemsFor(ctx, purchaseIDs)
	if err != nil {
		return PurchasePage{}, apperr.Internal(err)
	}
	byPurchase := make(map[uuid.UUID][]dbgen.DomainPurchaseItem, len(rows))
	for _, item := range items {
		byPurchase[item.PurchaseID] = append(byPurchase[item.PurchaseID], item)
	}
	out := make([]Purchase, len(rows))
	for i, row := range rows {
		out[i] = Purchase{DomainPurchase: row, Items: byPurchase[row.ID]}
	}
	return PurchasePage{Rows: out, Total: total}, nil
}

// ---------------------------------------------------------------------------
// Registrations the account owns

// ListRegistrations returns every domain the Cloudflare account owns.
func (s *Service) ListRegistrations(ctx context.Context) ([]Registration, error) {
	client, _, err := s.client(ctx, true)
	if err != nil {
		return nil, err
	}
	var out []Registration
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		result, err := client.ListRegistrations(ctx, cursor, cloudflare.MaxListPerPage)
		if err != nil {
			return nil, s.providerError(err, "list your domains")
		}
		out = append(out, result.Registrations...)
		if result.NextCursor == "" || result.NextCursor == cursor {
			break
		}
		cursor = result.NextCursor
	}
	return out, nil
}

// GetRegistration returns one domain the account owns.
func (s *Service) GetRegistration(ctx context.Context, domain string) (Registration, error) {
	name, err := NormalizeDomain(domain)
	if err != nil {
		return Registration{}, apperr.Validation("request parameters are invalid", apperr.FieldError{Field: "domain", Message: err.Error()})
	}
	client, _, err := s.client(ctx, true)
	if err != nil {
		return Registration{}, err
	}
	reg, err := client.GetRegistration(ctx, name)
	if errors.Is(err, cloudflare.ErrNotFound) {
		return Registration{}, apperr.NotFound("domain")
	}
	if err != nil {
		return Registration{}, s.providerError(err, "read the domain")
	}
	return reg, nil
}

// UpdateRegistration switches automatic renewal, the one setting Cloudflare lets the
// API change. Pending reports that Cloudflare accepted the change but had not applied
// it when it answered; the returned registration is then the state before it lands.
func (s *Service) UpdateRegistration(ctx context.Context, domain string, autoRenew bool) (Registration, bool, error) {
	name, err := NormalizeDomain(domain)
	if err != nil {
		return Registration{}, false, apperr.Validation("request parameters are invalid", apperr.FieldError{Field: "domain", Message: err.Error()})
	}
	client, _, err := s.client(ctx, true)
	if err != nil {
		return Registration{}, false, err
	}
	workflow, err := client.UpdateAutoRenew(ctx, name, autoRenew)
	if errors.Is(err, cloudflare.ErrNotFound) {
		return Registration{}, false, apperr.NotFound("domain")
	}
	if err != nil {
		return Registration{}, false, s.providerError(err, "update the domain")
	}
	switch workflow.State {
	case cloudflare.StateFailed, cloudflare.StateBlocked, cloudflare.StateActionRequired:
		message := "Cloudflare did not apply the change"
		if workflow.Error != nil && workflow.Error.Message != "" {
			message = workflow.Error.Message
		}
		return Registration{}, false, apperr.Conflict("%s", message)
	case cloudflare.StateSucceeded:
		if reg := workflow.Context.Registration; reg != nil {
			return *reg, false, nil
		}
	}
	reg, err := client.GetRegistration(ctx, name)
	if err != nil {
		return Registration{}, false, s.providerError(err, "read the domain")
	}
	return reg, workflow.State != cloudflare.StateSucceeded, nil
}
