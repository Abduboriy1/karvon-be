package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/business"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// ImportResult reports what an import did.
type ImportResult struct {
	Matched           int64
	Imported          int
	SkippedSuppressed int
	// SkippedExcluded counts addresses a global exclusion keeps out.
	SkippedExcluded int
	SkippedExisting int
	SkippedInvalid  int
	Capped          bool
}

// LeadDetail is one lead with its assignments, sends and timeline.
type LeadDetail struct {
	Lead        db.LeadRow
	Contact     dbgen.Contact
	Assignments []dbgen.VariantAssignment
	Sends       []dbgen.EmailSend
	Events      []dbgen.ContactEvent
}

// EstimateImport counts what a filter would bring in, without importing. It runs
// the same acceptance ImportLeads runs — normalisation, suppression, addresses
// the campaign already holds, duplicates inside the batch — against the same
// capped candidate window, so "would be imported" is the number the import goes
// on to produce rather than a bare match count.
func (s *Service) EstimateImport(ctx context.Context, campaignID uuid.UUID, f db.ImportFilter) (ImportResult, error) {
	if _, err := s.campaign(ctx, campaignID); err != nil {
		return ImportResult{}, err
	}
	total, err := s.store.CountImportCandidates(ctx, f)
	if err != nil {
		return ImportResult{}, apperr.Internal(err)
	}
	result := ImportResult{Matched: total, Capped: total > int64(s.cfg.MaxImport)}
	if total == 0 {
		return result, nil
	}
	candidates, err := s.store.ListImportCandidates(ctx, f, s.cfg.MaxImport)
	if err != nil {
		return ImportResult{}, apperr.Internal(err)
	}

	accepted := make([]string, 0, len(candidates))
	for _, c := range candidates {
		email, ok := business.NormalizeEmail(c.Email)
		if !ok || !strings.Contains(email, "@") {
			result.SkippedInvalid++
			continue
		}
		accepted = append(accepted, email)
	}

	states, err := s.store.ClassifyImportEmails(ctx, campaignID, accepted)
	if err != nil {
		return ImportResult{}, apperr.Internal(err)
	}
	// A repeat inside the batch is counted the way the import counts it: the
	// first one lands, the second collides with the lead it just created.
	seen := make(map[string]struct{}, len(accepted))
	for _, email := range accepted {
		state := states[email]
		if state.Excluded {
			result.SkippedExcluded++
			continue
		}
		if state.Suppressed {
			result.SkippedSuppressed++
			continue
		}
		if _, duplicate := seen[email]; duplicate || state.Existing {
			result.SkippedExisting++
			continue
		}
		seen[email] = struct{}{}
		result.Imported++
	}
	return result, nil
}

