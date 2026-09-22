package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/assign"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// CampaignInput creates or updates a campaign. Nil pointers on update mean "keep".
type CampaignInput struct {
	Name       *string
	Brief      map[string]any
	Schedule   map[string]any
	Settings   map[string]any
	Steps      *int
	StepDelays []int
}

// CampaignDetail is a campaign with everything its detail page shows.
type CampaignDetail struct {
	Campaign        db.CampaignRow
	SendingAccounts []dbgen.SendingAccount
	Variants        []dbgen.ListCampaignVariantsRow
	LeadCounts      map[string]int64
	StageCounts     map[string]int64
	Checklist       Checklist
}

// ChecklistItem is one launch precondition.
type ChecklistItem struct {
	Key      string
	OK       bool
	Blocking bool
	Message  string
}

// Checklist is the launch gate.
type Checklist struct {
	Ready bool
	Items []ChecklistItem
}

// VariantWeight attaches one variant to a campaign step with a weight.
type VariantWeight struct {
	VariantID uuid.UUID
	Step      int
	Weight    int
	Status    string
}

/* ----------------------------------------------------------------- CRUD */

// CreateCampaign creates a draft.
func (s *Service) CreateCampaign(ctx context.Context, in CampaignInput) (db.CampaignRow, error) {
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		return db.CampaignRow{}, apperr.Validation("campaign is invalid", apperr.FieldError{Field: "name", Message: "is required"})
	}
	steps := 1
	if in.Steps != nil {
		steps = *in.Steps
	}
	if err := validateSteps(steps, in.StepDelays); err != nil {
		return db.CampaignRow{}, err
	}
	row, err := s.store.CreateCampaign(ctx, dbgen.CreateCampaignParams{
		ID: ids.New(), Name: strings.TrimSpace(*in.Name),
		Brief: encodeMap(in.Brief), Schedule: encodeMap(in.Schedule), Settings: encodeMap(in.Settings),
		Steps: campaign.Int32(steps), StepDelays: encodeInts(in.StepDelays),
	})
	if err != nil {
		return db.CampaignRow{}, apperr.Internal(err)
	}
	return s.campaignRow(ctx, row.ID)
}

// GetCampaign returns one campaign with its counters.
func (s *Service) GetCampaign(ctx context.Context, id uuid.UUID) (db.CampaignRow, error) {
	return s.campaignRow(ctx, id)
}

func (s *Service) campaignRow(ctx context.Context, id uuid.UUID) (db.CampaignRow, error) {
	row, err := s.store.GetCampaignRow(ctx, id)
	if err != nil {
		return db.CampaignRow{}, notFound("campaign", err)
	}
	return row, nil
}

// ListCampaigns returns one page.
func (s *Service) ListCampaigns(ctx context.Context, f db.CampaignFilter, sort string, page, perPage int) (Page[db.CampaignRow], error) {
	rows, err := s.store.ListCampaigns(ctx, f, sort, perPage, (page-1)*perPage)
	if err != nil {
		return Page[db.CampaignRow]{}, apperr.Internal(err)
	}
	total, err := s.store.CountCampaigns(ctx, f)
	if err != nil {
		return Page[db.CampaignRow]{}, apperr.Internal(err)
	}
	return Page[db.CampaignRow]{Rows: rows, Total: total}, nil
}

