package scraper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/events"
	"github.com/bory/karvon-be/internal/ids"
)

// Service is the business logic behind every /jobs endpoint.
type Service struct {
	store      *db.Store
	queue      Enqueuer
	publisher  *events.Publisher
	log        *slog.Logger
	maxQueries int
}

// Enqueuer is the queue surface the service needs. It matches queue.Enqueuer.
type Enqueuer interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
	JobList(ctx context.Context, params *river.JobListParams) (*river.JobListResult, error)
	JobCancel(ctx context.Context, jobID int64) (*rivertype.JobRow, error)
}

// ServiceConfig configures the job service.
type ServiceConfig struct {
	MaxQueriesPerJob int
}

// NewService builds the job service.
func NewService(store *db.Store, q Enqueuer, publisher *events.Publisher, log *slog.Logger, cfg ServiceConfig) *Service {
	if cfg.MaxQueriesPerJob <= 0 {
		cfg.MaxQueriesPerJob = 500
	}
	return &Service{store: store, queue: q, publisher: publisher, log: log, maxQueries: cfg.MaxQueriesPerJob}
}

// CreateInput is a validated-on-entry job creation request.
type CreateInput struct {
	Name     string
	SourceID uuid.UUID
	Config   Config
}

// Estimate is the response of POST /jobs/estimate.
type Estimate struct {
	Queries        int
	EstListings    int
	EstCostCents   int64
	CostPer1kCents int
	// Unlimited says the run has no per-term cap, so the listing and cost figures
	// are an assumed ceiling rather than a bound the provider will honour.
	Unlimited bool
}

// Estimate prices a config without creating anything.
func (s *Service) Estimate(ctx context.Context, in CreateInput) (Estimate, error) {
	cfg := in.Config.Normalize()
	if err := cfg.Validate(s.maxQueries); err != nil {
		return Estimate{}, err
	}
	source, err := s.loadSource(ctx, in.SourceID)
	if err != nil {
		return Estimate{}, err
	}

	listings := cfg.EstimatedListings()
	return Estimate{
		Queries:        cfg.QueryCount(),
		EstListings:    listings,
		EstCostCents:   EstimateCostCents(listings, int(source.CostPer1kCents)),
		CostPer1kCents: int(source.CostPer1kCents),
		Unlimited:      cfg.IsUnlimited(),
	}, nil
}

// Create validates a config, stores the job and enqueues the first stage in one
// transaction, so a job row never exists without its queue entry.
func (s *Service) Create(ctx context.Context, in CreateInput) (db.JobRow, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return db.JobRow{}, apperr.Validation("job configuration is invalid",
			apperr.FieldError{Field: "name", Message: "is required"})
	}
	if len(name) > MaxNameLen {
		return db.JobRow{}, apperr.Validation("job configuration is invalid",
			apperr.FieldError{Field: "name", Message: fmt.Sprintf("must be at most %d characters", MaxNameLen)})
	}

	cfg := in.Config.Normalize()
	if err := cfg.Validate(s.maxQueries); err != nil {
		return db.JobRow{}, err
	}

	source, err := s.loadSource(ctx, in.SourceID)
	if err != nil {
		return db.JobRow{}, err
	}
	if err := requireUsableSource(source); err != nil {
		return db.JobRow{}, err
	}

	return s.insertJob(ctx, name, source.ID, cfg, newJob{
		stats: Stats{QueriesTotal: cfg.QueryCount()},
		args:  func(jobID uuid.UUID) river.JobArgs { return ScrapeArgs{JobID: jobID} },
	})
}

// newJob describes how a job row starts life: its initial counters, the queue entry
// that kicks it off and any extra rows to write in the same transaction.
type newJob struct {
	stats Stats
	args  func(jobID uuid.UUID) river.JobArgs
	// prepare runs inside the transaction after the job row exists, before the
	// queue entry is inserted.
	prepare func(ctx context.Context, q *dbgen.Queries, jobID uuid.UUID) error
}

func (s *Service) insertJob(ctx context.Context, name string, sourceID uuid.UUID, cfg Config, plan newJob) (db.JobRow, error) {
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return db.JobRow{}, apperr.Internal(fmt.Errorf("scraper: marshal config: %w", err))
	}
	statsJSON, err := json.Marshal(plan.stats)
	if err != nil {
		return db.JobRow{}, apperr.Internal(fmt.Errorf("scraper: marshal stats: %w", err))
	}

	jobID := ids.New()
	err = s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.CreateJob(ctx, dbgen.CreateJobParams{
			ID:       jobID,
			Name:     name,
			SourceID: sourceID,
			Config:   configJSON,
			Stats:    statsJSON,
		}); err != nil {
			return fmt.Errorf("scraper: create job: %w", err)
		}
		if plan.prepare != nil {
			if err := plan.prepare(ctx, q, jobID); err != nil {
				return err
			}
		}
		args := plan.args(jobID)
		if _, err := s.queue.InsertTx(ctx, tx, args, nil); err != nil {
			return fmt.Errorf("scraper: enqueue %s: %w", args.Kind(), err)
		}
		return nil
	})
	if err != nil {
		return db.JobRow{}, apperr.Internal(err)
	}

	row, err := s.store.GetJobRow(ctx, jobID)
	if err != nil {
		return db.JobRow{}, apperr.Internal(fmt.Errorf("scraper: reload job: %w", err))
	}
	return row, nil
}

