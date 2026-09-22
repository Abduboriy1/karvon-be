package jobs

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/events"
	"github.com/bory/karvon-be/internal/scraper"
)

// advanceAfterQueries moves a job from the query stage to the crawl stage (or straight
// to finalize) once every query has reached a terminal state.
//
// Several workers can reach this at the same time; every follow-up job is unique by
// its arguments, so duplicate inserts collapse in the queue.
func (d *Deps) advanceAfterQueries(ctx context.Context, jobID uuid.UUID, cfg scraper.Config) error {
	pending, err := d.Store.CountPendingJobQueries(ctx, jobID)
	if err != nil {
		return fmt.Errorf("jobs: count pending queries: %w", err)
	}
	if pending > 0 {
		return nil
	}

	if !cfg.CrawlEmails {
		d.logLine(ctx, jobID, events.LevelInfo, "email crawling disabled, finalizing")
		return d.enqueue(ctx, scraper.FinalizeArgs{JobID: jobID})
	}

	sitesTotal, err := d.Store.CountJobSitesTotal(ctx, jobID)
	if err != nil {
		return fmt.Errorf("jobs: count sites: %w", err)
	}
	targets, err := d.Store.ListJobCrawlTargets(ctx, jobID)
	if err != nil {
		return fmt.Errorf("jobs: list crawl targets: %w", err)
	}

	st, err := d.stats(ctx, jobID)
	if err != nil {
		return err
	}
	st.SitesTotal = int(sitesTotal)
	// Sites that already carry an address from the provider need no crawl and count
	// as done, so the progress bar starts from the right place.
	st.SitesCrawled = int(sitesTotal) - len(targets)
	if st.SitesCrawled < 0 {
		st.SitesCrawled = 0
	}
	if err := d.saveStats(ctx, jobID, st); err != nil {
		return err
	}

	if len(targets) == 0 {
		d.logLine(ctx, jobID, events.LevelInfo, "no websites left to crawl, finalizing")
		d.publishProgress(ctx, jobID)
		return d.enqueue(ctx, scraper.FinalizeArgs{JobID: jobID})
	}

	inserts := make([]river.InsertManyParams, 0, len(targets))
	for _, target := range targets {
		inserts = append(inserts, river.InsertManyParams{
			Args: scraper.CrawlArgs{JobID: jobID, BusinessID: target.ID},
		})
	}
	if _, err := d.Queue.InsertMany(ctx, inserts); err != nil {
		return fmt.Errorf("jobs: enqueue crawls: %w", err)
	}

	d.logLine(ctx, jobID, events.LevelInfo, "queued %d websites for email crawling", len(targets))
	d.publishProgress(ctx, jobID)
	return nil
}

// advanceAfterCrawl finalizes the job once every site has been visited.
func (d *Deps) advanceAfterCrawl(ctx context.Context, jobID uuid.UUID) error {
	st, err := d.stats(ctx, jobID)
	if err != nil {
		return err
	}
	if st.SitesTotal > 0 && st.SitesCrawled < st.SitesTotal {
		return nil
	}
	return d.enqueue(ctx, scraper.FinalizeArgs{JobID: jobID})
}
