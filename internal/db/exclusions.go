package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/db/dbgen"
)

// This file is the only Go code that knows how a global exclusion is matched. The
// matching itself lives in two views (migrations/00012_global_exclusions.sql, restated
// by 00020_exclusion_contains.sql):
//
//	global_excluded_businesses (business_id, exclusion_id)
//	global_excluded_addresses  (email, exclusion_id)
//
// Every query builder, sweep and check reads them through the helpers below, so a
// change to the rules changes one place. The views are driven from the rules, so a
// predicate over them is computed once per statement and hash-joined, rather than
// normalising every row.

// ExclusionRef names the rule that excludes a record.
type ExclusionRef struct {
	ID           uuid.UUID
	Kind         string
	Value        string
	DisplayValue string
}

// ExclusionAffected is how many stored records one rule covers.
type ExclusionAffected struct {
	Businesses int64
	Emails     int64
	Contacts   int64
	LiveLeads  int64
}

// excludedBusinessCond renders the predicate "the business with this id is (or is
// not) globally excluded".
func excludedBusinessCond(idExpr string, excluded bool) string {
	cond := "EXISTS (SELECT 1 FROM global_excluded_businesses gxb WHERE gxb.business_id = " + idExpr + ")"
	if excluded {
		return cond
	}
	return "NOT " + cond
}

// excludedEmailCond renders the predicate "this address is (or is not) globally
// excluded". emailExpr must be a citext expression.
func excludedEmailCond(emailExpr string, excluded bool) string {
	cond := "EXISTS (SELECT 1 FROM global_excluded_addresses gxa WHERE gxa.email = " + emailExpr + ")"
	if excluded {
		return cond
	}
	return "NOT " + cond
}

const exclusionRefColumns = "ge.id, ge.kind, ge.value, ge.display_value"

// matchColumns is exclusionRefColumns plus the creation time the single-record
// matches order by, so the oldest rule is always the one reported.
const matchColumns = exclusionRefColumns + ", ge.created_at"

