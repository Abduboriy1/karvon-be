// Package apify implements provider.Provider and provider.AsyncProvider on top of the
// Apify `compass/crawler-google-places` actor.
//
// Two shapes of call live here. Search is the short synchronous one, used to prove a
// stored API key works. Everything a real job does goes through the asynchronous path:
// a state-sized run takes hours, and the synchronous endpoint abandons the HTTP
// request after five minutes while the run itself keeps going and keeps billing.
package apify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bory/karvon-be/internal/scraper/provider"
)

// ActorID is the Apify actor this provider drives.
const ActorID = "compass~crawler-google-places"

// maxBodyBytes caps a single response read. A dataset page of 1000 places is a few
// megabytes; the limit is there so a runaway response cannot exhaust memory.
const maxBodyBytes = 64 << 20

// DefaultPageSize is how many dataset items one fetch asks for.
const DefaultPageSize = 1000

// Apify run statuses.
const (
	statusReady     = "READY"
	statusRunning   = "RUNNING"
	statusSucceeded = "SUCCEEDED"
	statusAborting  = "ABORTING"
	statusTimingOut = "TIMING-OUT"
)

// Settings are the run-level knobs that cost or save money, kept separate from the
// transport options because they describe the actor, not the HTTP client.
type Settings struct {
	// MemoryMB is the memory an actor run is given. More memory means a faster run
	// and a larger share of the account's memory limit, not a larger bill.
	MemoryMB int
	// MaxChargeUSD caps what one run may spend. Zero means no cap, which on an
	// unlimited-results run is the one setting that can produce a surprise invoice.
	MaxChargeUSD float64
	// ScrapeContacts turns on the website-contacts enrichment, which is billed per
	// place on top of the place itself.
	ScrapeContacts bool
	// SkipClosedPlaces drops permanently and temporarily closed businesses.
	SkipClosedPlaces bool
	// CountryCode is the ISO-3166 alpha-2 country the structured location fields are
	// resolved inside.
	CountryCode string
	// Language is the results language.
	Language string
}

func (s Settings) withDefaults() Settings {
	if s.MemoryMB <= 0 {
		s.MemoryMB = 8192
	}
	if s.CountryCode == "" {
		s.CountryCode = "us"
	}
	if s.Language == "" {
		s.Language = "en"
	}
	return s
}

// Provider searches Google Maps through Apify.
type Provider struct {
	opts     provider.Options
	settings Settings
}

// New builds an Apify provider.
func New(opts provider.Options) *Provider { return NewWithSettings(opts, Settings{}) }

// NewWithSettings builds an Apify provider with explicit run settings.
func NewWithSettings(opts provider.Options, settings Settings) *Provider {
	if opts.BaseURL == "" {
		opts.BaseURL = "https://api.apify.com"
	}
	return &Provider{opts: opts, settings: settings.withDefaults()}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "apify" }

// runInput is the actor's input document.
//
// The location is described with the structured fields rather than a free-text query
// whenever the state is one the geocoder knows, because those resolve to an exact
// administrative polygon. Two such polygons never overlap, which is what makes running
// several states at once safe: no place can fall inside two runs and be billed twice.
type runInput struct {
	SearchStringsArray        []string `json:"searchStringsArray"`
	LocationQuery             string   `json:"locationQuery,omitempty"`
	CountryCode               string   `json:"countryCode,omitempty"`
	State                     string   `json:"state,omitempty"`
	City                      string   `json:"city,omitempty"`
	MaxCrawledPlacesPerSearch int      `json:"maxCrawledPlacesPerSearch,omitempty"`
	Language                  string   `json:"language"`
	SkipClosedPlaces          bool     `json:"skipClosedPlaces"`
	ScrapeContacts            bool     `json:"scrapeContacts"`
}

// buildInput renders one query as actor input.
func (p *Provider) buildInput(q provider.SearchQuery) runInput {
	in := runInput{
		SearchStringsArray: q.TermList(),
		Language:           p.settings.Language,
		SkipClosedPlaces:   p.settings.SkipClosedPlaces,
		ScrapeContacts:     p.settings.ScrapeContacts,
	}
	// A negative maximum is the caller asking for everything the area holds, which
	// the actor expresses by the field being absent.
	if q.Max > 0 {
		in.MaxCrawledPlacesPerSearch = q.Max
	}

	state := provider.StateName(q.State)
	switch {
	case state != "":
		in.CountryCode = p.settings.CountryCode
		in.State = state
		in.City = q.City
	case q.City != "":
		in.CountryCode = p.settings.CountryCode
		in.City = q.City
	default:
		in.LocationQuery = q.LocationQuery()
	}
	return in
}

// Search implements provider.Provider: one blocking run, used for key probes and
// small ad-hoc searches. A job-sized search uses the asynchronous path instead.
func (p *Provider) Search(ctx context.Context, q provider.SearchQuery) ([]provider.Listing, error) {
	body, err := p.marshalInput(q)
	if err != nil {
		return nil, err
	}

	endpoint := p.url("/v2/acts/" + url.PathEscape(ActorID) + "/run-sync-get-dataset-items")
	payload, header, err := p.do(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, err
	}
	return ParseDatasetItems(payload, header.Get("X-Apify-Run-Id"))
}

