// Package queue narrows the River client to the operations the application uses, so
// services and workers can be exercised with a stub in tests.
package queue

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Queue names. Each stage gets its own queue so concurrency can be tuned per stage.
const (
	QueueDefault  = river.QueueDefault
	QueueQueries  = "scrape_queries"
	QueueCrawl    = "scrape_crawl"
	QueueFinalize = "scrape_finalize"
	// Social profile pages are read through a separate service with its own proxy
	// pool, so they get their own pool sized to it rather than the website crawl's.
	QueueSocial = "scrape_social"
	// Verification has its own queues so a paid-provider outage can never starve
	// the scrape pipeline, and so the two passes are tuned independently.
	QueueVerifySelf  = "verify_self"
	QueueVerifyThird = "verify_third"
	// The campaign module keeps pushing, event processing, reconciliation and the
	// newsletter on separate queues so a provider outage parks one kind of work
	// without touching the others.
	QueueCampaignPush   = "campaign_push"
	QueueCampaignEvents = "campaign_events"
	QueueCampaignSync   = "campaign_sync"
	QueueNewsletter     = "newsletter"
	// Domain purchases run one at a time on their own queue: they spend money, and
	// nothing else should ever wait behind a registrar call.
	QueueDomains = "domains"
	// Workspace setups wait on Google for minutes at a time (by snoozing) and create
	// paid mailboxes; they get their own small pool.
	QueueWorkspace = "workspace"
)

// Enqueuer is the subset of *river.Client used by application code.
type Enqueuer interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
	InsertMany(ctx context.Context, params []river.InsertManyParams) ([]*rivertype.JobInsertResult, error)
	JobList(ctx context.Context, params *river.JobListParams) (*river.JobListResult, error)
	JobCancel(ctx context.Context, jobID int64) (*rivertype.JobRow, error)
}