// Get returns one job or a 404.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (db.JobRow, error) {
	row, err := s.store.GetJobRow(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.JobRow{}, apperr.NotFound("job")
	}
	if err != nil {
		return db.JobRow{}, apperr.Internal(err)
	}
	return row, nil
}

// ListResult is one page of jobs.
type ListResult struct {
	Jobs  []db.JobRow
	Total int64
}

// List returns a filtered, sorted page of jobs.
func (s *Service) List(ctx context.Context, filter db.JobFilter, sort string, page, perPage int) (ListResult, error) {
	total, err := s.store.CountJobs(ctx, filter)
	if err != nil {
		return ListResult{}, apperr.Internal(err)
	}
	rows, err := s.store.ListJobs(ctx, filter, sort, perPage, (page-1)*perPage)
	if err != nil {
		return ListResult{}, apperr.Internal(err)
	}
	return ListResult{Jobs: rows, Total: total}, nil
}

// Cancel stops a queued or running job cooperatively: the row flips to cancelled,
// every pending queue entry for it is cancelled, and running workers notice on their
// next status check.
func (s *Service) Cancel(ctx context.Context, id uuid.UUID) (db.JobRow, error) {
	row, err := s.Get(ctx, id)
	if err != nil {
		return db.JobRow{}, err
	}
	if row.Status != StatusQueued && row.Status != StatusRunning {
		return db.JobRow{}, apperr.Conflict("job is already %s", row.Status)
	}

	cancelled, err := s.store.CancelJob(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another request won the race.
		return s.Get(ctx, id)
	}
	if err != nil {
		return db.JobRow{}, apperr.Internal(err)
	}
	if err := s.store.CancelPendingJobQueries(ctx, id); err != nil {
		return db.JobRow{}, apperr.Internal(err)
	}

	s.cancelQueueJobs(ctx, id)
	// Enqueued after the sweep above, and deliberately untagged, so the entry that
	// stops the vendor spending is not cancelled along with the job it cleans up.
	if _, err := s.queue.Insert(ctx, AbortRunsArgs{JobID: id}, nil); err != nil {
		s.log.Warn("could not queue provider run cleanup", "job_id", id, "error", err)
	}

	finishedAt := cancelled.FinishedAt
	if err := s.publisher.Status(ctx, id, events.Status{
		Status:     StatusCancelled,
		FinishedAt: finishedAt,
	}); err != nil {
		s.log.Warn("could not publish cancel event", "job_id", id, "error", err)
	}
	return s.Get(ctx, id)
}

// cancelQueueJobs asks River to cancel every not-yet-finished job tagged with this
// scrape job id. Failures are logged, not returned: the cooperative status check in
// the workers is the authoritative stop signal.
func (s *Service) cancelQueueJobs(ctx context.Context, jobID uuid.UUID) {
	params := river.NewJobListParams().
		Metadata(MetadataFilter(jobID)).
		States(rivertype.JobStateAvailable, rivertype.JobStateScheduled,
			rivertype.JobStateRetryable, rivertype.JobStateRunning, rivertype.JobStatePending).
		First(1000)

	result, err := s.queue.JobList(ctx, params)
	if err != nil {
		s.log.Warn("could not list queue jobs for cancellation", "job_id", jobID, "error", err)
		return
	}
	for _, job := range result.Jobs {
		if _, err := s.queue.JobCancel(ctx, job.ID); err != nil {
			s.log.Warn("could not cancel queue job", "job_id", jobID, "river_job_id", job.ID, "error", err)
		}
	}
}

// Rerun clones a finished job's immutable config into a fresh job.
func (s *Service) Rerun(ctx context.Context, id uuid.UUID) (db.JobRow, error) {
	row, err := s.Get(ctx, id)
	if err != nil {
		return db.JobRow{}, err
	}
	cfg, err := DecodeConfig(row.Config)
	if err != nil {
		return db.JobRow{}, apperr.Internal(err)
	}
	source, err := s.loadSource(ctx, row.SourceID)
	if err != nil {
		return db.JobRow{}, err
	}
	if err := requireUsableSource(source); err != nil {
		return db.JobRow{}, err
	}

	// A re-run always searches again, even when the source job was itself a re-crawl.
	cfg.RecrawlOf = nil
	cfg = cfg.Normalize()
	if err := cfg.Validate(s.maxQueries); err != nil {
		return db.JobRow{}, err
	}
	return s.insertJob(ctx, suffixedName(row.Name, rerunSuffix), row.SourceID, cfg, newJob{
		stats: Stats{QueriesTotal: cfg.QueryCount()},
		args:  func(jobID uuid.UUID) river.JobArgs { return ScrapeArgs{JobID: jobID} },
	})
}

