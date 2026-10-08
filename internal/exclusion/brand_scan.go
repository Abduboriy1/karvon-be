package exclusion

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// The brand scan finds the chains and franchises in the scraped data, so they can
// be excluded in a few clicks instead of being found one listing at a time. It
// suggests, it never excludes: every group carries the rule that would cover it,
// and the operator previews and saves it through the ordinary endpoints (or
// CreateBulk).

// Brand scan limits.
const (
	// DefaultBrandMinLocations is how many listings a group needs by default.
	DefaultBrandMinLocations = 3
	// MaxBrandMinThreshold caps the min_* parameters.
	MaxBrandMinThreshold = 100000
	// MaxBulkRules is how many rules one bulk request may create.
	MaxBulkRules = 100
	// maxDismissalKeyLen mirrors brand_scan_dismissals.key.
	maxDismissalKeyLen = 320
)

// platformDomains are registrable domains shared by unrelated businesses: website
// builders, link pages, social networks and booking or ordering platforms. A
// hundred gyms with a facebook.com page are a hundred small gyms, not a brand, so
// the domain grouping skips them. A brand that lives on one of these is still
// found by the name groupings.
var platformDomains = []string{
	// Social networks and link pages.
	"facebook.com", "fb.com", "instagram.com", "tiktok.com", "twitter.com", "x.com", "youtube.com",
	"linkedin.com", "pinterest.com", "nextdoor.com", "yelp.com", "linktr.ee", "linktree.com",
	"beacons.ai", "linkin.bio", "bio.link", "taplink.cc", "msha.ke", "campsite.bio",
	// Website builders and hosting.
	"google.com", "business.site", "wixsite.com", "wix.com", "squarespace.com", "square.site",
	"squareup.com", "godaddysites.com", "godaddy.com", "wsimg.com", "weebly.com", "wordpress.com",
	"blogspot.com", "carrd.co", "mystrikingly.com", "strikingly.com", "webflow.io", "myshopify.com",
	"jimdosite.com", "jimdo.com", "site123.me", "yolasite.com", "webs.com", "ueniweb.com",
	"github.io", "netlify.app", "vercel.app", "pages.dev", "web.app", "firebaseapp.com",
	"herokuapp.com", "hubspotpagebuilder.com", "durablesites.com", "duda.co", "editorx.io",
	// Booking, ordering and payments.
	"mindbodyonline.com", "mindbody.io", "vagaro.com", "booksy.com", "glossgenius.com",
	"styleseat.com", "schedulicity.com", "setmore.com", "acuityscheduling.com", "square.com",
	"toasttab.com", "clover.com", "doordash.com", "ubereats.com", "grubhub.com", "wellnessliving.com",
	"pushpress.com", "zenplanner.com", "gymdesk.com", "clubready.com", "fresha.com", "calendly.com",
}

// BrandScanInput is a scan request.
type BrandScanInput struct {
	db.BrandScanFilter
	Sort    string
	Page    int
	PerPage int
}

// BrandCandidate is a group the scan found, with the rule that would cover it.
type BrandCandidate struct {
	db.BrandCandidate
	GroupBy   string
	Suggested Input
}

// BrandScanPage is one page of a scan.
type BrandScanPage struct {
	Rows  []BrandCandidate
	Total int64
}

// BrandScan lists groups of businesses that look like one brand: many listings
// under one name, one name prefix or one website domain.
func (s *Service) BrandScan(ctx context.Context, in BrandScanInput) (BrandScanPage, error) {
	f := in.BrandScanFilter
	if f.GroupBy == "" {
		f.GroupBy = db.BrandGroupDomain
	}
	if f.MinLocations == 0 {
		f.MinLocations = DefaultBrandMinLocations
	}
	if f.MinCities == 0 {
		f.MinCities = 1
	}
	var fields []apperr.FieldError
	if !slices.Contains(db.BrandGroupings, f.GroupBy) {
		fields = append(fields, apperr.FieldError{Field: "group_by", Message: "must be one of " + strings.Join(db.BrandGroupings, ", ")})
	}
	for _, p := range []struct {
		field string
		value int
		min   int
	}{{"min_locations", f.MinLocations, 2}, {"min_cities", f.MinCities, 1}, {"min_states", f.MinStates, 0}} {
		if p.value < p.min || p.value > MaxBrandMinThreshold {
			fields = append(fields, apperr.FieldError{Field: p.field, Message: fmt.Sprintf("must be between %d and %d", p.min, MaxBrandMinThreshold)})
		}
	}
	if len(fields) > 0 {
		return BrandScanPage{}, apperr.Validation("invalid query parameters", fields...)
	}
	f.SkipDomains = skipDomains()

	rows, total, err := s.store.BrandScan(ctx, f, in.Sort, in.PerPage, (in.Page-1)*in.PerPage)
	if err != nil {
		return BrandScanPage{}, apperr.Internal(err)
	}
	out := make([]BrandCandidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, BrandCandidate{BrandCandidate: r, GroupBy: f.GroupBy, Suggested: suggestedRule(f.GroupBy, r)})
	}
	return BrandScanPage{Rows: out, Total: total}, nil
}

