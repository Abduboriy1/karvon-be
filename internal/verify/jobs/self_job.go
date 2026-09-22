package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/verify"
)

// SelfWorker is the free stage. It runs every enabled free provider over one address
// and combines their answers into a weighted score.
//
// It is the filter that stops the paid stage from ever seeing obvious rubbish, and
// now also the thing that decides an address is good enough that paying for it would
// add nothing. Nothing in here can fail because a provider is down: a provider that
// does not answer is left out of the average and its weight moves to the ones that
// did.
type SelfWorker struct {
	river.WorkerDefaults[verify.SelfArgs]
	deps *Deps
}

// NewSelfWorker builds the free-stage worker.
func NewSelfWorker(deps *Deps) *SelfWorker { return &SelfWorker{deps: deps} }

// Work implements river.Worker.
func (w *SelfWorker) Work(ctx context.Context, rj *river.Job[verify.SelfArgs]) error {
	d := w.deps
	runID, verificationID := rj.Args.RunID, rj.Args.VerificationID

	if _, active, err := d.runActive(ctx, runID); err != nil {
		return err
	} else if !active {
		return nil
	}

	row, err := d.Store.GetEmailVerification(ctx, verificationID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The address was deleted while the run was queued.
		return d.finishItem(ctx, runID, verificationID, verify.ItemSkipped, 0, "the address no longer exists")
	}
	if err != nil {
		return fmt.Errorf("verify jobs: load verification: %w", err)
	}

	// Settings are read here rather than trusted from when the run was created, so
	// an operator who re-balances the weights mid-run affects the rest of it.
	settings := d.Settings.Settings(ctx)
	result := d.Stage.Run(ctx, settings, row.Email)

	checks, err := json.Marshal(result.Pass1.Checks)
	if err != nil {
		return fmt.Errorf("verify jobs: marshal checks: %w", err)
	}
	providers, err := verify.EncodeProviderScores(result.Score.Providers)
	if err != nil {
		return fmt.Errorf("verify jobs: marshal provider results: %w", err)
	}

	finalScore, tag := finalScoreFor(row, result.Score.Score)

	if _, err := d.Store.UpdateVerificationPass1(ctx, dbgen.UpdateVerificationPass1Params{
		ID:              verificationID,
		Pass1Score:      clampInt32(result.Pass1.Score),
		Pass1Checks:     checks,
		Pass1HardFail:   optional(result.Pass1.HardFail),
		FreeScore:       clampInt32(result.Score.Score),
		ProviderResults: providers,
		TypoSuggestion:  optional(result.Pass1.TypoSuggestion),
		FinalScore:      clampInt32(finalScore),
		VerificationTag: string(tag),
	}); err != nil {
		return fmt.Errorf("verify jobs: store free stage result: %w", err)
	}

	return d.finishItem(ctx, runID, verificationID, verify.ItemDone, 0, "")
}

// optional maps an empty string to a NULL column value.
func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
