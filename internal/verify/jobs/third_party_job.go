package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/verify"
	"github.com/bory/karvon-be/internal/verify/verifier"
)

// defaultRateLimitSnooze is how long a throttled job waits when the provider did not
// say how long to wait for.
const defaultRateLimitSnooze = 30 * time.Second

// ThirdPartyWorker is the only job in the system that can spend money.
//
// Everything it does before calling out is a guard: the free-stage gate and the send
// lock are re-checked here rather than trusted from when the run was created, and the
// lock is then claimed in the database, so an address cannot be sent to a third party
// twice and cannot be sent at all unless it survived the free pass.
type ThirdPartyWorker struct {
	river.WorkerDefaults[verify.ThirdPartyArgs]
	deps *Deps
}

// NewThirdPartyWorker builds the Pass 2 worker.
func NewThirdPartyWorker(deps *Deps) *ThirdPartyWorker { return &ThirdPartyWorker{deps: deps} }

// Work implements river.Worker.
func (w *ThirdPartyWorker) Work(ctx context.Context, rj *river.Job[verify.ThirdPartyArgs]) error {
	d := w.deps
	runID, verificationID := rj.Args.RunID, rj.Args.VerificationID

	run, active, err := d.runActive(ctx, runID)
	if err != nil {
		return err
	}
	if !active {
		return nil
	}

	row, err := d.Store.GetEmailVerification(ctx, verificationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return d.finishItem(ctx, runID, verificationID, verify.ItemSkipped, 0, "the address no longer exists")
	}
	if err != nil {
		return fmt.Errorf("verify jobs: load verification: %w", err)
	}

	if reason := d.skipReason(ctx, row); reason != "" {
		return d.finishItem(ctx, runID, verificationID, verify.ItemSkipped, 0, reason)
	}

	client, source, err := w.client(ctx, run.SourceID)
	if err != nil {
		d.failRun(ctx, runID, "the email verifier is not usable: "+err.Error())
		return d.finishItem(ctx, runID, verificationID, verify.ItemFailed, 0, err.Error())
	}

	// Space the calls out before spending anything.
	if err := d.Limiter.Wait(ctx); err != nil {
		return err
	}

	// Claim the address's one and only send. The read above can be stale — two runs
	// can hold the same address, and this job can be retried — so the claim, not the
	// read, is what authorises the call: it updates a row only while the address has
	// never been sent, and the loser of a race gets nothing back and skips.
	claimed, err := d.Store.ClaimThirdPartySend(ctx, verificationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return d.finishItem(ctx, runID, verificationID, verify.ItemSkipped, 0,
			"skipped: this address has already been sent to a third party")
	}
	if err != nil {
		return fmt.Errorf("verify jobs: claim third-party send: %w", err)
	}
	row = claimed

	result, err := client.Verify(ctx, row.Email)
	if err != nil {
		return w.handleError(ctx, rj, row, err)
	}

	return w.store(ctx, runID, row, source, result)
}

// releaseSend hands the claim back, for a call that never reached the provider. A
// rejected key, a throttle or a dropped connection is our failure, not an answer
// about the address, and it must not consume the single send the address is allowed.
func (d *Deps) releaseSend(ctx context.Context, row dbgen.EmailVerification) {
	if _, err := d.Store.ReleaseThirdPartySend(ctx, row.ID); err != nil {
		d.Log.Error("could not release the third-party send lock; this address is now locked out without ever having been answered",
			"verification_id", row.ID, "email", row.Email, "error", err)
	}
}

// skipReason returns why this address must not be sent to the provider, or "".
//
// Every condition here is re-checked immediately before spending money, rather than
// trusted from when the run was created, because the run may have been queued for
// hours behind a backlog.
func (d *Deps) skipReason(ctx context.Context, row dbgen.EmailVerification) string {
	settings := d.Settings.Settings(ctx)
	// The one-send rule comes first: no setting and no score can lift it.
	if row.ThirdPartySentAt != nil {
		return fmt.Sprintf("skipped: this address was sent to a third party on %s",
			row.ThirdPartySentAt.Format(time.DateOnly))
	}
	if !settings.PaidEnabled {
		return "skipped: paid verification is switched off"
	}
	if row.FreeScoredAt == nil {
		return "skipped: the address has not been through the free checks"
	}
	band := verify.PaidBandOf(settings)
	if int(row.FreeScore) < band.Min {
		return fmt.Sprintf("skipped: the free checks scored %d, below the %d needed",
			row.FreeScore, band.Min)
	}
	if int(row.FreeScore) >= band.Max {
		return fmt.Sprintf(
			"skipped: the free checks scored %d, at or above the %d confidence threshold",
			row.FreeScore, band.Max)
	}
	return ""
}

// client resolves the verifier the run was created against.
func (w *ThirdPartyWorker) client(ctx context.Context, sourceID uuid.NullUUID) (verifier.Verifier, dbgen.Source, error) {
	if !sourceID.Valid {
		return nil, dbgen.Source{}, errors.New("the run has no verifier source")
	}
	source, err := w.deps.Store.GetSource(ctx, sourceID.UUID)
	if err != nil {
		return nil, dbgen.Source{}, fmt.Errorf("load source: %w", err)
	}
	client, err := w.deps.Verifiers.For(ctx, source)
	if err != nil {
		return nil, dbgen.Source{}, err
	}
	return client, source, nil
}

