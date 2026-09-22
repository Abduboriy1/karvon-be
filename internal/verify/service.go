package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/business"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// balanceTTL is how long a provider balance is reused before being fetched again.
// The number only decorates the UI, so a minute of staleness costs nothing.
const balanceTTL = time.Minute

// Enqueuer is the queue surface the service needs; it matches queue.Enqueuer.
type Enqueuer interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
	JobList(ctx context.Context, params *river.JobListParams) (*river.JobListResult, error)
	JobCancel(ctx context.Context, jobID int64) (*rivertype.JobRow, error)
}

// ServiceConfig holds the deployment-level knobs, the ones an operator cannot change
// from the UI. The policy that *is* editable — weights, toggles and the paid band —
// lives in Settings and is read per request.
type ServiceConfig struct {
	// MaxRunEmails bounds a single bulk run.
	MaxRunEmails int
}

// PaidBand is the score range an address must fall in to be worth paying for.
//
// The floor and the ceiling exist for opposite reasons. Below the floor the address
// is rubbish and a provider would only confirm what the free checks already know.
// At or above the ceiling the free providers already agree, and paying adds nothing
// but cost. Only the band between them is genuinely uncertain.
type PaidBand struct {
	// Min is inclusive.
	Min int
	// Max is exclusive.
	Max int
}

// PaidBandOf reads the band out of the settings.
func PaidBandOf(s Settings) PaidBand { return PaidBand{Min: s.PaidMinScore, Max: s.PaidThreshold} }

// apply narrows a data-layer filter to the band.
func (b PaidBand) apply(filter *db.VerificationFilter) {
	minScore, maxScore := b.Min, b.Max
	filter.MinFreeScore = &minScore
	filter.MaxFreeScore = &maxScore
}

// Service implements every /verification endpoint.
type Service struct {
	store     *db.Store
	queue     Enqueuer
	verifiers VerifierFactory
	ingestor  *business.Ingestor
	settings  *SettingsStore
	stage     *FreeStage
	log       *slog.Logger
	cfg       ServiceConfig

	balanceMu   sync.Mutex
	balance     *int64
	balanceAt   time.Time
	balanceFail time.Time
}

// NewService builds the verification service.
func NewService(store *db.Store, queue Enqueuer, verifiers VerifierFactory,
	ingestor *business.Ingestor, settings *SettingsStore, stage *FreeStage,
	log *slog.Logger, cfg ServiceConfig,
) *Service {
	if cfg.MaxRunEmails <= 0 {
		cfg.MaxRunEmails = 50_000
	}
	return &Service{
		store: store, queue: queue, verifiers: verifiers,
		ingestor: ingestor, settings: settings, stage: stage, log: log, cfg: cfg,
	}
}

// Settings returns the current policy, falling back to the configured defaults when
// the row cannot be read. It never fails, because every caller would rather verify
// with the defaults than not verify at all.
func (s *Service) Settings(ctx context.Context) Settings {
	if s.settings == nil {
		return DefaultSettings()
	}
	return s.settings.Settings(ctx)
}

// Config exposes the policy knobs to the workers, which re-check them at execution
// time rather than trusting what was true when the run was created.
func (s *Service) Config() ServiceConfig { return s.cfg }

/* ------------------------------------------------------------------ listing */

// ListResult is one page of verified addresses.
type ListResult struct {
	Rows  []db.VerificationRow
	Total int64
}

// List returns a filtered, sorted page.
func (s *Service) List(ctx context.Context, filter db.VerificationFilter, sort string, page, perPage int) (ListResult, error) {
	total, err := s.store.CountVerifications(ctx, filter)
	if err != nil {
		return ListResult{}, apperr.Internal(err)
	}
	rows, err := s.store.ListVerifications(ctx, filter, sort, perPage, (page-1)*perPage)
	if err != nil {
		return ListResult{}, apperr.Internal(err)
	}
	return ListResult{Rows: rows, Total: total}, nil
}

// Detail is one address with its full breakdown and the businesses that hold it.
type Detail struct {
	Row dbgen.EmailVerification
	// Checks is the local pipeline's own breakdown, one line per local check.
	Checks []CheckResult
	// Providers is the weighted breakdown: what each free provider answered, the
	// weight it carried and the points it contributed.
	Providers  []ProviderScore
	Businesses []dbgen.ListBusinessesForEmailRow
}

