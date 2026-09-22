// Package scraper orchestrates scrape jobs: validation, cost estimation, enqueueing
// and cancellation. The River workers that execute a job live in scraper/jobs.
package scraper

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/apperr"
)

// Status values mirror the jobs.status check constraint.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Limits are the hard bounds on a job config. They are enforced server-side
// regardless of what the client sends.
const (
	MaxTerms       = 20
	MaxLocations   = 300
	MaxPerQueryCap = 1000
	MaxConcurrency = 32
	MaxNameLen     = 200
	MaxTermLen     = 120
	MaxCityLen     = 120
	MaxStateLen    = 60
)

// Defaults applied when a client omits an optional config field.
const (
	DefaultMaxPerQuery = 100
	DefaultConcurrency = 8
)

// MaxPerQueryUnlimited asks the provider for every place in the area rather than a
// fixed number. It is how a whole state is covered, and it is also the setting with no
// upper bound on spend, so a per-run charge cap belongs with it.
const MaxPerQueryUnlimited = -1

// EstimateUnlimitedPerQuery is the per-term figure an unlimited config is priced at.
// Nothing knows the real count before the run, so the estimate uses the ceiling a
// bounded query would have hit and says so.
const EstimateUnlimitedPerQuery = MaxPerQueryCap

// Run states track a vendor-side run through its life. They are the claim that makes
// a retry safe: a row leaves "none" exactly once, so a worker that dies mid-flight
// resumes the run it already paid for instead of starting another.
const (
	RunStateNone      = "none"
	RunStateStarting  = "starting"
	RunStatePolling   = "polling"
	RunStateIngesting = "ingesting"
	RunStateFinished  = "finished"
)

// Location is one place to search in. Both fields are optional: a city with no
// state is resolved by the provider, a state with no city covers the whole state,
// and neither means "everywhere" — Normalize expands that to one entry per state.
type Location struct {
	City  string `json:"city"`
	State string `json:"state"`
}

// USStates are the state codes an empty location expands to.
var USStates = []string{
	"AL", "AK", "AZ", "AR", "CA", "CO", "CT", "DE", "FL", "GA",
	"HI", "ID", "IL", "IN", "IA", "KS", "KY", "LA", "ME", "MD",
	"MA", "MI", "MN", "MS", "MO", "MT", "NE", "NV", "NH", "NJ",
	"NM", "NY", "NC", "ND", "OH", "OK", "OR", "PA", "RI", "SC",
	"SD", "TN", "TX", "UT", "VT", "VA", "WA", "WV", "WI", "WY", "DC",
}

// Config is the immutable configuration of a job, stored as JSONB.
type Config struct {
	Terms       []string   `json:"terms"`
	Locations   []Location `json:"locations"`
	MaxPerQuery int        `json:"max_per_query"`
	CrawlEmails bool       `json:"crawl_emails"`
	Concurrency int        `json:"concurrency"`
	// RecrawlOf is set on a re-crawl job: the job whose businesses were copied so
	// their websites could be crawled again without another provider search.
	RecrawlOf *uuid.UUID `json:"recrawl_of,omitempty"`
}

// IsRecrawl reports whether the job re-crawls another job's businesses instead of
// running provider searches.
func (c Config) IsRecrawl() bool { return c.RecrawlOf != nil }

// Stats is the live progress snapshot stored as JSONB on the job.
type Stats struct {
	QueriesTotal  int `json:"queries_total"`
	QueriesDone   int `json:"queries_done"`
	QueriesFailed int `json:"queries_failed"`
	ListingsFound int `json:"listings_found"`
	SitesTotal    int `json:"sites_total"`
	SitesCrawled  int `json:"sites_crawled"`
	EmailsFound   int `json:"emails_found"`
	// Duplicates is listings that landed on a business the job already had. Areas
	// that do not overlap should produce almost none, so a number that climbs is the
	// first sign that two runs are covering the same ground and being billed twice.
	Duplicates int   `json:"duplicates"`
	CostCents  int64 `json:"cost_cents"`
}

