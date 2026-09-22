package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bory/karvon-be/internal/scraper/provider"
)

// AsyncProvider is a deterministic stand-in for a vendor whose runs outlive a request.
//
// It exists to exercise the part of the pipeline that protects money: a run is started
// once, recorded, polled, and drained page by page, and a second start for the same
// unit of work would be a second bill. StartCount is what a test asserts on to prove
// that never happened.
type AsyncProvider struct {
	mu sync.Mutex

	// PerQuery is how many listings a run produces.
	PerQuery int
	// PageSize is how many of them one fetch returns, so paging is real in tests.
	PageSize int
	// PollsBeforeDone is how many polls a run stays running for.
	PollsBeforeDone int
	// FailWith, when set, is the terminal status a run ends in instead of succeeding.
	FailWith string
	// StartErr, when set, is returned by StartRun.
	StartErr error

	starts     int
	resurrects int
	aborts     int
	runs       map[string]*fakeRun
	queries    []provider.SearchQuery
}

type fakeRun struct {
	query    provider.SearchQuery
	listings []provider.Listing
	polls    int
	started  time.Time
}

// NewAsync builds an asynchronous fake that produces perQuery listings per run.
func NewAsync(perQuery int) *AsyncProvider {
	return &AsyncProvider{
		PerQuery:        perQuery,
		PageSize:        perQuery,
		PollsBeforeDone: 1,
		runs:            make(map[string]*fakeRun),
	}
}

// Name implements provider.Provider.
func (p *AsyncProvider) Name() string { return "fake-async" }

// Search implements provider.Provider for the key-probe path.
func (p *AsyncProvider) Search(_ context.Context, q provider.SearchQuery) ([]provider.Listing, error) {
	return Synthesize(q, p.PerQuery), nil
}

// StartRun implements provider.AsyncProvider.
func (p *AsyncProvider) StartRun(_ context.Context, q provider.SearchQuery) (provider.RunHandle, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.StartErr != nil {
		return provider.RunHandle{}, p.StartErr
	}

	p.starts++
	p.queries = append(p.queries, q)
	id := fmt.Sprintf("fake-run-%d", p.starts)
	p.runs[id] = &fakeRun{
		query:    q,
		listings: Synthesize(q, p.PerQuery),
		started:  time.Now().UTC(),
	}
	return provider.RunHandle{RunID: id, DatasetID: "ds-" + id, StartedAt: p.runs[id].started}, nil
}

// RunState implements provider.AsyncProvider.
func (p *AsyncProvider) RunState(_ context.Context, runID string) (provider.RunState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	run, ok := p.runs[runID]
	if !ok {
		return provider.RunState{}, fmt.Errorf("fake: unknown run %q", runID)
	}
	run.polls++
	if run.polls <= p.PollsBeforeDone {
		return provider.RunState{Status: "RUNNING", DatasetID: "ds-" + runID}, nil
	}
	if p.FailWith != "" {
		return provider.RunState{Status: p.FailWith, Terminal: true, DatasetID: "ds-" + runID}, nil
	}
	return provider.RunState{
		Status:    "SUCCEEDED",
		Terminal:  true,
		OK:        true,
		DatasetID: "ds-" + runID,
		CostUSD:   float64(len(run.listings)) * 0.0015,
	}, nil
}

// FetchPage implements provider.AsyncProvider.
func (p *AsyncProvider) FetchPage(
	_ context.Context,
	datasetID string,
	offset, limit int,
) (provider.Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	run, ok := p.runs[trimDatasetPrefix(datasetID)]
	if !ok {
		return provider.Page{}, fmt.Errorf("fake: unknown dataset %q", datasetID)
	}
	if p.PageSize > 0 && p.PageSize < limit {
		limit = p.PageSize
	}
	if offset >= len(run.listings) {
		return provider.Page{}, nil
	}
	end := offset + limit
	if end > len(run.listings) {
		end = len(run.listings)
	}
	window := run.listings[offset:end]
	return provider.Page{Listings: window, Items: len(window)}, nil
}

// Resurrect implements provider.AsyncProvider: the run resumes and then succeeds.
func (p *AsyncProvider) Resurrect(_ context.Context, runID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	run, ok := p.runs[runID]
	if !ok {
		return fmt.Errorf("fake: unknown run %q", runID)
	}
	p.resurrects++
	p.FailWith = ""
	run.polls = p.PollsBeforeDone
	return nil
}

// AbortRun implements provider.AsyncProvider.
func (p *AsyncProvider) AbortRun(_ context.Context, runID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.aborts++
	if run, ok := p.runs[runID]; ok {
		run.polls = p.PollsBeforeDone + 1
	}
	return nil
}

// FindOrphanRun implements provider.AsyncProvider.
func (p *AsyncProvider) FindOrphanRun(
	_ context.Context,
	since time.Time,
	known map[string]struct{},
) (provider.RunHandle, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	from, until := since.Add(-time.Minute), since.Add(2*time.Minute)
	var found provider.RunHandle
	matches := 0
	for id, run := range p.runs {
		if _, seen := known[id]; seen {
			continue
		}
		if run.started.Before(from) || run.started.After(until) {
			continue
		}
		matches++
		found = provider.RunHandle{RunID: id, DatasetID: "ds-" + id, StartedAt: run.started}
	}
	return found, matches, nil
}

// Starts is how many runs this fake was asked to start. One per unit of work is the
// whole point; two means something paid twice.
func (p *AsyncProvider) Starts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts
}

// Resurrects is how many runs were resumed rather than restarted.
func (p *AsyncProvider) Resurrects() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resurrects
}

// Queries returns the search queries every started run was given.
func (p *AsyncProvider) Queries() []provider.SearchQuery {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.SearchQuery(nil), p.queries...)
}

func trimDatasetPrefix(datasetID string) string {
	const prefix = "ds-"
	if len(datasetID) > len(prefix) && datasetID[:len(prefix)] == prefix {
		return datasetID[len(prefix):]
	}
	return datasetID
}