// Get returns one address or a 404.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Detail, error) {
	row, err := s.store.GetEmailVerification(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, apperr.NotFound("verification")
	}
	if err != nil {
		return Detail{}, apperr.Internal(err)
	}

	checks, err := DecodeChecks(row.Pass1Checks)
	if err != nil {
		return Detail{}, apperr.Internal(err)
	}
	providers, err := DecodeProviderScores(row.ProviderResults)
	if err != nil {
		return Detail{}, apperr.Internal(err)
	}
	businesses, err := s.store.ListBusinessesForEmail(ctx, row.Email)
	if err != nil {
		return Detail{}, apperr.Internal(err)
	}
	return Detail{Row: row, Checks: checks, Providers: providers, Businesses: businesses}, nil
}

// DecodeChecks parses a stored breakdown.
func DecodeChecks(raw []byte) ([]CheckResult, error) {
	if len(raw) == 0 {
		return []CheckResult{}, nil
	}
	var checks []CheckResult
	if err := json.Unmarshal(raw, &checks); err != nil {
		return nil, fmt.Errorf("verify: decode checks: %w", err)
	}
	if checks == nil {
		checks = []CheckResult{}
	}
	return checks, nil
}

/* -------------------------------------------------------------------- stats */

// Stats gathers every number the Verification page shows.
func (s *Service) Stats(ctx context.Context) (Stats, error) {
	counters, err := s.store.VerificationCounters(ctx)
	if err != nil {
		return Stats{}, apperr.Internal(err)
	}
	needsSelf, err := s.store.CountEmailsNeedingSelfVerification(ctx)
	if err != nil {
		return Stats{}, apperr.Internal(err)
	}
	settings := s.Settings(ctx)
	band := PaidBandOf(settings)
	qualifying, err := s.store.CountQualifyingForThirdParty(ctx, dbgen.CountQualifyingForThirdPartyParams{
		MinScore: int32(band.Min), //nolint:gosec // G115: a score is 0-100
		MaxScore: int32(band.Max), //nolint:gosec // G115: a score is 0-100
	})
	if err != nil {
		return Stats{}, apperr.Internal(err)
	}
	active, err := s.store.CountActiveVerificationRuns(ctx)
	if err != nil {
		return Stats{}, apperr.Internal(err)
	}

	out := Stats{
		Total: counters.Total,
		ByTag: map[Tag]int64{
			TagGreen:      counters.TagGreen,
			TagLightGreen: counters.TagLightGreen,
			TagYellow:     counters.TagYellow,
			TagOrange:     counters.TagOrange,
			TagRed:        counters.TagRed,
		},
		SelfVerified:       counters.SelfVerified,
		ThirdPartyVerified: counters.ThirdPartyVerified,
		NeedsSelf:          needsSelf,
		QualifyingForThird: qualifying,
		CreditsUsedTotal:   counters.CreditsUsedTotal,
		CreditsUsed30d:     counters.CreditsUsed30d,
		ActiveRuns:         active,
	}

	if source, err := s.verifierSource(ctx); err == nil {
		out.QualifyingEstCostCents = CostCents(qualifying, int(source.CostPer1kCents))
		out.BalanceCredits = s.cachedBalance(ctx, source)
	}
	out.LastSelfRunAt = s.lastRunAt(ctx, PassSelf)
	out.LastThirdPartyRunAt = s.lastRunAt(ctx, PassThirdParty)
	return out, nil
}

func (s *Service) lastRunAt(ctx context.Context, pass Pass) *time.Time {
	row, err := s.store.LastFinishedVerificationRun(ctx, string(pass))
	if err != nil {
		return nil
	}
	return row.FinishedAt
}

/* ------------------------------------------------------------------ sources */