// StartRun implements provider.AsyncProvider.
func (p *Provider) StartRun(ctx context.Context, q provider.SearchQuery) (provider.RunHandle, error) {
	body, err := p.marshalInput(q)
	if err != nil {
		return provider.RunHandle{}, err
	}

	params := url.Values{}
	params.Set("memory", strconv.Itoa(p.settings.MemoryMB))
	// timeout=0 means "no wall-clock limit": a whole-state run is measured in hours,
	// and a run killed by a timeout has already been billed for what it scraped.
	params.Set("timeout", "0")
	if p.settings.MaxChargeUSD > 0 {
		params.Set("maxTotalChargeUsd", strconv.FormatFloat(p.settings.MaxChargeUSD, 'f', -1, 64))
	}

	endpoint := p.url("/v2/acts/"+url.PathEscape(ActorID)+"/runs") + "?" + params.Encode()
	payload, _, err := p.do(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return provider.RunHandle{}, err
	}

	run, err := decodeRun(payload)
	if err != nil {
		return provider.RunHandle{}, err
	}
	if run.ID == "" {
		return provider.RunHandle{}, fmt.Errorf("apify: run started without an id")
	}
	return run.handle(), nil
}

// RunState implements provider.AsyncProvider.
func (p *Provider) RunState(ctx context.Context, runID string) (provider.RunState, error) {
	endpoint := p.url("/v2/actor-runs/" + url.PathEscape(runID))
	payload, _, err := p.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return provider.RunState{}, err
	}
	run, err := decodeRun(payload)
	if err != nil {
		return provider.RunState{}, err
	}
	return run.state(), nil
}

// FetchPage implements provider.AsyncProvider.
func (p *Provider) FetchPage(
	ctx context.Context,
	datasetID string,
	offset, limit int,
) (provider.Page, error) {
	if datasetID == "" {
		return provider.Page{}, fmt.Errorf("apify: no dataset id for this run")
	}
	if limit <= 0 {
		limit = DefaultPageSize
	}

	params := url.Values{}
	params.Set("offset", strconv.Itoa(offset))
	params.Set("limit", strconv.Itoa(limit))
	// Deliberately not "clean": the server-side filter would make the window shorter
	// than the range it was asked for, and the offset is what a resumed drain counts
	// on. Unusable rows are dropped here instead, where they are still counted.
	endpoint := p.url("/v2/datasets/"+url.PathEscape(datasetID)+"/items") + "?" + params.Encode()
	payload, _, err := p.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return provider.Page{}, err
	}
	return ParsePage(payload, "")
}

// ParsePage converts a raw dataset payload into a page, keeping the count of items the
// vendor actually returned.
func ParsePage(payload []byte, runID string) (provider.Page, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(payload, &raws); err != nil {
		return provider.Page{}, fmt.Errorf("apify: decode dataset: %w", err)
	}
	listings, err := ParseDatasetItems(payload, runID)
	if err != nil {
		return provider.Page{}, err
	}
	return provider.Page{Listings: listings, Items: len(raws)}, nil
}

// Resurrect implements provider.AsyncProvider. The run continues into the dataset it
// already has, so places it scraped before it stopped are neither lost nor paid for
// a second time.
func (p *Provider) Resurrect(ctx context.Context, runID string) error {
	endpoint := p.url("/v2/actor-runs/" + url.PathEscape(runID) + "/resurrect")
	_, _, err := p.do(ctx, http.MethodPost, endpoint, nil)
	return err
}

// AbortRun implements provider.AsyncProvider.
func (p *Provider) AbortRun(ctx context.Context, runID string) error {
	endpoint := p.url("/v2/actor-runs/" + url.PathEscape(runID) + "/abort")
	_, _, err := p.do(ctx, http.MethodPost, endpoint, nil)
	return err
}