// ImportLeads brings the businesses a filter matches into a campaign as contacts
// and campaign leads. A globally excluded address never becomes a contact or a
// lead, a suppressed contact is never imported, an address the campaign already
// has is skipped, and an address that fails basic acceptance is counted as invalid.
func (s *Service) ImportLeads(ctx context.Context, campaignID uuid.UUID, f db.ImportFilter) (ImportResult, error) {
	camp, err := s.campaign(ctx, campaignID)
	if err != nil {
		return ImportResult{}, err
	}
	switch camp.Status {
	case campaign.CampaignArchived, campaign.CampaignCompleted:
		return ImportResult{}, apperr.Conflict("a %s campaign cannot take new leads", camp.Status)
	}
	candidates, err := s.store.ListImportCandidates(ctx, f, s.cfg.MaxImport)
	if err != nil {
		return ImportResult{}, apperr.Internal(err)
	}
	result := ImportResult{Matched: int64(len(candidates)), Capped: len(candidates) >= s.cfg.MaxImport}
	if len(candidates) == 0 {
		return result, nil
	}
	emails := make([]string, 0, len(candidates))
	for _, c := range candidates {
		emails = append(emails, c.Email)
	}
	excluded, err := s.store.ExcludedEmails(ctx, emails)
	if err != nil {
		return ImportResult{}, apperr.Internal(err)
	}
	// Businesses are loaded once per import for names and locations.
	err = s.store.InTx(ctx, func(q *dbgen.Queries) error {
		for _, c := range candidates {
			email, ok := business.NormalizeEmail(c.Email)
			if !ok || !strings.Contains(email, "@") {
				result.SkippedInvalid++
				continue
			}
			if _, skip := excluded[email]; skip {
				result.SkippedExcluded++
				continue
			}
			domain := email[strings.LastIndex(email, "@")+1:]
			contact, err := q.UpsertContact(ctx, dbgen.UpsertContactParams{
				ID: ids.New(), Email: email, Domain: domain, Company: campaign.Optional(c.Business),
				Phone: c.Phone, Website: c.Website,
				BusinessID: uuid.NullUUID{UUID: c.BusinessID, Valid: true}, Source: campaign.ContactSourceBusinessImport,
			})
			if err != nil {
				return err
			}
			if contact.SuppressedAt != nil {
				result.SkippedSuppressed++
				continue
			}
			lead, err := q.InsertCampaignLead(ctx, dbgen.InsertCampaignLeadParams{
				ID: ids.New(), CampaignID: campaignID, ContactID: contact.ID,
				BusinessID: uuid.NullUUID{UUID: c.BusinessID, Valid: true},
			})
			if errors.Is(err, pgx.ErrNoRows) {
				result.SkippedExisting++
				continue
			}
			if err != nil {
				return err
			}
			result.Imported++
			if _, _, err := campaign.RecordEvent(ctx, q, campaign.EventInput{
				ContactID: contact.ID, CampaignID: &campaignID, CampaignLeadID: &lead.ID,
				OccurredAt: s.now(), Type: campaign.EventImported, Source: campaign.EventSourceImport,
				StageBefore: campaign.Stage(contact.LifecycleStage), StageAfter: campaign.Stage(contact.LifecycleStage),
				Data: map[string]any{"business_id": c.BusinessID, "business": c.Business, "verification_tag": campaign.Deref(c.Tag)},
			}); err != nil {
				return err
			}
		}
		_, err := q.RecomputeCampaignCounts(ctx, campaignID)
		return err
	})
	if err != nil {
		return ImportResult{}, apperr.Internal(err)
	}
	return result, nil
}

// ListLeads returns one page of a campaign's leads.
func (s *Service) ListLeads(ctx context.Context, f db.LeadFilter, sort string, page, perPage int) (Page[db.LeadRow], error) {
	if _, err := s.campaign(ctx, f.CampaignID); err != nil {
		return Page[db.LeadRow]{}, err
	}
	rows, err := s.store.ListCampaignLeadRows(ctx, f, sort, perPage, (page-1)*perPage)
	if err != nil {
		return Page[db.LeadRow]{}, apperr.Internal(err)
	}
	total, err := s.store.CountCampaignLeadRows(ctx, f)
	if err != nil {
		return Page[db.LeadRow]{}, apperr.Internal(err)
	}
	return Page[db.LeadRow]{Rows: rows, Total: total}, nil
}

// GetLead loads one lead in full.
func (s *Service) GetLead(ctx context.Context, campaignID, leadID uuid.UUID) (LeadDetail, error) {
	row, err := s.store.GetCampaignLeadRow(ctx, leadID)
	if err != nil || row.CampaignID != campaignID {
		if err == nil {
			err = pgx.ErrNoRows
		}
		return LeadDetail{}, notFound("lead", err)
	}
	contact, err := s.contact(ctx, row.ContactID)
	if err != nil {
		return LeadDetail{}, err
	}
	assignments, err := s.store.ListVariantAssignmentsForLead(ctx, leadID)
	if err != nil {
		return LeadDetail{}, apperr.Internal(err)
	}
	sends, err := s.store.ListEmailSendsForLead(ctx, leadID)
	if err != nil {
		return LeadDetail{}, apperr.Internal(err)
	}
	events, err := s.store.ListCampaignLeadEvents(ctx, uuid.NullUUID{UUID: leadID, Valid: true})
	if err != nil {
		return LeadDetail{}, apperr.Internal(err)
	}
	return LeadDetail{Lead: row, Contact: contact, Assignments: assignments, Sends: sends, Events: events}, nil
}

