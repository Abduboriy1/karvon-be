// Package jobs contains the River workers that execute a verification run: expand
// the filter, score each address locally, send the survivors to the paid provider,
// and close the run out. Each stage is its own job kind, so a crash resumes at the
// address that failed rather than at the start of the run.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/queue"
	"github.com/bory/karvon-be/internal/verify"
)

// Deps is the shared dependency bundle every worker holds a pointer to.
//
// Queue is assigned after the River client is constructed, because the client needs
// the workers and the workers need the client.
type Deps struct {
	Store *db.Store
	// Stage runs every enabled free provider and combines their answers.
	Stage *verify.FreeStage
	// Settings is where the weights, the toggles and the paid band come from. The
	// workers read it at execution time rather than at enqueue time, so a settings
	// change takes effect during a run rather than after it.
	Settings  verify.SettingsSource
	Verifiers verify.VerifierFactory
	Limiter   *verify.RateLimiter
	Queue     queue.Enqueuer
	Log       *slog.Logger
	Config    verify.ServiceConfig
}

// NewDeps builds the worker dependency bundle.
func NewDeps(deps Deps) *Deps {
	if deps.Settings == nil {
		deps.Settings = verify.StaticSettings(verify.DefaultSettings())
	}
	if deps.Config.MaxRunEmails <= 0 {
		deps.Config.MaxRunEmails = 50_000
	}
	return &deps
}

// runActive reports whether a run is still worth working on. A missing run (deleted
// while queued) and a terminal run both stop the pipeline quietly.
func (d *Deps) runActive(ctx context.Context, runID uuid.UUID) (dbgen.VerificationRun, bool, error) {
	row, err := d.Store.GetVerificationRun(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.VerificationRun{}, false, nil
	}
	if err != nil {
		return dbgen.VerificationRun{}, false, fmt.Errorf("verify jobs: load run: %w", err)
	}
	switch row.Status {
	case verify.RunQueued, verify.RunRunning:
		return row, true, nil
	default:
		return row, false, nil
	}
}

// finishItem records one address's outcome and advances the run when it was the last
// one outstanding.
func (d *Deps) finishItem(ctx context.Context, runID, verificationID uuid.UUID,
	status string, credits int, note string,
) error {
	var notePtr *string
	if note != "" {
		notePtr = &note
	}
	if err := d.Store.MarkVerificationRunItem(ctx, dbgen.MarkVerificationRunItemParams{
		RunID:          runID,
		VerificationID: verificationID,
		Status:         status,
		Error:          notePtr,
		Credits:        clampInt32(credits),
	}); err != nil {
		return fmt.Errorf("verify jobs: mark run item: %w", err)
	}
	if _, err := d.Store.RecomputeVerificationRunStats(ctx, runID); err != nil {
		return fmt.Errorf("verify jobs: recompute run stats: %w", err)
	}
	return d.advance(ctx, runID)
}

// advance enqueues the finalize job once nothing is left queued. The finalize job is
// unique by run id, so several workers finishing at once collapse into one.
func (d *Deps) advance(ctx context.Context, runID uuid.UUID) error {
	pending, err := d.Store.CountPendingVerificationRunItems(ctx, runID)
	if err != nil {
		return fmt.Errorf("verify jobs: count pending items: %w", err)
	}
	if pending > 0 {
		return nil
	}
	return d.enqueue(ctx, verify.FinalizeArgs{RunID: runID})
}

func (d *Deps) enqueue(ctx context.Context, args river.JobArgs) error {
	if _, err := d.Queue.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("verify jobs: enqueue %s: %w", args.Kind(), err)
	}
	return nil
}

// failRun stops a whole run. It is used only for conditions no retry can fix: a
// rejected API key or an empty credit balance.
func (d *Deps) failRun(ctx context.Context, runID uuid.UUID, reason string) {
	msg := reason
	if err := d.Store.MarkVerificationRunTerminal(ctx, dbgen.MarkVerificationRunTerminalParams{
		ID:     runID,
		Status: verify.RunFailed,
		Error:  &msg,
	}); err != nil {
		d.Log.Error("could not mark the run failed", "run_id", runID, "error", err)
	}
	if err := d.Store.CancelPendingVerificationRunItems(ctx, runID); err != nil {
		d.Log.Warn("could not cancel pending run items", "run_id", runID, "error", err)
	}
	if _, err := d.Store.RecomputeVerificationRunStats(ctx, runID); err != nil {
		d.Log.Warn("could not recompute run stats", "run_id", runID, "error", err)
	}
}

// decodeFilter reads the filter stored on the run.
func decodeFilter(raw []byte) (verify.RunFilter, error) {
	var filter verify.RunFilter
	if len(raw) == 0 {
		return verify.RunFilter{Scope: verify.ScopeAll}, nil
	}
	if err := json.Unmarshal(raw, &filter); err != nil {
		return verify.RunFilter{}, fmt.Errorf("verify jobs: decode filter: %w", err)
	}
	if filter.Scope == "" {
		filter.Scope = verify.ScopeAll
	}
	return filter, nil
}

// finalScoreFor resolves the score shown for a row: the paid verdict when one is
// recorded, otherwise the weighted free score that was just computed.
//
// The paid verdict still wins outright rather than being averaged in. It is the only
// provider contracted to stand behind its answer, and that has been the rule since
// the two-pass design; the free stage decides who gets asked, not what the answer is
// worth once it arrives.
func finalScoreFor(row dbgen.EmailVerification, freeScore int) (int, verify.Tag) {
	if row.Pass2VerifiedAt != nil && row.Pass2Score != nil {
		score := int(*row.Pass2Score)
		return verify.Finalize(freeScore, &score)
	}
	return verify.Finalize(freeScore, nil)
}

// clampInt32 narrows a value to what a Postgres integer column accepts.
func clampInt32(v int) int32 {
	switch {
	case v < 0:
		return 0
	case v > math.MaxInt32:
		return math.MaxInt32
	default:
		return int32(v)
	}
}

// exclusionSkip returns the skip note for a globally excluded address, or "". It is
// re-checked by every item at execution time, because a rule can be added while a
// run sits in the queue.
func (d *Deps) exclusionSkip(ctx context.Context, email string) (string, error) {
	ref, err := d.Store.MatchEmail(ctx, email)
	if err != nil {
		return "", fmt.Errorf("verify jobs: check exclusion: %w", err)
	}
	if ref == nil {
		return "", nil
	}
	return fmt.Sprintf("skipped: globally excluded by %s %q", ref.Kind, ref.DisplayValue), nil
}
