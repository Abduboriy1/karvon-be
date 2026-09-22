// Package provider abstracts the Google Maps data vendors behind one interface so a
// new vendor is a new package, not a change to the scrape pipeline.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Kind identifies a provider implementation and matches sources.kind in the database.
type Kind string

// The provider implementations this service ships with.
const (
	KindApify      Kind = "apify"
	KindOutscraper Kind = "outscraper"
)

// SearchQuery is one unit of provider work: a location, and the terms to search for
// inside it.
//
// Term and Terms describe the same thing at two scales. A vendor that bills per call
// and takes one phrase at a time reads Term; a vendor that takes a list and bills per
// place returned reads Terms, because sending every term in one run is what stops the
// same place being paid for once per term.
type SearchQuery struct {
	Term  string
	Terms []string
	City  string
	State string
	Max   int
}

// TermList is every term to search for, falling back to the single Term.
func (q SearchQuery) TermList() []string {
	if len(q.Terms) > 0 {
		return q.Terms
	}
	if q.Term == "" {
		return nil
	}
	return []string{q.Term}
}

// LocationQuery renders the location the way both vendors expect it. Either half
// may be empty: a bare state searches the whole state, a bare city is resolved by
// the vendor.
func (q SearchQuery) LocationQuery() string {
	switch {
	case q.City == "":
		return q.State
	case q.State == "":
		return q.City
	default:
		return q.City + ", " + q.State
	}
}

// String renders the full "term in city, state" search phrase.
func (q SearchQuery) String() string {
	return fmt.Sprintf("%s in %s", q.Term, q.LocationQuery())
}

// Listing is one normalized Google Maps place.
type Listing struct {
	PlaceID  string
	Name     string
	Category string
	Address  string
	City     string
	State    string
	Zip      string
	Phone    string
	Website  string
	Rating   *float64
	Reviews  *int32
	Lat      *float64
	Lng      *float64
	// Emails are addresses the provider itself returned, stored with source "provider".
	Emails []string
	// RunID identifies the vendor-side run that produced this listing, when known.
	RunID string
	// Raw is the untouched vendor payload, kept in businesses.raw for debugging.
	Raw json.RawMessage
}

// Provider searches a vendor for places.
type Provider interface {
	// Search returns listings for one query. Implementations must honour ctx.
	Search(ctx context.Context, q SearchQuery) ([]Listing, error)
	// Name is the human-readable provider name used in logs.
	Name() string
}

// RunHandle identifies a vendor-side run and the dataset it writes into.
type RunHandle struct {
	RunID     string
	DatasetID string
	// StartedAt is the vendor's own start time when it reports one, which is what
	// an orphaned run is matched against.
	StartedAt time.Time
}

// Page is one window of a run's dataset.
//
// Items and Listings differ, and the difference matters: Items counts what the vendor
// returned, Listings what survived parsing. Paging advances by Items, or a dataset
// holding a single unusable row would be read forever.
type Page struct {
	Listings []Listing
	Items    int
}

// RunState is a vendor run's progress.
type RunState struct {
	// Status is the vendor's own status string, stored verbatim for operators.
	Status string
	// Terminal means the run has stopped, successfully or not.
	Terminal bool
	// OK means it stopped having finished its work.
	OK bool
	// DatasetID is repeated here because a run started by someone else (an adopted
	// orphan) is the only place we learn it.
	DatasetID string
	// CostUSD is what the vendor says the run has cost so far, when it says.
	CostUSD float64
}

