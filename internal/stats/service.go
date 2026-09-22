// Package stats serves the dashboard counters and the per-job email chart.
package stats

import (
	"context"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db"
)

// ChartJobs is how many recent jobs the dashboard chart shows.
const ChartJobs = 10

// Service implements GET /stats/scraper.
type Service struct {
	store *db.Store
}

// NewService builds the stats service.
func NewService(store *db.Store) *Service { return &Service{store: store} }

// JobEmails is one bar of the dashboard chart.
type JobEmails struct {
	JobID  uuid.UUID
	Name   string
	Emails int64
}

// Scraper is the full dashboard payload.
type Scraper struct {
	Businesses   int64
	WithEmail    int64
	EmailsTotal  int64
	JobsTotal    int64
	LastJob      *db.JobRow
	EmailsPerJob []JobEmails
}

// Scraper gathers every dashboard number in one call.
func (s *Service) Scraper(ctx context.Context) (Scraper, error) {
	counters, err := s.store.ScraperCounters(ctx)
	if err != nil {
		return Scraper{}, apperr.Internal(err)
	}

	chart, err := s.store.EmailsPerJob(ctx, ChartJobs)
	if err != nil {
		return Scraper{}, apperr.Internal(err)
	}
	perJob := make([]JobEmails, 0, len(chart))
	for _, row := range chart {
		perJob = append(perJob, JobEmails{JobID: row.ID, Name: row.Name, Emails: row.Emails})
	}

	out := Scraper{
		Businesses:   counters.Businesses,
		WithEmail:    counters.WithEmail,
		EmailsTotal:  counters.EmailsTotal,
		JobsTotal:    counters.JobsTotal,
		EmailsPerJob: perJob,
	}

	recent, err := s.store.ListJobs(ctx, db.JobFilter{}, "created_at:desc", 1, 0)
	if err != nil {
		return Scraper{}, apperr.Internal(err)
	}
	if len(recent) > 0 {
		out.LastJob = &recent[0]
	}
	return out, nil
}
