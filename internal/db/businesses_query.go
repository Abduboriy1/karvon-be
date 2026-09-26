package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BusinessFilter is the shared filter set for listing and exporting businesses.
type BusinessFilter struct {
	JobID      *uuid.UUID
	Category   *string
	State      *string
	City       *string
	Q          *string
	HasEmail   *bool
	Suppressed *bool
	// Excluded keeps only globally excluded businesses (true), hides them (false),
	// or ignores exclusion (nil).
	Excluded    *bool
	EmailSource *string
	// VerificationTags filters on the tag of the business's primary address.
	VerificationTags []string
	// IDs, when non-empty, restricts the result to exactly these businesses.
	IDs []uuid.UUID
}

// BusinessRow is a business enriched with its best email, as the API returns it.
type BusinessRow struct {
	ID                 uuid.UUID
	PlaceID            *string
	Name               string
	Category           *string
	Address            *string
	City               *string
	State              *string
	Zip                *string
	Phone              *string
	Website            *string
	Domain             *string
	Rating             *float64
	Reviews            *int32
	Lat                *float64
	Lng                *float64
	FirstJobID         *uuid.UUID
	FirstJobName       *string
	Suppressed         bool
	Notes              *string
	LastCrawledAt      *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	PrimaryEmail       *string
	PrimaryEmailSource *string
	PrimaryEmailTag    *string
	PrimaryEmailScore  *int32
	EmailsCount        int64
	// Exclusion is the global exclusion rule covering the business, or nil. Only
	// the list query fills it.
	Exclusion *ExclusionRef
	// AllEmails is only populated by the export query.
	AllEmails *string
}

var businessSorts = map[string]sortSpec{
	"created_at:desc": {expr: "b.created_at", desc: true},
	"created_at:asc":  {expr: "b.created_at"},
	"name:asc":        {expr: "b.name"},
	"name:desc":       {expr: "b.name", desc: true},
	"city:asc":        {expr: "b.city"},
	"city:desc":       {expr: "b.city", desc: true},
	"state:asc":       {expr: "b.state"},
	"state:desc":      {expr: "b.state", desc: true},
	"rating:asc":      {expr: "b.rating"},
	"rating:desc":     {expr: "b.rating", desc: true},
	"reviews:asc":     {expr: "b.reviews"},
	"reviews:desc":    {expr: "b.reviews", desc: true},
	// Sorting by verification reads the primary address's score, which the
	// LATERAL join below already resolved.
	"verification_score:asc":  {expr: "pe.verification_score"},
	"verification_score:desc": {expr: "pe.verification_score", desc: true},
}

const businessSelectColumns = `
    b.id, b.place_id, b.name, b.category, b.address, b.city, b.state, b.zip, b.phone,
    b.website, b.domain, b.rating, b.reviews, b.lat, b.lng, b.first_job_id, b.suppressed,
    b.notes, b.last_crawled_at, b.created_at, b.updated_at, fj.name AS first_job_name,
    pe.email AS primary_email, pe.source AS primary_email_source,
    pe.verification_tag AS primary_email_verified_status,
    pe.verification_score AS primary_email_verification_score,
    COALESCE(ec.cnt, 0) AS emails_count`

const businessEmailJoins = `
    LEFT JOIN jobs fj ON fj.id = b.first_job_id
    LEFT JOIN LATERAL (
        SELECT be.email::text AS email, be.source,
               ev.verification_tag, ev.final_score AS verification_score
        FROM business_emails be
                 LEFT JOIN email_verifications ev ON ev.email = be.email
        WHERE be.business_id = b.id
        ORDER BY be.is_primary DESC, be.found_at, be.email
        LIMIT 1
    ) pe ON true
    LEFT JOIN LATERAL (
        SELECT count(*) AS cnt FROM business_emails be WHERE be.business_id = b.id
    ) ec ON true`

