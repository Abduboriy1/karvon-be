// Package jobs contains the River workers of the campaign module: launching and
// pushing leads to Instantly, applying provider events, reconciling with both
// providers, and pushing newsletter subscriptions to Mailchimp.
//
// Every worker is idempotent. The unit of retry is small, the guard is re-checked
// at execution time rather than trusted from enqueue time, and anything that
// spends a provider call is claimed in the database first.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/queue"
	"github.com/bory/karvon-be/internal/verify"
)

// defaultRateLimitSnooze is how long a throttled job waits when the provider did not say.
const defaultRateLimitSnooze = 30 * time.Second

// Config bounds the workers.
type Config struct {
	LeadBatch    int
	LeadBatchGap time.Duration
	// SyncWindowDays is how far back reconciliation looks for sends.
	SyncWindowDays int
}

// Deps is the shared dependency bundle every worker holds a pointer to.
//
// Queue is assigned after the River client is constructed, because the client
// needs the workers and the workers need the client.
type Deps struct {
	Store   *db.Store
	Service *service.Service
	Limiter *verify.RateLimiter
	Queue   queue.Enqueuer
	Log     *slog.Logger
	Config  Config
	Now     func() time.Time
}

// NewDeps applies defaults.
func NewDeps(deps Deps) *Deps {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Config.LeadBatch <= 0 {
		deps.Config.LeadBatch = 100
	}
	if deps.Config.SyncWindowDays <= 0 {
		deps.Config.SyncWindowDays = 7
	}
	if deps.Limiter == nil {
		deps.Limiter = verify.NewRateLimiter(0)
	}
	return &deps
}

func (d *Deps) enqueue(ctx context.Context, args river.JobArgs) error {
	if _, err := d.Queue.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("campaign jobs: enqueue %s: %w", args.Kind(), err)
	}
	return nil
}

func (d *Deps) enqueueTx(ctx context.Context, tx pgx.Tx, args river.JobArgs) error {
	if _, err := d.Queue.InsertTx(ctx, tx, args, nil); err != nil {
		return fmt.Errorf("campaign jobs: enqueue %s: %w", args.Kind(), err)
	}
	return nil
}

// campaignLive loads a campaign and reports whether it still wants work done.
func (d *Deps) campaignLive(ctx context.Context, id uuid.UUID) (dbgen.Campaign, bool, error) {
	row, err := d.Store.GetCampaign(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.Campaign{}, false, nil
	}
	if err != nil {
		return dbgen.Campaign{}, false, fmt.Errorf("campaign jobs: load campaign: %w", err)
	}
	switch row.Status {
	case campaign.CampaignArchived, campaign.CampaignFailed, campaign.CampaignCompleted, campaign.CampaignDraft:
		return row, false, nil
	}
	return row, true, nil
}

// snoozeFor turns a provider throttle into a River snooze.
func snoozeFor(err error) error {
	wait, ok := provider.RetryAfter(err)
	if !ok {
		wait = defaultRateLimitSnooze
	}
	return river.JobSnooze(wait)
}

// fatal reports whether a provider error can never be fixed by retrying.
func fatal(err error) bool {
	return errors.Is(err, provider.ErrAuth) || errors.Is(err, provider.ErrPaymentRequired) ||
		errors.Is(err, provider.ErrNotConfigured) || errors.Is(err, provider.ErrInvalid)
}

// syncRun brackets a reconciliation in the sync_runs log.
type syncRun struct {
	d       *Deps
	row     dbgen.SyncRun
	seen    int
	updated int
	details map[string]any
}

func (d *Deps) startSync(ctx context.Context, kind string, target *uuid.UUID) (*syncRun, error) {
	row, err := d.Store.CreateSyncRun(ctx, dbgen.CreateSyncRunParams{ID: newID(), Kind: kind, TargetID: campaign.NullUUID(target)})
	if err != nil {
		return nil, fmt.Errorf("campaign jobs: start sync run: %w", err)
	}
	return &syncRun{d: d, row: row, details: map[string]any{}}, nil
}

func (r *syncRun) finish(ctx context.Context, runErr error) {
	status := "done"
	var errText *string
	if runErr != nil {
		status = "failed"
		errText = campaign.Ptr(runErr.Error())
	}
	if err := r.d.Store.FinishSyncRun(ctx, dbgen.FinishSyncRunParams{
		ID: r.row.ID, Status: status, ItemsSeen: campaign.Int32(r.seen), ItemsUpdated: campaign.Int32(r.updated),
		Error: errText, Details: service.EncodeJSON(r.details),
	}); err != nil {
		r.d.Log.Warn("could not finish sync run", "kind", r.row.Kind, "error", err)
	}
}
