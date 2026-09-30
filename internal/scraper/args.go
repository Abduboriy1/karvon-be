package scraper

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/queue"
)

// River job kinds. They are persisted in the queue table, so renaming one is a
// migration, not a refactor.
const (
	KindScrape     = "scrape"
	KindQuery      = "scrape_query"
	KindCrawl      = "scrape_crawl"
	KindRecrawl    = "scrape_recrawl"
	KindSocial     = "scrape_social"
	KindSocialPage = "scrape_social_page"
	KindFinalize   = "scrape_finalize"
	KindAbortRuns  = "scrape_abort_runs"
	KindPrune      = "prune_job_events"
)

// ScrapeArgs starts a job: it expands the config into job_queries and fans out.
type ScrapeArgs struct {
	JobID uuid.UUID `json:"job_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (ScrapeArgs) Kind() string { return KindScrape }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (a ScrapeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queue.QueueDefault,
		MaxAttempts: 3,
		Metadata:    JobMetadata(a.JobID),
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// QueryArgs runs one term × location against the provider.
type QueryArgs struct {
	JobID   uuid.UUID `json:"job_id" river:"unique"`
	QueryID uuid.UUID `json:"query_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (QueryArgs) Kind() string { return KindQuery }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (a QueryArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queue.QueueQueries,
		MaxAttempts: 3,
		Metadata:    JobMetadata(a.JobID),
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// CrawlArgs fetches one business website looking for addresses.
//
// Both fields take part in the uniqueness key: the same business can legitimately be
// crawled again by a later job, and deduplicating on the business alone would leave
// that job's crawl stage unfinished forever.
type CrawlArgs struct {
	JobID      uuid.UUID `json:"job_id" river:"unique"`
	BusinessID uuid.UUID `json:"business_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (CrawlArgs) Kind() string { return KindCrawl }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (a CrawlArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queue.QueueCrawl,
		MaxAttempts: 2,
		Metadata:    JobMetadata(a.JobID),
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// RecrawlArgs starts a re-crawl job: the businesses were copied from a finished job,
// so the provider is skipped and the pipeline enters directly at the crawl stage.
type RecrawlArgs struct {
	JobID uuid.UUID `json:"job_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (RecrawlArgs) Kind() string { return KindRecrawl }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (a RecrawlArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queue.QueueDefault,
		MaxAttempts: 3,
		Metadata:    JobMetadata(a.JobID),
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// SocialScrapeArgs starts a social media scrape: the job owns hand-picked businesses,
// and the pipeline reads their social profiles instead of searching or crawling.
type SocialScrapeArgs struct {
	JobID uuid.UUID `json:"job_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (SocialScrapeArgs) Kind() string { return KindSocial }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (a SocialScrapeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queue.QueueDefault,
		MaxAttempts: 3,
		Metadata:    JobMetadata(a.JobID),
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// SocialPageArgs reads one business's profile on one network.
//
// URL is carried along so the worker need not look the profile up again; it is not
// part of the uniqueness key, which is the job, the business and the network.
type SocialPageArgs struct {
	JobID      uuid.UUID `json:"job_id" river:"unique"`
	BusinessID uuid.UUID `json:"business_id" river:"unique"`
	Network    string    `json:"network" river:"unique"`
	URL        string    `json:"url"`
}

// Kind implements river.JobArgs.
func (SocialPageArgs) Kind() string { return KindSocialPage }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (a SocialPageArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queue.QueueSocial,
		MaxAttempts: 3,
		Metadata:    JobMetadata(a.JobID),
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// AbortRunsArgs stops any vendor-side run a cancelled or failed job left behind.
//
// It carries no job metadata on purpose. Cancelling a job sweeps every queue entry
// tagged with that job, and this is the entry that stops the spending, so it must not
// be swept along with the work it is cleaning up after.
type AbortRunsArgs struct {
	JobID uuid.UUID `json:"job_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (AbortRunsArgs) Kind() string { return KindAbortRuns }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (AbortRunsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queue.QueueDefault,
		MaxAttempts: 5,
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// FinalizeArgs recomputes stats and closes a job out.
type FinalizeArgs struct {
	JobID uuid.UUID `json:"job_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (FinalizeArgs) Kind() string { return KindFinalize }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (a FinalizeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queue.QueueFinalize,
		MaxAttempts: 5,
		Metadata:    JobMetadata(a.JobID),
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// PruneEventsArgs deletes job_events past the retention window.
type PruneEventsArgs struct{}

// Kind implements river.JobArgs.
func (PruneEventsArgs) Kind() string { return KindPrune }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (PruneEventsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: queue.QueueDefault, MaxAttempts: 3}
}

// JobMetadata tags a River job with the scrape job it belongs to, which is how
// cancellation finds every pending job for a scrape.
func JobMetadata(jobID uuid.UUID) []byte {
	return []byte(fmt.Sprintf(`{"job_id":%q}`, jobID.String()))
}

// MetadataFilter is the JSON filter passed to river.JobListParams.Metadata.
func MetadataFilter(jobID uuid.UUID) string {
	b, _ := json.Marshal(map[string]string{"job_id": jobID.String()})
	return string(b)
}
