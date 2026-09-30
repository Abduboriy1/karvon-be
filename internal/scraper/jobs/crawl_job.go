package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/business"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/events"
	"github.com/bory/karvon-be/internal/scraper"
	"github.com/bory/karvon-be/internal/scraper/crawler"
)

// Time limits for one crawl job. Each phase has its own budget so that a slow site
// can only ever cost its own crawl, never the bookkeeping that follows it: a site
// whose fetch outlived the job's deadline used to fail the write that records the
// visit, and after the last attempt the job waited forever on a site that was
// already done.
const (
	// crawlSlotWait is how long a job waits for one of its job's concurrency slots
	// before giving its worker back and trying again later.
	crawlSlotWait = time.Minute
	// crawlSiteBudget bounds the fetches for one site: robots.txt, the homepage and
	// every contact page after it. Whatever was found before it ran out is kept.
	crawlSiteBudget = 2 * time.Minute
	// crawlBookkeepingTimeout bounds the writes that record the visit. They run on a
	// context detached from the job's, so an expired crawl cannot cancel them.
	crawlBookkeepingTimeout = 30 * time.Second
	// crawlSlotRetry is how long a job that found no free slot waits before
	// trying again.
	crawlSlotRetry = 5 * time.Second
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

// Timeout implements river.Worker. River's default of one minute is shorter than a
// slow site takes, so the job gets room for every phase plus the lookups between
// them.
func (w *CrawlWorker) Timeout(*river.Job[scraper.CrawlArgs]) time.Duration {
	return crawlSlotWait + crawlSiteBudget + crawlBookkeepingTimeout + time.Minute
}

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

	// Honour the job's own concurrency setting on top of the worker pool size. A job
	// that cannot get a slot in time snoozes rather than failing: waiting is not an
	// error, and a failure here would spend one of its two attempts.
	slotCtx, cancelSlot := context.WithTimeout(ctx, crawlSlotWait)
	release, err := d.crawlSlots.Acquire(slotCtx, jobID, cfg.Concurrency)
	cancelSlot()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return river.JobSnooze(crawlSlotRetry)
		}
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

	// A re-crawl exists to fetch the site again, so it never reuses an earlier visit.
	emailsStored, err := w.crawl(ctx, jobID, biz, !cfg.IsRecrawl())
	if err != nil {
		// Record and continue: one unreachable site must not fail the job.
		d.logLine(ctx, jobID, events.LevelWarn, "could not crawl %s: %v", displayDomain(biz), err)
	}
	return d.finishSite(ctx, jobID, businessID, emailsStored)
}

// crawl reuses a fresh sibling's addresses when the same website was crawled
// recently and reuse is allowed, otherwise it fetches the site.
func (w *CrawlWorker) crawl(ctx context.Context, jobID uuid.UUID, biz dbgen.GetBusinessRow, reuse bool) (int, error) {
	d := w.deps

	if reuse && biz.Domain != nil && *biz.Domain != "" {
		siblingID, found, err := w.freshSibling(ctx, biz)
		if err != nil {
			return 0, err
		}
		if found {
			copied, copyErr := d.Ingestor.CopyEmailsFrom(ctx, siblingID, biz.ID, *biz.Domain)
			if copyErr != nil {
				return 0, copyErr
			}
			if _, copyErr := d.Ingestor.CopySocialsFrom(ctx, siblingID, biz.ID); copyErr != nil {
				return copied, copyErr
			}
			d.logLine(ctx, jobID, events.LevelInfo,
				"reused %d address(es) for %s from a crawl of the same page in the last %d days",
				copied, *biz.Domain, d.Config.RecrawlAfterDays)
			return copied, nil
		}
	}

	// Only the fetches run on the site budget. Storing what they found uses the job's
	// context, so a site that ran out of time still keeps the addresses it gave up.
	siteCtx, cancel := context.WithTimeout(ctx, crawlSiteBudget)
	result, crawlErr := d.Crawler.CrawlSite(siteCtx, *biz.Website)
	cancel()
	// Social profiles are kept whatever else the crawl found, including for a
	// business whose website is itself a social profile and so was not fetched.
	// A failure here is logged, not returned: it must not cost the emails below.
	if err := w.saveSocials(ctx, jobID, biz.ID, result); err != nil {
		d.logLine(ctx, jobID, events.LevelWarn, "could not store social profiles for %s: %v", displayDomain(biz), err)
	}
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

// freshSibling finds another listing of the same page crawled within the retention
// window. The query matches on the path, which SQL can compute; the query string is
// compared here, since pages on one path often differ only by it: every Facebook page
// without a vanity name is "facebook.com/profile.php?id=…".
func (w *CrawlWorker) freshSibling(ctx context.Context, biz dbgen.GetBusinessRow) (uuid.UUID, bool, error) {
	d := w.deps
	candidates, err := d.Store.ListFreshCrawledSiblings(ctx, dbgen.ListFreshCrawledSiblingsParams{
		Domain:     biz.Domain,
		PathKey:    business.WebsitePathKey(*biz.Website),
		ExcludeID:  biz.ID,
		MaxAgeDays: clampInt32(d.Config.RecrawlAfterDays),
	})
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("jobs: find crawled sibling: %w", err)
	}
	key := business.WebsiteKey(*biz.Website)
	for _, c := range candidates {
		if c.Website != nil && business.WebsiteKey(*c.Website) == key {
			return c.ID, true, nil
		}
	}
	return uuid.Nil, false, nil
}

// saveSocials stores the social profiles a crawl found.
func (w *CrawlWorker) saveSocials(ctx context.Context, jobID, businessID uuid.UUID, result crawler.SiteResult) error {
	if len(result.Socials) == 0 {
		return nil
	}
	found := make([]business.FoundSocial, 0, len(result.Socials))
	for _, link := range result.Socials {
		found = append(found, business.FoundSocial{
			Network: string(link.Network),
			Handle:  link.Handle,
			URL:     link.URL,
			PageURL: link.PageURL,
		})
	}
	stored, err := w.deps.Ingestor.SaveSocials(ctx, businessID, found)
	if err != nil {
		return err
	}
	if stored > 0 {
		w.deps.logLine(ctx, jobID, events.LevelDebug, "found %d social profile(s) for %s", stored, result.Domain)
	}
	return nil
}

// finishSite records the visit and advances the pipeline when the last site is done.
//
// It runs on a context detached from the job's. By the time it is called the site
// has been dealt with, and losing this write to a deadline is what strands a job
// short of its total: the retry crawls the same slow site into the same deadline,
// and once the attempts are spent nothing counts the site at all.
func (d *Deps) finishSite(ctx context.Context, jobID, businessID uuid.UUID, emailsStored int) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), crawlBookkeepingTimeout)
	defer cancel()

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
