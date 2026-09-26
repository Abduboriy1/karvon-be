// Package exclusion manages the global exclusion list: companies, addresses and
// domains that must never be verified, enrolled, exported or contacted.
//
// A rule is stored once and enforced everywhere by the database views behind
// internal/db/exclusions.go, so this package only has to get a rule into the table
// in its canonical form and answer the question "is this excluded?". Nothing is
// deleted or rewritten when a rule is added: the scraped data stays, and records
// stop being eligible because the views say so.
package exclusion

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"golang.org/x/net/publicsuffix"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/business"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// Rule kinds, mirroring the global_exclusions.kind check constraint.
const (
	KindCompany     = "company"
	KindEmail       = "email"
	KindEmailDomain = "email_domain"
	KindDomain      = "domain"
)

// Kinds lists every rule kind the schema accepts.
var Kinds = []string{KindCompany, KindEmail, KindEmailDomain, KindDomain}

// Match modes. Prefix is for company rules only.
const (
	MatchExact  = "exact"
	MatchPrefix = "prefix"
)

// Sources record where a rule was created from.
const (
	SourceManual       = "manual"
	SourceBusiness     = "business"
	SourceVerification = "verification"
	SourceLead         = "lead"
	SourceContact      = "contact"
)

// Sources lists every source the schema accepts.
var Sources = []string{SourceManual, SourceBusiness, SourceVerification, SourceLead, SourceContact}

// Limits on what a rule may hold.
const (
	MaxValueLen  = 500
	MaxReasonLen = 1000
	// minCompanyKeyLen keeps a stray "a" or "co" from excluding half the list.
	minCompanyKeyLen = 3
	// broadRuleThreshold is how many businesses a rule may cover before the
	// preview warns that it looks broader than intended.
	broadRuleThreshold = 250
)

// freeMailDomains are shared mailbox providers. A domain rule on one of them
// excludes every business that uses it for its contact address.
var freeMailDomains = map[string]struct{}{
	"gmail.com": {}, "googlemail.com": {}, "yahoo.com": {}, "ymail.com": {}, "hotmail.com": {},
	"outlook.com": {}, "live.com": {}, "msn.com": {}, "aol.com": {}, "icloud.com": {}, "me.com": {},
	"mac.com": {}, "protonmail.com": {}, "proton.me": {}, "gmx.com": {}, "mail.com": {},
	"zoho.com": {}, "yandex.com": {}, "comcast.net": {}, "att.net": {}, "sbcglobal.net": {},
	"verizon.net": {}, "bellsouth.net": {}, "cox.net": {}, "charter.net": {},
}

// Enqueuer is the queue surface the service needs.
type Enqueuer interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// Service implements the /exclusions endpoints and the one exclusion check the
// rest of the application relies on.
type Service struct {
	store *db.Store
	queue Enqueuer
}

// NewService builds the exclusion service. The queue is set later with SetQueue,
// because the River client is built after the services.
func NewService(store *db.Store) *Service {
	return &Service{store: store}
}

// SetQueue wires the River client in once it exists.
func (s *Service) SetQueue(queue Enqueuer) { s.queue = queue }

// Input is a rule to create or preview.
type Input struct {
	Kind        string
	Value       string
	MatchMode   string
	Reason      *string
	Source      string
	SourceRefID *uuid.UUID
}

// Rule is a stored rule with what it covers.
type Rule struct {
	dbgen.GlobalExclusion
	Affected db.ExclusionAffected
}

// Preview is what a prospective rule would cover.
type Preview struct {
	Kind         string
	Value        string
	DisplayValue string
	MatchMode    string
	ExistingID   *uuid.UUID
	db.ExclusionPreview
	Warning *string
}

// Subject is anything that may be excluded. Every field is optional; Check reports
// the first rule that covers any of them.
type Subject struct {
	BusinessID *uuid.UUID
	Email      string
	Domain     string
	Company    string
}

// Page is one page of rules.
type Page struct {
	Rows  []Rule
	Total int64
}

// List returns one page of rules with their current coverage.
func (s *Service) List(ctx context.Context, f db.ExclusionFilter, sort string, page, perPage int) (Page, error) {
	rows, err := s.store.ListExclusions(ctx, f, sort, perPage, (page-1)*perPage)
	if err != nil {
		return Page{}, apperr.Internal(err)
	}
	total, err := s.store.CountExclusions(ctx, f)
	if err != nil {
		return Page{}, apperr.Internal(err)
	}
	out, err := s.withCounts(ctx, rows)
	if err != nil {
		return Page{}, err
	}
	return Page{Rows: out, Total: total}, nil
}

// Get returns one rule with its coverage.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Rule, error) {
	row, err := s.store.GetGlobalExclusion(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, apperr.NotFound("exclusion")
	}
	if err != nil {
		return Rule{}, apperr.Internal(err)
	}
	out, err := s.withCounts(ctx, []dbgen.GlobalExclusion{row})
	if err != nil {
		return Rule{}, err
	}
	return out[0], nil
}