// handleError decides whether a provider failure stops the run, waits, or retries.
func (w *ThirdPartyWorker) handleError(ctx context.Context, rj *river.Job[verify.ThirdPartyArgs],
	row dbgen.EmailVerification, err error,
) error {
	d := w.deps
	runID, verificationID := rj.Args.RunID, rj.Args.VerificationID

	switch {
	// Nothing a retry can fix, and every further call would fail the same way.
	case errors.Is(err, verifier.ErrAuth):
		d.releaseSend(ctx, row)
		d.failRun(ctx, runID, "the provider rejected the API key")
		return d.finishItem(ctx, runID, verificationID, verify.ItemFailed, 0, "the provider rejected the API key")

	case errors.Is(err, verifier.ErrInsufficientCredits):
		d.releaseSend(ctx, row)
		d.failRun(ctx, runID, "the provider account is out of credit")
		return d.finishItem(ctx, runID, verificationID, verify.ItemFailed, 0, "the provider account is out of credit")

	// A throttle is not a failure: wait exactly as long as we were asked to.
	case errors.Is(err, verifier.ErrRateLimited):
		// The address was never looked at, and the snoozed job will claim it again.
		d.releaseSend(ctx, row)
		wait, ok := verifier.RetryAfter(err)
		if !ok {
			wait = defaultRateLimitSnooze
		}
		d.Log.Info("the verifier throttled us", "run_id", runID, "wait", wait)
		return river.JobSnooze(wait)

	// The provider took the address and has not finished with it. That is an answer
	// about the address, so the send stands and the lock is kept: re-asking later
	// would be a second send, which is exactly what is forbidden. The row keeps its
	// free score and settles as unknown.
	case errors.Is(err, verifier.ErrPending):
		if storeErr := w.storeInconclusive(ctx, row, verify.Pass2Unknown, nil, 0,
			"the provider accepted this address but returned no verdict"); storeErr != nil {
			return storeErr
		}
		return d.finishItem(ctx, runID, verificationID, verify.ItemDone, 0,
			"the provider returned no verdict; the address keeps its free score and will not be sent again")
	}

	// Anything else is transient: a dropped connection, a timeout, a 5xx. None of
	// those is the provider answering, so the claim goes back either way and the
	// address stays eligible for a genuine first send.
	d.releaseSend(ctx, row)
	if rj.Attempt >= rj.MaxAttempts {
		d.Log.Warn("giving up on an address", "run_id", runID, "email", row.Email, "error", err)
		if storeErr := w.storeInconclusive(ctx, row, verify.Pass2Error, nil, 0, err.Error()); storeErr != nil {
			return storeErr
		}
		return d.finishItem(ctx, runID, verificationID, verify.ItemFailed, 0, err.Error())
	}
	return fmt.Errorf("verify jobs: provider call: %w", err)
}

// store records a conclusive verdict and bills it, or records an inconclusive one.
func (w *ThirdPartyWorker) store(ctx context.Context, runID uuid.UUID, row dbgen.EmailVerification,
	source dbgen.Source, result verifier.Result,
) error {
	d := w.deps
	status := verify.Pass2Status(result.Status)

	score, conclusive := verify.ScoreFor(status)
	if !conclusive {
		if err := w.storeInconclusive(ctx, row, status, result.Raw, result.CreditsUsed, result.Reason); err != nil {
			return err
		}
		return d.finishItem(ctx, runID, row.ID, verify.ItemDone, result.CreditsUsed,
			"no verdict: "+result.Reason)
	}

	finalScore, tag := verify.Finalize(int(row.FreeScore), &score)
	if _, err := d.Store.UpdateVerificationPass2(ctx, dbgen.UpdateVerificationPass2Params{
		ID:              row.ID,
		Pass2Score:      ptrInt32(clampInt32(score)),
		Pass2Status:     ptrString(string(status)),
		Pass2Raw:        result.Raw,
		Pass2SourceID:   uuid.NullUUID{UUID: source.ID, Valid: true},
		Credits:         clampInt32(result.CreditsUsed),
		FinalScore:      clampInt32(finalScore),
		VerificationTag: string(tag),
	}); err != nil {
		return fmt.Errorf("verify jobs: store pass 2 result: %w", err)
	}

	return d.finishItem(ctx, runID, row.ID, verify.ItemDone, result.CreditsUsed, "")
}

// storeInconclusive records an unknown or errored outcome without touching
// pass2_verified_at, so no verdict is claimed for the address. Whether it may be
// sent again is decided by third_party_sent_at alone, which the caller has either
// kept (the provider answered) or released (it never did).
func (w *ThirdPartyWorker) storeInconclusive(ctx context.Context, row dbgen.EmailVerification,
	status verify.Pass2Status, raw []byte, credits int, note string,
) error {
	_, err := w.deps.Store.UpdateVerificationPass2Inconclusive(ctx,
		dbgen.UpdateVerificationPass2InconclusiveParams{
			ID:          row.ID,
			Pass2Status: ptrString(string(status)),
			Pass2Raw:    raw,
			Credits:     clampInt32(credits),
			LastError:   optional(note),
		})
	if err != nil {
		return fmt.Errorf("verify jobs: store inconclusive result: %w", err)
	}
	return nil
}

func ptrInt32(v int32) *int32 { return &v }

func ptrString(v string) *string { return &v }