// verifierSource finds the configured verifier. There is one today; if that ever
// changes, the enabled one wins.
func (s *Service) verifierSource(ctx context.Context) (dbgen.Source, error) {
	rows, err := s.store.ListSources(ctx)
	if err != nil {
		return dbgen.Source{}, apperr.Internal(err)
	}
	var fallback *dbgen.Source
	for i := range rows {
		if rows[i].Role != RoleVerifier {
			continue
		}
		if rows[i].Enabled && len(rows[i].ApiKeyEnc) > 0 {
			return rows[i], nil
		}
		if fallback == nil {
			fallback = &rows[i]
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	return dbgen.Source{}, apperr.NotFound("verifier source")
}

// requireUsableVerifier rejects a paid run the provider could not serve.
func requireUsableVerifier(source dbgen.Source) error {
	if !source.Enabled {
		return apperr.Validation("the verifier is not ready",
			apperr.FieldError{Field: "source_id", Message: "the email verifier is disabled"})
	}
	if len(source.ApiKeyEnc) == 0 {
		return apperr.Validation("the verifier is not ready",
			apperr.FieldError{Field: "source_id", Message: "the email verifier has no API key configured"})
	}
	return nil
}

// cachedBalance reports the provider's remaining credit, or nil when it cannot be
// reached. A provider outage must never fail the stats endpoint.
func (s *Service) cachedBalance(ctx context.Context, source dbgen.Source) *int64 {
	if requireUsableVerifier(source) != nil {
		return nil
	}

	s.balanceMu.Lock()
	defer s.balanceMu.Unlock()
	now := time.Now()
	if s.balance != nil && now.Sub(s.balanceAt) < balanceTTL {
		return s.balance
	}
	// Do not hammer a failing provider on every page load either.
	if now.Sub(s.balanceFail) < balanceTTL {
		return nil
	}

	client, err := s.verifiers.For(ctx, source)
	if err != nil {
		s.balanceFail = now
		return nil
	}
	balance, err := client.Balance(ctx)
	if err != nil {
		s.balanceFail = now
		s.log.Warn("could not read the verifier balance", "error", err)
		return nil
	}
	credits := balance.Credits
	s.balance, s.balanceAt = &credits, now
	return s.balance
}

/* ----------------------------------------------------------------- estimate */

// baseFilter turns a submitted run filter into a data-layer filter.
func (s *Service) baseFilter(filter RunFilter) db.VerificationFilter {
	out := db.VerificationFilter{
		BusinessIDs:       filter.BusinessIDs,
		JobID:             filter.JobID,
		Tags:              filter.Tags,
		MinScore:          filter.MinScore,
		IncludeSuppressed: filter.IncludeSuppressed,
	}
	if filter.Scope == ScopeSelection {
		out.IDs = filter.IDs
	}
	return out
}

// EstimateRun prices a run without creating anything. For a paid run the numbers it
// returns are exactly what will be billed: cached and non-qualifying addresses are
// already excluded.
func (s *Service) EstimateRun(ctx context.Context, pass Pass, filter RunFilter) (Estimate, error) {
	if err := validatePass(pass); err != nil {
		return Estimate{}, err
	}
	if err := validateFilter(filter); err != nil {
		return Estimate{}, err
	}

	base := s.baseFilter(filter)
	var out Estimate

	// Addresses on the master list that have no verification row yet. A selection
	// run names existing rows, so nothing can be missing from it.
	var missing int64
	if filter.Scope != ScopeSelection {
		var err error
		missing, err = s.store.CountAddressesWithoutVerification(ctx, base)
		if err != nil {
			return Estimate{}, apperr.Internal(err)
		}
	}

	if pass == PassSelf {
		selfFilter := base
		if filter.StaleAfterDays != nil {
			cutoff := time.Now().UTC().AddDate(0, 0, -*filter.StaleAfterDays)
			selfFilter.Pass1VerifiedBefore = &cutoff
		}
		existing, err := s.store.CountVerifications(ctx, selfFilter)
		if err != nil {
			return Estimate{}, apperr.Internal(err)
		}
		out.Emails = existing + missing
		return out, nil
	}

	settings := s.Settings(ctx)
	complete := true
	sent, notSent := true, false

	// A paid stage that is switched off covers nothing, and saying so here is what
	// keeps the confirmation dialog honest: the count it shows is the count that
	// would be billed.
	if !settings.PaidEnabled {
		out.NeedsSelf = 0
		return out, nil
	}

	qualifying := base
	PaidBandOf(settings).apply(&qualifying)
	qualifying.FreeComplete = &complete
	qualifying.ThirdPartySent = &notSent

	// What the run would skip because those addresses have already had their one
	// send. They are inside the band and will still never be sent again.
	spentFilter := base
	PaidBandOf(settings).apply(&spentFilter)
	spentFilter.FreeComplete = &complete
	spentFilter.ThirdPartySent = &sent

	incomplete := false
	pendingFilter := base
	pendingFilter.FreeComplete = &incomplete

	eligible, err := s.store.CountVerifications(ctx, qualifying)
	if err != nil {
		return Estimate{}, apperr.Internal(err)
	}
	alreadySent, err := s.store.CountVerifications(ctx, spentFilter)
	if err != nil {
		return Estimate{}, apperr.Internal(err)
	}
	pending, err := s.store.CountVerifications(ctx, pendingFilter)
	if err != nil {
		return Estimate{}, apperr.Internal(err)
	}

	out.Emails = eligible
	out.Cached = alreadySent
	out.NeedsSelf = pending + missing
	out.CreditsNeeded = eligible

	source, err := s.verifierSource(ctx)
	if err == nil {
		out.CostPer1kCents = int(source.CostPer1kCents)
		out.EstCostCents = CostCents(eligible, out.CostPer1kCents)
		out.BalanceCredits = s.cachedBalance(ctx, source)
	}
	return out, nil
}

/* --------------------------------------------------------------------- runs */

// CreateRunInput is a validated request to start a run.
type CreateRunInput struct {
	Pass   Pass
	Filter RunFilter
	// MaxCostCents is the ceiling the operator confirmed. It is required for a paid
	// run: the server re-estimates and refuses if the real cost has grown since the
	// dialog was shown.
	MaxCostCents *int64
}

// CreateRun records the run and enqueues its first job in one transaction, so a run
// row never exists without the queue entry that drives it.
func (s *Service) CreateRun(ctx context.Context, in CreateRunInput) (dbgen.VerificationRun, error) {
	if err := validatePass(in.Pass); err != nil {
		return dbgen.VerificationRun{}, err
	}
	if err := validateFilter(in.Filter); err != nil {
		return dbgen.VerificationRun{}, err
	}

	estimate, err := s.EstimateRun(ctx, in.Pass, in.Filter)
	if err != nil {
		return dbgen.VerificationRun{}, err
	}

	var sourceID uuid.NullUUID
	if in.Pass == PassThirdParty {
		if !s.Settings(ctx).PaidEnabled {
			return dbgen.VerificationRun{}, apperr.Conflict(
				"paid verification is switched off in the verification settings")
		}
		source, err := s.verifierSource(ctx)
		if err != nil {
			return dbgen.VerificationRun{}, apperr.Validation("the verifier is not ready",
				apperr.FieldError{Field: "source_id", Message: "no email verifier is configured"})
		}
		if err := requireUsableVerifier(source); err != nil {
			return dbgen.VerificationRun{}, err
		}
		if in.MaxCostCents == nil {
			return dbgen.VerificationRun{}, apperr.Validation("a paid run must be confirmed",
				apperr.FieldError{
					Field:   "max_cost_cents",
					Message: "send the estimated cost you displayed, so the run cannot cost more than was approved",
				})
		}
		if estimate.EstCostCents > *in.MaxCostCents {
			return dbgen.VerificationRun{}, apperr.Conflict(
				"the run now costs %d cents, above the %d cents that were approved; re-estimate and confirm again",
				estimate.EstCostCents, *in.MaxCostCents)
		}
		sourceID = uuid.NullUUID{UUID: source.ID, Valid: true}
	}

	if estimate.Emails > int64(s.cfg.MaxRunEmails) {
		return dbgen.VerificationRun{}, apperr.Validation("the run is too large",
			apperr.FieldError{
				Field:   "filter",
				Message: fmt.Sprintf("matches %d addresses, the maximum per run is %d", estimate.Emails, s.cfg.MaxRunEmails),
			})
	}

	filterJSON, err := json.Marshal(in.Filter)
	if err != nil {
		return dbgen.VerificationRun{}, apperr.Internal(fmt.Errorf("verify: marshal filter: %w", err))
	}

	runID := ids.New()
	err = s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.CreateVerificationRun(ctx, dbgen.CreateVerificationRunParams{
			ID:           runID,
			Pass:         string(in.Pass),
			Filter:       filterJSON,
			EstCostCents: estimate.EstCostCents,
			SourceID:     sourceID,
		}); err != nil {
			return fmt.Errorf("verify: create run: %w", err)
		}
		if _, err := s.queue.InsertTx(ctx, tx, RunArgs{RunID: runID}, nil); err != nil {
			return fmt.Errorf("verify: enqueue run: %w", err)
		}
		return nil
	})
	if err != nil {
		return dbgen.VerificationRun{}, apperr.Internal(err)
	}

	return s.GetRun(ctx, runID)
}

