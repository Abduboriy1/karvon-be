package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The brand scan groups stored businesses and lists the groups with many
// locations. Three groupings are offered, because a chain shows itself in
// different ways:
//
//	name         the normalised name (name_key): "Planet Fitness" in 150 cities.
//	name_prefix  the first two words of the normalised name: "Crunch Fitness -
//	             Amarillo" and "Crunch Fitness Lubbock" are one group.
//	domain       the registrable website domain: every HOTWORX location has its
//	             own name, but they all link to hotworx.net.
//
// Each group's key is computed with the same functions the exclusion views use,
// so a company rule on a name group's key, or a domain rule on a domain group's
// key, covers exactly the group the scan reported.
const (
	BrandGroupName       = "name"
	BrandGroupNamePrefix = "name_prefix"
	BrandGroupDomain     = "domain"
)

// BrandGroupings lists the accepted groupings.
var BrandGroupings = []string{BrandGroupDomain, BrandGroupName, BrandGroupNamePrefix}

// twoLabelSuffix matches the country-code second-level suffixes ("co.uk",
// "com.au") under which the registrable domain has three labels, not two.
const twoLabelSuffix = `'^(co|com|net|org|gov|edu|ac|ltd|plc)\.[a-z]{2}$'`

// registrableDomainExpr reduces a business's domain_suffixes to its registrable
// domain: the last entry, or the one before it under a two-label suffix.
const registrableDomainExpr = `CASE
	WHEN cardinality(b.domain_suffixes) = 0 THEN NULL
	WHEN cardinality(b.domain_suffixes) >= 2
	 AND b.domain_suffixes[cardinality(b.domain_suffixes)] ~ ` + twoLabelSuffix + `
	THEN b.domain_suffixes[cardinality(b.domain_suffixes) - 1]
	ELSE b.domain_suffixes[cardinality(b.domain_suffixes)]
END`

// minBrandKeyLen mirrors the shortest company key a rule may hold, so every name
// group the scan returns can be excluded.
const minBrandKeyLen = 3

// BrandScanFilter narrows a brand scan.
type BrandScanFilter struct {
	GroupBy      string
	MinLocations int
	MinCities    int
	MinStates    int
	Q            *string
	Categories   []string
	States       []string
	// IncludeExcluded keeps groups whose every location is already excluded.
	IncludeExcluded bool
	// IncludeDismissed keeps groups an operator has dismissed.
	IncludeDismissed bool
	// SkipDomains are registrable domains that are never a brand: website
	// builders, social networks and booking platforms shared by unrelated
	// businesses. Only the domain grouping uses them.
	SkipDomains []string
}

// BrandCandidate is one group of businesses the scan found.
type BrandCandidate struct {
	Key               string
	DisplayName       string
	TopDomain         *string
	Locations         int64
	Cities            int64
	States            int64
	StateList         []string
	DistinctNames     int64
	DistinctDomains   int64
	TotalReviews      int64
	AvgRating         *float64
	ExcludedLocations int64
	Existing          *ExclusionRef
	DismissalID       *uuid.UUID
	DismissalNote     *string
	SampleIDs         []uuid.UUID
	Samples           []BrandSample
}

// BrandSample is one location of a candidate brand.
type BrandSample struct {
	ID     uuid.UUID
	Name   string
	City   *string
	State  *string
	Domain *string
}

var brandScanSorts = map[string]sortSpec{
	"locations:desc": {expr: "g.locations", desc: true},
	"cities:desc":    {expr: "g.cities", desc: true},
	"states:desc":    {expr: "g.states", desc: true},
	"reviews:desc":   {expr: "g.total_reviews", desc: true},
	"names:desc":     {expr: "g.distinct_names", desc: true},
	"key:asc":        {expr: "g.key"},
}

// BrandScanSortKeys lists the accepted values of the brand scan `sort` parameter.
func BrandScanSortKeys() []string { return sortKeys(brandScanSorts) }

// brandGroupKey is the SQL expression a grouping groups by, and the exclusion
// kind a rule on that key would take.
func brandGroupKey(groupBy string) (expr, kind string) {
	switch groupBy {
	case BrandGroupName:
		return "b.name_key", "company"
	case BrandGroupNamePrefix:
		return "CASE WHEN cardinality(b.name_prefixes) >= 2 THEN b.name_prefixes[2] END", "company"
	default:
		return registrableDomainExpr, "domain"
	}
}