// Create stores a rule and queues the sweep that takes matching campaign leads out
// of their campaigns. An active rule with the same kind and key is a 409.
func (s *Service) Create(ctx context.Context, in Input) (Rule, error) {
	params, err := s.normalize(ctx, in)
	if err != nil {
		return Rule{}, err
	}
	var row dbgen.GlobalExclusion
	err = s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		created, err := dbgen.New(tx).CreateGlobalExclusion(ctx, params)
		if err != nil {
			return err
		}
		row = created
		return s.enqueueSweep(ctx, tx, created.ID, false)
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Rule{}, apperr.Conflict("%s %q is already excluded", kindLabel(params.Kind), params.Value)
		}
		return Rule{}, apperr.Internal(err)
	}
	return s.Get(ctx, row.ID)
}

// Remove retires a rule. It is kept for audit, stops applying at once, and the
// sweep puts back the leads it took out that never reached the provider.
func (s *Service) Remove(ctx context.Context, id uuid.UUID, note string) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	var notePtr *string
	if note = strings.TrimSpace(note); note != "" {
		if len(note) > MaxReasonLen {
			return apperr.Validation("the note is too long",
				apperr.FieldError{Field: "note", Message: fmt.Sprintf("must be at most %d characters", MaxReasonLen)})
		}
		notePtr = &note
	}
	err := s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		if _, err := dbgen.New(tx).RemoveGlobalExclusion(ctx, dbgen.RemoveGlobalExclusionParams{ID: id, Note: notePtr}); err != nil {
			return err
		}
		return s.enqueueSweep(ctx, tx, id, true)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.Conflict("this exclusion was already removed")
	}
	if err != nil {
		return apperr.Internal(err)
	}
	return nil
}

// Preview normalises a prospective rule and measures it without saving it.
func (s *Service) Preview(ctx context.Context, in Input) (Preview, error) {
	params, err := s.normalize(ctx, in)
	if err != nil {
		return Preview{}, err
	}
	out := Preview{Kind: params.Kind, Value: params.Value, DisplayValue: params.DisplayValue, MatchMode: params.MatchMode}
	existing, err := s.store.FindActiveGlobalExclusion(ctx, dbgen.FindActiveGlobalExclusionParams{Kind: params.Kind, Value: params.Value})
	switch {
	case err == nil:
		out.ExistingID = &existing.ID
	case !errors.Is(err, pgx.ErrNoRows):
		return Preview{}, apperr.Internal(err)
	}
	measured, err := s.store.PreviewExclusion(ctx, params)
	if err != nil {
		return Preview{}, apperr.Internal(err)
	}
	out.ExclusionPreview = measured
	out.Warning = warningFor(params, measured.Affected)
	return out, nil
}

// Check is the one answer to "is this business, address, domain or company globally
// excluded?". It returns the covering rule, or nil. Every workflow that is about to
// verify, enrol, export or contact something asks through here or through the same
// views in SQL.
func (s *Service) Check(ctx context.Context, subject Subject) (*db.ExclusionRef, error) {
	if subject.BusinessID != nil {
		ref, err := s.store.MatchBusiness(ctx, *subject.BusinessID)
		if err != nil || ref != nil {
			return ref, wrapInternal(err)
		}
	}
	if email := strings.TrimSpace(subject.Email); email != "" {
		ref, err := s.store.MatchEmail(ctx, email)
		if err != nil || ref != nil {
			return ref, wrapInternal(err)
		}
	}
	if host := domainOf(subject.Domain); host != "" {
		ref, err := s.store.MatchDomain(ctx, host)
		if err != nil || ref != nil {
			return ref, wrapInternal(err)
		}
	}
	if company := strings.TrimSpace(subject.Company); company != "" {
		ref, err := s.store.MatchCompany(ctx, company)
		if err != nil || ref != nil {
			return ref, wrapInternal(err)
		}
	}
	return nil, nil
}