// UpdateCampaign edits a campaign. Settings changes on a launched campaign are
// pushed to Instantly; the content shape (steps) is frozen after launch.
func (s *Service) UpdateCampaign(ctx context.Context, id uuid.UUID, in CampaignInput) (db.CampaignRow, error) {
	current, err := s.campaign(ctx, id)
	if err != nil {
		return db.CampaignRow{}, err
	}
	if current.Status == campaign.CampaignArchived {
		return db.CampaignRow{}, apperr.Conflict("an archived campaign cannot be edited")
	}
	launched := current.InstantlyCampaignID != nil
	if launched && (in.Steps != nil && int(current.Steps) != *in.Steps) {
		return db.CampaignRow{}, apperr.Conflict("the number of steps is fixed once the campaign has been launched")
	}
	params := dbgen.UpdateCampaignParams{ID: id}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || len(name) > 200 {
			return db.CampaignRow{}, apperr.Validation("campaign is invalid", apperr.FieldError{Field: "name", Message: "must be between 1 and 200 characters"})
		}
		params.Name = &name
	}
	if in.Brief != nil {
		params.Brief = encodeMap(in.Brief)
	}
	if in.Schedule != nil {
		params.Schedule = encodeMap(in.Schedule)
	}
	if in.Settings != nil {
		params.Settings = encodeMap(in.Settings)
	}
	if in.Steps != nil {
		if err := validateSteps(*in.Steps, in.StepDelays); err != nil {
			return db.CampaignRow{}, err
		}
		params.Steps = campaign.Ptr(campaign.Int32(*in.Steps))
	}
	if in.StepDelays != nil {
		params.StepDelays = encodeInts(in.StepDelays)
	}
	updated, err := s.store.UpdateCampaign(ctx, params)
	if err != nil {
		return db.CampaignRow{}, apperr.Internal(err)
	}
	if launched && (in.Name != nil || in.Schedule != nil || in.Settings != nil) {
		if err := s.pushCampaignSettings(ctx, updated); err != nil {
			s.log.Warn("campaign settings were saved locally but not pushed to Instantly", "campaign_id", id, "error", err)
			_ = s.store.SetCampaignSyncError(ctx, dbgen.SetCampaignSyncErrorParams{ID: id, Error: campaign.Ptr(err.Error())})
		}
	}
	return s.campaignRow(ctx, id)
}

func (s *Service) pushCampaignSettings(ctx context.Context, camp dbgen.Campaign) error {
	client, err := s.Instantly(ctx)
	if err != nil {
		return err
	}
	in := UpdateInputFor(camp)
	_, err = client.UpdateCampaign(ctx, *camp.InstantlyCampaignID, in)
	return err
}

// ArchiveCampaign soft-deletes: paused at Instantly if running, jobs cancelled.
func (s *Service) ArchiveCampaign(ctx context.Context, id uuid.UUID) (db.CampaignRow, error) {
	current, err := s.campaign(ctx, id)
	if err != nil {
		return db.CampaignRow{}, err
	}
	if current.Status == campaign.CampaignActive && current.InstantlyCampaignID != nil {
		if client, err := s.Instantly(ctx); err == nil {
			if err := client.PauseCampaign(ctx, *current.InstantlyCampaignID); err != nil && !errors.Is(err, provider.ErrNotFound) {
				s.log.Warn("could not pause the Instantly campaign while archiving", "campaign_id", id, "error", err)
			}
		}
	}
	if _, err := s.store.ArchiveCampaign(ctx, id); err != nil {
		return db.CampaignRow{}, notFound("campaign", err)
	}
	s.cancelJobs(ctx, id)
	return s.campaignRow(ctx, id)
}

/* -------------------------------------------------------------- detail */

// GetCampaignDetail loads everything the detail page needs.
func (s *Service) GetCampaignDetail(ctx context.Context, id uuid.UUID) (CampaignDetail, error) {
	row, err := s.campaignRow(ctx, id)
	if err != nil {
		return CampaignDetail{}, err
	}
	accounts, err := s.store.ListCampaignSendingAccounts(ctx, id)
	if err != nil {
		return CampaignDetail{}, apperr.Internal(err)
	}
	variants, err := s.store.ListCampaignVariants(ctx, id)
	if err != nil {
		return CampaignDetail{}, apperr.Internal(err)
	}
	leadCounts, err := s.store.CountCampaignLeadsByStatus(ctx, id)
	if err != nil {
		return CampaignDetail{}, apperr.Internal(err)
	}
	stageCounts, err := s.store.CountCampaignContactsByStage(ctx, id)
	if err != nil {
		return CampaignDetail{}, apperr.Internal(err)
	}
	detail := CampaignDetail{Campaign: row, SendingAccounts: accounts, Variants: variants,
		LeadCounts: map[string]int64{}, StageCounts: map[string]int64{}}
	for _, c := range leadCounts {
		detail.LeadCounts[c.Status] = c.Total
	}
	for _, c := range stageCounts {
		detail.StageCounts[c.LifecycleStage] = c.Total
	}
	detail.Checklist, err = s.checklist(ctx, row, accounts, variants)
	if err != nil {
		return CampaignDetail{}, err
	}
	return detail, nil
}

