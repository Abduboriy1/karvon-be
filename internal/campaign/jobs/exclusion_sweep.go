package jobs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// ExclusionSweepWorker brings campaign leads in line with the global exclusion
// list after a rule is added or removed.
//
// The push claim already refuses excluded contacts, so the sweep is what reaches
// the leads the claim can no longer see: those already pushed, whose follow-up
// steps Instantly would otherwise keep sending. Nothing about the contact changes;
// only the lead's delivery state does.
type ExclusionSweepWorker struct {
	river.WorkerDefaults[campaign.ExclusionSweepArgs]
	deps *Deps
}

// NewExclusionSweepWorker builds the worker.
func NewExclusionSweepWorker(deps *Deps) *ExclusionSweepWorker {
	return &ExclusionSweepWorker{deps: deps}
}

// Work implements river.Worker.
func (w *ExclusionSweepWorker) Work(ctx context.Context, rj *river.Job[campaign.ExclusionSweepArgs]) error {
	d := w.deps
	var stopped, removing, restored int
	err := d.Store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		leads, err := q.ExcludeLiveCampaignLeads(ctx)
		if err != nil {
			return fmt.Errorf("exclude leads: %w", err)
		}
		stopped = len(leads)
		for _, lead := range leads {
			if lead.InstantlyLeadID == nil || *lead.InstantlyLeadID == "" {
				continue
			}
			removing++
			if err := d.enqueueTx(ctx, tx, campaign.RemoveLeadArgs{CampaignLeadID: lead.ID}); err != nil {
				return err
			}
		}
		back, err := q.RestoreExcludedCampaignLeads(ctx)
		if err != nil {
			return fmt.Errorf("restore leads: %w", err)
		}
		restored = len(back)
		return nil
	})
	if err != nil {
		return fmt.Errorf("campaign jobs: exclusion sweep: %w", err)
	}
	d.Log.Info("exclusion sweep", "exclusion_id", rj.Args.ExclusionID, "removed_rule", rj.Args.Removed,
		"leads_stopped", stopped, "leads_removing_from_instantly", removing, "leads_restored", restored)
	return nil
}
