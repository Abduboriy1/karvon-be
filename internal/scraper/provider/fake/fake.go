// Package fake provides a deterministic provider for tests and local development.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/bory/karvon-be/internal/scraper/provider"
)

// Provider returns canned listings. It is safe for concurrent use.
type Provider struct {
	mu sync.Mutex

	// ByQuery maps SearchQuery.String() to the listings to return.
	ByQuery map[string][]provider.Listing
	// Default is returned when ByQuery has no entry for a query.
	Default []provider.Listing
	// Err, when set, is returned for every call.
	Err error
	// Generate, when true and no canned data matches, synthesises listings.
	Generate bool
	// PerQuery is how many listings Generate produces.
	PerQuery int

	calls []provider.SearchQuery
}

// New builds a fake that synthesises `perQuery` listings for any query.
func New(perQuery int) *Provider {
	return &Provider{Generate: true, PerQuery: perQuery}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "fake" }

// Search implements provider.Provider.
func (p *Provider) Search(ctx context.Context, q provider.SearchQuery) ([]provider.Listing, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.calls = append(p.calls, q)
	err := p.Err
	canned, ok := p.ByQuery[q.String()]
	def := p.Default
	generate, perQuery := p.Generate, p.PerQuery
	p.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if ok {
		return canned, nil
	}
	if len(def) > 0 {
		return def, nil
	}
	if !generate {
		return nil, nil
	}

	n := perQuery
	if q.Max > 0 && q.Max < n {
		n = q.Max
	}
	return Synthesize(q, n), nil
}

// Calls returns every query the fake has seen, in order.
func (p *Provider) Calls() []provider.SearchQuery {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.SearchQuery(nil), p.calls...)
}

// Synthesize builds n deterministic listings for a query. Place ids are stable across
// runs, so the same query always dedupes onto the same businesses.
func Synthesize(q provider.SearchQuery, n int) []provider.Listing {
	slug := strings.ToLower(strings.NewReplacer(" ", "-", ",", "").Replace(q.Term + "-" + q.LocationQuery()))
	listings := make([]provider.Listing, 0, n)
	for i := 1; i <= n; i++ {
		rating := 3.5 + float64(i%15)/10
		reviews := int32(10 * i) //nolint:gosec // G115: i is bounded by n
		lat := 30.0 + float64(i)/1000
		lng := -97.0 - float64(i)/1000
		domain := fmt.Sprintf("%s-%d.example.test", slug, i)
		listings = append(listings, provider.Listing{
			PlaceID:  fmt.Sprintf("place-%s-%d", slug, i),
			Name:     fmt.Sprintf("%s %d", q.Term, i),
			Category: q.Term,
			Address:  fmt.Sprintf("%d Main St", 100+i),
			City:     q.City,
			State:    q.State,
			Zip:      fmt.Sprintf("%05d", 70000+i),
			Phone:    fmt.Sprintf("+1512555%04d", i),
			Website:  "https://" + domain,
			Rating:   &rating,
			Reviews:  &reviews,
			Lat:      &lat,
			Lng:      &lng,
			RunID:    "fake-run-" + slug,
			Raw:      json.RawMessage(fmt.Sprintf(`{"placeId":%q,"title":%q}`, "place-"+slug, q.Term)),
		})
	}
	return listings
}
