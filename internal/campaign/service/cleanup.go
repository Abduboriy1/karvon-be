package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// Instantly cleanup: Instantly's plans cap the contacts sitting in campaigns, and
// deleting a lead from its campaign frees the slot. A cleanup deletes the leads
// Instantly has finished with; Karvon keeps each one — row, sends, timeline — and
// stamps it provider_removed_at, so the record of who was emailed survives and an
// import can keep those addresses out of the next campaign.

// uniqueViolation is the Postgres SQLSTATE for a duplicate key.
const uniqueViolation = "23505"

// CleanupPolicy is what a cleanup deletes.
type CleanupPolicy struct {
	Scope          string
	MinIdleDays    int
	IncludeReplied bool
}

// CleanupRequest starts a cleanup run.
type CleanupRequest struct {
	CleanupPolicy
	CampaignIDs []uuid.UUID
	// MaxLeads caps the run; nil deletes everything that matches.
	MaxLeads *int
}

// CleanupSettings is the saved policy, the automatic switch and the plan's cap.
type CleanupSettings struct {
	CleanupPolicy
	AutoEnabled bool
	// ContactLimit is the Instantly plan's uploaded-contacts cap; nil when unknown.
	ContactLimit *int
}

// InstantlyCapacity is the estimated use of Instantly's uploaded-contacts cap.
type InstantlyCapacity struct {
	Limit         *int
	KarvonLeads   int64
	ImportedLeads int64
	PendingLeads  int64
	// InUse is KarvonLeads + ImportedLeads.
	InUse int64
	// Available is Limit - InUse, nil when the limit is unknown. It can be
	// negative when the estimate runs over.
	Available *int64
}

// CleanupPreview is what a cleanup with a given request would delete.
type CleanupPreview struct {
	Eligible  int64
	Campaigns []db.CleanupCampaignCount
	Capacity  InstantlyCapacity
}

// CleanupOverview is the cleanup page: settings, capacity, and the runs in view.
type CleanupOverview struct {
	Settings  CleanupSettings
	Capacity  InstantlyCapacity
	ActiveRun *dbgen.InstantlyCleanupRun
	LastRun   *dbgen.InstantlyCleanupRun
	// EligibleNow is what the saved policy would delete right now.
	EligibleNow int64
}

// CleanupFilterFor turns a policy into the store's filter, measured from asOf.
func CleanupFilterFor(p CleanupPolicy, campaignIDs []uuid.UUID, asOf time.Time) db.CleanupFilter {
	return db.CleanupFilter{
		Scope:          p.Scope,
		IdleBefore:     asOf.Add(-time.Duration(p.MinIdleDays) * 24 * time.Hour),
		IncludeReplied: p.IncludeReplied,
		CampaignIDs:    campaignIDs,
	}
}

// CleanupFilterForRun rebuilds a run's filter. It is measured from the run's
// creation, so every batch of the run selects against the same cut-off.
func CleanupFilterForRun(run dbgen.InstantlyCleanupRun) db.CleanupFilter {
	f := CleanupFilterFor(CleanupPolicy{Scope: run.Scope, MinIdleDays: int(run.MinIdleDays), IncludeReplied: run.IncludeReplied},
		run.CampaignIds, run.CreatedAt)
	f.SkipRunID = uuid.NullUUID{UUID: run.ID, Valid: true}
	return f
}

func validatePolicy(p CleanupPolicy) []apperr.FieldError {
	var fields []apperr.FieldError
	if !slices.Contains(campaign.CleanupScopes, p.Scope) {
		fields = append(fields, apperr.FieldError{Field: "scope", Message: "must be finished or emailed"})
	}
	if p.MinIdleDays < 0 || p.MinIdleDays > 90 {
		fields = append(fields, apperr.FieldError{Field: "min_idle_days", Message: "must be between 0 and 90"})
	}
	return fields
}

/* ------------------------------------------------------------- settings */

func settingsFrom(row dbgen.CampaignSetting) CleanupSettings {
	out := CleanupSettings{
		CleanupPolicy: CleanupPolicy{Scope: row.CleanupScope, MinIdleDays: int(row.CleanupMinIdleDays), IncludeReplied: row.CleanupIncludeReplied},
		AutoEnabled:   row.CleanupAutoEnabled,
	}
	if row.InstantlyContactLimit != nil {
		limit := int(*row.InstantlyContactLimit)
		out.ContactLimit = &limit
	}
	return out
}

// GetCleanupSettings returns the saved cleanup settings.
func (s *Service) GetCleanupSettings(ctx context.Context) (CleanupSettings, error) {
	row, err := s.store.GetCampaignSettings(ctx)
	if err != nil {
		return CleanupSettings{}, apperr.Internal(err)
	}
	return settingsFrom(row), nil
}

