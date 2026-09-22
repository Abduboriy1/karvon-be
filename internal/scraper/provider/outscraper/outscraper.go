// Package outscraper implements provider.Provider on top of the Outscraper
// Google Maps search API.
package outscraper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bory/karvon-be/internal/scraper/provider"
)

// Provider searches Google Maps through Outscraper.
type Provider struct {
	opts provider.Options
}

// New builds an Outscraper provider.
func New(opts provider.Options) *Provider {
	if opts.BaseURL == "" {
		opts.BaseURL = "https://api.outscraper.cloud"
	}
	return &Provider{opts: opts}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "outscraper" }

// Search implements provider.Provider.
func (p *Provider) Search(ctx context.Context, q provider.SearchQuery) ([]provider.Listing, error) {
	if p.opts.APIKey == "" {
		return nil, fmt.Errorf("%w: no api key configured", provider.ErrAuth)
	}

	params := url.Values{}
	params.Set("query", q.String())
	params.Set("limit", strconv.Itoa(q.Max))
	params.Set("async", "false")
	params.Set("language", "en")

	endpoint := strings.TrimSuffix(p.opts.BaseURL, "/") + "/maps/search-v3?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("outscraper: build request: %w", err)
	}
	req.Header.Set("X-API-KEY", p.opts.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := p.opts.Client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("outscraper: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("outscraper: read response: %w", err)
	}
	if err := provider.ClassifyStatus(resp.StatusCode, payload); err != nil {
		return nil, err
	}
	return ParseSearchResponse(payload)
}

type searchResponse struct {
	ID     string              `json:"id"`
	Status string              `json:"status"`
	Data   [][]json.RawMessage `json:"data"`
}

type place struct {
	PlaceID    string  `json:"place_id"`
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	Category   string  `json:"category"`
	FullAddr   string  `json:"full_address"`
	City       string  `json:"city"`
	State      string  `json:"state"`
	PostalCode string  `json:"postal_code"`
	Phone      string  `json:"phone"`
	Site       string  `json:"site"`
	Rating     float64 `json:"rating"`
	Reviews    int32   `json:"reviews"`
	Latitude   float64 `json:"latitude"`
	Longitude  float64 `json:"longitude"`
	Email1     string  `json:"email_1"`
	Email2     string  `json:"email_2"`
	Email3     string  `json:"email_3"`
}

// ParseSearchResponse converts a raw Outscraper payload into listings. Exported so
// recorded fixtures can be replayed in tests.
func ParseSearchResponse(payload []byte) ([]provider.Listing, error) {
	var resp searchResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("outscraper: decode response: %w", err)
	}
	if resp.Status != "" && !strings.EqualFold(resp.Status, "success") &&
		!strings.EqualFold(resp.Status, "finished") {
		return nil, fmt.Errorf("outscraper: run status %q", resp.Status)
	}

	var listings []provider.Listing
	for _, group := range resp.Data {
		for _, raw := range group {
			var pl place
			if err := json.Unmarshal(raw, &pl); err != nil {
				continue
			}
			if pl.Name == "" {
				continue
			}
			category := pl.Type
			if category == "" {
				category = pl.Category
			}
			var emails []string
			for _, e := range []string{pl.Email1, pl.Email2, pl.Email3} {
				if e != "" {
					emails = append(emails, e)
				}
			}
			listings = append(listings, provider.Listing{
				PlaceID:  pl.PlaceID,
				Name:     pl.Name,
				Category: category,
				Address:  pl.FullAddr,
				City:     pl.City,
				State:    pl.State,
				Zip:      pl.PostalCode,
				Phone:    pl.Phone,
				Website:  pl.Site,
				Rating:   provider.PtrFloat(pl.Rating),
				Reviews:  provider.PtrInt32(pl.Reviews),
				Lat:      provider.PtrFloat(pl.Latitude),
				Lng:      provider.PtrFloat(pl.Longitude),
				Emails:   emails,
				RunID:    resp.ID,
				Raw:      raw,
			})
		}
	}
	return listings, nil
}