// GetRun returns one run or a 404.
func (s *Service) GetRun(ctx context.Context, id uuid.UUID) (dbgen.VerificationRun, error) {
	row, err := s.store.GetVerificationRun(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.VerificationRun{}, apperr.NotFound("verification run")
	}
	if err != nil {
		return dbgen.VerificationRun{}, apperr.Internal(err)
	}
	return row, nil
}

// RunListResult is one page of run history.
type RunListResult struct {
	Runs  []dbgen.VerificationRun
	Total int64
}

// ListRuns returns the run history, newest first.
func (s *Service) ListRuns(ctx context.Context, pass, status *string, page, perPage int) (RunListResult, error) {
	total, err := s.store.CountVerificationRuns(ctx, dbgen.CountVerificationRunsParams{
		Pass: pass, Status: status,
	})
	if err != nil {
		return RunListResult{}, apperr.Internal(err)
	}
	rows, err := s.store.ListVerificationRuns(ctx, dbgen.ListVerificationRunsParams{
		Pass: pass, Status: status,
		Lim: int32(perPage),              //nolint:gosec // G115: bounded by MaxPerPage
		Off: int32((page - 1) * perPage), //nolint:gosec // G115: bounded by the page parameter
	})
	if err != nil {
		return RunListResult{}, apperr.Internal(err)
	}
	return RunListResult{Runs: rows, Total: total}, nil
}