// exportEmailJoins is businessEmailJoins for a CSV that may end up in an outreach
// tool: a globally excluded address is never the primary email, nor counted.
var exportEmailJoins = `
    LEFT JOIN jobs fj ON fj.id = b.first_job_id
    LEFT JOIN LATERAL (
        SELECT be.email::text AS email, be.source,
               ev.verification_tag, ev.final_score AS verification_score
        FROM business_emails be
                 LEFT JOIN email_verifications ev ON ev.email = be.email
        WHERE be.business_id = b.id AND ` + excludedEmailCond("be.email", false) + `
        ORDER BY be.is_primary DESC, be.found_at, be.email
        LIMIT 1
    ) pe ON true
    LEFT JOIN LATERAL (
        SELECT count(*) AS cnt FROM business_emails be
        WHERE be.business_id = b.id AND ` + excludedEmailCond("be.email", false) + `
    ) ec ON true`

// buildBusinessWhere renders the FROM and WHERE fragments for a filter.
func buildBusinessWhere(f BusinessFilter, a *argSet) (from string, where string) {
	var joins strings.Builder
	joins.WriteString(" FROM businesses b")

	conds := []string{"TRUE"}

	if f.JobID != nil {
		joins.WriteString(" JOIN job_results jr ON jr.business_id = b.id AND jr.job_id = " + a.add(*f.JobID))
	}
	if len(f.IDs) > 0 {
		conds = append(conds, "b.id = ANY("+a.add(f.IDs)+")")
	}
	if f.Category != nil && *f.Category != "" {
		conds = append(conds, "b.category = "+a.add(*f.Category))
	}
	if f.State != nil && *f.State != "" {
		conds = append(conds, "b.state = "+a.add(*f.State))
	}
	if f.City != nil && *f.City != "" {
		conds = append(conds, "b.city = "+a.add(*f.City))
	}
	if f.Q != nil && strings.TrimSpace(*f.Q) != "" {
		// gin_trgm_ops indexes on name and domain make these ILIKE scans index-backed.
		pattern := "%" + strings.TrimSpace(*f.Q) + "%"
		p := a.add(pattern)
		conds = append(conds, "(b.name ILIKE "+p+" OR b.domain ILIKE "+p+")")
	}
	if f.HasEmail != nil {
		if *f.HasEmail {
			conds = append(conds, "EXISTS (SELECT 1 FROM business_emails be WHERE be.business_id = b.id)")
		} else {
			conds = append(conds, "NOT EXISTS (SELECT 1 FROM business_emails be WHERE be.business_id = b.id)")
		}
	}
	if f.EmailSource != nil && *f.EmailSource != "" {
		conds = append(conds, "EXISTS (SELECT 1 FROM business_emails be WHERE be.business_id = b.id AND be.source = "+a.add(*f.EmailSource)+")")
	}
	if len(f.VerificationTags) > 0 {
		conds = append(conds, `(SELECT ev.verification_tag
                                FROM business_emails be
                                         LEFT JOIN email_verifications ev ON ev.email = be.email
                                WHERE be.business_id = b.id
                                ORDER BY be.is_primary DESC, be.found_at, be.email
                                LIMIT 1) = ANY(`+a.add(f.VerificationTags)+")")
	}
	suppressed := false
	if f.Suppressed != nil {
		suppressed = *f.Suppressed
	}
	conds = append(conds, "b.suppressed = "+a.add(suppressed))
	if f.Excluded != nil {
		conds = append(conds, excludedBusinessCond("b.id", *f.Excluded))
	}

	return joins.String(), " WHERE " + strings.Join(conds, " AND ")
}

// CountBusinesses returns the total number of rows matching a filter.
func (s *Store) CountBusinesses(ctx context.Context, f BusinessFilter) (int64, error) {
	a := &argSet{}
	from, where := buildBusinessWhere(f, a)
	var total int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*)"+from+where, a.values()...).Scan(&total); err != nil {
		return 0, fmt.Errorf("db: count businesses: %w", err)
	}
	return total, nil
}

