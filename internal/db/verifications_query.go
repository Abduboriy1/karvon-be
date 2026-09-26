package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// VerificationFilter is the shared filter behind listing, counting and expanding a
// run. Every field is optional; the zero value lists every address that belongs to
// at least one business that is not suppressed.
type VerificationFilter struct {
	// IDs restricts the result to exactly these verification rows.
	IDs []uuid.UUID
	// BusinessIDs keeps only addresses held by those businesses.
	BusinessIDs []uuid.UUID
	// JobID keeps only addresses of the businesses a scrape job found.
	JobID *uuid.UUID
	// Tags keeps only rows currently carrying one of these tags.
	Tags []string
	// MinScore and MaxScore bound the final score.
	MinScore *int
	MaxScore *int
	// Pass2Status keeps only rows with that third-party verdict.
	Pass2Status *string
	// HasTypo keeps rows that do (or do not) carry a correction suggestion.
	HasTypo *bool
	// Q is a trigram search over the address.
	Q *string
	// IncludeSuppressed keeps addresses whose only businesses are suppressed.
	IncludeSuppressed bool
	// Excluded keeps only globally excluded addresses (true), hides them (false),
	// or ignores exclusion (nil). Every run and estimate sets false.
	Excluded *bool

	// FreeComplete keeps rows that have (or have not) been through the free stage.
	FreeComplete *bool
	// MinFreeScore is the paid floor: below it, an address is not worth paying for.
	MinFreeScore *int
	// MaxFreeScore is the paid ceiling, exclusive: at or above it the free providers
	// are already confident enough that paying adds nothing.
	MaxFreeScore *int
	// Pass1VerifiedBefore keeps rows whose local result is older than this, which
	// is how a self run skips addresses scored moments ago.
	Pass1VerifiedBefore *time.Time
	// ThirdPartySent keeps rows that have (false) or have not (true) already been
	// sent to a third-party verifier. An address is sent at most once, ever, so
	// false is what every paid run selects on and true is what it would skip.
	ThirdPartySent *bool
}

// VerificationRow is one address with its two passes, as the API returns it.
type VerificationRow struct {
	ID              uuid.UUID
	Email           string
	Domain          string
	Pass1Score      int32
	Pass1Checks     []byte
	Pass1HardFail   *string
	Pass1VerifiedAt *time.Time
	FreeScore       int32
	FreeScoredAt    *time.Time
	ProviderResults []byte
	Pass2Score      *int32
	Pass2Status     *string
	Pass2VerifiedAt *time.Time
	// ThirdPartySentAt is when the address was handed to a third-party verifier.
	// Once set it is never cleared, and the address can never be sent again.
	ThirdPartySentAt *time.Time
	Pass2Credits     int32
	FinalScore       int32
	VerificationTag  string
	TypoSuggestion   *string
	LastError        *string
	BusinessCount    int64
	UpdatedAt        time.Time
	// Exclusion is the global exclusion rule covering the address, or nil. Only
	// the list query fills it.
	Exclusion *ExclusionRef
}

var verificationSorts = map[string]sortSpec{
	"final_score:desc":         {expr: "ev.final_score", desc: true},
	"final_score:asc":          {expr: "ev.final_score"},
	"free_score:desc":          {expr: "ev.free_score", desc: true},
	"free_score:asc":           {expr: "ev.free_score"},
	"email:asc":                {expr: "ev.email"},
	"email:desc":               {expr: "ev.email", desc: true},
	"pass1_verified_at:desc":   {expr: "ev.pass1_verified_at", desc: true},
	"pass1_verified_at:asc":    {expr: "ev.pass1_verified_at"},
	"pass2_verified_at:desc":   {expr: "ev.pass2_verified_at", desc: true},
	"pass2_verified_at:asc":    {expr: "ev.pass2_verified_at"},
	"third_party_sent_at:desc": {expr: "ev.third_party_sent_at", desc: true},
	"third_party_sent_at:asc":  {expr: "ev.third_party_sent_at"},
	"updated_at:desc":          {expr: "ev.updated_at", desc: true},
	"updated_at:asc":           {expr: "ev.updated_at"},
}

// VerificationSortKeys lists the accepted values of the `sort` parameter.
func VerificationSortKeys() []string { return sortKeys(verificationSorts) }

const verificationSelectColumns = `
    ev.id, ev.email::text, ev.domain, ev.pass1_score, ev.pass1_checks, ev.pass1_hard_fail,
    ev.pass1_verified_at, ev.free_score, ev.free_scored_at, ev.provider_results,
    ev.pass2_score, ev.pass2_status, ev.pass2_verified_at, ev.third_party_sent_at,
    ev.pass2_credits, ev.final_score, ev.verification_tag, ev.typo_suggestion,
    ev.last_error, ev.updated_at,
    (SELECT count(*) FROM business_emails be WHERE be.email = ev.email)::bigint AS business_count`

