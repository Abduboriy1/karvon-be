package jobs

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/campaign/suppression"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// suppressionApply is the jobs' entry into the shared suppression cascade, with
// follow-up jobs enqueued in the caller's transaction.
func suppressionApply(ctx context.Context, q *dbgen.Queries, contactID, leadID uuid.UUID, reason, source, note string,
	d *Deps, tx pgx.Tx,
) (suppression.Result, error) {
	return suppression.Apply(ctx, q, suppression.ApplyInput{
		ContactID: contactID, Reason: reason, Source: source, Note: note, CampaignLeadID: &leadID, OccurredAt: d.Now(),
		Enqueue: func(ctx context.Context, args river.JobArgs) error { return d.enqueueTx(ctx, tx, args) },
	})
}
