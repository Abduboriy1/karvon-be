package workspace

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
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
	"github.com/bory/karvon-be/internal/registrar"
	"github.com/bory/karvon-be/internal/registrar/cloudflare"
	"github.com/bory/karvon-be/internal/workspace/google"
)

// passwordLength is the length of a generated mailbox password: about 119 bits.
const passwordLength = 20

const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// Enqueuer is the queue surface the service needs.
type Enqueuer interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// DNSConnector hands out the Cloudflare client the mail records are published with.
// The domains module owns the Cloudflare connection.
type DNSConnector interface {
	DNSClient(ctx context.Context) (*cloudflare.Client, error)
}

// Config holds everything the service needs besides the stored credentials.
type Config struct {
	TokenURL            string
	DirectoryURL        string
	SiteVerificationURL string
	// Timeout bounds one Google call.
	Timeout    time.Duration
	HTTPClient *http.Client
	// PollInterval is how often Google is asked again to verify a domain whose
	// record it cannot see yet.
	PollInterval time.Duration
	// VerifyMaxWait is how long after publishing the records a setup keeps asking
	// before handing the domain to a person.
	VerifyMaxWait time.Duration
	// InstantlyPollInterval is how often an open Instantly OAuth session is asked
	// about while a person signs in.
	InstantlyPollInterval time.Duration
}

// Service implements the /workspace endpoints and the setup worker.
type Service struct {
	store     *db.Store
	cipher    *crypto.Cipher
	cfg       Config
	dns       DNSConnector
	instantly InstantlyConnector
	queue     Enqueuer
	log       *slog.Logger
	now       func() time.Time
}

// NewService builds the workspace service. The queue is set later with SetQueue,
// because the River client is built after the services.
func NewService(store *db.Store, cipher *crypto.Cipher, dns DNSConnector, cfg Config, log *slog.Logger) *Service {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Minute
	}
	if cfg.VerifyMaxWait <= 0 {
		cfg.VerifyMaxWait = 2 * time.Hour
	}
	if cfg.InstantlyPollInterval <= 0 {
		cfg.InstantlyPollInterval = 3 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: store, cipher: cipher, cfg: cfg, dns: dns, log: log, now: time.Now}
}

// SetQueue wires the River client in once it exists.
func (s *Service) SetQueue(queue Enqueuer) { s.queue = queue }

// ---------------------------------------------------------------------------
// Settings

// Settings is the Google connection as the Mailboxes page shows it. The key is never
// returned, only whether one is stored and whose it is.
type Settings struct {
	SourceID               uuid.UUID
	AdminEmail             *string
	ServiceAccountEmail    *string
	ServiceAccountClientID *string
	HasKey                 bool
	Enabled                bool
	LastTestedAt           *time.Time
	LastTestOK             *bool
}

// Ready reports whether setups can run.
func (s Settings) Ready() bool {
	return s.AdminEmail != nil && s.HasKey && s.Enabled
}

// SettingsInput distinguishes "leave it alone" (Present false) from "clear it"
// (Present true, value nil) for the admin email and the key.
type SettingsInput struct {
	AdminEmailPresent bool
	AdminEmail        *string
	KeyPresent        bool
	Key               *string
	Enabled           *bool
}

// Settings returns the current connection.
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	settings, _, err := s.loadSettings(ctx)
	return settings, err
}