// RemoveLead takes a lead out of a campaign. A lead that was never pushed is
// deleted outright; a pushed lead is marked skipped and removed at Instantly.
// Suppression is a contact-level decision and lives elsewhere.
func (s *Service) RemoveLead(ctx context.Context, campaignID, leadID uuid.UUID) error {
	lead, err := s.store.GetCampaignLead(ctx, leadID)
	if err != nil || lead.CampaignID != campaignID {
		if err == nil {
			err = pgx.ErrNoRows
		}
		return notFound("lead", err)
	}
	if lead.PushedAt == nil {
		if _, err := s.store.DeleteCampaignLead(ctx, leadID); err != nil {
			return apperr.Internal(err)
		}
		_, _ = s.store.RecomputeCampaignCounts(ctx, campaignID)
		return nil
	}
	err = s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		if err := q.MarkCampaignLeadTerminal(ctx, dbgen.MarkCampaignLeadTerminalParams{ID: leadID, Status: campaign.LeadSkipped}); err != nil {
			return err
		}
		if _, _, err := campaign.RecordEvent(ctx, q, campaign.EventInput{
			ContactID: lead.ContactID, CampaignID: &campaignID, CampaignLeadID: &leadID,
			OccurredAt: s.now(), Type: campaign.EventSkipped, Source: campaign.EventSourceManual, Data: map[string]any{"reason": "removed by operator"},
		}); err != nil {
			return err
		}
		return s.enqueueTx(ctx, tx, campaign.RemoveLeadArgs{CampaignLeadID: leadID})
	})
	if err != nil {
		return apperr.Internal(err)
	}
	return nil
}

// ListCampaignActivity returns the timeline entries of one campaign.
func (s *Service) ListCampaignActivity(ctx context.Context, campaignID uuid.UUID, types []string, page, perPage int) (Page[dbgen.ContactEvent], error) {
	if _, err := s.campaign(ctx, campaignID); err != nil {
		return Page[dbgen.ContactEvent]{}, err
	}
	return s.ListActivity(ctx, ActivityFilter{CampaignID: &campaignID, Types: types}, page, perPage)
}

// ActivityFilter narrows the global activity feed.
type ActivityFilter struct {
	CampaignID *uuid.UUID
	ContactID  *uuid.UUID
	Types      []string
	Since      *pgTime
}

type pgTime = struct{ T *string }

// ListActivity returns one page of the global feed, newest first.
func (s *Service) ListActivity(ctx context.Context, f ActivityFilter, page, perPage int) (Page[dbgen.ContactEvent], error) {
	types := f.Types
	if types == nil {
		types = []string{}
	}
	rows, err := s.store.ListActivity(ctx, dbgen.ListActivityParams{
		CampaignID: campaign.NullUUID(f.CampaignID), ContactID: campaign.NullUUID(f.ContactID), Types: types,
		Lim: campaign.Int32(perPage), Off: campaign.Int32((page - 1) * perPage),
	})
	if err != nil {
		return Page[dbgen.ContactEvent]{}, apperr.Internal(err)
	}
	total, err := s.store.CountActivity(ctx, dbgen.CountActivityParams{
		CampaignID: campaign.NullUUID(f.CampaignID), ContactID: campaign.NullUUID(f.ContactID), Types: types,
	})
	if err != nil {
		return Page[dbgen.ContactEvent]{}, apperr.Internal(err)
	}
	return Page[dbgen.ContactEvent]{Rows: rows, Total: total}, nil
}