// normalize validates a rule and computes its canonical match key.
func (s *Service) normalize(ctx context.Context, in Input) (dbgen.CreateGlobalExclusionParams, error) {
	var fields []apperr.FieldError
	display := strings.Join(strings.Fields(in.Value), " ")
	if display == "" {
		fields = append(fields, apperr.FieldError{Field: "value", Message: "is required"})
	} else if len(display) > MaxValueLen {
		fields = append(fields, apperr.FieldError{Field: "value", Message: fmt.Sprintf("must be at most %d characters", MaxValueLen)})
	}
	if !slices.Contains(Kinds, in.Kind) {
		fields = append(fields, apperr.FieldError{Field: "kind", Message: "must be one of " + strings.Join(Kinds, ", ")})
	}
	mode := in.MatchMode
	if mode == "" {
		mode = MatchExact
	}
	switch {
	case mode != MatchExact && mode != MatchPrefix:
		fields = append(fields, apperr.FieldError{Field: "match_mode", Message: "must be exact or prefix"})
	case mode == MatchPrefix && in.Kind != KindCompany:
		fields = append(fields, apperr.FieldError{Field: "match_mode", Message: "prefix matching applies to company rules only"})
	}
	source := in.Source
	if source == "" {
		source = SourceManual
	}
	if !slices.Contains(Sources, source) {
		fields = append(fields, apperr.FieldError{Field: "source", Message: "must be one of " + strings.Join(Sources, ", ")})
	}
	var reason *string
	if in.Reason != nil {
		if r := strings.TrimSpace(*in.Reason); r != "" {
			if len(r) > MaxReasonLen {
				fields = append(fields, apperr.FieldError{Field: "reason", Message: fmt.Sprintf("must be at most %d characters", MaxReasonLen)})
			}
			reason = &r
		}
	}
	if len(fields) > 0 {
		return dbgen.CreateGlobalExclusionParams{}, apperr.Validation("exclusion is invalid", fields...)
	}

	value, err := s.matchKey(ctx, in.Kind, display)
	if err != nil {
		return dbgen.CreateGlobalExclusionParams{}, err
	}
	var ref uuid.NullUUID
	if in.SourceRefID != nil {
		ref = uuid.NullUUID{UUID: *in.SourceRefID, Valid: true}
	}
	return dbgen.CreateGlobalExclusionParams{
		ID: ids.New(), Kind: in.Kind, Value: value, DisplayValue: display, MatchMode: mode,
		Reason: reason, Source: source, SourceRefID: ref,
	}, nil
}

// matchKey reduces a value to the form the views compare against.
func (s *Service) matchKey(ctx context.Context, kind, display string) (string, error) {
	invalid := func(msg string) error {
		return apperr.Validation("exclusion is invalid", apperr.FieldError{Field: "value", Message: msg})
	}
	switch kind {
	case KindEmail:
		email, ok := business.NormalizeEmail(display)
		if !ok {
			return "", invalid("is not a valid email address")
		}
		return email, nil
	case KindDomain, KindEmailDomain:
		host := domainOf(display)
		if host == "" {
			return "", invalid("is not a valid domain")
		}
		if suffix, _ := publicsuffix.PublicSuffix(host); suffix == host {
			return "", invalid(fmt.Sprintf("%q is a public suffix; name a domain under it", host))
		}
		return host, nil
	default:
		key, err := s.store.ExclusionCompanyKey(ctx, display)
		if err != nil {
			return "", apperr.Internal(err)
		}
		if len(key) < minCompanyKeyLen {
			return "", invalid("is too short to match reliably once punctuation and legal suffixes are removed")
		}
		return key, nil
	}
}

// domainOf reduces a URL, a bare host, "@host" or an address to its lower-case host
// without "www.", or "" when there is no usable host.
func domainOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.LastIndex(raw, "@"); i >= 0 {
		raw = raw[i+1:]
	}
	raw = strings.TrimSuffix(raw, ".")
	_, host, ok := business.NormalizeWebsite(raw)
	if !ok {
		return ""
	}
	return strings.TrimSuffix(host, ".")
}

func (s *Service) withCounts(ctx context.Context, rows []dbgen.GlobalExclusion) ([]Rule, error) {
	active := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if r.RemovedAt == nil {
			active = append(active, r.ID)
		}
	}
	counts, err := s.store.ExclusionAffectedCounts(ctx, active)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	out := make([]Rule, 0, len(rows))
	for _, r := range rows {
		out = append(out, Rule{GlobalExclusion: r, Affected: counts[r.ID]})
	}
	return out, nil
}

func (s *Service) enqueueSweep(ctx context.Context, tx pgx.Tx, id uuid.UUID, removed bool) error {
	if s.queue == nil {
		return nil
	}
	if _, err := s.queue.InsertTx(ctx, tx, campaign.ExclusionSweepArgs{ExclusionID: id, Removed: removed}, nil); err != nil {
		return fmt.Errorf("exclusion: enqueue sweep: %w", err)
	}
	return nil
}

func warningFor(params dbgen.CreateGlobalExclusionParams, affected db.ExclusionAffected) *string {
	var msg string
	switch {
	case (params.Kind == KindDomain || params.Kind == KindEmailDomain) && isFreeMail(params.Value):
		msg = fmt.Sprintf("%s is a shared mailbox provider: this excludes every address at it, "+
			"including small businesses that use it for their contact email.", params.Value)
	case affected.Businesses >= broadRuleThreshold:
		msg = fmt.Sprintf("This rule covers %d businesses. Check the samples before saving.", affected.Businesses)
	case params.Kind == KindCompany && params.MatchMode == MatchPrefix && !strings.Contains(params.Value, " "):
		msg = "A one-word prefix can match unrelated businesses that start with the same word."
	default:
		return nil
	}
	return &msg
}

func isFreeMail(host string) bool {
	_, ok := freeMailDomains[host]
	return ok
}

func kindLabel(kind string) string {
	return strings.ReplaceAll(kind, "_", " ")
}

func wrapInternal(err error) error {
	if err == nil {
		return nil
	}
	return apperr.Internal(err)
}