// SaveSettings stores the admin email, the service-account key (encrypted) and the
// enabled flag. The key is parsed first, so a file that is not a service-account key
// is refused rather than stored.
func (s *Service) SaveSettings(ctx context.Context, in SettingsInput) (Settings, error) {
	var fields []apperr.FieldError
	var adminEmail *string
	if in.AdminEmailPresent && in.AdminEmail != nil && strings.TrimSpace(*in.AdminEmail) != "" {
		email, err := NormalizeAdminEmail(*in.AdminEmail)
		if err != nil {
			fields = append(fields, apperr.FieldError{Field: "admin_email", Message: err.Error()})
		}
		adminEmail = &email
	}
	var keyEnc []byte
	var key google.Key
	if in.KeyPresent && in.Key != nil && strings.TrimSpace(*in.Key) != "" {
		raw := strings.TrimSpace(*in.Key)
		parsed, err := google.ParseKey([]byte(raw))
		switch {
		case len(raw) > MaxKeyLength:
			fields = append(fields, apperr.FieldError{Field: "service_account_key",
				Message: fmt.Sprintf("must be at most %d characters", MaxKeyLength)})
		case err != nil:
			fields = append(fields, apperr.FieldError{Field: "service_account_key", Message: err.Error()})
		default:
			encrypted, err := s.cipher.EncryptString(raw)
			if err != nil {
				return Settings{}, apperr.Internal(err)
			}
			keyEnc, key = encrypted, parsed
		}
	}
	if len(fields) > 0 {
		return Settings{}, apperr.Validation("mailbox settings are invalid", fields...)
	}

	source, err := s.source(ctx)
	if err != nil {
		return Settings{}, err
	}
	err = s.store.InTx(ctx, func(q *dbgen.Queries) error {
		if in.AdminEmailPresent {
			if _, err := q.SetWorkspaceAdminEmail(ctx, adminEmail); err != nil {
				return err
			}
		}
		if in.KeyPresent {
			params := dbgen.SetWorkspaceServiceAccountParams{}
			if keyEnc != nil {
				params.ServiceAccountEmail, params.ServiceAccountClientID = &key.ClientEmail, &key.ClientID
			}
			if _, err := q.SetWorkspaceServiceAccount(ctx, params); err != nil {
				return err
			}
		}
		if in.KeyPresent || in.Enabled != nil {
			_, err := q.UpdateSource(ctx, dbgen.UpdateSourceParams{
				ID:        source.ID,
				Enabled:   in.Enabled,
				SetKey:    in.KeyPresent,
				ApiKeyEnc: keyEnc,
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

// TestResult is the outcome of a connection test: the Workspace account the service
// account reached.
type TestResult struct {
	OK            bool
	TestedAt      time.Time
	PrimaryDomain *string
	DomainCount   int
}

// TestConnection proves the key, the domain-wide delegation, the scopes and the admin
// with one free read. A disabled connection can be tested. The result is recorded on
// the source row.
func (s *Service) TestConnection(ctx context.Context) (TestResult, error) {
	client, settings, err := s.client(ctx, false)
	if err != nil {
		return TestResult{}, err
	}
	domains, err := client.ListDomains(ctx)
	s.recordTest(ctx, settings.SourceID, err == nil)
	if err != nil {
		return TestResult{}, s.providerError(err, "test the connection")
	}
	result := TestResult{OK: true, TestedAt: s.now().UTC(), DomainCount: len(domains)}
	for _, d := range domains {
		if d.IsPrimary {
			name := d.DomainName
			result.PrimaryDomain = &name
		}
	}
	return result, nil
}

func (s *Service) recordTest(ctx context.Context, sourceID uuid.UUID, ok bool) {
	if err := s.store.SetSourceTestResult(ctx, dbgen.SetSourceTestResultParams{ID: sourceID, Ok: &ok}); err != nil {
		s.log.Warn("could not record the Google Workspace test result", "error", err)
	}
}

func (s *Service) source(ctx context.Context) (dbgen.Source, error) {
	row, err := s.store.GetSourceByKind(ctx, KindGoogleWorkspace)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.Source{}, apperr.Internal(errors.New("workspace: the google_workspace source row is missing; run the migrations"))
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
	row, err := s.store.GetWorkspaceSettings(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Settings{}, dbgen.Source{}, apperr.Internal(err)
	}
	return Settings{
		SourceID:               source.ID,
		AdminEmail:             row.AdminEmail,
		ServiceAccountEmail:    row.ServiceAccountEmail,
		ServiceAccountClientID: row.ServiceAccountClientID,
		HasKey:                 len(source.ApiKeyEnc) > 0,
		Enabled:                source.Enabled,
		LastTestedAt:           source.LastTestedAt,
		LastTestOK:             source.LastTestOk,
	}, source, nil
}

// client builds a Google client from the stored settings. requireEnabled is false
// only for the connection test.
func (s *Service) client(ctx context.Context, requireEnabled bool) (*google.Client, Settings, error) {
	settings, source, err := s.loadSettings(ctx)
	if err != nil {
		return nil, Settings{}, err
	}
	switch {
	case settings.AdminEmail == nil:
		return nil, settings, apperr.Conflict("the Workspace admin email is not set; add it under Settings → Mailboxes")
	case !settings.HasKey:
		return nil, settings, apperr.Conflict("no service-account key is stored; add one under Settings → Mailboxes")
	case requireEnabled && !settings.Enabled:
		return nil, settings, apperr.Conflict("the Google Workspace connection is switched off; enable it under Settings → Mailboxes")
	}
	raw, err := s.cipher.DecryptString(source.ApiKeyEnc)
	if err != nil {
		// A key that cannot be decrypted means KARVON_SECRET_KEY changed.
		return nil, settings, apperr.ProviderAuth("the stored service-account key cannot be decrypted; save it again").WithCause(err)
	}
	key, err := google.ParseKey([]byte(raw))
	if err != nil {
		return nil, settings, apperr.ProviderAuth("the stored service-account key %s; save it again", err.Error())
	}
	return google.New(google.Config{
		TokenURL:            s.cfg.TokenURL,
		DirectoryURL:        s.cfg.DirectoryURL,
		SiteVerificationURL: s.cfg.SiteVerificationURL,
		Key:                 key,
		Subject:             *settings.AdminEmail,
		Timeout:             s.cfg.Timeout,
		HTTPClient:          s.cfg.HTTPClient,
	}), settings, nil
}

// providerError turns a Google or Cloudflare failure into the error the client sees.
func (s *Service) providerError(err error, action string) error {
	var googleErr *google.APIError
	var cloudflareErr *cloudflare.APIError
	switch {
	case errors.Is(err, google.ErrAuth):
		return apperr.ProviderAuth("%s", authMessage(err)).WithCause(err)
	case errors.Is(err, google.ErrRateLimited):
		return apperr.ProviderError("Google is rate limiting this account; try again in a minute").WithCause(err)
	case errors.As(err, &googleErr) && google.Definite(err):
		detail := googleErr.Detail()
		if detail == "" {
			detail = "Google rejected the request"
		}
		return apperr.Validation(detail).WithCause(err)
	case errors.Is(err, cloudflare.ErrAuth):
		return apperr.ProviderAuth("%s", dnsAuthMessage).WithCause(err)
	case errors.As(err, &cloudflareErr) && cloudflare.Definite(err):
		detail := cloudflareErr.Detail()
		if detail == "" {
			detail = "Cloudflare rejected the DNS change"
		}
		return apperr.Validation(detail).WithCause(err)
	}
	s.log.Warn("workspace provider call failed", "action", action, "error", err)
	return apperr.ProviderError("could not %s", action).WithCause(err)
}

const dnsAuthMessage = "Cloudflare refused the DNS change; the API token needs Zone → Zone → Read and " +
	"Zone → DNS → Edit for this domain as well as the Registrar permission"

// authMessage explains a refused Google call. The token endpoint's refusals nearly
// always mean domain-wide delegation is missing or the admin is wrong.
func authMessage(err error) string {
	var apiErr *google.APIError
	detail := ""
	if errors.As(err, &apiErr) && apiErr.Detail() != "" {
		detail = " (" + apiErr.Detail() + ")"
	}
	if errors.As(err, &apiErr) && apiErr.Token {
		return "Google refused the service account" + detail + "; check domain-wide delegation is set up for its " +
			"client ID with the scopes shown under Settings → Mailboxes, and that the admin email is a super admin"
	}
	return "Google refused the request" + detail + "; check the admin email is a super admin and the scopes are delegated"
}

// ---------------------------------------------------------------------------
// Setups

// MailboxInput is one mailbox to create.
type MailboxInput struct {
	LocalPart  string
	GivenName  string
	FamilyName string
}

// SetupInput is a confirmed setup of one domain.
type SetupInput struct {
	Domain    string
	Mailboxes []MailboxInput
	// Confirm must be true. Every mailbox is a paid licence, so nothing is created
	// by a stray call.
	Confirm bool
}

// Setup is a domain with its mailboxes.
type Setup struct {
	dbgen.WorkspaceDomain
	Mailboxes []dbgen.WorkspaceMailbox
}

// SetupPage is one page of setups.
type SetupPage struct {
	Rows  []Setup
	Total int64
}

// Credentials are what a person signs in to a mailbox with.
type Credentials struct {
	Email    string
	Password string
}

type validSetup struct {
	domain    string
	mailboxes []MailboxInput
}

// CreateSetup checks the domain can be set up — both connections ready, its DNS in
// the Cloudflare account, not set up already — and queues it.
func (s *Service) CreateSetup(ctx context.Context, in SetupInput) (Setup, error) {
	valid, err := validateSetup(in)
	if err != nil {
		return Setup{}, err
	}
	if s.queue == nil {
		return Setup{}, apperr.Internal(errors.New("workspace: no queue is configured"))
	}
	if existing, err := s.store.GetWorkspaceDomainByName(ctx, valid.domain); err == nil {
		return Setup{}, apperr.Conflict("%s is already set up (%s)", valid.domain, existing.Status)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Setup{}, apperr.Internal(err)
	}
	if _, _, err := s.client(ctx, true); err != nil {
		return Setup{}, err
	}
	dns, err := s.dns.DNSClient(ctx)
	if err != nil {
		return Setup{}, err
	}
	if _, err := dns.FindZone(ctx, valid.domain); err != nil {
		if errors.Is(err, cloudflare.ErrNotFound) {
			return Setup{}, apperr.Conflict("%s has no DNS zone in the Cloudflare account; its DNS must be on Cloudflare", valid.domain)
		}
		return Setup{}, s.providerError(err, "find the domain's DNS zone")
	}

	positions := []int16{0, 1, 2, 3, 4}[:len(valid.mailboxes)]
	rows, err := s.newMailboxRows(valid.domain, valid.mailboxes, positions)
	if err != nil {
		return Setup{}, err
	}

	domainID := ids.New()
	err = s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.CreateWorkspaceDomain(ctx, dbgen.CreateWorkspaceDomainParams{ID: domainID, DomainName: valid.domain}); err != nil {
			return err
		}
		for _, row := range rows {
			row.DomainID = domainID
			if _, err := q.CreateWorkspaceMailbox(ctx, row); err != nil {
				return err
			}
		}
		_, err := s.queue.InsertTx(ctx, tx, SetupArgs{DomainID: domainID}, nil)
		return err
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Setup{}, apperr.Conflict("%s or one of its addresses is already set up", valid.domain)
		}
		return Setup{}, apperr.Internal(err)
	}
	s.log.Info("workspace setup queued", "domain", valid.domain, "mailboxes", len(valid.mailboxes))
	return s.setup(ctx, domainID)
}

// validateSetup applies the rules that need no network.
func validateSetup(in SetupInput) (validSetup, error) {
	var fields []apperr.FieldError
	if !in.Confirm {
		fields = append(fields, apperr.FieldError{Field: "confirm", Message: "must be true; every mailbox is a paid Workspace licence"})
	}
	domain, err := registrar.NormalizeDomain(in.Domain)
	if err != nil {
		fields = append(fields, apperr.FieldError{Field: "domain", Message: err.Error()})
	}
	mailboxes, mailboxFields := validateMailboxes(in.Mailboxes)
	fields = append(fields, mailboxFields...)
	if len(fields) > 0 {
		return validSetup{}, apperr.Validation("setup is invalid", fields...)
	}
	return validSetup{domain: domain, mailboxes: mailboxes}, nil
}

// validateMailboxes checks one to MaxMailboxesPerDomain new mailboxes.
func validateMailboxes(in []MailboxInput) ([]MailboxInput, []apperr.FieldError) {
	var fields []apperr.FieldError
	switch n := len(in); {
	case n == 0:
		fields = append(fields, apperr.FieldError{Field: "mailboxes", Message: "must hold at least one mailbox"})
	case n > MaxMailboxesPerDomain:
		fields = append(fields, apperr.FieldError{Field: "mailboxes",
			Message: fmt.Sprintf("at most %d mailboxes per domain, got %d", MaxMailboxesPerDomain, n)})
	}
	out := make([]MailboxInput, 0, len(in))
	seen := map[string]int{}
	for i, mb := range in {
		field := fmt.Sprintf("mailboxes[%d]", i)
		local, err := NormalizeLocalPart(mb.LocalPart)
		if err != nil {
			fields = append(fields, apperr.FieldError{Field: field + ".local_part", Message: err.Error()})
		} else if first, dup := seen[local]; dup {
			fields = append(fields, apperr.FieldError{Field: field + ".local_part", Message: fmt.Sprintf("repeats mailboxes[%d]", first)})
		} else {
			seen[local] = i
		}
		given, err := NormalizeName(mb.GivenName)
		if err != nil {
			fields = append(fields, apperr.FieldError{Field: field + ".given_name", Message: err.Error()})
		}
		family, err := NormalizeName(mb.FamilyName)
		if err != nil {
			fields = append(fields, apperr.FieldError{Field: field + ".family_name", Message: err.Error()})
		}
		out = append(out, MailboxInput{LocalPart: local, GivenName: given, FamilyName: family})
	}
	return out, fields
}

// newMailboxRows builds the rows for new mailboxes, each with a generated password,
// encrypted. DomainID is left for the caller.
func (s *Service) newMailboxRows(domain string, mailboxes []MailboxInput, positions []int16) ([]dbgen.CreateWorkspaceMailboxParams, error) {
	rows := make([]dbgen.CreateWorkspaceMailboxParams, len(mailboxes))
	for i, mb := range mailboxes {
		password, err := generatePassword()
		if err != nil {
			return nil, apperr.Internal(err)
		}
		encrypted, err := s.cipher.EncryptString(password)
		if err != nil {
			return nil, apperr.Internal(err)
		}
		rows[i] = dbgen.CreateWorkspaceMailboxParams{
			ID:          ids.New(),
			Position:    positions[i],
			Email:       mb.LocalPart + "@" + domain,
			GivenName:   mb.GivenName,
			FamilyName:  mb.FamilyName,
			PasswordEnc: encrypted,
		}
	}
	return rows, nil
}

// AddMailboxes creates more mailboxes on a domain that is already set up. The domain
// goes back to provisioning for as long as they take; every finished step is skipped.
// A domain holds at most MaxMailboxesPerDomain mailboxes, failed ones included until
// they are removed.
func (s *Service) AddMailboxes(ctx context.Context, domain string, in []MailboxInput, confirm bool) (Setup, error) {
	mailboxes, fields := validateMailboxes(in)
	if !confirm {
		fields = append(fields, apperr.FieldError{Field: "confirm", Message: "must be true; every mailbox is a paid Workspace licence"})
	}
	if len(fields) > 0 {
		return Setup{}, apperr.Validation("mailboxes are invalid", fields...)
	}
	row, err := s.domainByName(ctx, domain)
	if err != nil {
		return Setup{}, err
	}
	switch row.Status {
	case DomainProvisioning:
		return Setup{}, apperr.Conflict("%s is still being set up; add mailboxes once it finishes", row.DomainName)
	case DomainFailed:
		return Setup{}, apperr.Conflict("%s failed; retry it before adding mailboxes", row.DomainName)
	}
	if s.queue == nil {
		return Setup{}, apperr.Internal(errors.New("workspace: no queue is configured"))
	}
	existing, err := s.store.ListWorkspaceMailboxes(ctx, row.ID)
	if err != nil {
		return Setup{}, apperr.Internal(err)
	}
	used := map[int16]bool{}
	taken := map[string]bool{}
	for _, mb := range existing {
		used[mb.Position] = true
		taken[mb.Email] = true
	}
	for i, mb := range mailboxes {
		if taken[mb.LocalPart+"@"+row.DomainName] {
			fields = append(fields, apperr.FieldError{Field: fmt.Sprintf("mailboxes[%d].local_part", i),
				Message: fmt.Sprintf("%s@%s is already on this domain", mb.LocalPart, row.DomainName)})
		}
	}
	var free []int16
	for p := int16(0); p < MaxMailboxesPerDomain; p++ {
		if !used[p] {
			free = append(free, p)
		}
	}
	if len(mailboxes) > len(free) {
		fields = append(fields, apperr.FieldError{Field: "mailboxes", Message: fmt.Sprintf(
			"%s has %d of %d mailboxes; add at most %d, or remove failed ones first",
			row.DomainName, len(existing), MaxMailboxesPerDomain, len(free))})
	}
	if len(fields) > 0 {
		return Setup{}, apperr.Validation("mailboxes are invalid", fields...)
	}
	if _, _, err := s.client(ctx, true); err != nil {
		return Setup{}, err
	}

	rows, err := s.newMailboxRows(row.DomainName, mailboxes, free[:len(mailboxes)])
	if err != nil {
		return Setup{}, err
	}
	err = s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.ReopenWorkspaceDomain(ctx, row.ID); err != nil {
			return err
		}
		for _, mb := range rows {
			mb.DomainID = row.ID
			if _, err := q.CreateWorkspaceMailbox(ctx, mb); err != nil {
				return err
			}
		}
		_, err := s.queue.InsertTx(ctx, tx, SetupArgs{DomainID: row.ID}, nil)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Setup{}, apperr.Conflict("%s changed while the mailboxes were added; reload it", row.DomainName)
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Setup{}, apperr.Conflict("one of these addresses is already set up")
		}
		return Setup{}, apperr.Internal(err)
	}
	s.log.Info("workspace mailboxes added", "domain", row.DomainName, "mailboxes", len(rows))
	return s.setup(ctx, row.ID)
}

// DeleteMailbox removes a mailbox that was never created, freeing its slot. A created
// mailbox is a Workspace user: it is deleted in the Admin console, not here.
func (s *Service) DeleteMailbox(ctx context.Context, id uuid.UUID) error {
	if _, err := s.store.DeleteFailedWorkspaceMailbox(ctx, id); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return apperr.Internal(err)
	}
	mb, err := s.store.GetWorkspaceMailbox(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.NotFound("mailbox")
	}
	if err != nil {
		return apperr.Internal(err)
	}
	return apperr.Conflict("only a failed mailbox can be removed; %s is %s (delete a created one in the Admin console)",
		mb.Email, mb.Status)
}

// GetSetup returns one domain's setup.
func (s *Service) GetSetup(ctx context.Context, domain string) (Setup, error) {
	row, err := s.domainByName(ctx, domain)
	if err != nil {
		return Setup{}, err
	}
	return s.setup(ctx, row.ID)
}

// ListSetups returns one page of setups, newest first, with their mailboxes.
func (s *Service) ListSetups(ctx context.Context, page, perPage int) (SetupPage, error) {
	rows, err := s.store.ListWorkspaceDomains(ctx, dbgen.ListWorkspaceDomainsParams{
		Limit:  int32(perPage),              //nolint:gosec // G115: bounded by the HTTP layer
		Offset: int32((page - 1) * perPage), //nolint:gosec // G115: bounded by the HTTP layer
	})
	if err != nil {
		return SetupPage{}, apperr.Internal(err)
	}
	total, err := s.store.CountWorkspaceDomains(ctx)
	if err != nil {
		return SetupPage{}, apperr.Internal(err)
	}
	domainIDs := make([]uuid.UUID, len(rows))
	for i, row := range rows {
		domainIDs[i] = row.ID
	}
	mailboxes, err := s.store.ListWorkspaceMailboxesFor(ctx, domainIDs)
	if err != nil {
		return SetupPage{}, apperr.Internal(err)
	}
	byDomain := make(map[uuid.UUID][]dbgen.WorkspaceMailbox, len(rows))
	for _, mb := range mailboxes {
		byDomain[mb.DomainID] = append(byDomain[mb.DomainID], mb)
	}
	out := make([]Setup, len(rows))
	for i, row := range rows {
		out[i] = Setup{WorkspaceDomain: row, Mailboxes: byDomain[row.ID]}
	}
	return SetupPage{Rows: out, Total: total}, nil
}

// RetrySetup queues a failed setup again. It picks up where it stopped: finished
// steps are skipped, refused mailboxes are sent again, and an address that belonged
// to someone else stays failed.
func (s *Service) RetrySetup(ctx context.Context, domain string) (Setup, error) {
	row, err := s.domainByName(ctx, domain)
	if err != nil {
		return Setup{}, err
	}
	if s.queue == nil {
		return Setup{}, apperr.Internal(errors.New("workspace: no queue is configured"))
	}
	if row.Status != DomainFailed {
		return Setup{}, apperr.Conflict("only a failed setup can be retried; %s is %s", row.DomainName, row.Status)
	}
	err = s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.RetryWorkspaceDomain(ctx, row.ID); err != nil {
			return err
		}
		if err := q.ResetFailedWorkspaceMailboxes(ctx, row.ID); err != nil {
			return err
		}
		_, err := s.queue.InsertTx(ctx, tx, SetupArgs{DomainID: row.ID}, nil)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Setup{}, apperr.Conflict("%s is no longer failed", row.DomainName)
	}
	if err != nil {
		return Setup{}, apperr.Internal(err)
	}
	s.log.Info("workspace setup retried", "domain", row.DomainName)
	return s.setup(ctx, row.ID)
}