// AsyncProvider is a vendor whose searches outlive a single HTTP request: the run is
// started, polled and then drained page by page.
//
// The split exists for billing, not for speed. A run that is started and then lost —
// because a five-minute request timed out, or a worker died — is still charged for
// every place it scrapes, so the run id has to be written down the moment it exists
// and every later step has to resume from it rather than start again.
type AsyncProvider interface {
	Provider

	// StartRun begins a run and returns its identifiers. It must never be called
	// twice for the same unit of work.
	StartRun(ctx context.Context, q SearchQuery) (RunHandle, error)
	// RunState reports a run's progress.
	RunState(ctx context.Context, runID string) (RunState, error)
	// FetchPage returns one window of a finished or partial dataset.
	FetchPage(ctx context.Context, datasetID string, offset, limit int) (Page, error)
	// Resurrect restarts a run that stopped early, continuing into the same dataset.
	Resurrect(ctx context.Context, runID string) error
	// AbortRun stops a run that has outlived its budget, so it stops spending.
	AbortRun(ctx context.Context, runID string) error
	// FindOrphanRun looks for runs this account started around `since` that nothing
	// has recorded, so a worker that died between starting a run and saving its id
	// adopts that run instead of paying for a second one. known holds the run ids
	// already recorded. The count is how many candidates matched: zero means no run
	// was started, and more than one is ambiguous and must not be guessed at.
	FindOrphanRun(ctx context.Context, since time.Time, known map[string]struct{}) (RunHandle, int, error)
}

// ErrAuth signals that the vendor rejected the API key. The pipeline fails the whole
// job immediately on this error rather than retrying.
var ErrAuth = errors.New("provider: authentication failed")

// ErrRateLimited signals a vendor-side throttle; the pipeline retries with backoff.
var ErrRateLimited = errors.New("provider: rate limited")

// Options configure a provider client.
type Options struct {
	APIKey     string
	BaseURL    string
	Timeout    time.Duration
	HTTPClient *http.Client
}

// Client returns the HTTP client to use, defaulting to one bounded by Timeout.
func (o Options) Client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &http.Client{Timeout: timeout}
}

// ClassifyStatus maps an HTTP status to a provider-level error.
func ClassifyStatus(status int, body []byte) error {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return fmt.Errorf("%w (status %d)", ErrAuth, status)
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("%w (status %d)", ErrRateLimited, status)
	case status >= 400:
		return fmt.Errorf("provider: unexpected status %d: %s", status, Truncate(string(body), 300))
	default:
		return nil
	}
}

// Truncate shortens a body excerpt for error messages.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// PtrFloat returns a pointer to v unless v is zero.
func PtrFloat(v float64) *float64 {
	if v == 0 {
		return nil
	}
	return &v
}

// PtrInt32 returns a pointer to v unless v is zero.
func PtrInt32(v int32) *int32 {
	if v == 0 {
		return nil
	}
	return &v
}

// usStateNames maps the two-letter codes a job config stores to the full names a
// geocoder recognises. A vendor that resolves a location itself is given the full
// name, because "TX" alone matches nothing in OpenStreetMap.
var usStateNames = map[string]string{
	"AL": "Alabama", "AK": "Alaska", "AZ": "Arizona", "AR": "Arkansas",
	"CA": "California", "CO": "Colorado", "CT": "Connecticut", "DE": "Delaware",
	"FL": "Florida", "GA": "Georgia", "HI": "Hawaii", "ID": "Idaho",
	"IL": "Illinois", "IN": "Indiana", "IA": "Iowa", "KS": "Kansas",
	"KY": "Kentucky", "LA": "Louisiana", "ME": "Maine", "MD": "Maryland",
	"MA": "Massachusetts", "MI": "Michigan", "MN": "Minnesota", "MS": "Mississippi",
	"MO": "Missouri", "MT": "Montana", "NE": "Nebraska", "NV": "Nevada",
	"NH": "New Hampshire", "NJ": "New Jersey", "NM": "New Mexico", "NY": "New York",
	"NC": "North Carolina", "ND": "North Dakota", "OH": "Ohio", "OK": "Oklahoma",
	"OR": "Oregon", "PA": "Pennsylvania", "RI": "Rhode Island", "SC": "South Carolina",
	"SD": "South Dakota", "TN": "Tennessee", "TX": "Texas", "UT": "Utah",
	"VT": "Vermont", "VA": "Virginia", "WA": "Washington", "WV": "West Virginia",
	"WI": "Wisconsin", "WY": "Wyoming", "DC": "District of Columbia",
}

// StateName expands a two-letter US state code. Anything else is returned unchanged,
// so a full name or a foreign region passes straight through.
func StateName(state string) string {
	if full, ok := usStateNames[strings.ToUpper(strings.TrimSpace(state))]; ok {
		return full
	}
	return state
}