// CancelRun stops a run cooperatively: the row flips, its queued items are marked
// skipped, pending queue entries are cancelled and running workers notice on their
// next status check.
func (s *Service) CancelRun(ctx context.Context, id uuid.UUID) (dbgen.VerificationRun, error) {
	row, err := s.GetRun(ctx, id)
	if err != nil {
		return dbgen.VerificationRun{}, err
	}
	if row.Status != RunQueued && row.Status != RunRunning {
		return dbgen.VerificationRun{}, apperr.Conflict("run is already %s", row.Status)
	}

	if _, err := s.store.CancelVerificationRun(ctx, id); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return dbgen.VerificationRun{}, apperr.Internal(err)
	}
	if err := s.store.CancelPendingVerificationRunItems(ctx, id); err != nil {
		return dbgen.VerificationRun{}, apperr.Internal(err)
	}
	s.cancelQueueJobs(ctx, id)
	return s.GetRun(ctx, id)
}

// cancelQueueJobs asks River to drop every pending job tagged with this run.
func (s *Service) cancelQueueJobs(ctx context.Context, runID uuid.UUID) {
	params := river.NewJobListParams().
		Metadata(MetadataFilter(runID)).
		States(rivertype.JobStateAvailable, rivertype.JobStateScheduled,
			rivertype.JobStateRetryable, rivertype.JobStateRunning, rivertype.JobStatePending).
		First(10_000)

	result, err := s.queue.JobList(ctx, params)
	if err != nil {
		s.log.Warn("could not list queue jobs for cancellation", "run_id", runID, "error", err)
		return
	}
	for _, job := range result.Jobs {
		if _, err := s.queue.JobCancel(ctx, job.ID); err != nil {
			s.log.Warn("could not cancel queue job", "run_id", runID, "river_job_id", job.ID, "error", err)
		}
	}
}

/* ------------------------------------------------------- single-email actions */