// ExcludedEmails returns the oldest rule behind each excluded address among emails,
// keyed by the lower-cased address. Addresses that are not excluded are absent.
func (s *Store) ExcludedEmails(ctx context.Context, emails []string) (map[string]ExclusionRef, error) {
	out := map[string]ExclusionRef{}
	if len(emails) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (lower(x.email::text)) lower(x.email::text), `+exclusionRefColumns+`
		FROM global_excluded_addresses x
		JOIN global_exclusions ge ON ge.id = x.exclusion_id
		WHERE x.email = ANY($1::citext[])
		ORDER BY lower(x.email::text), ge.created_at, ge.id`, emails)
	if err != nil {
		return nil, fmt.Errorf("db: excluded emails: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var email string
		var ref ExclusionRef
		if err := rows.Scan(&email, &ref.ID, &ref.Kind, &ref.Value, &ref.DisplayValue); err != nil {
			return nil, fmt.Errorf("db: scan excluded email: %w", err)
		}
		out[email] = ref
	}
	return out, rows.Err()
}

// annotateExclusions looks up the rule behind each row's address in one query and
// hands it to set; rows whose address is not excluded are left alone.
func annotateExclusions[T any](ctx context.Context, s *Store, rows []T, email func(*T) string,
	set func(*T, *ExclusionRef),
) error {
	emails := make([]string, len(rows))
	for i := range rows {
		emails[i] = email(&rows[i])
	}
	excluded, err := s.ExcludedEmails(ctx, emails)
	if err != nil {
		return err
	}
	for i := range rows {
		if ref, ok := excluded[strings.ToLower(email(&rows[i]))]; ok {
			set(&rows[i], &ref)
		}
	}
	return nil
}

// ExcludedBusinesses returns the oldest rule behind each excluded business among ids.
func (s *Store) ExcludedBusinesses(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ExclusionRef, error) {
	out := map[uuid.UUID]ExclusionRef{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (x.business_id) x.business_id, `+exclusionRefColumns+`
		FROM global_excluded_businesses x
		JOIN global_exclusions ge ON ge.id = x.exclusion_id
		WHERE x.business_id = ANY($1)
		ORDER BY x.business_id, ge.created_at, ge.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("db: excluded businesses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var ref ExclusionRef
		if err := rows.Scan(&id, &ref.ID, &ref.Kind, &ref.Value, &ref.DisplayValue); err != nil {
			return nil, fmt.Errorf("db: scan excluded business: %w", err)
		}
		out[id] = ref
	}
	return out, rows.Err()
}

// MatchEmail answers "is this address globally excluded?" for any address, stored
// or not: a rule on the address or its domain, or an excluded business or contact
// holding it.
func (s *Store) MatchEmail(ctx context.Context, email string) (*ExclusionRef, error) {
	return s.matchOne(ctx, `
		SELECT `+matchColumns+` FROM global_exclusions ge
		WHERE ge.removed_at IS NULL
		  AND ((ge.kind = 'email' AND ge.value = lower(btrim($1)))
		    OR (ge.kind IN ('email_domain', 'domain') AND ge.match_mode = 'exact'
		        AND ge.value = ANY (exclusion_domain_suffixes(split_part(lower(btrim($1)), '@', 2))))
		    OR (ge.kind IN ('email_domain', 'domain') AND ge.match_mode = 'contains'
		        AND exclusion_host(split_part(lower(btrim($1)), '@', 2)) LIKE '%' || ge.value || '%'))
		UNION ALL
		SELECT `+matchColumns+` FROM global_excluded_addresses x
		JOIN global_exclusions ge ON ge.id = x.exclusion_id
		WHERE x.email = btrim($1)::citext
		ORDER BY 5, 1 LIMIT 1`, email)
}

// MatchBusiness answers "is this stored business globally excluded?".
func (s *Store) MatchBusiness(ctx context.Context, id uuid.UUID) (*ExclusionRef, error) {
	return s.matchOne(ctx, `
		SELECT `+matchColumns+` FROM global_excluded_businesses x
		JOIN global_exclusions ge ON ge.id = x.exclusion_id
		WHERE x.business_id = $1
		ORDER BY ge.created_at, ge.id LIMIT 1`, id)
}

// MatchDomain answers "is this website or mail domain globally excluded?". Both
// domain kinds count: an operator asking about a domain wants to know whether
// anything at it will be contacted.
func (s *Store) MatchDomain(ctx context.Context, host string) (*ExclusionRef, error) {
	return s.matchOne(ctx, `
		SELECT `+matchColumns+` FROM global_exclusions ge
		WHERE ge.removed_at IS NULL AND ge.kind IN ('domain', 'email_domain')
		  AND CASE ge.match_mode
		        WHEN 'contains' THEN exclusion_host($1) LIKE '%' || ge.value || '%'
		        ELSE ge.value = ANY (exclusion_domain_suffixes($1))
		      END
		ORDER BY ge.created_at, ge.id LIMIT 1`, host)
}

// MatchCompany answers "is a business with this name globally excluded?".
func (s *Store) MatchCompany(ctx context.Context, name string) (*ExclusionRef, error) {
	return s.matchOne(ctx, `
		SELECT `+matchColumns+` FROM global_exclusions ge
		WHERE ge.removed_at IS NULL AND ge.kind = 'company'
		  AND CASE ge.match_mode
		        WHEN 'contains' THEN ' ' || exclusion_company_key($1) || ' ' LIKE '% ' || ge.value || ' %'
		        WHEN 'prefix' THEN ge.value = ANY (exclusion_name_prefixes(exclusion_company_key($1)))
		        ELSE ge.value = exclusion_company_key($1)
		      END
		ORDER BY ge.created_at, ge.id LIMIT 1`, name)
}

// matchOne runs a query selecting matchColumns and returns its first row.
func (s *Store) matchOne(ctx context.Context, query string, arg any) (*ExclusionRef, error) {
	var ref ExclusionRef
	var createdAt time.Time
	err := s.pool.QueryRow(ctx, query, arg).Scan(&ref.ID, &ref.Kind, &ref.Value, &ref.DisplayValue, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: match exclusion: %w", err)
	}
	return &ref, nil
}

// ExclusionFilter narrows the exclusion list.
type ExclusionFilter struct {
	Q     *string
	Kinds []string
	// Status is "active" (the default), "removed" or "all".
	Status string
}

var exclusionSorts = map[string]sortSpec{
	"created_at:desc": {expr: "ge.created_at", desc: true},
	"created_at:asc":  {expr: "ge.created_at"},
	"value:asc":       {expr: "ge.value"},
	"value:desc":      {expr: "ge.value", desc: true},
	"kind:asc":        {expr: "ge.kind"},
}

// ExclusionSortKeys lists the accepted values of the /exclusions `sort` parameter.
func ExclusionSortKeys() []string { return sortKeys(exclusionSorts) }

func buildExclusionWhere(f ExclusionFilter, a *argSet) string {
	conds := []string{"TRUE"}
	switch f.Status {
	case "removed":
		conds = append(conds, "ge.removed_at IS NOT NULL")
	case "all":
	default:
		conds = append(conds, "ge.removed_at IS NULL")
	}
	if len(f.Kinds) > 0 {
		conds = append(conds, "ge.kind = ANY("+a.add(f.Kinds)+")")
	}
	if f.Q != nil && strings.TrimSpace(*f.Q) != "" {
		p := a.add("%" + strings.TrimSpace(*f.Q) + "%")
		conds = append(conds, "(ge.display_value ILIKE "+p+" OR ge.value ILIKE "+p+" OR ge.reason ILIKE "+p+")")
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// CountExclusions counts the rules matching a filter.
func (s *Store) CountExclusions(ctx context.Context, f ExclusionFilter) (int64, error) {
	a := &argSet{}
	var total int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM global_exclusions ge"+buildExclusionWhere(f, a),
		a.values()...).Scan(&total); err != nil {
		return 0, fmt.Errorf("db: count exclusions: %w", err)
	}
	return total, nil
}

// ListExclusions returns one page of rules.
func (s *Store) ListExclusions(ctx context.Context, f ExclusionFilter, sort string, limit, offset int) ([]dbgen.GlobalExclusion, error) {
	a := &argSet{}
	where := buildExclusionWhere(f, a)
	spec := lookupSort(exclusionSorts, sort, "created_at:desc")
	rows, err := s.pool.Query(ctx, `
		SELECT ge.id, ge.kind, ge.value, ge.display_value, ge.match_mode, ge.reason, ge.source,
		       ge.source_ref_id, ge.created_at, ge.removed_at, ge.removed_note
		FROM global_exclusions ge`+where+spec.orderBy("ge.id")+
		" LIMIT "+a.add(limit)+" OFFSET "+a.add(offset), a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: list exclusions: %w", err)
	}
	defer rows.Close()
	out := make([]dbgen.GlobalExclusion, 0, limit)
	for rows.Next() {
		var r dbgen.GlobalExclusion
		if err := rows.Scan(&r.ID, &r.Kind, &r.Value, &r.DisplayValue, &r.MatchMode, &r.Reason, &r.Source,
			&r.SourceRefID, &r.CreatedAt, &r.RemovedAt, &r.RemovedNote); err != nil {
			return nil, fmt.Errorf("db: scan exclusion: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ExclusionAffectedCounts counts, per rule, the records it covers right now. A
// record covered by two rules counts for both. Removed rules cover nothing.
func (s *Store) ExclusionAffectedCounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ExclusionAffected, error) {
	return exclusionAffectedCounts(ctx, s.pool, ids)
}

func exclusionAffectedCounts(ctx context.Context, q dbgen.DBTX, ids []uuid.UUID) (map[uuid.UUID]ExclusionAffected, error) {
	out := make(map[uuid.UUID]ExclusionAffected, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		WITH addr AS (
		    SELECT DISTINCT x.exclusion_id, x.email FROM global_excluded_addresses x
		    WHERE x.exclusion_id = ANY($1)
		)
		SELECT id,
		    (SELECT count(DISTINCT xb.business_id) FROM global_excluded_businesses xb WHERE xb.exclusion_id = r.id),
		    (SELECT count(*) FROM addr WHERE addr.exclusion_id = r.id
		        AND (EXISTS (SELECT 1 FROM business_emails be WHERE be.email = addr.email)
		          OR EXISTS (SELECT 1 FROM email_verifications ev WHERE ev.email = addr.email))),
		    (SELECT count(*) FROM addr JOIN contacts c ON c.email = addr.email WHERE addr.exclusion_id = r.id),
		    (SELECT count(*) FROM addr
		         JOIN contacts c ON c.email = addr.email
		         JOIN campaign_leads cl ON cl.contact_id = c.id
		     WHERE addr.exclusion_id = r.id
		       AND cl.status IN ('pending', 'pushing', 'active', 'paused', 'excluded'))
		FROM unnest($1::uuid[]) AS r (id)`, ids)
	if err != nil {
		return nil, fmt.Errorf("db: exclusion counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var c ExclusionAffected
		if err := rows.Scan(&id, &c.Businesses, &c.Emails, &c.Contacts, &c.LiveLeads); err != nil {
			return nil, fmt.Errorf("db: scan exclusion counts: %w", err)
		}
		out[id] = c
	}
	return out, rows.Err()
}