// GetChecklist evaluates the launch gate.
func (s *Service) GetChecklist(ctx context.Context, id uuid.UUID) (Checklist, error) {
	detail, err := s.GetCampaignDetail(ctx, id)
	if err != nil {
		return Checklist{}, err
	}
	return detail.Checklist, nil
}

func (s *Service) checklist(ctx context.Context, row db.CampaignRow, accounts []dbgen.SendingAccount,
	variants []dbgen.ListCampaignVariantsRow,
) (Checklist, error) {
	var items []ChecklistItem
	add := func(key string, ok, blocking bool, msg string) {
		items = append(items, ChecklistItem{Key: key, OK: ok, Blocking: blocking, Message: msg})
	}

	source, err := s.instantlySource(ctx)
	hasKey := err == nil && len(source.ApiKeyEnc) > 0 && source.Enabled
	add("instantly_key", hasKey, true, "An enabled Instantly source with an API key is required")

	settings, err := s.store.GetCampaignSettings(ctx)
	if err != nil {
		return Checklist{}, apperr.Internal(err)
	}
	add("public_base_url", s.cfg.PublicBaseURL != "", false, "KARVON_PUBLIC_BASE_URL is empty, so webhooks cannot be registered; reconciliation still keeps data correct")
	webhookOK := settings.InstantlyWebhookID != nil && (settings.InstantlyWebhookStatus == nil || *settings.InstantlyWebhookStatus >= 0)
	add("webhook_registered", webhookOK, false, "The Instantly webhook is not registered; events arrive only through reconciliation")

	add("sending_accounts", len(accounts) > 0, true, "Choose at least one sending account")

	steps := int(row.Steps)
	weightsOK, approvedOK := true, true
	for step := 1; step <= steps; step++ {
		total := 0
		count := 0
		for _, v := range variants {
			if int(v.Step) != step || v.AssignmentStatus != campaign.CampaignVariantActive {
				continue
			}
			count++
			total += int(v.Weight)
			if !campaign.ContentUsable(v.VariantStatus) {
				approvedOK = false
			}
		}
		if count == 0 || total != 100 {
			weightsOK = false
		}
	}
	add("variants_weights_100", weightsOK, true, "Every step needs active variants whose weights total 100")
	add("variants_approved", approvedOK, true, "Every attached variant must be approved")

	pending, err := s.store.CountPendingCampaignLeads(ctx, row.ID)
	if err != nil {
		return Checklist{}, apperr.Internal(err)
	}
	add("leads_pending", pending > 0 || row.LeadsPushed > 0, true, "Import at least one lead")

	_, schedErr := ScheduleFor(row.Schedule)
	add("schedule_valid", schedErr == nil, true, "The sending schedule is invalid: "+errText(schedErr))

	ready := true
	for _, it := range items {
		if it.Blocking && !it.OK {
			ready = false
		}
	}
	return Checklist{Ready: ready, Items: items}, nil
}

/* -------------------------------------------------------------- launch */