// VerifyOne queues one address through the same machinery as a bulk run, so there is
// exactly one execution path.
func (s *Service) VerifyOne(ctx context.Context, id uuid.UUID, pass Pass) (dbgen.VerificationRun, error) {
	if err := validatePass(pass); err != nil {
		return dbgen.VerificationRun{}, err
	}
	detail, err := s.Get(ctx, id)
	if err != nil {
		return dbgen.VerificationRun{}, err
	}

	if pass == PassThirdParty {
		if err := s.checkGate(ctx, detail.Row); err != nil {
			return dbgen.VerificationRun{}, err
		}
	}

	input := CreateRunInput{
		Pass:   pass,
		Filter: RunFilter{Scope: ScopeSelection, IDs: []uuid.UUID{id}, IncludeSuppressed: true},
	}
	if pass == PassThirdParty {
		source, err := s.verifierSource(ctx)
		if err != nil {
			return dbgen.VerificationRun{}, apperr.Validation("the verifier is not ready",
				apperr.FieldError{Field: "source_id", Message: "no email verifier is configured"})
		}
		// A single address is one credit; the ceiling is implied by the action.
		ceiling := CostCents(1, int(source.CostPer1kCents))
		input.MaxCostCents = &ceiling
	}
	return s.CreateRun(ctx, input)
}

// checkGate rejects a paid verification the pipeline would refuse anyway, so the
// operator gets a clear reason instead of a run that skips its only item.
func (s *Service) checkGate(ctx context.Context, row dbgen.EmailVerification) error {
	// The one-send rule is checked before anything else, because it is the only
	// condition here that no setting, band or retry can ever lift.
	if row.ThirdPartySentAt != nil {
		return apperr.Conflict(
			"this address was sent to a third-party verifier on %s; an address is never sent to one twice",
			row.ThirdPartySentAt.Format(time.DateOnly))
	}
	settings := s.Settings(ctx)
	if !settings.PaidEnabled {
		return apperr.Conflict("paid verification is switched off in the verification settings")
	}
	if row.FreeScoredAt == nil {
		return apperr.Conflict("this address has not been through the free checks yet; run them first")
	}
	band := PaidBandOf(settings)
	if int(row.FreeScore) < band.Min {
		return apperr.Conflict("the free checks scored %d, below the %d needed for a paid check",
			row.FreeScore, band.Min)
	}
	if int(row.FreeScore) >= band.Max {
		return apperr.Conflict(
			"the free checks already scored %d, at or above the %d confidence threshold; a paid check would add nothing",
			row.FreeScore, band.Max)
	}
	return nil
}