// FindOrphanRun implements provider.AsyncProvider: it looks for a run of this actor
// that started around `since` and that nothing has recorded.
//
// This is the recovery path for a worker that died in the seconds between Apify
// accepting a run and the run id reaching the database. Starting a fresh run there
// would pay for the same state twice; adopting the orphan pays once. An ambiguous
// answer — two unrecorded runs in the window — returns false rather than guessing,
// because adopting the wrong run would file one state's places under another.
func (p *Provider) FindOrphanRun(
	ctx context.Context,
	since time.Time,
	known map[string]struct{},
) (provider.RunHandle, int, error) {
	params := url.Values{}
	params.Set("desc", "1")
	params.Set("limit", "50")

	endpoint := p.url("/v2/acts/"+url.PathEscape(ActorID)+"/runs") + "?" + params.Encode()
	payload, _, err := p.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return provider.RunHandle{}, 0, err
	}

	var envelope struct {
		Data struct {
			Items []apiRun `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return provider.RunHandle{}, 0, fmt.Errorf("apify: decode run list: %w", err)
	}

	// A minute of slack each way absorbs clock skew between this host and Apify.
	from := since.Add(-time.Minute)
	until := since.Add(2 * time.Minute)

	var found provider.RunHandle
	matches := 0
	for _, run := range envelope.Data.Items {
		if run.ID == "" || run.StartedAt.IsZero() {
			continue
		}
		if run.StartedAt.Before(from) || run.StartedAt.After(until) {
			continue
		}
		if _, seen := known[run.ID]; seen {
			continue
		}
		matches++
		found = run.handle()
	}
	return found, matches, nil
}

// apiRun is the subset of Apify's run object this package reads.
type apiRun struct {
	ID               string    `json:"id"`
	Status           string    `json:"status"`
	DefaultDatasetID string    `json:"defaultDatasetId"`
	StartedAt        time.Time `json:"startedAt"`
	UsageTotalUsd    float64   `json:"usageTotalUsd"`
}

func (r apiRun) handle() provider.RunHandle {
	return provider.RunHandle{RunID: r.ID, DatasetID: r.DefaultDatasetID, StartedAt: r.StartedAt}
}

func (r apiRun) state() provider.RunState {
	status := strings.ToUpper(r.Status)
	live := status == statusReady || status == statusRunning ||
		status == statusAborting || status == statusTimingOut
	return provider.RunState{
		Status:    r.Status,
		Terminal:  !live,
		OK:        status == statusSucceeded,
		DatasetID: r.DefaultDatasetID,
		CostUSD:   r.UsageTotalUsd,
	}
}

func decodeRun(payload []byte) (apiRun, error) {
	var envelope struct {
		Data apiRun `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return apiRun{}, fmt.Errorf("apify: decode run: %w", err)
	}
	return envelope.Data, nil
}

func (p *Provider) marshalInput(q provider.SearchQuery) ([]byte, error) {
	if p.opts.APIKey == "" {
		return nil, fmt.Errorf("%w: no api key configured", provider.ErrAuth)
	}
	body, err := json.Marshal(p.buildInput(q))
	if err != nil {
		return nil, fmt.Errorf("apify: marshal input: %w", err)
	}
	return body, nil
}

func (p *Provider) url(path string) string {
	return strings.TrimSuffix(p.opts.BaseURL, "/") + path
}

// do performs one API call and classifies the result.
func (p *Provider) do(ctx context.Context, method, endpoint string, body []byte) ([]byte, http.Header, error) {
	if p.opts.APIKey == "" {
		return nil, nil, fmt.Errorf("%w: no api key configured", provider.ErrAuth)
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("apify: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The key travels in the Authorization header, never in the URL, so it cannot
	// leak into proxy or server access logs.
	req.Header.Set("Authorization", "Bearer "+p.opts.APIKey)

	resp, err := p.opts.Client().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("apify: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("apify: read response: %w", err)
	}
	if err := provider.ClassifyStatus(resp.StatusCode, payload); err != nil {
		return nil, nil, err
	}
	return payload, resp.Header, nil
}

// place mirrors the subset of the actor's output we store.
type place struct {
	PlaceID      string   `json:"placeId"`
	Title        string   `json:"title"`
	CategoryName string   `json:"categoryName"`
	Address      string   `json:"address"`
	City         string   `json:"city"`
	State        string   `json:"state"`
	PostalCode   string   `json:"postalCode"`
	Phone        string   `json:"phone"`
	Website      string   `json:"website"`
	TotalScore   float64  `json:"totalScore"`
	ReviewsCount int32    `json:"reviewsCount"`
	Emails       []string `json:"emails"`
	Location     struct {
		Lat float64 `json:"lat"`
		Lng float64 `json:"lng"`
	} `json:"location"`
}

// ParseDatasetItems converts a raw Apify dataset payload into listings. It is exported
// so recorded fixtures can be replayed in tests without an HTTP round trip.
func ParseDatasetItems(payload []byte, runID string) ([]provider.Listing, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(payload, &raws); err != nil {
		return nil, fmt.Errorf("apify: decode dataset: %w", err)
	}

	listings := make([]provider.Listing, 0, len(raws))
	for _, raw := range raws {
		var pl place
		if err := json.Unmarshal(raw, &pl); err != nil {
			// One malformed item must not discard a whole run.
			continue
		}
		if pl.Title == "" {
			continue
		}
		listings = append(listings, provider.Listing{
			PlaceID:  pl.PlaceID,
			Name:     pl.Title,
			Category: pl.CategoryName,
			Address:  pl.Address,
			City:     pl.City,
			State:    pl.State,
			Zip:      pl.PostalCode,
			Phone:    pl.Phone,
			Website:  pl.Website,
			Rating:   provider.PtrFloat(pl.TotalScore),
			Reviews:  provider.PtrInt32(pl.ReviewsCount),
			Lat:      provider.PtrFloat(pl.Location.Lat),
			Lng:      provider.PtrFloat(pl.Location.Lng),
			Emails:   pl.Emails,
			RunID:    runID,
			Raw:      raw,
		})
	}
	return listings, nil
}