// ExclusionPreview is what a prospective rule would cover.
type ExclusionPreview struct {
	Affected         ExclusionAffected
	SampleBusinesses []PreviewBusiness
	SampleEmails     []string
}

// PreviewBusiness is one business a prospective rule would exclude.
type PreviewBusiness struct {
	ID     uuid.UUID
	Name   string
	Domain *string
}

// PreviewExclusion measures a rule without keeping it: the rule is inserted inside
// a transaction that is always rolled back, so the preview is computed by exactly
// the same views that will enforce it.
func (s *Store) PreviewExclusion(ctx context.Context, params dbgen.CreateGlobalExclusionParams) (ExclusionPreview, error) {
	var out ExclusionPreview
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("db: preview begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// A duplicate of an active rule would violate the unique index; measure it as
	// a fresh rule anyway by retiring the original inside this transaction.
	if _, err := tx.Exec(ctx, `UPDATE global_exclusions SET removed_at = now()
		WHERE kind = $1 AND value = $2 AND removed_at IS NULL`, params.Kind, params.Value); err != nil {
		return out, fmt.Errorf("db: preview retire duplicate: %w", err)
	}
	row, err := dbgen.New(tx).CreateGlobalExclusion(ctx, params)
	if err != nil {
		return out, fmt.Errorf("db: preview insert: %w", err)
	}
	counts, err := exclusionAffectedCounts(ctx, tx, []uuid.UUID{row.ID})
	if err != nil {
		return out, err
	}
	out.Affected = counts[row.ID]

	rows, err := tx.Query(ctx, `
		SELECT b.id, b.name, b.domain FROM global_excluded_businesses xb
		JOIN businesses b ON b.id = xb.business_id
		WHERE xb.exclusion_id = $1 ORDER BY b.name, b.id LIMIT 10`, row.ID)
	if err != nil {
		return out, fmt.Errorf("db: preview businesses: %w", err)
	}
	out.SampleBusinesses, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PreviewBusiness, error) {
		var b PreviewBusiness
		return b, r.Scan(&b.ID, &b.Name, &b.Domain)
	})
	if err != nil {
		return out, fmt.Errorf("db: scan preview businesses: %w", err)
	}

	rows, err = tx.Query(ctx, `
		SELECT DISTINCT x.email::text FROM global_excluded_addresses x
		WHERE x.exclusion_id = $1 ORDER BY 1 LIMIT 10`, row.ID)
	if err != nil {
		return out, fmt.Errorf("db: preview emails: %w", err)
	}
	out.SampleEmails, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return out, fmt.Errorf("db: scan preview emails: %w", err)
	}
	return out, nil
}