// ListBusinessIDs returns the ids of every business matching the filter, at most
// limit of them.
func (s *Store) ListBusinessIDs(ctx context.Context, f BusinessFilter, limit int) ([]uuid.UUID, error) {
	a := &argSet{}
	from, where := buildBusinessWhere(f, a)
	rows, err := s.pool.Query(ctx, "SELECT b.id"+from+where+" ORDER BY b.id LIMIT "+a.add(limit), a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: list business ids: %w", err)
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: scan business id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListBusinesses returns one page of businesses ordered by a whitelisted sort key.
func (s *Store) ListBusinesses(ctx context.Context, f BusinessFilter, sort string, limit, offset int) ([]BusinessRow, error) {
	a := &argSet{}
	from, where := buildBusinessWhere(f, a)
	spec := lookupSort(businessSorts, sort, "created_at:desc")

	q := "SELECT" + businessSelectColumns + from + businessEmailJoins + where +
		spec.orderBy("b.id") + " LIMIT " + a.add(limit) + " OFFSET " + a.add(offset)

	rows, err := s.pool.Query(ctx, q, a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: list businesses: %w", err)
	}
	defer rows.Close()

	out := make([]BusinessRow, 0, limit)
	for rows.Next() {
		row, err := scanBusinessRow(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	ids := make([]uuid.UUID, len(out))
	for i := range out {
		ids[i] = out[i].ID
	}
	excluded, err := s.ExcludedBusinesses(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if ref, ok := excluded[out[i].ID]; ok {
			out[i].Exclusion = &ref
		}
	}
	return out, nil
}

// StreamBusinessesForExport walks every matching row in id order and hands each one to
// fn. Rows are never buffered, so a CSV of any size streams in constant memory.
//
// An export is a contact list, so it never carries a globally excluded business or
// address, whatever the filter says.
func (s *Store) StreamBusinessesForExport(ctx context.Context, f BusinessFilter, fn func(BusinessRow) error) error {
	notExcluded := false
	f.Excluded = &notExcluded
	a := &argSet{}
	from, where := buildBusinessWhere(f, a)

	q := "SELECT" + businessSelectColumns + `,
    (SELECT string_agg(be.email::text, ';' ORDER BY be.is_primary DESC, be.email)
     FROM business_emails be WHERE be.business_id = b.id AND ` + excludedEmailCond("be.email", false) + `) AS all_emails` +
		from + exportEmailJoins + where + " ORDER BY b.created_at DESC, b.id DESC"

	rows, err := s.pool.Query(ctx, q, a.values()...)
	if err != nil {
		return fmt.Errorf("db: export businesses: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		row, err := scanBusinessRow(rows, true)
		if err != nil {
			return err
		}
		if err := fn(row); err != nil {
			return err
		}
	}
	return rows.Err()
}

func scanBusinessRow(rows pgx.Rows, withAllEmails bool) (BusinessRow, error) {
	var r BusinessRow
	dest := []any{
		&r.ID, &r.PlaceID, &r.Name, &r.Category, &r.Address, &r.City, &r.State, &r.Zip,
		&r.Phone, &r.Website, &r.Domain, &r.Rating, &r.Reviews, &r.Lat, &r.Lng,
		&r.FirstJobID, &r.Suppressed, &r.Notes, &r.LastCrawledAt, &r.CreatedAt, &r.UpdatedAt,
		&r.FirstJobName, &r.PrimaryEmail, &r.PrimaryEmailSource, &r.PrimaryEmailTag,
		&r.PrimaryEmailScore, &r.EmailsCount,
	}
	if withAllEmails {
		dest = append(dest, &r.AllEmails)
	}
	if err := rows.Scan(dest...); err != nil {
		return BusinessRow{}, fmt.Errorf("db: scan business: %w", err)
	}
	return r, nil
}

// BusinessSortKeys lists the accepted values of the /businesses `sort` parameter.
func BusinessSortKeys() []string { return sortKeys(businessSorts) }