// buildVerificationWhere renders the WHERE clause for a filter.
func buildVerificationWhere(f VerificationFilter, a *argSet) string {
	conds := []string{"TRUE"}

	if len(f.IDs) > 0 {
		conds = append(conds, "ev.id = ANY("+a.add(f.IDs)+")")
	}
	if len(f.Tags) > 0 {
		conds = append(conds, "ev.verification_tag = ANY("+a.add(f.Tags)+")")
	}
	if f.MinScore != nil {
		conds = append(conds, "ev.final_score >= "+a.add(*f.MinScore))
	}
	if f.MaxScore != nil {
		conds = append(conds, "ev.final_score <= "+a.add(*f.MaxScore))
	}
	if f.Pass2Status != nil && *f.Pass2Status != "" {
		conds = append(conds, "ev.pass2_status = "+a.add(*f.Pass2Status))
	}
	if f.HasTypo != nil {
		if *f.HasTypo {
			conds = append(conds, "ev.typo_suggestion IS NOT NULL")
		} else {
			conds = append(conds, "ev.typo_suggestion IS NULL")
		}
	}
	if f.Q != nil && strings.TrimSpace(*f.Q) != "" {
		// The gin_trgm_ops index on email::text keeps this off a sequential scan.
		conds = append(conds, "ev.email::text ILIKE "+a.add("%"+strings.TrimSpace(*f.Q)+"%"))
	}
	if f.FreeComplete != nil {
		if *f.FreeComplete {
			conds = append(conds, "ev.free_scored_at IS NOT NULL")
		} else {
			conds = append(conds, "ev.free_scored_at IS NULL")
		}
	}
	if f.MinFreeScore != nil {
		conds = append(conds, "ev.free_score >= "+a.add(*f.MinFreeScore))
	}
	if f.MaxFreeScore != nil {
		conds = append(conds, "ev.free_score < "+a.add(*f.MaxFreeScore))
	}
	if f.Pass1VerifiedBefore != nil {
		conds = append(conds,
			"(ev.pass1_verified_at IS NULL OR ev.pass1_verified_at < "+a.add(*f.Pass1VerifiedBefore)+")")
	}
	if f.ThirdPartySent != nil {
		// The one-send rule. It is a plain NULL test rather than a time window:
		// having been sent is permanent and nothing expires it.
		if *f.ThirdPartySent {
			conds = append(conds, "ev.third_party_sent_at IS NOT NULL")
		} else {
			conds = append(conds, "ev.third_party_sent_at IS NULL")
		}
	}

	if f.Excluded != nil {
		conds = append(conds, excludedEmailCond("ev.email", *f.Excluded))
	}

	if scope := buildAddressScope(f, a, "ev.email"); scope != "" {
		conds = append(conds, scope)
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// buildAddressScope limits a query to addresses reachable from the business list.
// It is also what hides addresses whose only businesses are suppressed.
func buildAddressScope(f VerificationFilter, a *argSet, emailExpr string) string {
	var inner []string
	joins := ""

	if f.JobID != nil {
		joins = " JOIN job_results jr ON jr.business_id = b.id AND jr.job_id = " + a.add(*f.JobID)
	}
	if len(f.BusinessIDs) > 0 {
		inner = append(inner, "b.id = ANY("+a.add(f.BusinessIDs)+")")
	}
	if !f.IncludeSuppressed {
		inner = append(inner, "NOT b.suppressed")
	}
	if joins == "" && len(inner) == 0 {
		return ""
	}

	where := ""
	if len(inner) > 0 {
		where = " AND " + strings.Join(inner, " AND ")
	}
	return "EXISTS (SELECT 1 FROM business_emails be JOIN businesses b ON b.id = be.business_id" +
		joins + " WHERE be.email = " + emailExpr + where + ")"
}

// CountVerifications returns how many addresses match a filter.
func (s *Store) CountVerifications(ctx context.Context, f VerificationFilter) (int64, error) {
	a := &argSet{}
	where := buildVerificationWhere(f, a)
	var total int64
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM email_verifications ev"+where, a.values()...).Scan(&total); err != nil {
		return 0, fmt.Errorf("db: count verifications: %w", err)
	}
	return total, nil
}

// ListVerifications returns one page ordered by a whitelisted sort key.
func (s *Store) ListVerifications(ctx context.Context, f VerificationFilter, sort string, limit, offset int) ([]VerificationRow, error) {
	a := &argSet{}
	where := buildVerificationWhere(f, a)
	spec := lookupSort(verificationSorts, sort, "final_score:desc")

	query := "SELECT" + verificationSelectColumns + " FROM email_verifications ev" + where +
		spec.orderBy("ev.id") + " LIMIT " + a.add(limit) + " OFFSET " + a.add(offset)

	rows, err := s.pool.Query(ctx, query, a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: list verifications: %w", err)
	}
	defer rows.Close()

	out := make([]VerificationRow, 0, limit)
	for rows.Next() {
		var r VerificationRow
		if err := rows.Scan(&r.ID, &r.Email, &r.Domain, &r.Pass1Score, &r.Pass1Checks,
			&r.Pass1HardFail, &r.Pass1VerifiedAt, &r.FreeScore, &r.FreeScoredAt,
			&r.ProviderResults, &r.Pass2Score, &r.Pass2Status,
			&r.Pass2VerifiedAt, &r.ThirdPartySentAt, &r.Pass2Credits, &r.FinalScore, &r.VerificationTag,
			&r.TypoSuggestion, &r.LastError, &r.UpdatedAt, &r.BusinessCount); err != nil {
			return nil, fmt.Errorf("db: scan verification: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	err = annotateExclusions(ctx, s, out, func(r *VerificationRow) string { return r.Email },
		func(r *VerificationRow, ref *ExclusionRef) { r.Exclusion = ref })
	return out, err
}

// SelectVerificationIDs resolves a filter into the rows a run will process. The
// limit bounds how large a single run can get.
func (s *Store) SelectVerificationIDs(ctx context.Context, f VerificationFilter, limit int) ([]uuid.UUID, error) {
	a := &argSet{}
	where := buildVerificationWhere(f, a)

	query := "SELECT ev.id FROM email_verifications ev" + where +
		" ORDER BY ev.id LIMIT " + a.add(limit)

	rows, err := s.pool.Query(ctx, query, a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: select verification ids: %w", err)
	}
	defer rows.Close()

	out := make([]uuid.UUID, 0, 256)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: scan verification id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListAddressesWithoutVerification finds addresses on the master list that have no
// verification row yet, so a run can create them before it fans out.
func (s *Store) ListAddressesWithoutVerification(ctx context.Context, f VerificationFilter, limit int) ([]string, error) {
	a := &argSet{}
	scope := buildAddressScope(f, a, "be2.email")

	conds := []string{"NOT EXISTS (SELECT 1 FROM email_verifications ev WHERE ev.email = be2.email)"}
	if scope != "" {
		conds = append(conds, scope)
	}
	if f.Excluded != nil {
		conds = append(conds, excludedEmailCond("be2.email", *f.Excluded))
	}

	query := "SELECT DISTINCT be2.email::text FROM business_emails be2 WHERE " +
		strings.Join(conds, " AND ") + " ORDER BY 1 LIMIT " + a.add(limit)

	rows, err := s.pool.Query(ctx, query, a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: list unverified addresses: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0, 256)
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, fmt.Errorf("db: scan address: %w", err)
		}
		out = append(out, email)
	}
	return out, rows.Err()
}

// CountAddressesWithoutVerification is ListAddressesWithoutVerification as a count,
// used by the estimate a paid run is confirmed against.
func (s *Store) CountAddressesWithoutVerification(ctx context.Context, f VerificationFilter) (int64, error) {
	a := &argSet{}
	scope := buildAddressScope(f, a, "be2.email")

	conds := []string{"NOT EXISTS (SELECT 1 FROM email_verifications ev WHERE ev.email = be2.email)"}
	if scope != "" {
		conds = append(conds, scope)
	}
	if f.Excluded != nil {
		conds = append(conds, excludedEmailCond("be2.email", *f.Excluded))
	}

	query := "SELECT count(DISTINCT be2.email) FROM business_emails be2 WHERE " +
		strings.Join(conds, " AND ")

	var total int64
	if err := s.pool.QueryRow(ctx, query, a.values()...).Scan(&total); err != nil {
		return 0, fmt.Errorf("db: count unverified addresses: %w", err)
	}
	return total, nil
}

// InsertVerifications creates the rows for a batch of addresses. The ids are
// generated by the caller so every row keeps a time-ordered UUIDv7, and the domain
// is derived from the address itself.
func (s *Store) InsertVerifications(ctx context.Context, ids []uuid.UUID, emails []string) error {
	if len(ids) == 0 || len(ids) != len(emails) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO email_verifications (id, email, domain)
		SELECT seeded.id, seeded.email::citext, split_part(seeded.email, '@', 2)
		FROM unnest($1::uuid[], $2::text[]) AS seeded (id, email)
		ON CONFLICT (email) DO NOTHING`, ids, emails)
	if err != nil {
		return fmt.Errorf("db: insert verifications: %w", err)
	}
	return nil
}
