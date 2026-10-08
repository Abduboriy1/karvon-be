// Package jobs contains the River workers that execute a scrape: expand the config,
// call the provider, crawl websites and finalize the job. Each stage is its own job
// kind, so a crash resumes at the stage that failed.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/business"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/events"
	"github.com/bory/karvon-be/internal/queue"
	"github.com/bory/karvon-be/internal/scraper"
	"github.com/bory/karvon-be/internal/scraper/crawler"
)

// Config holds the worker-side tunables.
type Config struct {
	ProviderTimeout    time.Duration
	RecrawlAfterDays   int
	EventRetentionDays int

	// RunPollInterval is how long a worker waits between asking a vendor whether a
	// long run has finished. The worker holds nothing open while it waits.
	RunPollInterval time.Duration
	// SlotWaitInterval is how often a query that found every run slot taken asks
	// again. It is shorter than the poll interval so a queued location starts soon
	// after another finishes; the claim is one small locked count, so it is cheap.
	// The cap itself is the source's max_active_runs, which the operator sets.
	SlotWaitInterval time.Duration
	// MaxRunDuration is when a run is treated as stuck and aborted, so that a run
	// nobody is watching cannot keep spending indefinitely.
	MaxRunDuration time.Duration
	// RunPageSize is how many dataset items one fetch reads.
	RunPageSize int
	// SocialPageTimeout bounds one social profile page, as the scraper's client does.
	SocialPageTimeout time.Duration
}

// Defaults for the asynchronous run settings.
const (
	defaultRunPollInterval   = time.Minute
	defaultSlotWaitInterval  = 15 * time.Second
	defaultMaxRunDuration    = 12 * time.Hour
	defaultRunPageSize       = 1000
	defaultSocialPageTimeout = 3 * time.Minute
)

// withDefaults fills in the run settings a caller left at zero.
func (c Config) withDefaults() Config {
	if c.RunPollInterval <= 0 {
		c.RunPollInterval = defaultRunPollInterval
	}
	if c.SlotWaitInterval <= 0 {
		c.SlotWaitInterval = min(defaultSlotWaitInterval, c.RunPollInterval)
	}
	if c.MaxRunDuration <= 0 {
		c.MaxRunDuration = defaultMaxRunDuration
	}
	if c.RunPageSize <= 0 {
		c.RunPageSize = defaultRunPageSize
	}
	if c.SocialPageTimeout <= 0 {
		c.SocialPageTimeout = defaultSocialPageTimeout
	}
	return c
}

// Deps is the shared dependency bundle every worker holds a pointer to.
//
// Queue is assigned after the River client is constructed, because the client needs
// the workers and the workers need the client.
type Deps struct {
	Store     *db.Store
	Publisher *events.Publisher
	Ingestor  *business.Ingestor
	Crawler   *crawler.Crawler
	// Facebook reads Facebook Pages for a social media scrape; nil when the
	// fb-scrape service is not enabled.
	Facebook  FacebookScraper
	Providers scraper.ProviderFactory
	Queue     queue.Enqueuer
	Log       *slog.Logger
	Config    Config

	// crawlSlots caps concurrent crawls per job, from the job's own config.
	crawlSlots *jobLimiter
}

// NewDeps builds the worker dependency bundle. Queue is assigned afterwards, because
// the River client needs the workers and the workers need the client.
func NewDeps(deps Deps) *Deps {
	deps.crawlSlots = newJobLimiter()
	deps.Config = deps.Config.withDefaults()
	return &deps
}

// jobActive reports whether a scrape job is still worth working on. A missing job
// (deleted while queued) and a terminal job both stop the pipeline quietly.
func (d *Deps) jobActive(ctx context.Context, jobID uuid.UUID) (dbgen.GetJobRow, bool, error) {
	row, err := d.Store.GetJob(ctx, jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.GetJobRow{}, false, nil
	}
	if err != nil {
		return dbgen.GetJobRow{}, false, fmt.Errorf("jobs: load job: %w", err)
	}
	switch row.Status {
	case scraper.StatusQueued, scraper.StatusRunning:
		return row, true, nil
	default:
		return row, false, nil
	}
}

// bump applies an atomic delta to one stats counter.
func (d *Deps) bump(ctx context.Context, jobID uuid.UUID, key string, delta int64) error {
	if delta == 0 {
		return nil
	}
	return d.Store.BumpJobStats(ctx, dbgen.BumpJobStatsParams{ID: jobID, Key: key, Delta: delta})
}