// LaunchCampaign marks a campaign ready and queues the launch job. It refuses with
// the failing checklist items when the gate is not met.
func (s *Service) LaunchCampaign(ctx context.Context, id uuid.UUID) (db.CampaignRow, error) {
	detail, err := s.GetCampaignDetail(ctx, id)
	if err != nil {
		return db.CampaignRow{}, err
	}
	switch detail.Campaign.Status {
	case campaign.CampaignDraft, campaign.CampaignReady, campaign.CampaignFailed:
	default:
		return db.CampaignRow{}, apperr.Conflict("a %s campaign cannot be launched", detail.Campaign.Status)
	}
	if !detail.Checklist.Ready {
		var fields []apperr.FieldError
		for _, it := range detail.Checklist.Items {
			if it.Blocking && !it.OK {
				fields = append(fields, apperr.FieldError{Field: it.Key, Message: it.Message})
			}
		}
		return db.CampaignRow{}, &apperr.Error{Code: apperr.CodeConflict, Status: 409,
			Message: "the campaign is not ready to launch", Fields: fields}
	}
	err = s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if _, err := q.MarkCampaignReady(ctx, id); err != nil {
			return fmt.Errorf("mark ready: %w", err)
		}
		return s.enqueueTx(ctx, tx, campaign.LaunchArgs{CampaignID: id})
	})
	if err != nil {
		return db.CampaignRow{}, apperr.Internal(err)
	}
	return s.campaignRow(ctx, id)
}

// PauseCampaign pauses at Instantly and locally.
func (s *Service) PauseCampaign(ctx context.Context, id uuid.UUID) (db.CampaignRow, error) {
	current, err := s.campaign(ctx, id)
	if err != nil {
		return db.CampaignRow{}, err
	}
	if current.Status != campaign.CampaignActive || current.InstantlyCampaignID == nil {
		return db.CampaignRow{}, apperr.Conflict("only an active campaign can be paused")
	}
	client, err := s.Instantly(ctx)
	if err != nil {
		return db.CampaignRow{}, err
	}
	if err := client.PauseCampaign(ctx, *current.InstantlyCampaignID); err != nil {
		return db.CampaignRow{}, providerErr("Instantly could not pause the campaign", err)
	}
	if _, err := s.store.MarkCampaignPaused(ctx, id); err != nil {
		return db.CampaignRow{}, notFound("campaign", err)
	}
	return s.campaignRow(ctx, id)
}

// ResumeCampaign reactivates at Instantly and locally.
func (s *Service) ResumeCampaign(ctx context.Context, id uuid.UUID) (db.CampaignRow, error) {
	current, err := s.campaign(ctx, id)
	if err != nil {
		return db.CampaignRow{}, err
	}
	if current.Status != campaign.CampaignPaused || current.InstantlyCampaignID == nil {
		return db.CampaignRow{}, apperr.Conflict("only a paused campaign can be resumed")
	}
	client, err := s.Instantly(ctx)
	if err != nil {
		return db.CampaignRow{}, err
	}
	if err := client.ActivateCampaign(ctx, *current.InstantlyCampaignID); err != nil {
		return db.CampaignRow{}, providerErr("Instantly could not resume the campaign", err)
	}
	if _, err := s.store.MarkCampaignActive(ctx, id); err != nil {
		return db.CampaignRow{}, notFound("campaign", err)
	}
	return s.campaignRow(ctx, id)
}

// SyncCampaign queues a reconciliation now.
func (s *Service) SyncCampaign(ctx context.Context, id uuid.UUID) error {
	current, err := s.campaign(ctx, id)
	if err != nil {
		return err
	}
	if current.InstantlyCampaignID == nil {
		return apperr.Conflict("the campaign has not been launched, there is nothing to sync")
	}
	// A fresh request id, so pressing "sync now" is never swallowed by a
	// reconcile that is already in flight with older data.
	return s.enqueue(ctx, campaign.SyncCampaignArgs{CampaignID: id, RequestID: ids.New()})
}

/* ---------------------------------------------------- accounts & variants */