// UpdateCleanupSettings replaces the saved cleanup settings.
func (s *Service) UpdateCleanupSettings(ctx context.Context, in CleanupSettings) (CleanupSettings, error) {
	fields := validatePolicy(in.CleanupPolicy)
	if in.ContactLimit != nil && (*in.ContactLimit < 1 || *in.ContactLimit > 10_000_000) {
		fields = append(fields, apperr.FieldError{Field: "contact_limit", Message: "must be between 1 and 10000000"})
	}
	if len(fields) > 0 {
		return CleanupSettings{}, apperr.Validation("cleanup settings are invalid", fields...)
	}
	var limit *int32
	if in.ContactLimit != nil {
		limit = campaign.Ptr(campaign.Int32(*in.ContactLimit))
	}
	row, err := s.store.SetCleanupSettings(ctx, dbgen.SetCleanupSettingsParams{
		ContactLimit: limit, AutoEnabled: in.AutoEnabled, Scope: in.Scope,
		MinIdleDays: campaign.Int32(in.MinIdleDays), IncludeReplied: in.IncludeReplied,
	})
	if err != nil {
		return CleanupSettings{}, apperr.Internal(err)
	}
	return settingsFrom(row), nil
}

/* ------------------------------------------------------------- capacity */

func (s *Service) capacity(ctx context.Context, limit *int) (InstantlyCapacity, error) {
	usage, err := s.store.GetInstantlyUsage(ctx)
	if err != nil {
		return InstantlyCapacity{}, apperr.Internal(err)
	}
	out := InstantlyCapacity{
		Limit: limit, KarvonLeads: usage.KarvonLeads, ImportedLeads: usage.ImportedLeads,
		PendingLeads: usage.PendingLeads, InUse: usage.KarvonLeads + usage.ImportedLeads,
	}
	if limit != nil {
		available := int64(*limit) - out.InUse
		out.Available = &available
	}
	return out, nil
}

// GetCleanupOverview assembles the cleanup page.
func (s *Service) GetCleanupOverview(ctx context.Context) (CleanupOverview, error) {
	settings, err := s.GetCleanupSettings(ctx)
	if err != nil {
		return CleanupOverview{}, err
	}
	capacity, err := s.capacity(ctx, settings.ContactLimit)
	if err != nil {
		return CleanupOverview{}, err
	}
	eligible, err := s.store.CountCleanupCandidates(ctx, CleanupFilterFor(settings.CleanupPolicy, nil, s.now()))
	if err != nil {
		return CleanupOverview{}, apperr.Internal(err)
	}
	out := CleanupOverview{Settings: settings, Capacity: capacity, EligibleNow: eligible}
	if run, err := s.store.GetActiveCleanupRun(ctx); err == nil {
		out.ActiveRun = &run
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return CleanupOverview{}, apperr.Internal(err)
	}
	if run, err := s.store.GetLatestCleanupRun(ctx); err == nil {
		out.LastRun = &run
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return CleanupOverview{}, apperr.Internal(err)
	}
	return out, nil
}

// PreviewCleanup reports what a cleanup with this request would delete, without
// deleting anything.
func (s *Service) PreviewCleanup(ctx context.Context, in CleanupRequest) (CleanupPreview, error) {
	if fields := validatePolicy(in.CleanupPolicy); len(fields) > 0 {
		return CleanupPreview{}, apperr.Validation("cleanup is invalid", fields...)
	}
	settings, err := s.GetCleanupSettings(ctx)
	if err != nil {
		return CleanupPreview{}, err
	}
	f := CleanupFilterFor(in.CleanupPolicy, in.CampaignIDs, s.now())
	eligible, err := s.store.CountCleanupCandidates(ctx, f)
	if err != nil {
		return CleanupPreview{}, apperr.Internal(err)
	}
	if in.MaxLeads != nil && int64(*in.MaxLeads) < eligible {
		eligible = int64(*in.MaxLeads)
	}
	campaigns, err := s.store.CountCleanupByCampaign(ctx, f)
	if err != nil {
		return CleanupPreview{}, apperr.Internal(err)
	}
	capacity, err := s.capacity(ctx, settings.ContactLimit)
	if err != nil {
		return CleanupPreview{}, err
	}
	return CleanupPreview{Eligible: eligible, Campaigns: campaigns, Capacity: capacity}, nil
}

/* ----------------------------------------------------------------- runs */

// ErrNothingToClean is returned when no lead matches a cleanup request.
var ErrNothingToClean = apperr.Conflict("no lead at Instantly matches this cleanup; nothing to remove")

// ErrCleanupRunning is returned while another cleanup run is queued or running.
var ErrCleanupRunning = apperr.Conflict("an Instantly cleanup is already running; wait for it to finish")

