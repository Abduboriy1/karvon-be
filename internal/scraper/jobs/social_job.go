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
	"github.com/bory/karvon-be/internal/scraper/fbscrape"
)

// FacebookScraper reads the public details box of one Facebook Page. It is the
// fb-scrape service's client, and nil when that service is not enabled.
type FacebookScraper interface {
	Scrape(ctx context.Context, pageURL string) (fbscrape.Page, error)
}

// SocialScrapeWorker is the entry stage of a social media scrape. The job already
// owns its businesses (attached when it was created), so there is nothing to search
// and no website to crawl: the worker queues one SocialPageArgs per business with a
// profile on each requested network and hands the rest to SocialPageWorker.
type SocialScrapeWorker struct {
	river.WorkerDefaults[scraper.SocialScrapeArgs]
	deps *Deps
}

// NewSocialScrapeWorker builds the social media scrape entry worker.
func NewSocialScrapeWorker(deps *Deps) *SocialScrapeWorker { return &SocialScrapeWorker{deps: deps} }

// Work implements river.Worker.
func (w *SocialScrapeWorker) Work(ctx context.Context, rj *river.Job[scraper.SocialScrapeArgs]) error {
	d := w.deps
	jobID := rj.Args.JobID

	row, active, err := d.jobActive(ctx, jobID)
	if err != nil {
		return err
	}
	if !active {
		d.Log.Info("skipping social scrape for inactive job", "job_id", jobID, "status", row.Status)
		return nil
	}
	cfg, err := decodeConfig(row.Config)
	if err != nil {
		d.failJob(ctx, jobID, "job configuration could not be read")
		return nil //nolint:nilerr // the job is already marked failed
	}

	if err := d.Store.MarkJobRunning(ctx, jobID); err != nil {
		return fmt.Errorf("jobs: mark running: %w", err)
	}
	if err := d.Publisher.Status(ctx, jobID, events.Status{Status: scraper.StatusRunning}); err != nil {
		d.Log.Warn("could not publish running event", "job_id", jobID, "error", err)
	}

	results, err := d.Store.CountJobResults(ctx, jobID)
	if err != nil {
		return fmt.Errorf("jobs: count job results: %w", err)
	}
	var inserts []river.InsertManyParams
	for _, network := range cfg.SocialNetworks {
		label := scraper.SocialNetworkLabel(network)
		// The scraper may have been switched off between the request and now.
		if network != scraper.SocialNetworkFacebook || d.Facebook == nil {
			d.logLine(ctx, jobID, events.LevelWarn, "the %s scraper is not enabled; skipping %s", label, label)
			continue
		}
		targets, err := d.Store.ListJobSocialTargets(ctx, dbgen.ListJobSocialTargetsParams{
			Network:          network,
			Jid:              jobID,
			MissingEmailOnly: cfg.SocialMissingEmailOnly,
		})
		if err != nil {
			return fmt.Errorf("jobs: list social targets: %w", err)
		}
		for _, target := range targets {
			inserts = append(inserts, river.InsertManyParams{Args: scraper.SocialPageArgs{
				JobID:      jobID,
				BusinessID: target.ID,
				Network:    network,
				URL:        target.Url,
			}})
		}
		without := "a"
		if cfg.SocialMissingEmailOnly {
			without = "no email and a"
		}
		d.logLine(ctx, jobID, events.LevelInfo, "%d of %d business(es) have %s %s page to read",
			len(targets), results, without, label)
	}

	// The pages are the job's whole workload, so the progress bar counts them the way
	// a re-crawl counts its websites.
	st, err := d.stats(ctx, jobID)
	if err != nil {
		return err
	}
	st.SitesTotal = len(inserts)
	st.SitesCrawled = 0
	if err := d.saveStats(ctx, jobID, st); err != nil {
		return err
	}

	if len(inserts) == 0 {
		d.logLine(ctx, jobID, events.LevelInfo, "no social profiles to read, finalizing")
		d.publishProgress(ctx, jobID)
		return d.enqueue(ctx, scraper.FinalizeArgs{JobID: jobID})
	}
	if _, err := d.Queue.InsertMany(ctx, inserts); err != nil {
		return fmt.Errorf("jobs: enqueue social pages: %w", err)
	}
	d.logLine(ctx, jobID, events.LevelInfo, "queued %d social profile page(s)", len(inserts))
	d.publishProgress(ctx, jobID)
	return nil
}

// SocialPageWorker reads one business's social profile and stores the email address
// and phone number it shows.
//
// A page that shows nothing is never fatal, and neither is the service failing once
// the page's attempts are spent: the page is counted, a line goes on the job's log,
// and the pipeline moves on.
type SocialPageWorker struct {
	river.WorkerDefaults[scraper.SocialPageArgs]
	deps *Deps
}