// BrandScan returns one page of candidate brands and the number of groups that
// match the filter.
func (s *Store) BrandScan(ctx context.Context, f BrandScanFilter, sort string, limit, offset int) ([]BrandCandidate, int64, error) {
	a := &argSet{}
	keyExpr, kind := brandGroupKey(f.GroupBy)

	baseConds := []string{"k.key IS NOT NULL", "length(k.key) >= " + a.add(minBrandKeyLen)}
	if f.GroupBy == BrandGroupDomain {
		baseConds = append(baseConds, "k.key !~ "+twoLabelSuffix)
		if len(f.SkipDomains) > 0 {
			baseConds = append(baseConds, "k.key <> ALL("+a.add(f.SkipDomains)+"::text[])")
		}
	}
	if len(f.Categories) > 0 {
		baseConds = append(baseConds, "b.category = ANY("+a.add(f.Categories)+"::text[])")
	}
	if len(f.States) > 0 {
		baseConds = append(baseConds, "upper(b.state) = ANY("+a.add(upperAll(f.States))+"::text[])")
	}

	having := []string{
		"count(*) >= " + a.add(f.MinLocations),
		"count(DISTINCT lower(base.city) || '|' || upper(coalesce(base.state, ''))) >= " + a.add(f.MinCities),
		"count(DISTINCT upper(nullif(base.state, ''))) >= " + a.add(f.MinStates),
	}
	if !f.IncludeExcluded {
		having = append(having, "count(*) FILTER (WHERE NOT base.excluded) > 0")
	}

	outer := []string{"TRUE"}
	if !f.IncludeDismissed {
		outer = append(outer, "d.id IS NULL")
	}
	if f.Q != nil && strings.TrimSpace(*f.Q) != "" {
		p := a.add("%" + strings.TrimSpace(*f.Q) + "%")
		outer = append(outer, "(g.key ILIKE "+p+" OR g.display_name ILIKE "+p+")")
	}

	spec := lookupSort(brandScanSorts, sort, "locations:desc")
	groupByArg := a.add(f.GroupBy)
	kindArg := a.add(kind)

	// Excluded businesses are computed once from the view and hash-joined, rather
	// than probing the view per row.
	query := `
		WITH excluded AS (
		    SELECT DISTINCT business_id FROM global_excluded_businesses
		),
		base AS (
		    SELECT b.id, b.name, b.city, b.state, b.reviews, b.rating, k.key,
		           ` + registrableDomainExpr + ` AS reg_domain,
		           ex.business_id IS NOT NULL AS excluded
		    FROM businesses b
		    CROSS JOIN LATERAL (SELECT ` + keyExpr + ` AS key) k
		    LEFT JOIN excluded ex ON ex.business_id = b.id
		    WHERE ` + strings.Join(baseConds, " AND ") + `
		),
		g AS (
		    SELECT base.key,
		           mode() WITHIN GROUP (ORDER BY base.name) AS display_name,
		           mode() WITHIN GROUP (ORDER BY base.reg_domain) AS top_domain,
		           count(*) AS locations,
		           count(DISTINCT lower(base.city) || '|' || upper(coalesce(base.state, ''))) AS cities,
		           count(DISTINCT upper(nullif(base.state, ''))) AS states,
		           coalesce(array_agg(DISTINCT upper(base.state)) FILTER (WHERE base.state <> ''), '{}') AS state_list,
		           count(DISTINCT lower(base.name)) AS distinct_names,
		           count(DISTINCT base.reg_domain) AS distinct_domains,
		           coalesce(sum(base.reviews), 0)::bigint AS total_reviews,
		           avg(base.rating) AS avg_rating,
		           count(*) FILTER (WHERE base.excluded) AS excluded_locations,
		           (array_agg(base.id ORDER BY base.reviews DESC NULLS LAST, base.name, base.id))[1:5] AS sample_ids
		    FROM base
		    GROUP BY base.key
		    HAVING ` + strings.Join(having, " AND ") + `
		)
		SELECT g.key, g.display_name, g.top_domain, g.locations, g.cities, g.states, g.state_list,
		       g.distinct_names, g.distinct_domains, g.total_reviews, g.avg_rating, g.excluded_locations,
		       g.sample_ids, ge.id, ge.kind, ge.value, ge.display_value, d.id, d.note,
		       count(*) OVER () AS total
		FROM g
		LEFT JOIN brand_scan_dismissals d ON d.group_by = ` + groupByArg + ` AND d.key = g.key
		LEFT JOIN global_exclusions ge ON ge.removed_at IS NULL AND ge.kind = ` + kindArg + ` AND ge.value = g.key
		WHERE ` + strings.Join(outer, " AND ") +
		spec.orderBy("g.key") + " LIMIT " + a.add(limit) + " OFFSET " + a.add(offset)

	rows, err := s.pool.Query(ctx, query, a.values()...)
	if err != nil {
		return nil, 0, fmt.Errorf("db: brand scan: %w", err)
	}
	defer rows.Close()
	var total int64
	out := make([]BrandCandidate, 0, limit)
	for rows.Next() {
		var c BrandCandidate
		var geID uuid.NullUUID
		var geKind, geValue, geDisplay *string
		var dID uuid.NullUUID
		if err := rows.Scan(&c.Key, &c.DisplayName, &c.TopDomain, &c.Locations, &c.Cities, &c.States, &c.StateList,
			&c.DistinctNames, &c.DistinctDomains, &c.TotalReviews, &c.AvgRating, &c.ExcludedLocations,
			&c.SampleIDs, &geID, &geKind, &geValue, &geDisplay, &dID, &c.DismissalNote, &total); err != nil {
			return nil, 0, fmt.Errorf("db: scan brand candidate: %w", err)
		}
		if geID.Valid {
			c.Existing = &ExclusionRef{ID: geID.UUID, Kind: *geKind, Value: *geValue, DisplayValue: *geDisplay}
		}
		if dID.Valid {
			c.DismissalID = &dID.UUID
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("db: brand scan rows: %w", err)
	}
	if err := s.attachBrandSamples(ctx, out); err != nil {
		return nil, 0, err
	}
	// The window total rides on the page's rows; a page past the end has none, so
	// it is counted again without the page.
	if len(out) == 0 && offset > 0 {
		total, err = s.countBrandScan(ctx, query, a)
		if err != nil {
			return nil, 0, err
		}
	}
	return out, total, nil
}

// countBrandScan counts the groups of a brand scan query that returned no rows on
// its page. The last two arguments are the page's LIMIT and OFFSET.
func (s *Store) countBrandScan(ctx context.Context, query string, a *argSet) (int64, error) {
	args := a.values()
	page := args[:len(args)-2]
	cut := strings.LastIndex(query, " ORDER BY ")
	var total int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM ("+query[:cut]+") q", page...).Scan(&total); err != nil {
		return 0, fmt.Errorf("db: count brand scan: %w", err)
	}
	return total, nil
}

// attachBrandSamples loads the sample locations of every candidate in one query.
func (s *Store) attachBrandSamples(ctx context.Context, candidates []BrandCandidate) error {
	var ids []uuid.UUID
	for _, c := range candidates {
		ids = append(ids, c.SampleIDs...)
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, name, city, state, domain FROM businesses WHERE id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("db: brand samples: %w", err)
	}
	samples, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (BrandSample, error) {
		var b BrandSample
		return b, r.Scan(&b.ID, &b.Name, &b.City, &b.State, &b.Domain)
	})
	if err != nil {
		return fmt.Errorf("db: scan brand samples: %w", err)
	}
	byID := make(map[uuid.UUID]BrandSample, len(samples))
	for _, b := range samples {
		byID[b.ID] = b
	}
	for i := range candidates {
		candidates[i].Samples = make([]BrandSample, 0, len(candidates[i].SampleIDs))
		for _, id := range candidates[i].SampleIDs {
			if b, ok := byID[id]; ok {
				candidates[i].Samples = append(candidates[i].Samples, b)
			}
		}
	}
	return nil
}

func upperAll(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = strings.ToUpper(strings.TrimSpace(v))
	}
	return out
}