// StartCleanup records a cleanup run and queues its first batch. The run deletes
// what matches when each batch runs, up to MaxLeads; selected is the count at
// the start, for the progress bar.
func (s *Service) StartCleanup(ctx context.Context, in CleanupRequest, trigger string) (dbgen.InstantlyCleanupRun, error) {
	fields := validatePolicy(in.CleanupPolicy)
	if in.MaxLeads != nil && (*in.MaxLeads < 1 || *in.MaxLeads > 1_000_000) {
		fields = append(fields, apperr.FieldError{Field: "max_leads", Message: "must be between 1 and 1000000"})
	}
	if len(fields) > 0 {
		return dbgen.InstantlyCleanupRun{}, apperr.Validation("cleanup is invalid", fields...)
	}
	if _, err := s.Instantly(ctx); err != nil {
		return dbgen.InstantlyCleanupRun{}, providerErr("Instantly cleanup cannot start", err)
	}
	if _, err := s.store.GetActiveCleanupRun(ctx); err == nil {
		return dbgen.InstantlyCleanupRun{}, ErrCleanupRunning
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return dbgen.InstantlyCleanupRun{}, apperr.Internal(err)
	}
	campaignIDs := uniqueIDs(in.CampaignIDs)
	if campaignIDs == nil {
		campaignIDs = []uuid.UUID{}
	}
	asOf := s.now()
	selected, err := s.store.CountCleanupCandidates(ctx, CleanupFilterFor(in.CleanupPolicy, campaignIDs, asOf))
	if err != nil {
		return dbgen.InstantlyCleanupRun{}, apperr.Internal(err)
	}
	var maxLeads *int32
	if in.MaxLeads != nil {
		maxLeads = campaign.Ptr(campaign.Int32(*in.MaxLeads))
		selected = min(selected, int64(*in.MaxLeads))
	}
	if selected == 0 {
		return dbgen.InstantlyCleanupRun{}, ErrNothingToClean
	}
	var run dbgen.InstantlyCleanupRun
	err = s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		var err error
		run, err = q.CreateCleanupRun(ctx, dbgen.CreateCleanupRunParams{
			ID: ids.New(), Trigger: trigger, Scope: in.Scope, MinIdleDays: campaign.Int32(in.MinIdleDays),
			IncludeReplied: in.IncludeReplied, CampaignIds: campaignIDs, MaxLeads: maxLeads, Selected: campaign.Int32(int(selected)),
			CreatedAt: asOf,
		})
		if err != nil {
			return err
		}
		return s.enqueueTx(ctx, tx, campaign.InstantlyCleanupArgs{RunID: run.ID})
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return dbgen.InstantlyCleanupRun{}, ErrCleanupRunning
	}
	if err != nil {
		return dbgen.InstantlyCleanupRun{}, apperr.Internal(fmt.Errorf("start cleanup: %w", err))
	}
	return run, nil
}

// GetCleanupRun returns one run.
func (s *Service) GetCleanupRun(ctx context.Context, id uuid.UUID) (dbgen.InstantlyCleanupRun, error) {
	run, err := s.store.GetCleanupRun(ctx, id)
	if err != nil {
		return dbgen.InstantlyCleanupRun{}, notFound("cleanup run", err)
	}
	return run, nil
}

// ListCleanupRuns returns one page of runs, newest first.
func (s *Service) ListCleanupRuns(ctx context.Context, page, perPage int) (Page[dbgen.InstantlyCleanupRun], error) {
	rows, err := s.store.ListCleanupRuns(ctx, dbgen.ListCleanupRunsParams{Lim: campaign.Int32(perPage), Off: campaign.Int32((page - 1) * perPage)})
	if err != nil {
		return Page[dbgen.InstantlyCleanupRun]{}, apperr.Internal(err)
	}
	total, err := s.store.CountCleanupRuns(ctx)
	if err != nil {
		return Page[dbgen.InstantlyCleanupRun]{}, apperr.Internal(err)
	}
	return Page[dbgen.InstantlyCleanupRun]{Rows: rows, Total: total}, nil
}

// StartAutoCleanup is the periodic pass: a run with the saved policy when
// automatic cleanup is on. Nothing to delete and a run already going are both
// quiet no-ops.
func (s *Service) StartAutoCleanup(ctx context.Context) (*dbgen.InstantlyCleanupRun, error) {
	settings, err := s.GetCleanupSettings(ctx)
	if err != nil || !settings.AutoEnabled {
		return nil, err
	}
	run, err := s.StartCleanup(ctx, CleanupRequest{CleanupPolicy: settings.CleanupPolicy}, campaign.CleanupTriggerAuto)
	if errors.Is(err, ErrNothingToClean) || errors.Is(err, ErrCleanupRunning) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}