// ApplyTypo rewrites every business_emails row carrying the misspelled address to
// the suggested one, merging into an existing corrected row where a business already
// holds it, then queues a self verification for the corrected address.
func (s *Service) ApplyTypo(ctx context.Context, id uuid.UUID) (Detail, error) {
	detail, err := s.Get(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	if detail.Row.TypoSuggestion == nil || *detail.Row.TypoSuggestion == "" {
		return Detail{}, apperr.Conflict("this address has no suggested correction")
	}

	oldEmail := detail.Row.Email
	newEmail := NormalizeAddress(*detail.Row.TypoSuggestion)
	if newEmail == oldEmail {
		return Detail{}, apperr.Conflict("the suggested correction is the address itself")
	}
	if _, _, ok := SplitAddress(newEmail); !ok {
		return Detail{}, apperr.Internal(fmt.Errorf("verify: stored suggestion %q is not an address", newEmail))
	}

	affected, err := s.store.ListBusinessIDsForEmail(ctx, oldEmail)
	if err != nil {
		return Detail{}, apperr.Internal(err)
	}

	err = s.store.InTx(ctx, func(q *dbgen.Queries) error {
		// A business already holding the corrected address keeps that row; its
		// typo'd row is dropped, because (business_id, email) is unique.
		if _, err := q.DeleteShadowedBusinessEmails(ctx, dbgen.DeleteShadowedBusinessEmailsParams{
			OldEmail: oldEmail,
			NewEmail: newEmail,
		}); err != nil {
			return fmt.Errorf("verify: drop shadowed addresses: %w", err)
		}
		if _, err := q.RetargetBusinessEmails(ctx, dbgen.RetargetBusinessEmailsParams{
			OldEmail: oldEmail,
			NewEmail: newEmail,
		}); err != nil {
			return fmt.Errorf("verify: retarget addresses: %w", err)
		}
		return nil
	})
	if err != nil {
		return Detail{}, apperr.Internal(err)
	}

	for _, businessID := range affected {
		if err := s.ingestor.RepickPrimary(ctx, businessID); err != nil {
			s.log.Warn("could not re-pick the primary address after a correction",
				"business_id", businessID, "error", err)
		}
	}

	// The old row keeps its history but loses the suggestion, so the correction is
	// not offered twice.
	if err := s.store.ClearTypoSuggestion(ctx, id); err != nil {
		s.log.Warn("could not clear the typo suggestion", "verification_id", id, "error", err)
	}

	corrected, err := s.store.UpsertEmailVerification(ctx, dbgen.UpsertEmailVerificationParams{
		ID:     ids.New(),
		Email:  newEmail,
		Domain: domainOf(newEmail),
	})
	if err != nil {
		return Detail{}, apperr.Internal(fmt.Errorf("verify: create corrected row: %w", err))
	}

	if _, err := s.VerifyOne(ctx, corrected.ID, PassSelf); err != nil {
		s.log.Warn("could not queue verification for the corrected address",
			"verification_id", corrected.ID, "error", err)
	}
	return s.Get(ctx, corrected.ID)
}

func domainOf(email string) string {
	_, domain, ok := SplitAddress(email)
	if !ok {
		return ""
	}
	return normalizeDomain(domain)
}

/* --------------------------------------------------------------- validation */

func validatePass(pass Pass) error {
	if pass != PassSelf && pass != PassThirdParty {
		return apperr.Validation("the run is invalid",
			apperr.FieldError{Field: "pass", Message: `must be "self" or "third_party"`})
	}
	return nil
}

// MaxSelectionIDs bounds an explicit selection, matching the bulk limits elsewhere.
const MaxSelectionIDs = 5000

func validateFilter(filter RunFilter) error {
	var fields []apperr.FieldError

	switch filter.Scope {
	case ScopeAll:
	case ScopeSelection:
		if len(filter.IDs) == 0 && len(filter.BusinessIDs) == 0 {
			fields = append(fields, apperr.FieldError{
				Field: "filter.ids", Message: "a selection needs at least one id",
			})
		}
	default:
		fields = append(fields, apperr.FieldError{
			Field: "filter.scope", Message: `must be "all" or "selection"`,
		})
	}
	if len(filter.IDs) > MaxSelectionIDs {
		fields = append(fields, apperr.FieldError{
			Field:   "filter.ids",
			Message: fmt.Sprintf("at most %d ids are allowed", MaxSelectionIDs),
		})
	}
	if len(filter.BusinessIDs) > MaxSelectionIDs {
		fields = append(fields, apperr.FieldError{
			Field:   "filter.business_ids",
			Message: fmt.Sprintf("at most %d ids are allowed", MaxSelectionIDs),
		})
	}
	for _, tag := range filter.Tags {
		if !ValidTag(tag) {
			fields = append(fields, apperr.FieldError{
				Field:   "filter.tags",
				Message: fmt.Sprintf("%q is not a tag; use one of %s", tag, strings.Join(tagValues(), ", ")),
			})
		}
	}
	if filter.MinScore != nil && (*filter.MinScore < 0 || *filter.MinScore > 100) {
		fields = append(fields, apperr.FieldError{
			Field: "filter.min_score", Message: "must be between 0 and 100",
		})
	}
	if filter.StaleAfterDays != nil && *filter.StaleAfterDays < 0 {
		fields = append(fields, apperr.FieldError{
			Field: "filter.stale_after_days", Message: "must not be negative",
		})
	}
	if len(fields) > 0 {
		return apperr.Validation("the run filter is invalid", fields...)
	}
	return nil
}

func tagValues() []string {
	out := make([]string, 0, len(Tags))
	for _, tag := range Tags {
		out = append(out, string(tag))
	}
	return out
}

// SetQueue closes the loop between the service and the River client, which cannot be
// constructed until the workers exist and the workers need the client.
func (s *Service) SetQueue(queue Enqueuer) { s.queue = queue }