// Recrawl creates a new job that visits the websites of a finished job's businesses
// again, looking for addresses, without running any provider search. Businesses
// that already carry an address are skipped by the crawl stage, so the new job only
// spends time on the sites that came up empty.
//
// The provider's API key is not needed, so a disabled or key-less source does not
// block a re-crawl.
func (s *Service) Recrawl(ctx context.Context, id uuid.UUID) (db.JobRow, error) {
	row, err := s.Get(ctx, id)
	if err != nil {
		return db.JobRow{}, err
	}
	if row.Status == StatusQueued || row.Status == StatusRunning {
		return db.JobRow{}, apperr.Conflict("job is still %s; wait for it to finish before re-crawling", row.Status)
	}
	cfg, err := DecodeConfig(row.Config)
	if err != nil {
		return db.JobRow{}, apperr.Internal(err)
	}

	results, err := s.store.CountJobResults(ctx, id)
	if err != nil {
		return db.JobRow{}, apperr.Internal(fmt.Errorf("scraper: count job results: %w", err))
	}
	if results == 0 {
		return db.JobRow{}, apperr.Conflict("job has no businesses to re-crawl")
	}

	// Point at the original search, not at an intermediate re-crawl, so the chain
	// stays one level deep however often the user re-crawls.
	origin := id
	if cfg.RecrawlOf != nil {
		origin = *cfg.RecrawlOf
	}
	cfg.CrawlEmails = true
	cfg.RecrawlOf = &origin
	cfg = cfg.Normalize()
	if err := cfg.Validate(s.maxQueries); err != nil {
		return db.JobRow{}, err
	}

	return s.insertJob(ctx, suffixedName(row.Name, recrawlSuffix), row.SourceID, cfg, newJob{
		stats: Stats{ListingsFound: int(results)},
		args:  func(jobID uuid.UUID) river.JobArgs { return RecrawlArgs{JobID: jobID} },
		prepare: func(ctx context.Context, q *dbgen.Queries, jobID uuid.UUID) error {
			copied, err := q.CopyJobResults(ctx, dbgen.CopyJobResultsParams{
				TargetJobID: jobID,
				SourceJobID: id,
			})
			if err != nil {
				return fmt.Errorf("scraper: copy job results: %w", err)
			}
			if copied == 0 {
				return fmt.Errorf("scraper: copy job results: nothing copied")
			}
			return nil
		},
	})
}

// SourceRoleMaps is the sources.role value of a Google Maps data provider. The same
// table holds the email verifier, which a scrape must never select.
const SourceRoleMaps = "maps"

// Name suffixes for derived jobs.
const (
	rerunSuffix   = " (re-run)"
	recrawlSuffix = " (re-crawl)"
)

// suffixedName appends a marker while respecting the column length limit. A name
// that already ends with the marker is left alone; a different marker is swapped
// out, so "Gyms (re-run)" re-crawled reads "Gyms (re-crawl)", not both.
func suffixedName(name, suffix string) string {
	if strings.HasSuffix(name, suffix) {
		return name
	}
	for _, other := range []string{rerunSuffix, recrawlSuffix} {
		if other != suffix && strings.HasSuffix(name, other) {
			name = strings.TrimSuffix(name, other)
			break
		}
	}
	if len(name)+len(suffix) > MaxNameLen {
		name = name[:MaxNameLen-len(suffix)]
	}
	return name + suffix
}

// rerunName is kept for callers and tests that predate suffixedName.
func rerunName(name string) string { return suffixedName(name, rerunSuffix) }

// Delete removes a job and everything that hangs off it. Businesses are kept: they
// belong to the master list, not to the job that discovered them.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	row, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if row.Status == StatusQueued || row.Status == StatusRunning {
		// Stop the workers first so they cannot write rows back after the delete.
		if _, err := s.store.CancelJob(ctx, id); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return apperr.Internal(err)
		}
		s.cancelQueueJobs(ctx, id)
	}

	affected, err := s.store.DeleteJob(ctx, id)
	if err != nil {
		return apperr.Internal(err)
	}
	if affected == 0 {
		return apperr.NotFound("job")
	}
	return nil
}

func (s *Service) loadSource(ctx context.Context, id uuid.UUID) (dbgen.Source, error) {
	source, err := s.store.GetSource(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.Source{}, apperr.Validation("job configuration is invalid",
			apperr.FieldError{Field: "source_id", Message: "unknown source"})
	}
	if err != nil {
		return dbgen.Source{}, apperr.Internal(err)
	}
	return source, nil
}

// requireUsableSource rejects a source that cannot actually run a scrape.
func requireUsableSource(source dbgen.Source) error {
	// The sources table also holds the email verifier, which supplies no listings.
	if source.Role != SourceRoleMaps {
		return apperr.Validation("job configuration is invalid",
			apperr.FieldError{Field: "source_id", Message: "this source is not a Google Maps provider"})
	}
	if !source.Enabled {
		return apperr.Validation("job configuration is invalid",
			apperr.FieldError{Field: "source_id", Message: "source is disabled"})
	}
	if len(source.ApiKeyEnc) == 0 {
		return apperr.Validation("job configuration is invalid",
			apperr.FieldError{Field: "source_id", Message: "source has no API key configured"})
	}
	return nil
}
