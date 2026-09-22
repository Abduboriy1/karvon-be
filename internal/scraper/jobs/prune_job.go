package jobs

import (
	"context"
	"fmt"

	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/scraper"
)

// PruneWorker deletes job_events past the retention window. It is registered as a
// periodic River job so retention runs without an external scheduler.
type PruneWorker struct {
	river.WorkerDefaults[scraper.PruneEventsArgs]
	deps *Deps
}

// NewPruneWorker builds the retention worker.
func NewPruneWorker(deps *Deps) *PruneWorker { return &PruneWorker{deps: deps} }

// Work implements river.Worker.
func (w *PruneWorker) Work(ctx context.Context, _ *river.Job[scraper.PruneEventsArgs]) error {
	days := w.deps.Config.EventRetentionDays
	if days <= 0 {
		return nil
	}
	deleted, err := w.deps.Store.PruneJobEvents(ctx, clampInt32(days))
	if err != nil {
		return fmt.Errorf("jobs: prune events: %w", err)
	}
	if deleted > 0 {
		w.deps.Log.Info("pruned job events", "deleted", deleted, "retention_days", days)
	}
	return nil
}