// PublishDKIM writes the DKIM record the Admin console generated and marks the domain
// active. Publishing again replaces the record, which is how a key is rotated. After
// it, "Start authentication" still has to be pressed in the Admin console.
func (s *Service) PublishDKIM(ctx context.Context, domain, selector, value string) (Setup, error) {
	var fields []apperr.FieldError
	sel, err := NormalizeSelector(selector)
	if err != nil {
		fields = append(fields, apperr.FieldError{Field: "selector", Message: err.Error()})
	}
	record, err := NormalizeDKIM(value)
	if err != nil {
		fields = append(fields, apperr.FieldError{Field: "value", Message: err.Error()})
	}
	if len(fields) > 0 {
		return Setup{}, apperr.Validation("DKIM record is invalid", fields...)
	}

	row, err := s.domainByName(ctx, domain)
	if err != nil {
		return Setup{}, err
	}
	if row.Status != DomainDKIMRequired && row.Status != DomainActive {
		return Setup{}, apperr.Conflict("DKIM is published once %s is verified and its mailboxes are created; it is %s",
			row.DomainName, row.Status)
	}
	dns, err := s.dns.DNSClient(ctx)
	if err != nil {
		return Setup{}, err
	}
	if err := upsertDKIM(ctx, dns, row.DomainName, sel, record); err != nil {
		return Setup{}, s.providerError(err, "publish the DKIM record")
	}
	if _, err := s.store.PublishWorkspaceDKIM(ctx, dbgen.PublishWorkspaceDKIMParams{ID: row.ID, DkimSelector: &sel}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Setup{}, apperr.Conflict("%s changed while the record was published; reload it", row.DomainName)
		}
		return Setup{}, apperr.Internal(err)
	}
	s.log.Info("workspace DKIM published", "domain", row.DomainName, "selector", sel)
	return s.setup(ctx, row.ID)
}