// stats reads the current counters.
func (d *Deps) stats(ctx context.Context, jobID uuid.UUID) (scraper.Stats, error) {
	raw, err := d.Store.GetJobStats(ctx, jobID)
	if err != nil {
		return scraper.Stats{}, fmt.Errorf("jobs: read stats: %w", err)
	}
	return scraper.DecodeStats(raw)
}

// publishProgress emits the current counters to the SSE stream.
func (d *Deps) publishProgress(ctx context.Context, jobID uuid.UUID) {
	st, err := d.stats(ctx, jobID)
	if err != nil {
		d.Log.Warn("could not read stats for progress event", "job_id", jobID, "error", err)
		return
	}
	if err := d.Publisher.Progress(ctx, jobID, events.Progress{
		QueriesDone:   st.QueriesDone,
		QueriesTotal:  st.QueriesTotal,
		ListingsFound: st.ListingsFound,
		SitesCrawled:  st.SitesCrawled,
		SitesTotal:    st.SitesTotal,
		EmailsFound:   st.EmailsFound,
		CostCents:     st.CostCents,
	}); err != nil {
		d.Log.Warn("could not publish progress event", "job_id", jobID, "error", err)
	}
}

// logLine records a human-readable line on the job's event stream and in the server log.
func (d *Deps) logLine(ctx context.Context, jobID uuid.UUID, level events.Level, format string, args ...any) {
	if err := d.Publisher.Log(ctx, jobID, level, format, args...); err != nil {
		d.Log.Warn("could not publish log event", "job_id", jobID, "error", err)
	}
}

// failJob marks a whole job failed and stops everything still pending for it.
func (d *Deps) failJob(ctx context.Context, jobID uuid.UUID, reason string) {
	msg := reason
	if err := d.Store.MarkJobTerminal(ctx, dbgen.MarkJobTerminalParams{
		ID:     jobID,
		Status: scraper.StatusFailed,
		Error:  &msg,
	}); err != nil {
		d.Log.Error("could not mark job failed", "job_id", jobID, "error", err)
	}
	if err := d.Store.CancelPendingJobQueries(ctx, jobID); err != nil {
		d.Log.Warn("could not cancel pending queries", "job_id", jobID, "error", err)
	}
	// A failed job can still have vendor runs in flight, and they bill whether or
	// not anything is left to read them.
	if _, err := d.Queue.Insert(ctx, scraper.AbortRunsArgs{JobID: jobID}, nil); err != nil {
		d.Log.Warn("could not queue provider run cleanup", "job_id", jobID, "error", err)
	}
	now := time.Now().UTC()
	if err := d.Publisher.Status(ctx, jobID, events.Status{
		Status:     scraper.StatusFailed,
		Error:      &msg,
		FinishedAt: &now,
	}); err != nil {
		d.Log.Warn("could not publish failure event", "job_id", jobID, "error", err)
	}
}

// enqueue inserts one follow-up job, logging rather than failing when the queue
// rejects a duplicate.
func (d *Deps) enqueue(ctx context.Context, args river.JobArgs) error {
	if _, err := d.Queue.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("jobs: enqueue %s: %w", args.Kind(), err)
	}
	return nil
}

// decodeConfig is a thin wrapper that keeps worker code readable.
func decodeConfig(raw []byte) (scraper.Config, error) {
	cfg, err := scraper.DecodeConfig(raw)
	if err != nil {
		return scraper.Config{}, err
	}
	return cfg, nil
}

// saveStats writes the full counters document back.
func (d *Deps) saveStats(ctx context.Context, jobID uuid.UUID, st scraper.Stats) error {
	raw, err := jsonMarshal(st)
	if err != nil {
		return err
	}
	return d.Store.SetJobStats(ctx, dbgen.SetJobStatsParams{ID: jobID, Stats: raw})
}

// jsonMarshal keeps the encoding/json import local to one helper.
func jsonMarshal(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("jobs: marshal: %w", err)
	}
	return raw, nil
}

// clampInt32 narrows a configuration value to the range a Postgres integer column
// accepts, so an absurd setting cannot overflow into a negative number.
func clampInt32(v int) int32 {
	switch {
	case v < 0:
		return 0
	case v > math.MaxInt32:
		return math.MaxInt32
	default:
		return int32(v)
	}
}