// NewSocialPageWorker builds the social profile page worker.
func NewSocialPageWorker(deps *Deps) *SocialPageWorker { return &SocialPageWorker{deps: deps} }

// Timeout implements river.Worker. A page can wait in the service's queue for a proxy
// that is resting, which takes longer than River's one-minute default.
func (w *SocialPageWorker) Timeout(*river.Job[scraper.SocialPageArgs]) time.Duration {
	return w.deps.Config.SocialPageTimeout + crawlBookkeepingTimeout + time.Minute
}

// Work implements river.Worker.
func (w *SocialPageWorker) Work(ctx context.Context, rj *river.Job[scraper.SocialPageArgs]) error {
	d := w.deps
	args := rj.Args

	if _, active, err := d.jobActive(ctx, args.JobID); err != nil {
		return err
	} else if !active {
		return nil
	}
	if args.Network != scraper.SocialNetworkFacebook || d.Facebook == nil {
		d.logLine(ctx, args.JobID, events.LevelWarn, "the %s scraper is not enabled; skipped %s",
			scraper.SocialNetworkLabel(args.Network), args.URL)
		return d.finishSocialPage(ctx, args.JobID, 0)
	}

	page, err := d.Facebook.Scrape(ctx, args.URL)
	if err != nil {
		// The service itself failed, not the page: try again after River's backoff,
		// so a container that is restarting does not empty the whole job. Only a page
		// out of attempts is given up.
		if rj.Attempt < rj.MaxAttempts {
			return err
		}
		d.logLine(ctx, args.JobID, events.LevelWarn, "could not read %s: %v", args.URL, err)
		return d.finishSocialPage(ctx, args.JobID, 0)
	}
	if page.Error != "" {
		d.logLine(ctx, args.JobID, events.LevelDebug, "nothing to read on %s: %s", args.URL, page.Error)
		return d.finishSocialPage(ctx, args.JobID, 0)
	}

	stored, err := w.save(ctx, args, page)
	if err != nil {
		d.logLine(ctx, args.JobID, events.LevelWarn, "could not store what %s shows: %v", args.URL, err)
	}
	return d.finishSocialPage(ctx, args.JobID, stored)
}

// save stores the page's email address as one of the business's addresses, and its
// phone number when the business has none.
func (w *SocialPageWorker) save(ctx context.Context, args scraper.SocialPageArgs, page fbscrape.Page) (int, error) {
	d := w.deps
	biz, err := d.Store.GetBusiness(ctx, args.BusinessID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("load business: %w", err)
	}

	stored := 0
	if page.Email != "" {
		domain := ""
		if biz.Domain != nil {
			domain = *biz.Domain
		}
		stored, err = d.Ingestor.SaveFound(ctx, biz.ID, domain, []business.FoundEmail{{
			Email:   page.Email,
			Source:  business.EmailSourceFacebook,
			PageURL: args.URL,
		}})
		if err != nil {
			return 0, err
		}
	}
	phoneAdded := false
	if page.Phone != "" {
		phone := page.Phone
		filled, err := d.Store.FillBusinessPhone(ctx, dbgen.FillBusinessPhoneParams{ID: biz.ID, Phone: &phone})
		if err != nil {
			return stored, fmt.Errorf("fill phone: %w", err)
		}
		phoneAdded = filled > 0
	}

	switch {
	case stored > 0 && phoneAdded:
		d.logLine(ctx, args.JobID, events.LevelInfo, "read %s: %d email(s) and a phone number", args.URL, stored)
	case stored > 0:
		d.logLine(ctx, args.JobID, events.LevelInfo, "read %s: %d email(s)", args.URL, stored)
	case phoneAdded:
		d.logLine(ctx, args.JobID, events.LevelInfo, "read %s: a phone number, no email", args.URL)
	default:
		d.logLine(ctx, args.JobID, events.LevelDebug, "read %s: no email", args.URL)
	}
	return stored, nil
}

// finishSocialPage counts one page as done and finalizes the job after the last one.
// Like finishSite it runs on a context detached from the job's, so a page that used
// up its time still gets counted.
func (d *Deps) finishSocialPage(ctx context.Context, jobID uuid.UUID, emailsStored int) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), crawlBookkeepingTimeout)
	defer cancel()

	if err := d.bump(ctx, jobID, scraper.StatKeySitesCrawled, 1); err != nil {
		return err
	}
	if err := d.bump(ctx, jobID, scraper.StatKeyEmailsFound, int64(emailsStored)); err != nil {
		return err
	}
	d.publishProgress(ctx, jobID)
	return d.advanceAfterCrawl(ctx, jobID)
}