// MailboxCredentials returns a created mailbox's address and initial password.
func (s *Service) MailboxCredentials(ctx context.Context, id uuid.UUID) (Credentials, error) {
	mb, err := s.store.GetWorkspaceMailbox(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credentials{}, apperr.NotFound("mailbox")
	}
	if err != nil {
		return Credentials{}, apperr.Internal(err)
	}
	if mb.Status != MailboxCreated {
		return Credentials{}, apperr.Conflict("%s has not been created (%s)", mb.Email, mb.Status)
	}
	password, err := s.cipher.DecryptString(mb.PasswordEnc)
	if err != nil {
		return Credentials{}, apperr.Internal(fmt.Errorf("workspace: decrypt mailbox password: %w", err))
	}
	return Credentials{Email: mb.Email, Password: password}, nil
}

func (s *Service) domainByName(ctx context.Context, domain string) (dbgen.WorkspaceDomain, error) {
	name, err := registrar.NormalizeDomain(domain)
	if err != nil {
		return dbgen.WorkspaceDomain{}, apperr.Validation("request parameters are invalid",
			apperr.FieldError{Field: "domain", Message: err.Error()})
	}
	row, err := s.store.GetWorkspaceDomainByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.WorkspaceDomain{}, apperr.NotFound("domain setup")
	}
	if err != nil {
		return dbgen.WorkspaceDomain{}, apperr.Internal(err)
	}
	return row, nil
}

func (s *Service) setup(ctx context.Context, id uuid.UUID) (Setup, error) {
	row, err := s.store.GetWorkspaceDomain(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Setup{}, apperr.NotFound("domain setup")
	}
	if err != nil {
		return Setup{}, apperr.Internal(err)
	}
	mailboxes, err := s.store.ListWorkspaceMailboxes(ctx, id)
	if err != nil {
		return Setup{}, apperr.Internal(err)
	}
	return Setup{WorkspaceDomain: row, Mailboxes: mailboxes}, nil
}

// generatePassword returns a random password Workspace accepts (8-100 characters).
func generatePassword() (string, error) {
	var b strings.Builder
	limit := big.NewInt(int64(len(passwordAlphabet)))
	for range passwordLength {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("workspace: generate password: %w", err)
		}
		b.WriteByte(passwordAlphabet[n.Int64()])
	}
	return b.String(), nil
}
