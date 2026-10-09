package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

/* ------------------------------------------------------- instantly cleanup */

// CleanupFilter decides which pushed leads an Instantly cleanup may delete. The
// preview, the run's selection and the worker's batches all read it through the
// same WHERE clause, so the number previewed is the number removed.
type CleanupFilter struct {
	// Scope is "finished" (Instantly will not email the lead again) or "emailed"
	// (emailed at least once, follow-ups or not).
	Scope string
	// IdleBefore keeps leads whose last send or reply is newer than this; a reply
	// to a recent email still lands on a lead Instantly knows.
	IdleBefore time.Time
	// IncludeReplied also takes leads that replied. A lead marked interested (or
	// further along) is always kept: it is pipeline, not a spent slot.
	IncludeReplied bool
	// CampaignIDs narrows to these campaigns; empty means every Karvon campaign.
	CampaignIDs []uuid.UUID
	// SkipRunID leaves out leads this run already tried and could not delete.
	SkipRunID uuid.NullUUID
}

// CleanupCandidate is one lead a cleanup would delete from Instantly.
type CleanupCandidate struct {
	ID              uuid.UUID
	CampaignID      uuid.UUID
	ContactID       uuid.UUID
	Status          string
	InstantlyLeadID string
}

// CleanupCampaignCount is how many leads one campaign holds at Instantly and how
// many of them a cleanup would delete.
type CleanupCampaignCount struct {
	CampaignID   uuid.UUID
	Name         string
	Status       string
	InInstantly  int64
	Eligible     int64
	LastActivity *time.Time
}

// Only campaigns launched from Karvon: an imported campaign's leads were never
// mirrored here, so deleting them would erase the only record of the contact.
const cleanupFrom = ` FROM campaign_leads cl JOIN campaigns c ON c.id = cl.campaign_id`

func buildCleanupWhere(f CleanupFilter, a *argSet) string {
	conds := []string{
		"c.source = 'karvon'",
		"c.instantly_campaign_id IS NOT NULL",
		"cl.instantly_lead_id IS NOT NULL",
		"cl.provider_removed_at IS NULL",
		// Emailed at least once, by either record of it.
		"(cl.last_contacted_at IS NOT NULL OR EXISTS (SELECT 1 FROM email_sends s WHERE s.campaign_lead_id = cl.id))",
		// Never a lead someone marked interested, booked or won.
		"(cl.interest_status IS NULL OR cl.interest_status NOT IN (1, 2, 3, 4))",
		// Quiet for long enough: GREATEST skips the NULLs.
		"COALESCE(GREATEST(cl.last_contacted_at, cl.last_replied_at, " +
			"(SELECT max(s.sent_at) FROM email_sends s WHERE s.campaign_lead_id = cl.id)), '-infinity') <= " + a.add(f.IdleBefore),
	}
	if f.Scope != "emailed" {
		// Instantly is done with it: its own status says completed, bounced,
		// unsubscribed or skipped, or Karvon stopped it, or the campaign ended.
		conds = append(conds, "(cl.status IN ('completed', 'replied', 'bounced', 'unsubscribed', 'suppressed', 'skipped', 'excluded')"+
			" OR cl.instantly_status IN (3, -1, -2, -3) OR c.status IN ('completed', 'archived'))")
	}
	if !f.IncludeReplied {
		conds = append(conds, "cl.reply_count = 0", "cl.last_replied_at IS NULL", "cl.status <> 'replied'")
	}
	if len(f.CampaignIDs) > 0 {
		conds = append(conds, "cl.campaign_id = ANY("+a.add(f.CampaignIDs)+")")
	}
	if f.SkipRunID.Valid {
		conds = append(conds, "cl.cleanup_run_id IS DISTINCT FROM "+a.add(f.SkipRunID.UUID))
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// CountCleanupCandidates counts the leads a cleanup filter would delete.
func (s *Store) CountCleanupCandidates(ctx context.Context, f CleanupFilter) (int64, error) {
	var a argSet
	var total int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*)"+cleanupFrom+buildCleanupWhere(f, &a), a.values()...).Scan(&total); err != nil {
		return 0, fmt.Errorf("db: count cleanup candidates: %w", err)
	}
	return total, nil
}

