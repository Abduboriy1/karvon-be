package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

/* -------------------------------------------------------------- campaigns */

// CampaignFilter is the filter behind GET /campaigns.
type CampaignFilter struct {
	Statuses []string
	Q        *string
	// IncludeArchived keeps archived campaigns; by default they are hidden.
	IncludeArchived bool
}

// CampaignRow is one campaign with the counters the list shows.
type CampaignRow struct {
	ID                    uuid.UUID
	Name                  string
	Status                string
	Brief                 []byte
	Schedule              []byte
	Settings              []byte
	Steps                 int32
	StepDelays            []byte
	WeightsVersion        int32
	InstantlyCampaignID   *string
	InstantlyStatus       *int32
	InstantlySendingState *string
	LaunchedAt            *time.Time
	PausedAt              *time.Time
	CompletedAt           *time.Time
	ArchivedAt            *time.Time
	LastSyncedAt          *time.Time
	LastSyncError         *string
	Error                 *string
	LeadsTotal            int32
	LeadsPushed           int32
	CreatedAt             time.Time
	UpdatedAt             time.Time

	Contacted      int64
	Replied        int64
	Interested     int64
	Bounced        int64
	Unsubscribed   int64
	SendsTotal     int64
	SendsBounced   int64
	Eligible       int64
	Subscribed     int64
	LastActivityAt *time.Time
}

var campaignSorts = map[string]sortSpec{
	"created_at:desc":  {expr: "c.created_at", desc: true},
	"created_at:asc":   {expr: "c.created_at"},
	"name:asc":         {expr: "c.name"},
	"name:desc":        {expr: "c.name", desc: true},
	"launched_at:desc": {expr: "c.launched_at", desc: true},
	"launched_at:asc":  {expr: "c.launched_at"},
	"leads_total:desc": {expr: "c.leads_total", desc: true},
	"leads_total:asc":  {expr: "c.leads_total"},
	"updated_at:desc":  {expr: "c.updated_at", desc: true},
	"updated_at:asc":   {expr: "c.updated_at"},
}

// CampaignSortKeys lists the accepted values of the `sort` parameter.
func CampaignSortKeys() []string { return sortKeys(campaignSorts) }

const campaignSelect = `
    c.id, c.name, c.status, c.brief, c.schedule, c.settings, c.steps, c.step_delays, c.weights_version,
    c.instantly_campaign_id, c.instantly_status, c.instantly_sending_status, c.launched_at, c.paused_at,
    c.completed_at, c.archived_at, c.last_synced_at, c.last_sync_error, c.error, c.leads_total, c.leads_pushed,
    c.created_at, c.updated_at,
    (SELECT count(*) FROM campaign_leads l WHERE l.campaign_id = c.id AND l.last_contacted_at IS NOT NULL)::bigint,
    (SELECT count(*) FROM campaign_leads l WHERE l.campaign_id = c.id AND l.reply_count > 0)::bigint,
    (SELECT count(*) FROM campaign_leads l WHERE l.campaign_id = c.id AND l.interest_status IN (1,2,3,4))::bigint,
    (SELECT count(*) FROM campaign_leads l WHERE l.campaign_id = c.id AND l.status = 'bounced')::bigint,
    (SELECT count(*) FROM campaign_leads l WHERE l.campaign_id = c.id AND l.status = 'unsubscribed')::bigint,
    (SELECT count(*) FROM email_sends s WHERE s.campaign_id = c.id)::bigint,
    (SELECT count(*) FROM email_sends s WHERE s.campaign_id = c.id AND s.bounced_at IS NOT NULL)::bigint,
    (SELECT count(*) FROM campaign_leads l JOIN contacts ct ON ct.id = l.contact_id
       WHERE l.campaign_id = c.id AND ct.lifecycle_stage IN ('newsletter_eligible','mailchimp_pending','mailchimp_subscribed'))::bigint,
    (SELECT count(*) FROM campaign_leads l JOIN contacts ct ON ct.id = l.contact_id
       WHERE l.campaign_id = c.id AND ct.lifecycle_stage = 'mailchimp_subscribed')::bigint,
    (SELECT max(e.occurred_at) FROM contact_events e WHERE e.campaign_id = c.id)`