// Stats JSONB keys used by the atomic bump query.
const (
	StatKeyQueriesDone   = "queries_done"
	StatKeyQueriesFailed = "queries_failed"
	StatKeyListingsFound = "listings_found"
	StatKeySitesCrawled  = "sites_crawled"
	StatKeyEmailsFound   = "emails_found"
	StatKeyCostCents     = "cost_cents"
)

// DecodeConfig parses a stored config document.
func DecodeConfig(raw []byte) (Config, error) {
	var c Config
	if len(raw) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("scraper: decode config: %w", err)
	}
	return c, nil
}

// DecodeStats parses a stored stats document, tolerating an empty object.
func DecodeStats(raw []byte) (Stats, error) {
	var s Stats
	if len(raw) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("scraper: decode stats: %w", err)
	}
	return s, nil
}

// Queries expands the config into the units of provider work: one per location,
// carrying every search term.
//
// The grouping is a billing decision, not a tidiness one. Providers charge per place
// returned and deduplicate only inside a single run, so a gym that answers to both
// "gym" and "fitness center" is paid for once when the two terms share a run and twice
// when they do not. Locations stay apart because their areas must not overlap.
func (c Config) Queries() []PlannedQuery {
	if len(c.Terms) == 0 {
		return nil
	}
	out := make([]PlannedQuery, 0, len(c.Locations))
	for _, loc := range c.Locations {
		out = append(out, PlannedQuery{Terms: c.Terms, City: loc.City, State: loc.State})
	}
	return out
}

// PlannedQuery is one expanded query row: every term, in one location.
type PlannedQuery struct {
	Terms []string
	City  string
	State string
}

// Label renders the terms as the single human-readable string the query row stores.
func (q PlannedQuery) Label() string { return strings.Join(q.Terms, ", ") }

// QueryCount is len(Queries()) without allocating.
func (c Config) QueryCount() int {
	if len(c.Terms) == 0 {
		return 0
	}
	return len(c.Locations)
}

// EstimatedListings is the upper bound on places a job can return: the per-term cap
// applies to each term of each query.
func (c Config) EstimatedListings() int {
	perQuery := c.MaxPerQuery
	if perQuery <= 0 {
		perQuery = EstimateUnlimitedPerQuery
	}
	return c.QueryCount() * len(c.Terms) * perQuery
}

// IsUnlimited reports whether the config asks for every place in each area.
func (c Config) IsUnlimited() bool { return c.MaxPerQuery == MaxPerQueryUnlimited }

// Normalize trims input, drops blanks and duplicates, and applies defaults. It runs
// before validation so the stored config is always canonical.
func (c Config) Normalize() Config {
	out := Config{
		MaxPerQuery: c.MaxPerQuery,
		CrawlEmails: c.CrawlEmails,
		Concurrency: c.Concurrency,
		RecrawlOf:   c.RecrawlOf,
	}

	seenTerm := make(map[string]struct{}, len(c.Terms))
	for _, term := range c.Terms {
		term = strings.Join(strings.Fields(term), " ")
		if term == "" {
			continue
		}
		key := strings.ToLower(term)
		if _, dup := seenTerm[key]; dup {
			continue
		}
		seenTerm[key] = struct{}{}
		out.Terms = append(out.Terms, term)
	}

	seenLoc := make(map[string]struct{}, len(c.Locations))
	addLoc := func(city, state string) {
		key := strings.ToLower(city + "|" + state)
		if _, dup := seenLoc[key]; dup {
			return
		}
		seenLoc[key] = struct{}{}
		out.Locations = append(out.Locations, Location{City: city, State: state})
	}

	// No locations at all — or an entry with neither field — means "search
	// everywhere", which expands to one query per state.
	allStates := len(c.Locations) == 0
	for _, loc := range c.Locations {
		city := strings.Join(strings.Fields(loc.City), " ")
		state := NormalizeState(loc.State)
		if city == "" && state == "" {
			allStates = true
			continue
		}
		addLoc(city, state)
	}
	if allStates {
		for _, state := range USStates {
			addLoc("", state)
		}
	}

	// Only an explicit -1 means unlimited; anything else non-positive is an omitted
	// field and takes the default.
	if out.MaxPerQuery != MaxPerQueryUnlimited && out.MaxPerQuery <= 0 {
		out.MaxPerQuery = DefaultMaxPerQuery
	}
	if out.Concurrency <= 0 {
		out.Concurrency = DefaultConcurrency
	}
	return out
}