// suggestedRule is the rule that covers a group: a domain rule on a domain group,
// an exact company rule on a name group, and a prefix company rule on a name
// prefix group, so "crunch fitness" also catches "Crunch Fitness - Amarillo".
func suggestedRule(groupBy string, c db.BrandCandidate) Input {
	reason := fmt.Sprintf("brand scan: %d locations in %d cities", c.Locations, c.Cities)
	switch groupBy {
	case db.BrandGroupName:
		return Input{Kind: KindCompany, Value: c.DisplayName, MatchMode: MatchExact, Reason: &reason, Source: SourceManual}
	case db.BrandGroupNamePrefix:
		return Input{Kind: KindCompany, Value: c.Key, MatchMode: MatchPrefix, Reason: &reason, Source: SourceManual}
	default:
		return Input{Kind: KindDomain, Value: c.Key, MatchMode: MatchExact, Reason: &reason, Source: SourceManual}
	}
}

// skipDomains is every registrable domain the domain grouping ignores.
func skipDomains() []string {
	out := slices.Concat(platformDomains, slices.Collect(maps.Keys(freeMailDomains)))
	slices.Sort(out)
	return slices.Compact(out)
}

// DismissalInput is a group an operator has decided to keep.
type DismissalInput struct {
	GroupBy string
	Key     string
	Note    *string
}

// Dismiss hides a group from later scans. Dismissing a group twice is a 409.
func (s *Service) Dismiss(ctx context.Context, in DismissalInput) (dbgen.BrandScanDismissal, error) {
	var fields []apperr.FieldError
	if !slices.Contains(db.BrandGroupings, in.GroupBy) {
		fields = append(fields, apperr.FieldError{Field: "group_by", Message: "must be one of " + strings.Join(db.BrandGroupings, ", ")})
	}
	key := strings.TrimSpace(in.Key)
	switch {
	case key == "":
		fields = append(fields, apperr.FieldError{Field: "key", Message: "is required"})
	case len(key) > maxDismissalKeyLen:
		fields = append(fields, apperr.FieldError{Field: "key", Message: fmt.Sprintf("must be at most %d characters", maxDismissalKeyLen)})
	}
	var note *string
	if in.Note != nil {
		if n := strings.TrimSpace(*in.Note); n != "" {
			if len(n) > MaxReasonLen {
				fields = append(fields, apperr.FieldError{Field: "note", Message: fmt.Sprintf("must be at most %d characters", MaxReasonLen)})
			}
			note = &n
		}
	}
	if len(fields) > 0 {
		return dbgen.BrandScanDismissal{}, apperr.Validation("dismissal is invalid", fields...)
	}
	row, err := s.store.CreateBrandScanDismissal(ctx, dbgen.CreateBrandScanDismissalParams{
		ID: ids.New(), GroupBy: in.GroupBy, Key: key, Note: note,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return dbgen.BrandScanDismissal{}, apperr.Conflict("%q is already dismissed", key)
		}
		return dbgen.BrandScanDismissal{}, apperr.Internal(err)
	}
	return row, nil
}

// Undismiss puts a dismissed group back into the scan.
func (s *Service) Undismiss(ctx context.Context, id uuid.UUID) error {
	n, err := s.store.DeleteBrandScanDismissal(ctx, id)
	if err != nil {
		return apperr.Internal(err)
	}
	if n == 0 {
		return apperr.NotFound("dismissal")
	}
	return nil
}

// DismissalPage is one page of dismissals.
type DismissalPage struct {
	Rows  []dbgen.BrandScanDismissal
	Total int64
}

// ListDismissals returns one page of dismissals, newest first.
func (s *Service) ListDismissals(ctx context.Context, groupBy *string, page, perPage int) (DismissalPage, error) {
	if groupBy != nil && !slices.Contains(db.BrandGroupings, *groupBy) {
		return DismissalPage{}, apperr.Validation("invalid query parameters",
			apperr.FieldError{Field: "group_by", Message: "must be one of " + strings.Join(db.BrandGroupings, ", ")})
	}
	rows, err := s.store.ListBrandScanDismissals(ctx, dbgen.ListBrandScanDismissalsParams{
		GroupBy: groupBy,
		Lim:     int32(perPage),              //nolint:gosec // G115: bounded by MaxPerPage
		Off:     int32((page - 1) * perPage), //nolint:gosec // G115: bounded by the page parameter
	})
	if err != nil {
		return DismissalPage{}, apperr.Internal(err)
	}
	total, err := s.store.CountBrandScanDismissals(ctx, groupBy)
	if err != nil {
		return DismissalPage{}, apperr.Internal(err)
	}
	return DismissalPage{Rows: rows, Total: total}, nil
}

// BulkResult is the outcome of one rule in a bulk request: the rule, or why it
// was not created.
type BulkResult struct {
	Rule *Rule
	Err  *apperr.Error
}

// CreateBulk creates each rule independently, as Create would, so one duplicate,
// invalid or failed rule does not hold back the rest. A failure is reported on its
// item; the rules before it stay created.
func (s *Service) CreateBulk(ctx context.Context, in []Input) ([]BulkResult, error) {
	if len(in) == 0 || len(in) > MaxBulkRules {
		return nil, apperr.Validation("the request is invalid",
			apperr.FieldError{Field: "items", Message: fmt.Sprintf("must hold between 1 and %d rules", MaxBulkRules)})
	}
	out := make([]BulkResult, len(in))
	for i, item := range in {
		rule, err := s.Create(ctx, item)
		if err != nil {
			out[i] = BulkResult{Err: apperr.From(err)}
			continue
		}
		out[i] = BulkResult{Rule: &rule}
	}
	return out, nil
}