// SetSendingAccounts replaces the sending accounts a campaign uses.
func (s *Service) SetSendingAccounts(ctx context.Context, id uuid.UUID, accountIDs []uuid.UUID) ([]dbgen.SendingAccount, error) {
	current, err := s.campaign(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.Status == campaign.CampaignArchived {
		return nil, apperr.Conflict("an archived campaign cannot be edited")
	}
	accounts, err := s.store.ListSendingAccountsByIDs(ctx, accountIDs)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	if len(accounts) != len(uniqueIDs(accountIDs)) {
		return nil, apperr.Validation("unknown sending account",
			apperr.FieldError{Field: "sending_account_ids", Message: "one or more ids do not exist; sync the accounts first"})
	}
	err = s.store.InTx(ctx, func(q *dbgen.Queries) error {
		if err := q.SetCampaignSendingAccounts(ctx, id); err != nil {
			return err
		}
		for _, a := range accounts {
			if err := q.AddCampaignSendingAccount(ctx, dbgen.AddCampaignSendingAccountParams{CampaignID: id, SendingAccountID: a.ID}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, apperr.Internal(err)
	}
	if current.InstantlyCampaignID != nil {
		if client, err := s.Instantly(ctx); err == nil {
			emails := make([]string, 0, len(accounts))
			for _, a := range accounts {
				emails = append(emails, a.Email)
			}
			if _, err := client.UpdateCampaign(ctx, *current.InstantlyCampaignID, UpdateCampaignInputEmailList(emails)); err != nil {
				s.log.Warn("sending accounts saved locally but not pushed to Instantly", "campaign_id", id, "error", err)
			}
		}
	}
	return s.store.ListCampaignSendingAccounts(ctx, id)
}

// ListCampaignVariants lists the attached variants with weights.
func (s *Service) ListCampaignVariants(ctx context.Context, id uuid.UUID) ([]dbgen.ListCampaignVariantsRow, error) {
	if _, err := s.campaign(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.store.ListCampaignVariants(ctx, id)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	return rows, nil
}

// SetCampaignVariants replaces the campaign's variant set and weights. Weights
// total 100 per step; every variant must be approved or active; a variant that
// already has locked assignments cannot be removed. The weights version is bumped
// so new leads are assigned under the new distribution while existing assignments
// stay exactly as they were.
func (s *Service) SetCampaignVariants(ctx context.Context, id uuid.UUID, items []VariantWeight) ([]dbgen.ListCampaignVariantsRow, error) {
	current, err := s.campaign(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.Status == campaign.CampaignArchived {
		return nil, apperr.Conflict("an archived campaign cannot be edited")
	}
	var fields []apperr.FieldError
	byStep := map[int][]int{}
	seen := map[uuid.UUID]bool{}
	variantIDs := make([]uuid.UUID, 0, len(items))
	for i, it := range items {
		field := fmt.Sprintf("items[%d]", i)
		if it.Step < 1 || it.Step > int(current.Steps) {
			fields = append(fields, apperr.FieldError{Field: field + ".step", Message: fmt.Sprintf("must be between 1 and %d", current.Steps)})
		}
		if it.Weight < 0 || it.Weight > 100 {
			fields = append(fields, apperr.FieldError{Field: field + ".weight", Message: "must be between 0 and 100"})
		}
		if it.Status != "" && it.Status != campaign.CampaignVariantActive && it.Status != campaign.CampaignVariantPaused {
			fields = append(fields, apperr.FieldError{Field: field + ".status", Message: "must be active or paused"})
		}
		if seen[it.VariantID] {
			fields = append(fields, apperr.FieldError{Field: field + ".variant_id", Message: "is listed twice"})
		}
		seen[it.VariantID] = true
		variantIDs = append(variantIDs, it.VariantID)
		if it.Status != campaign.CampaignVariantPaused {
			byStep[it.Step] = append(byStep[it.Step], it.Weight)
		}
	}
	for step := 1; step <= int(current.Steps); step++ {
		if err := assign.ValidateWeights(byStep[step]); err != nil {
			fields = append(fields, apperr.FieldError{Field: fmt.Sprintf("step_%d", step), Message: err.Error()})
		}
	}
	if len(fields) > 0 {
		return nil, apperr.Validation("variant weights are invalid", fields...)
	}
	variants, err := s.store.ListEmailVariantsByIDs(ctx, variantIDs)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	if len(variants) != len(variantIDs) {
		return nil, apperr.Validation("variant weights are invalid", apperr.FieldError{Field: "items", Message: "one or more variants do not exist"})
	}
	stepOf := map[uuid.UUID]int32{}
	for _, v := range variants {
		if !campaign.ContentUsable(v.Status) {
			return nil, apperr.Validation("variant weights are invalid",
				apperr.FieldError{Field: "items", Message: fmt.Sprintf("variant %q is %s; approve it first", v.Name, v.Status)})
		}
		stepOf[v.ID] = v.Step
	}
	// A variant with locked assignments must stay attached (weight 0 is fine).
	existing, err := s.store.ListCampaignVariants(ctx, id)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	for _, ex := range existing {
		if seen[ex.VariantID] {
			continue
		}
		locked, err := s.store.CountLockedAssignmentsForCampaignVariant(ctx, dbgen.CountLockedAssignmentsForCampaignVariantParams{CampaignID: id, VariantID: ex.VariantID})
		if err != nil {
			return nil, apperr.Internal(err)
		}
		if locked > 0 {
			return nil, apperr.Conflict("variant %q has already been sent to %d leads and cannot be detached; set its weight to 0 instead", ex.Name, locked)
		}
	}
	err = s.store.InTx(ctx, func(q *dbgen.Queries) error {
		if err := q.ClearCampaignVariants(ctx, id); err != nil {
			return err
		}
		for _, it := range items {
			status := it.Status
			if status == "" {
				status = campaign.CampaignVariantActive
			}
			if err := q.AddCampaignVariant(ctx, dbgen.AddCampaignVariantParams{
				CampaignID: id, VariantID: it.VariantID, Step: campaign.Int32(it.Step), Weight: campaign.Int32(it.Weight), Status: status,
			}); err != nil {
				return err
			}
		}
		_, err := q.BumpCampaignWeightsVersion(ctx, id)
		return err
	})
	if err != nil {
		return nil, apperr.Internal(err)
	}
	return s.store.ListCampaignVariants(ctx, id)
}

/* -------------------------------------------------------------- helpers */

func validateSteps(steps int, delays []int) error {
	if steps < 1 || steps > campaign.MaxSteps {
		return apperr.Validation("campaign is invalid", apperr.FieldError{Field: "steps", Message: fmt.Sprintf("must be between 1 and %d", campaign.MaxSteps)})
	}
	for i, d := range delays {
		if d < 0 || d > 90 {
			return apperr.Validation("campaign is invalid", apperr.FieldError{Field: fmt.Sprintf("step_delays[%d]", i), Message: "must be between 0 and 90 days"})
		}
	}
	return nil
}

func encodeMap(m map[string]any) []byte {
	if m == nil {
		m = map[string]any{}
	}
	raw, _ := json.Marshal(m)
	return raw
}

func encodeInts(v []int) []byte {
	if v == nil {
		v = []int{}
	}
	raw, _ := json.Marshal(v)
	return raw
}

func uniqueIDs(ids []uuid.UUID) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	out := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// providerErr maps a provider failure onto the API envelope.
func providerErr(msg string, err error) error {
	var appErr *apperr.Error
	if errors.As(err, &appErr) {
		return err
	}
	switch {
	case errors.Is(err, provider.ErrAuth):
		return apperr.ProviderAuth("%s: the provider rejected the API key", msg).WithCause(err)
	case errors.Is(err, provider.ErrNotConfigured):
		return apperr.ProviderAuth("%s: no API key is configured", msg).WithCause(err)
	case errors.Is(err, provider.ErrPaymentRequired):
		return apperr.ProviderError("%s: the provider workspace has no active plan", msg).WithCause(err)
	case errors.Is(err, provider.ErrNotFound):
		return apperr.ProviderError("%s: the provider no longer has this resource", msg).WithCause(err)
	case errors.Is(err, provider.ErrRateLimited):
		return apperr.ProviderError("%s: the provider is rate limiting; try again shortly", msg).WithCause(err)
	default:
		return apperr.ProviderError("%s", msg).WithCause(err)
	}
}