// ListCleanupCandidates returns up to limit leads a cleanup filter would delete,
// the longest-idle first.
func (s *Store) ListCleanupCandidates(ctx context.Context, f CleanupFilter, limit int) ([]CleanupCandidate, error) {
	var a argSet
	query := "SELECT cl.id, cl.campaign_id, cl.contact_id, cl.status, cl.instantly_lead_id" + cleanupFrom +
		buildCleanupWhere(f, &a) + " ORDER BY cl.last_contacted_at NULLS FIRST, cl.id LIMIT " + a.add(limit)
	rows, err := s.pool.Query(ctx, query, a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: list cleanup candidates: %w", err)
	}
	defer rows.Close()
	out := []CleanupCandidate{}
	for rows.Next() {
		var c CleanupCandidate
		if err := rows.Scan(&c.ID, &c.CampaignID, &c.ContactID, &c.Status, &c.InstantlyLeadID); err != nil {
			return nil, fmt.Errorf("db: scan cleanup candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountCleanupByCampaign reports, for every Karvon campaign that still has leads
// at Instantly, how many it holds and how many the filter would delete.
func (s *Store) CountCleanupByCampaign(ctx context.Context, f CleanupFilter) ([]CleanupCampaignCount, error) {
	var a argSet
	query := `SELECT c.id, c.name, c.status,
        (SELECT count(*) FROM campaign_leads x
          WHERE x.campaign_id = c.id AND x.instantly_lead_id IS NOT NULL AND x.provider_removed_at IS NULL)::bigint,
        (SELECT count(*)` + cleanupFrom + buildCleanupWhere(f, &a) + ` AND cl.campaign_id = c.id)::bigint AS eligible,
        (SELECT max(x.last_contacted_at) FROM campaign_leads x WHERE x.campaign_id = c.id)
    FROM campaigns c
    WHERE c.source = 'karvon' AND EXISTS (SELECT 1 FROM campaign_leads x
          WHERE x.campaign_id = c.id AND x.instantly_lead_id IS NOT NULL AND x.provider_removed_at IS NULL)
    ORDER BY eligible DESC, c.created_at`
	rows, err := s.pool.Query(ctx, query, a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: count cleanup by campaign: %w", err)
	}
	defer rows.Close()
	out := []CleanupCampaignCount{}
	for rows.Next() {
		var r CleanupCampaignCount
		if err := rows.Scan(&r.CampaignID, &r.Name, &r.Status, &r.InInstantly, &r.Eligible, &r.LastActivity); err != nil {
			return nil, fmt.Errorf("db: scan cleanup campaign count: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InstantlyUsage estimates how many of Instantly's "uploaded contacts" slots the
// workspace uses. Instantly has no endpoint for the figure, so it is assembled:
// the Karvon leads still at Instantly are counted exactly; a campaign started in
// Instantly contributes its lead count from the latest analytics sync.
type InstantlyUsage struct {
	// KarvonLeads are leads Karvon pushed that are still in an Instantly campaign.
	KarvonLeads int64
	// ImportedLeads are the leads of campaigns started in Instantly's own app.
	ImportedLeads int64
	// PendingLeads are leads waiting to be pushed by a campaign that is ready,
	// scheduled, launching or live: slots about to be taken.
	PendingLeads int64
}

// GetInstantlyUsage assembles the usage estimate.
func (s *Store) GetInstantlyUsage(ctx context.Context) (InstantlyUsage, error) {
	const query = `SELECT
        (SELECT count(*) FROM campaign_leads cl JOIN campaigns c ON c.id = cl.campaign_id
          WHERE c.source = 'karvon' AND cl.instantly_lead_id IS NOT NULL AND cl.provider_removed_at IS NULL)::bigint,
        (SELECT COALESCE(sum(c.leads_total), 0) FROM campaigns c
          WHERE c.source = 'instantly' AND c.instantly_campaign_id IS NOT NULL)::bigint,
        (SELECT count(*) FROM campaign_leads cl JOIN campaigns c ON c.id = cl.campaign_id
          JOIN contacts ct ON ct.id = cl.contact_id
          WHERE c.status IN ('ready', 'scheduled', 'launching', 'active', 'paused')
            AND cl.status IN ('pending', 'pushing') AND ct.suppressed_at IS NULL)::bigint`
	var u InstantlyUsage
	if err := s.pool.QueryRow(ctx, query).Scan(&u.KarvonLeads, &u.ImportedLeads, &u.PendingLeads); err != nil {
		return InstantlyUsage{}, fmt.Errorf("db: instantly usage: %w", err)
	}
	return u, nil
}

// ContactedEmails returns which of the given addresses belong to a contact that
// has been emailed by any campaign, so an import can keep them out. A lead that
// was pushed but never sent to does not count.
func (s *Store) ContactedEmails(ctx context.Context, emails []string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if len(emails) == 0 {
		return out, nil
	}
	const query = `SELECT c.email::text FROM contacts c
    WHERE c.email = ANY($1::citext[])
      AND EXISTS (SELECT 1 FROM campaign_leads cl WHERE cl.contact_id = c.id
                  AND (cl.last_contacted_at IS NOT NULL
                       OR EXISTS (SELECT 1 FROM email_sends s WHERE s.campaign_lead_id = cl.id)))`
	rows, err := s.pool.Query(ctx, query, emails)
	if err != nil {
		return nil, fmt.Errorf("db: contacted emails: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, fmt.Errorf("db: scan contacted email: %w", err)
		}
		out[strings.ToLower(email)] = struct{}{}
	}
	return out, rows.Err()
}
