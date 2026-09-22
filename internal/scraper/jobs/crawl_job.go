package jobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/business"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/events"
	"github.com/bory/karvon-be/internal/scraper"
	"github.com/bory/karvon-be/internal/scraper/crawler"
)

// CrawlWorker is stage 3: fetch one business website and store the addresses found.
//
// A single site failing is never fatal. The site is marked as visited, a warning goes
// on the job's log, and the pipeline moves on.
type CrawlWorker struct {
	river.WorkerDefaults[scraper.CrawlArgs]
	deps *Deps
}

// NewCrawlWorker builds the stage 3 worker.
func NewCrawlWorker(deps *Deps) *CrawlWorker { return &CrawlWorker{deps: deps} }

// Work implements river.Worker.
func (w *CrawlWorker) Work(ctx context.Context, rj *river.Job[scraper.CrawlArgs]) error {
	d := w.deps
	jobID, businessID := rj.Args.JobID, rj.Args.BusinessID

	jobRow, active, err := d.jobActive(ctx, jobID)
	if err != nil {
		return err
	}
	if !active {
		return nil
	}

	cfg, err := decodeConfig(jobRow.Config)
	if err != nil {
		d.failJob(ctx, jobID, "job configuration could not be read")
		return nil //nolint:nilerr // the job is already marked failed
	}

	// Honour the job's own concurrency setting on top of the worker pool size.
	release, err := d.crawlSlots.Acquire(ctx, jobID, cfg.Concurrency)
	if err != nil {
		return err
	}
	defer release()

	// The job may have been cancelled while this worker waited for a slot.
	if _, stillActive, err := d.jobActive(ctx, jobID); err != nil {
		return err
	} else if !stillActive {
		return nil
	}

	biz, err := d.Store.GetBusiness(ctx, businessID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("jobs: load business: %w", err)
	}
	if biz.Website == nil || *biz.Website == "" {
		return d.finishSite(ctx, jobID, businessID, 0)
	}

	emailsStored, err := w.crawl(ctx, jobID, biz)
	if err != nil {
		// Record and continue: one unreachable site must not fail the job.
		d.logLine(ctx, jobID, events.LevelWarn, "could not crawl %s: %v", displayDomain(biz), err)
	}
	return d.finishSite(ctx, jobID, businessID, emailsStored)
}

// crawl reuses a fresh sibling's addresses when the same domain was crawled recently,
// otherwise it fetches the site.
func (w *CrawlWorker) crawl(ctx context.Context, jobID uuid.UUID, biz dbgen.GetBusinessRow) (int, error) {
	d := w.deps

	if biz.Domain != nil && *biz.Domain != "" {
		siblingID, err := d.Store.FindFreshCrawledSibling(ctx, dbgen.FindFreshCrawledSiblingParams{
			Domain:     biz.Domain,
			ExcludeID:  biz.ID,
			MaxAgeDays: clampInt32(d.Config.RecrawlAfterDays),
		})
		switch {
		case err == nil:
			copied, copyErr := d.Ingestor.CopyEmailsFrom(ctx, siblingID, biz.ID, *biz.Domain)
			if copyErr != nil {
				return 0, copyErr
			}
			d.logLine(ctx, jobID, events.LevelInfo,
				"reused %d address(es) for %s from a crawl in the last %d days",
				copied, *biz.Domain, d.Config.RecrawlAfterDays)
			return copied, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return 0, fmt.Errorf("jobs: find crawled sibling: %w", err)
		}
	}

	result, crawlErr := d.Crawler.CrawlSite(ctx, *biz.Website)
	if result.Skipped != crawler.SkipNone {
		d.logLine(ctx, jobID, events.LevelDebug, "skipped %s (%s)", displayDomain(biz), result.Skipped)
		return 0, nil
	}

	found := make([]business.FoundEmail, 0, len(result.Emails))
	for _, hit := range result.Emails {
		found = append(found, business.FoundEmail{
			Email:   hit.Email,
			Source:  string(hit.Source),
			PageURL: hit.PageURL,
		})
	}

	stored := 0
	if len(found) > 0 {
		var err error
		stored, err = d.Ingestor.SaveFound(ctx, biz.ID, result.Domain, found)
		if err != nil {
			return 0, err
		}
	}

	switch {
	case stored > 0:
		d.logLine(ctx, jobID, events.LevelInfo, "crawled %s: %d email(s)", result.Domain, stored)
	case crawlErr == nil:
		d.logLine(ctx, jobID, events.LevelDebug, "crawled %s: no emails found", result.Domain)
	}
	return stored, crawlErr
}

// finishSite records the visit and advances the pipeline when the last site is done.
func (d *Deps) finishSite(ctx context.Context, jobID, businessID uuid.UUID, emailsStored int) error {
	if err := d.Store.MarkBusinessCrawled(ctx, businessID); err != nil {
		return fmt.Errorf("jobs: mark business crawled: %w", err)
	}
	if err := d.bump(ctx, jobID, scraper.StatKeySitesCrawled, 1); err != nil {
		return err
	}
	if err := d.bump(ctx, jobID, scraper.StatKeyEmailsFound, int64(emailsStored)); err != nil {
		return err
	}
	d.publishProgress(ctx, jobID)
	return d.advanceAfterCrawl(ctx, jobID)
}

func displayDomain(biz dbgen.GetBusinessRow) string {
	if biz.Domain != nil && *biz.Domain != "" {
		return *biz.Domain
	}
	if biz.Website != nil {
		return *biz.Website
	}
	return biz.Name
}