// NormalizeState trims a state and upper-cases a two-letter code, leaving longer
// names (the providers accept those too) as written.
func NormalizeState(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) == 2 {
		return strings.ToUpper(s)
	}
	return s
}

// Validate checks a normalized config against the documented limits.
func (c Config) Validate(maxQueries int) error {
	var fields []apperr.FieldError

	switch {
	case len(c.Terms) == 0:
		fields = append(fields, apperr.FieldError{Field: "config.terms", Message: "at least one search term is required"})
	case len(c.Terms) > MaxTerms:
		fields = append(fields, apperr.FieldError{
			Field:   "config.terms",
			Message: fmt.Sprintf("at most %d search terms are allowed", MaxTerms),
		})
	}
	for i, term := range c.Terms {
		if len(term) > MaxTermLen {
			fields = append(fields, apperr.FieldError{
				Field:   fmt.Sprintf("config.terms[%d]", i),
				Message: fmt.Sprintf("must be at most %d characters", MaxTermLen),
			})
		}
	}

	switch {
	case len(c.Locations) == 0:
		fields = append(fields, apperr.FieldError{Field: "config.locations", Message: "at least one location is required"})
	case len(c.Locations) > MaxLocations:
		fields = append(fields, apperr.FieldError{
			Field:   "config.locations",
			Message: fmt.Sprintf("at most %d locations are allowed", MaxLocations),
		})
	}
	for i, loc := range c.Locations {
		if len(loc.City) > MaxCityLen {
			fields = append(fields, apperr.FieldError{
				Field:   fmt.Sprintf("config.locations[%d].city", i),
				Message: fmt.Sprintf("must be at most %d characters", MaxCityLen),
			})
		}
		if len(loc.State) > MaxStateLen {
			fields = append(fields, apperr.FieldError{
				Field:   fmt.Sprintf("config.locations[%d].state", i),
				Message: fmt.Sprintf("must be at most %d characters", MaxStateLen),
			})
		}
	}

	if !c.IsUnlimited() && (c.MaxPerQuery < 1 || c.MaxPerQuery > MaxPerQueryCap) {
		fields = append(fields, apperr.FieldError{
			Field: "config.max_per_query",
			Message: fmt.Sprintf("must be between 1 and %d, or %d for every place in the area",
				MaxPerQueryCap, MaxPerQueryUnlimited),
		})
	}
	if c.Concurrency < 1 || c.Concurrency > MaxConcurrency {
		fields = append(fields, apperr.FieldError{
			Field:   "config.concurrency",
			Message: fmt.Sprintf("must be between 1 and %d", MaxConcurrency),
		})
	}

	if total := c.QueryCount(); total > maxQueries {
		fields = append(fields, apperr.FieldError{
			Field: "config",
			Message: fmt.Sprintf("%d locations produces %d queries, the maximum is %d",
				len(c.Locations), total, maxQueries),
		})
	}

	if len(fields) > 0 {
		return apperr.Validation("job configuration is invalid", fields...)
	}
	return nil
}

// EstimateCostCents prices an upper bound on listings at the per-1k rate.
func EstimateCostCents(listings, costPer1kCents int) int64 {
	return int64(listings) * int64(costPer1kCents) / 1000
}

// ActualCostCents prices the listings a query really returned.
func ActualCostCents(listings, costPer1kCents int) int64 {
	return int64(listings) * int64(costPer1kCents) / 1000
}