func buildCampaignWhere(f CampaignFilter, a *argSet) string {
	conds := []string{"TRUE"}
	if len(f.Statuses) > 0 {
		conds = append(conds, "c.status = ANY("+a.add(f.Statuses)+")")
	} else if !f.IncludeArchived {
		conds = append(conds, "c.status <> 'archived'")
	}
	if f.Q != nil && strings.TrimSpace(*f.Q) != "" {
		conds = append(conds, "c.name ILIKE "+a.add("%"+strings.TrimSpace(*f.Q)+"%"))
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// ListCampaigns returns one page of campaigns.
func (s *Store) ListCampaigns(ctx context.Context, f CampaignFilter, sort string, limit, offset int) ([]CampaignRow, error) {
	var a argSet
	query := "SELECT " + campaignSelect + " FROM campaigns c" + buildCampaignWhere(f, &a) +
		lookupSort(campaignSorts, sort, "created_at:desc").orderBy("c.id") +
		" LIMIT " + a.add(limit) + " OFFSET " + a.add(offset)

	rows, err := s.pool.Query(ctx, query, a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: list campaigns: %w", err)
	}
	defer rows.Close()

	out := []CampaignRow{}
	for rows.Next() {
		var r CampaignRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Status, &r.Brief, &r.Schedule, &r.Settings, &r.Steps, &r.StepDelays,
			&r.WeightsVersion, &r.InstantlyCampaignID, &r.InstantlyStatus, &r.InstantlySendingState, &r.LaunchedAt,
			&r.PausedAt, &r.CompletedAt, &r.ArchivedAt, &r.LastSyncedAt, &r.LastSyncError, &r.Error, &r.LeadsTotal,
			&r.LeadsPushed, &r.CreatedAt, &r.UpdatedAt, &r.Contacted, &r.Replied, &r.Interested, &r.Bounced,
			&r.Unsubscribed, &r.SendsTotal, &r.SendsBounced, &r.Eligible, &r.Subscribed, &r.LastActivityAt); err != nil {
			return nil, fmt.Errorf("db: scan campaign: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetCampaignRow returns one campaign with its counters.
func (s *Store) GetCampaignRow(ctx context.Context, id uuid.UUID) (CampaignRow, error) {
	var a argSet
	query := "SELECT " + campaignSelect + " FROM campaigns c WHERE c.id = " + a.add(id)
	rows, err := s.pool.Query(ctx, query, a.values()...)
	if err != nil {
		return CampaignRow{}, fmt.Errorf("db: get campaign: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return CampaignRow{}, err
		}
		return CampaignRow{}, pgx.ErrNoRows
	}
	var r CampaignRow
	if err := rows.Scan(&r.ID, &r.Name, &r.Status, &r.Brief, &r.Schedule, &r.Settings, &r.Steps, &r.StepDelays,
		&r.WeightsVersion, &r.InstantlyCampaignID, &r.InstantlyStatus, &r.InstantlySendingState, &r.LaunchedAt,
		&r.PausedAt, &r.CompletedAt, &r.ArchivedAt, &r.LastSyncedAt, &r.LastSyncError, &r.Error, &r.LeadsTotal,
		&r.LeadsPushed, &r.CreatedAt, &r.UpdatedAt, &r.Contacted, &r.Replied, &r.Interested, &r.Bounced,
		&r.Unsubscribed, &r.SendsTotal, &r.SendsBounced, &r.Eligible, &r.Subscribed, &r.LastActivityAt); err != nil {
		return CampaignRow{}, fmt.Errorf("db: scan campaign: %w", err)
	}
	return r, nil
}

// CountCampaigns counts the campaigns a filter matches.
func (s *Store) CountCampaigns(ctx context.Context, f CampaignFilter) (int64, error) {
	var a argSet
	var total int64
	err := s.pool.QueryRow(ctx, "SELECT count(*) FROM campaigns c"+buildCampaignWhere(f, &a), a.values()...).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("db: count campaigns: %w", err)
	}
	return total, nil
}

/* --------------------------------------------------------- campaign leads */

// LeadFilter is the filter behind GET /campaigns/{id}/leads.
type LeadFilter struct {
	CampaignID uuid.UUID
	Statuses   []string
	Stages     []string
	Q          *string
}

// LeadRow is one campaign lead joined with its contact.
type LeadRow struct {
	ID              uuid.UUID
	CampaignID      uuid.UUID
	ContactID       uuid.UUID
	BusinessID      uuid.NullUUID
	Status          string
	InstantlyLeadID *string
	InstantlyStatus *int32
	InterestStatus  *int32
	InterestLabel   *string
	PushedAt        *time.Time
	PushAttempts    int32
	LastPushError   *string
	LastContactedAt *time.Time
	LastOpenedAt    *time.Time
	LastClickedAt   *time.Time
	LastRepliedAt   *time.Time
	OpenCount       int32
	ClickCount      int32
	ReplyCount      int32
	CreatedAt       time.Time
	UpdatedAt       time.Time

	Email          string
	FirstName      *string
	LastName       *string
	Company        *string
	Title          *string
	LifecycleStage string
	SuppressedAt   *time.Time
	VariantNames   []string
	SendsTotal     int64
	// Exclusion is the global exclusion rule covering the contact, or nil.
	Exclusion *ExclusionRef
}

var leadSorts = map[string]sortSpec{
	"created_at:desc":        {expr: "l.created_at", desc: true},
	"created_at:asc":         {expr: "l.created_at"},
	"email:asc":              {expr: "c.email"},
	"email:desc":             {expr: "c.email", desc: true},
	"last_contacted_at:desc": {expr: "l.last_contacted_at", desc: true},
	"last_contacted_at:asc":  {expr: "l.last_contacted_at"},
	"last_replied_at:desc":   {expr: "l.last_replied_at", desc: true},
	"status:asc":             {expr: "l.status"},
	"stage:asc":              {expr: "c.lifecycle_stage"},
}

// LeadSortKeys lists the accepted values of the `sort` parameter.
func LeadSortKeys() []string { return sortKeys(leadSorts) }

const leadSelect = `
    l.id, l.campaign_id, l.contact_id, l.business_id, l.status, l.instantly_lead_id, l.instantly_status,
    l.interest_status, l.interest_label, l.pushed_at, l.push_attempts, l.last_push_error, l.last_contacted_at,
    l.last_opened_at, l.last_clicked_at, l.last_replied_at, l.open_count, l.click_count, l.reply_count,
    l.created_at, l.updated_at,
    c.email::text, c.first_name, c.last_name, c.company, c.title, c.lifecycle_stage, c.suppressed_at,
    COALESCE((SELECT array_agg(v.name ORDER BY va.step) FROM variant_assignments va JOIN email_variants v ON v.id = va.variant_id
              WHERE va.campaign_lead_id = l.id), '{}')::text[],
    (SELECT count(*) FROM email_sends s WHERE s.campaign_lead_id = l.id)::bigint`

func buildLeadWhere(f LeadFilter, a *argSet) string {
	conds := []string{"l.campaign_id = " + a.add(f.CampaignID)}
	if len(f.Statuses) > 0 {
		conds = append(conds, "l.status = ANY("+a.add(f.Statuses)+")")
	}
	if len(f.Stages) > 0 {
		conds = append(conds, "c.lifecycle_stage = ANY("+a.add(f.Stages)+")")
	}
	if f.Q != nil && strings.TrimSpace(*f.Q) != "" {
		q := "%" + strings.TrimSpace(*f.Q) + "%"
		p := a.add(q)
		conds = append(conds, "(c.email::text ILIKE "+p+" OR c.company ILIKE "+p+" OR c.first_name ILIKE "+p+" OR c.last_name ILIKE "+p+")")
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// ListCampaignLeadRows returns one page of a campaign's leads.
func (s *Store) ListCampaignLeadRows(ctx context.Context, f LeadFilter, sort string, limit, offset int) ([]LeadRow, error) {
	var a argSet
	query := "SELECT " + leadSelect + " FROM campaign_leads l JOIN contacts c ON c.id = l.contact_id" +
		buildLeadWhere(f, &a) + lookupSort(leadSorts, sort, "created_at:desc").orderBy("l.id") +
		" LIMIT " + a.add(limit) + " OFFSET " + a.add(offset)
	rows, err := s.pool.Query(ctx, query, a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: list campaign leads: %w", err)
	}
	defer rows.Close()
	out := []LeadRow{}
	for rows.Next() {
		var r LeadRow
		if err := rows.Scan(&r.ID, &r.CampaignID, &r.ContactID, &r.BusinessID, &r.Status, &r.InstantlyLeadID,
			&r.InstantlyStatus, &r.InterestStatus, &r.InterestLabel, &r.PushedAt, &r.PushAttempts, &r.LastPushError,
			&r.LastContactedAt, &r.LastOpenedAt, &r.LastClickedAt, &r.LastRepliedAt, &r.OpenCount, &r.ClickCount,
			&r.ReplyCount, &r.CreatedAt, &r.UpdatedAt, &r.Email, &r.FirstName, &r.LastName, &r.Company, &r.Title,
			&r.LifecycleStage, &r.SuppressedAt, &r.VariantNames, &r.SendsTotal); err != nil {
			return nil, fmt.Errorf("db: scan campaign lead: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	err = annotateExclusions(ctx, s, out, func(r *LeadRow) string { return r.Email },
		func(r *LeadRow, ref *ExclusionRef) { r.Exclusion = ref })
	return out, err
}

// GetCampaignLeadRow returns one lead joined with its contact.
func (s *Store) GetCampaignLeadRow(ctx context.Context, id uuid.UUID) (LeadRow, error) {
	var a argSet
	query := "SELECT " + leadSelect + " FROM campaign_leads l JOIN contacts c ON c.id = l.contact_id WHERE l.id = " + a.add(id)
	rows, err := s.pool.Query(ctx, query, a.values()...)
	if err != nil {
		return LeadRow{}, fmt.Errorf("db: get campaign lead: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return LeadRow{}, err
		}
		return LeadRow{}, pgx.ErrNoRows
	}
	var r LeadRow
	if err := rows.Scan(&r.ID, &r.CampaignID, &r.ContactID, &r.BusinessID, &r.Status, &r.InstantlyLeadID,
		&r.InstantlyStatus, &r.InterestStatus, &r.InterestLabel, &r.PushedAt, &r.PushAttempts, &r.LastPushError,
		&r.LastContactedAt, &r.LastOpenedAt, &r.LastClickedAt, &r.LastRepliedAt, &r.OpenCount, &r.ClickCount,
		&r.ReplyCount, &r.CreatedAt, &r.UpdatedAt, &r.Email, &r.FirstName, &r.LastName, &r.Company, &r.Title,
		&r.LifecycleStage, &r.SuppressedAt, &r.VariantNames, &r.SendsTotal); err != nil {
		return LeadRow{}, fmt.Errorf("db: scan campaign lead: %w", err)
	}
	rows.Close()
	one := []LeadRow{r}
	err = annotateExclusions(ctx, s, one, func(r *LeadRow) string { return r.Email },
		func(r *LeadRow, ref *ExclusionRef) { r.Exclusion = ref })
	return one[0], err
}

// CountCampaignLeadRows counts the leads a filter matches.
func (s *Store) CountCampaignLeadRows(ctx context.Context, f LeadFilter) (int64, error) {
	var a argSet
	var total int64
	err := s.pool.QueryRow(ctx, "SELECT count(*) FROM campaign_leads l JOIN contacts c ON c.id = l.contact_id"+
		buildLeadWhere(f, &a), a.values()...).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("db: count campaign leads: %w", err)
	}
	return total, nil
}

/* --------------------------------------------------------------- contacts */

// ContactFilter is the filter behind GET /contacts.
type ContactFilter struct {
	Stages     []string
	Suppressed *bool
	HasConsent *bool
	CampaignID *uuid.UUID
	Q          *string
	// Excluded keeps only globally excluded contacts (true), hides them (false),
	// or ignores exclusion (nil).
	Excluded *bool
}

// ContactRow is one contact with the counts the list shows.
type ContactRow struct {
	ID                uuid.UUID
	Email             string
	Domain            string
	FirstName         *string
	LastName          *string
	Company           *string
	Title             *string
	Phone             *string
	Website           *string
	BusinessID        uuid.NullUUID
	Source            string
	LifecycleStage    string
	StageChangedAt    time.Time
	SuppressedAt      *time.Time
	SuppressionReason *string
	Attributes        []byte
	LastEventAt       *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	HasConsent        bool
	CampaignCount     int64
	SubscriptionCount int64
	// Exclusion is the global exclusion rule covering the contact, or nil.
	Exclusion *ExclusionRef
}

var contactSorts = map[string]sortSpec{
	"created_at:desc":       {expr: "c.created_at", desc: true},
	"created_at:asc":        {expr: "c.created_at"},
	"email:asc":             {expr: "c.email"},
	"email:desc":            {expr: "c.email", desc: true},
	"stage_changed_at:desc": {expr: "c.stage_changed_at", desc: true},
	"last_event_at:desc":    {expr: "c.last_event_at", desc: true},
	"stage:asc":             {expr: "c.lifecycle_stage"},
}

// ContactSortKeys lists the accepted values of the `sort` parameter.
func ContactSortKeys() []string { return sortKeys(contactSorts) }

const contactSelect = `
    c.id, c.email::text, c.domain, c.first_name, c.last_name, c.company, c.title, c.phone, c.website, c.business_id,
    c.source, c.lifecycle_stage, c.stage_changed_at, c.suppressed_at, c.suppression_reason, c.attributes,
    c.last_event_at, c.created_at, c.updated_at,
    EXISTS (SELECT 1 FROM contact_consents cc WHERE cc.contact_id = c.id AND cc.revoked_at IS NULL),
    (SELECT count(*) FROM campaign_leads l WHERE l.contact_id = c.id)::bigint,
    (SELECT count(*) FROM newsletter_subscriptions ns WHERE ns.contact_id = c.id)::bigint`

func buildContactWhere(f ContactFilter, a *argSet) string {
	conds := []string{"TRUE"}
	if len(f.Stages) > 0 {
		conds = append(conds, "c.lifecycle_stage = ANY("+a.add(f.Stages)+")")
	}
	if f.Suppressed != nil {
		if *f.Suppressed {
			conds = append(conds, "c.suppressed_at IS NOT NULL")
		} else {
			conds = append(conds, "c.suppressed_at IS NULL")
		}
	}
	if f.HasConsent != nil {
		clause := "EXISTS (SELECT 1 FROM contact_consents cc WHERE cc.contact_id = c.id AND cc.revoked_at IS NULL)"
		if !*f.HasConsent {
			clause = "NOT " + clause
		}
		conds = append(conds, clause)
	}
	if f.CampaignID != nil {
		conds = append(conds, "c.id IN (SELECT contact_id FROM campaign_leads WHERE campaign_id = "+a.add(*f.CampaignID)+")")
	}
	if f.Excluded != nil {
		conds = append(conds, excludedEmailCond("c.email", *f.Excluded))
	}
	if f.Q != nil && strings.TrimSpace(*f.Q) != "" {
		q := "%" + strings.TrimSpace(*f.Q) + "%"
		p := a.add(q)
		conds = append(conds, "(c.email::text ILIKE "+p+" OR c.company ILIKE "+p+" OR c.first_name ILIKE "+p+" OR c.last_name ILIKE "+p+")")
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// ListContactRows returns one page of contacts.
func (s *Store) ListContactRows(ctx context.Context, f ContactFilter, sort string, limit, offset int) ([]ContactRow, error) {
	var a argSet
	query := "SELECT " + contactSelect + " FROM contacts c" + buildContactWhere(f, &a) +
		lookupSort(contactSorts, sort, "created_at:desc").orderBy("c.id") +
		" LIMIT " + a.add(limit) + " OFFSET " + a.add(offset)
	rows, err := s.pool.Query(ctx, query, a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: list contacts: %w", err)
	}
	defer rows.Close()
	out := []ContactRow{}
	for rows.Next() {
		var r ContactRow
		if err := rows.Scan(&r.ID, &r.Email, &r.Domain, &r.FirstName, &r.LastName, &r.Company, &r.Title, &r.Phone,
			&r.Website, &r.BusinessID, &r.Source, &r.LifecycleStage, &r.StageChangedAt, &r.SuppressedAt,
			&r.SuppressionReason, &r.Attributes, &r.LastEventAt, &r.CreatedAt, &r.UpdatedAt, &r.HasConsent,
			&r.CampaignCount, &r.SubscriptionCount); err != nil {
			return nil, fmt.Errorf("db: scan contact: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	err = annotateExclusions(ctx, s, out, func(r *ContactRow) string { return r.Email },
		func(r *ContactRow, ref *ExclusionRef) { r.Exclusion = ref })
	return out, err
}

// GetContactRow returns one contact with its counts.
func (s *Store) GetContactRow(ctx context.Context, id uuid.UUID) (ContactRow, error) {
	var a argSet
	rows, err := s.pool.Query(ctx, "SELECT "+contactSelect+" FROM contacts c WHERE c.id = "+a.add(id), a.values()...)
	if err != nil {
		return ContactRow{}, fmt.Errorf("db: get contact: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return ContactRow{}, err
		}
		return ContactRow{}, pgx.ErrNoRows
	}
	var r ContactRow
	if err := rows.Scan(&r.ID, &r.Email, &r.Domain, &r.FirstName, &r.LastName, &r.Company, &r.Title, &r.Phone,
		&r.Website, &r.BusinessID, &r.Source, &r.LifecycleStage, &r.StageChangedAt, &r.SuppressedAt,
		&r.SuppressionReason, &r.Attributes, &r.LastEventAt, &r.CreatedAt, &r.UpdatedAt, &r.HasConsent,
		&r.CampaignCount, &r.SubscriptionCount); err != nil {
		return ContactRow{}, fmt.Errorf("db: scan contact: %w", err)
	}
	rows.Close()
	one := []ContactRow{r}
	err = annotateExclusions(ctx, s, one, func(r *ContactRow) string { return r.Email },
		func(r *ContactRow, ref *ExclusionRef) { r.Exclusion = ref })
	return one[0], err
}

// CountContactRows counts the contacts a filter matches.
func (s *Store) CountContactRows(ctx context.Context, f ContactFilter) (int64, error) {
	var a argSet
	var total int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM contacts c"+buildContactWhere(f, &a), a.values()...).Scan(&total); err != nil {
		return 0, fmt.Errorf("db: count contacts: %w", err)
	}
	return total, nil
}

/* ------------------------------------------------------- business import */

// ImportFilter selects businesses (and their primary email) to bring into a campaign.
type ImportFilter struct {
	VerificationTags []string
	State            *string
	City             *string
	Category         *string
	JobID            *uuid.UUID
	BusinessIDs      []uuid.UUID
	// PrimaryOnly keeps only each business's primary address; otherwise every
	// address the business holds is a candidate.
	PrimaryOnly bool
}

// ImportCandidate is one address ready to become a contact.
type ImportCandidate struct {
	BusinessID uuid.UUID
	Business   string
	Email      string
	Website    *string
	Phone      *string
	City       *string
	State      *string
	Tag        *string
}

func buildImportWhere(f ImportFilter, a *argSet) string {
	conds := []string{"b.suppressed = false"}
	if len(f.VerificationTags) > 0 {
		conds = append(conds, "ev.verification_tag = ANY("+a.add(f.VerificationTags)+")")
	}
	if f.State != nil && *f.State != "" {
		conds = append(conds, "b.state = "+a.add(*f.State))
	}
	if f.City != nil && *f.City != "" {
		conds = append(conds, "b.city ILIKE "+a.add(*f.City))
	}
	if f.Category != nil && *f.Category != "" {
		conds = append(conds, "b.category ILIKE "+a.add("%"+*f.Category+"%"))
	}
	if f.JobID != nil {
		conds = append(conds, "EXISTS (SELECT 1 FROM job_results jr WHERE jr.business_id = b.id AND jr.job_id = "+a.add(*f.JobID)+")")
	}
	if len(f.BusinessIDs) > 0 {
		conds = append(conds, "b.id = ANY("+a.add(f.BusinessIDs)+")")
	}
	if f.PrimaryOnly {
		conds = append(conds, "be.is_primary")
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

const importFrom = ` FROM business_emails be
    JOIN businesses b ON b.id = be.business_id
    LEFT JOIN email_verifications ev ON ev.email = be.email`

// ListImportCandidates streams the addresses an import filter matches, capped.
func (s *Store) ListImportCandidates(ctx context.Context, f ImportFilter, limit int) ([]ImportCandidate, error) {
	var a argSet
	query := "SELECT b.id, b.name, be.email::text, b.website, b.phone, b.city, b.state, ev.verification_tag" +
		importFrom + buildImportWhere(f, &a) + " ORDER BY b.created_at, be.email LIMIT " + a.add(limit)
	rows, err := s.pool.Query(ctx, query, a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: list import candidates: %w", err)
	}
	defer rows.Close()
	out := []ImportCandidate{}
	for rows.Next() {
		var c ImportCandidate
		if err := rows.Scan(&c.BusinessID, &c.Business, &c.Email, &c.Website, &c.Phone, &c.City, &c.State, &c.Tag); err != nil {
			return nil, fmt.Errorf("db: scan import candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountImportCandidates counts what an import filter matches.
func (s *Store) CountImportCandidates(ctx context.Context, f ImportFilter) (int64, error) {
	var a argSet
	var total int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*)"+importFrom+buildImportWhere(f, &a), a.values()...).Scan(&total); err != nil {
		return 0, fmt.Errorf("db: count import candidates: %w", err)
	}
	return total, nil
}

// ImportEmailState is what an address already is, from the point of view of one
// campaign: a suppressed contact is never imported, and an address the campaign
// already holds is skipped.
type ImportEmailState struct {
	Suppressed bool
	Existing   bool
	// Excluded is set when a global exclusion covers the address.
	Excluded bool
}

// ClassifyImportEmails answers, for a batch of addresses, what an import would
// make of each one. It is the read half of ImportLeads: the estimate runs the
// same decision without the upsert, so the two report the same counts.
func (s *Store) ClassifyImportEmails(ctx context.Context, campaignID uuid.UUID, emails []string) (map[string]ImportEmailState, error) {
	out := make(map[string]ImportEmailState, len(emails))
	if len(emails) == 0 {
		return out, nil
	}
	const query = `SELECT c.email::text,
        c.suppressed_at IS NOT NULL,
        cl.id IS NOT NULL
    FROM contacts c
    LEFT JOIN campaign_leads cl ON cl.contact_id = c.id AND cl.campaign_id = $1
    WHERE c.email = ANY($2::citext[])`
	rows, err := s.pool.Query(ctx, query, campaignID, emails)
	if err != nil {
		return nil, fmt.Errorf("db: classify import emails: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var email string
		var state ImportEmailState
		if err := rows.Scan(&email, &state.Suppressed, &state.Existing); err != nil {
			return nil, fmt.Errorf("db: scan import email state: %w", err)
		}
		out[strings.ToLower(email)] = state
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	excluded, err := s.ExcludedEmails(ctx, emails)
	if err != nil {
		return nil, err
	}
	for email := range excluded {
		state := out[email]
		state.Excluded = true
		out[email] = state
	}
	return out, nil
}
