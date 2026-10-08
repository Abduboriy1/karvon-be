package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
)

// inboxPagesPerRun bounds one inbox sync. GET /emails allows about twenty calls a
// minute, so a first backfill of a busy workspace is spread over several runs;
// each run walks oldest-first and the next starts where it stopped.
const inboxPagesPerRun = 30

// SyncInboxWorker mirrors the received emails Instantly's Unibox holds.
type SyncInboxWorker struct {
	river.WorkerDefaults[campaign.SyncInboxArgs]
	deps *Deps
}

// NewSyncInboxWorker builds the worker.
func NewSyncInboxWorker(deps *Deps) *SyncInboxWorker { return &SyncInboxWorker{deps: deps} }

// Timeout implements river.Worker: a full run is thirty rate-limited calls, well
// past River's one-minute default.
func (w *SyncInboxWorker) Timeout(*river.Job[campaign.SyncInboxArgs]) time.Duration {
	return 5 * time.Minute
}

// Work implements river.Worker.
func (w *SyncInboxWorker) Work(ctx context.Context, rj *river.Job[campaign.SyncInboxArgs]) error {
	d := w.deps
	client, err := d.Service.Instantly(ctx)
	if err != nil {
		//nolint:nilerr // not configured: nothing to mirror.
		return nil
	}
	run, err := d.startSync(ctx, campaign.SyncKindInstantlyInbox, nil)
	if err != nil {
		return err
	}
	syncErr := w.sync(ctx, client, run)
	run.finish(ctx, syncErr)
	if syncErr != nil {
		if errors.Is(syncErr, provider.ErrRateLimited) {
			return snoozeFor(syncErr)
		}
		if fatal(syncErr) || rj.Attempt >= rj.MaxAttempts {
			return nil
		}
		return syncErr
	}
	return nil
}

// sync walks the received emails created since the newest one stored, oldest
// first, so a run cut short by the page cap leaves no gap behind it.
func (w *SyncInboxWorker) sync(ctx context.Context, client instantly.Client, run *syncRun) error {
	d := w.deps
	since, err := d.Service.InboxSince(ctx)
	if err != nil {
		return err
	}
	run.details["since"] = since.Format(time.RFC3339)
	limiter := d.Service.EmailsLimiter()
	cursor := ""
	for pages := 0; pages < inboxPagesPerRun; pages++ {
		if err := limiter.Wait(ctx); err != nil {
			return err
		}
		page, err := client.ListEmails(ctx, instantly.ListEmailsInput{
			EmailType: "received", SortOrder: "asc", Limit: 100, MinTimestampCreated: &since, StartingAfter: cursor,
		})
		if err != nil {
			return fmt.Errorf("list emails: %w", err)
		}
		run.details["pages"] = pages + 1
		for _, email := range page.Items {
			run.seen++
			inserted, err := d.Service.MirrorEmail(ctx, email)
			if err != nil {
				return err
			}
			if inserted {
				run.updated++
			}
		}
		if page.NextStartingAfter == "" || len(page.Items) == 0 {
			run.details["complete"] = true
			return nil
		}
		cursor = page.NextStartingAfter
	}
	// More is waiting: queue the next run, which carries on from the new watermark.
	run.details["complete"] = false
	return d.enqueue(ctx, campaign.SyncInboxArgs{RequestID: newID()})
}
