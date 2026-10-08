package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

/* ------------------------------------------------------------------ inbox */

// InboxFilter is the filter behind GET /inbox. The inbox lists received emails
// only; a thread's sent side is read through the thread endpoint.
type InboxFilter struct {
	Unread     *bool
	Accounts   []string
	CampaignID *uuid.UUID
	Q          *string
	// LatestOfThread keeps only the newest received email of each thread, the way
	// the Unibox lists conversations.
	LatestOfThread bool
	// IncludeAutoReplies keeps auto-replies and out-of-office answers.
	IncludeAutoReplies bool
}

// InboxRow is one received email with the names it is shown under.
type InboxRow struct {
	ID                  string
	ThreadID            *string
	Direction           string
	EmailAccount        *string
	LeadEmail           *string
	FromAddress         *string
	ToAddresses         *string
	Subject             *string
	ContentPreview      *string
	IsUnread            bool
	IsAutoReply         bool
	InterestStatus      *int32
	AIInterestValue     *float64
	InstantlyCampaignID *string
	CampaignID          uuid.NullUUID
	ContactID           uuid.NullUUID
	SentAt              time.Time

	CampaignName *string
	FirstName    *string
	LastName     *string
	Company      *string
	ThreadEmails int64
	// The campaign lead the email belongs to, with its current interest and
	// outcome, which can be newer than the label the email itself carries.
	CampaignLeadID uuid.NullUUID
	LeadInterest   *int32
	LeadOutcome    *string
}

var inboxSorts = map[string]sortSpec{
	"sent_at:desc": {expr: "e.sent_at", desc: true},
	"sent_at:asc":  {expr: "e.sent_at"},
}

// InboxSortKeys lists the accepted values of the `sort` parameter.
func InboxSortKeys() []string { return sortKeys(inboxSorts) }

const inboxSelect = `
    e.id, e.thread_id, e.direction, e.email_account::text, e.lead_email::text, e.from_address::text, e.to_addresses,
    e.subject, e.content_preview, e.is_unread, e.is_auto_reply, e.interest_status, e.ai_interest_value,
    e.instantly_campaign_id, e.campaign_id, e.contact_id, e.sent_at,
    cp.name, ct.first_name, ct.last_name, ct.company,
    (SELECT count(*) FROM inbox_emails t WHERE t.thread_id = e.thread_id)::bigint,
    l.id, l.interest_status, ` + outcomeExpr

// The lead an email belongs to is the contact's lead in the email's campaign.
const inboxFrom = `
    FROM inbox_emails e
    LEFT JOIN campaigns cp ON cp.id = e.campaign_id
    LEFT JOIN contacts ct ON ct.id = e.contact_id
    LEFT JOIN campaign_leads l ON l.campaign_id = e.campaign_id AND l.contact_id = e.contact_id
    LEFT JOIN LATERAL (SELECT count(*)::bigint AS sends_total FROM email_sends es WHERE es.campaign_lead_id = l.id) s ON TRUE`

func buildInboxWhere(f InboxFilter, a *argSet) string {
	conds := []string{"e.direction = 'received'"}
	if f.Unread != nil {
		conds = append(conds, "e.is_unread = "+a.add(*f.Unread))
	}
	if len(f.Accounts) > 0 {
		conds = append(conds, "e.email_account = ANY("+a.add(f.Accounts)+"::citext[])")
	}
	if f.CampaignID != nil {
		conds = append(conds, "e.campaign_id = "+a.add(*f.CampaignID))
	}
	if !f.IncludeAutoReplies {
		conds = append(conds, "NOT e.is_auto_reply")
	}
	if f.Q != nil && strings.TrimSpace(*f.Q) != "" {
		p := a.add("%" + strings.TrimSpace(*f.Q) + "%")
		conds = append(conds, "(e.lead_email::text ILIKE "+p+" OR e.from_address::text ILIKE "+p+
			" OR e.subject ILIKE "+p+" OR e.content_preview ILIKE "+p+" OR ct.company ILIKE "+p+")")
	}
	if f.LatestOfThread {
		conds = append(conds, `(e.thread_id IS NULL OR NOT EXISTS (
            SELECT 1 FROM inbox_emails n
            WHERE n.thread_id = e.thread_id AND n.direction = 'received'
              AND (n.sent_at, n.id) > (e.sent_at, e.id)))`)
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// ListInboxRows returns one page of received emails.
func (s *Store) ListInboxRows(ctx context.Context, f InboxFilter, sort string, limit, offset int) ([]InboxRow, error) {
	var a argSet
	query := "SELECT " + inboxSelect + inboxFrom + buildInboxWhere(f, &a) +
		lookupSort(inboxSorts, sort, "sent_at:desc").orderBy("e.id") +
		" LIMIT " + a.add(limit) + " OFFSET " + a.add(offset)
	rows, err := s.pool.Query(ctx, query, a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: list inbox: %w", err)
	}
	defer rows.Close()
	out := []InboxRow{}
	for rows.Next() {
		var r InboxRow
		if err := rows.Scan(&r.ID, &r.ThreadID, &r.Direction, &r.EmailAccount, &r.LeadEmail, &r.FromAddress,
			&r.ToAddresses, &r.Subject, &r.ContentPreview, &r.IsUnread, &r.IsAutoReply, &r.InterestStatus,
			&r.AIInterestValue, &r.InstantlyCampaignID, &r.CampaignID, &r.ContactID, &r.SentAt,
			&r.CampaignName, &r.FirstName, &r.LastName, &r.Company, &r.ThreadEmails,
			&r.CampaignLeadID, &r.LeadInterest, &r.LeadOutcome); err != nil {
			return nil, fmt.Errorf("db: scan inbox email: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountInboxRows counts the received emails a filter matches.
func (s *Store) CountInboxRows(ctx context.Context, f InboxFilter) (int64, error) {
	var a argSet
	var total int64
	err := s.pool.QueryRow(ctx, "SELECT count(*)"+inboxFrom+buildInboxWhere(f, &a), a.values()...).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("db: count inbox: %w", err)
	}
	return total, nil
}

/* --------------------------------------------------------------- outreach */

// outcomeExpr folds a campaign lead into one outreach outcome (campaign.Outcomes).
// The interest status set in the Unibox wins; a lead nobody labelled is described
// by what happened to it. It expects the lead as l and its send count as s.
const outcomeExpr = `CASE
        WHEN l.id IS NULL THEN NULL
        WHEN l.interest_status IN (2, 3, 4) THEN 'successful'
        WHEN l.interest_status IN (1, -4) THEN 'potential'
        WHEN l.interest_status IN (-1, -2, -3) OR l.status = 'unsubscribed' THEN 'bad'
        WHEN l.status = 'bounced' THEN 'bounced'
        WHEN l.reply_count > 0 OR l.last_replied_at IS NOT NULL OR l.status = 'replied' THEN 'replied'
        WHEN l.last_contacted_at IS NOT NULL OR s.sends_total > 0 THEN 'no_reply'
        ELSE 'not_contacted'
    END`

// OutreachFilter is the filter behind GET /outreach/leads and its CSV export.
type OutreachFilter struct {
	Outcomes   []string
	CampaignID *uuid.UUID
	Q          *string
	// IncludeNotContacted keeps leads that have not been emailed yet; by default
	// the table lists only the people outreach has actually reached.
	IncludeNotContacted bool
}

// OutreachRow is one campaign lead with its outcome: who was emailed, from which
// mailbox, how often, and how they answered.
type OutreachRow struct {
	LeadID           uuid.UUID
	CampaignID       uuid.UUID
	CampaignName     string
	ContactID        uuid.UUID
	Email            string
	FirstName        *string
	LastName         *string
	Company          *string
	Title            *string
	Phone            *string
	Website          *string
	Status           string
	InterestStatus   *int32
	InterestLabel    *string
	Outcome          string
	SendingAccount   *string
	EmailsSent       int64
	FirstContactedAt *time.Time
	LastContactedAt  *time.Time
	OpenCount        int32
	ClickCount       int32
	ReplyCount       int32
	LastRepliedAt    *time.Time
	LastReplySubject *string
	LastReplyPreview *string
	LastReplyThread  *string
}

var outreachSorts = map[string]sortSpec{
	"last_contacted_at:desc": {expr: "o.last_contacted_at", desc: true},
	"last_contacted_at:asc":  {expr: "o.last_contacted_at"},
	"last_replied_at:desc":   {expr: "o.last_replied_at", desc: true},
	"email:asc":              {expr: "o.email"},
	"email:desc":             {expr: "o.email", desc: true},
	"campaign:asc":           {expr: "o.campaign_name"},
	"outcome:asc":            {expr: "array_position(ARRAY['successful','potential','bad','replied','no_reply','bounced','not_contacted'], o.outcome)"},
}

// OutreachSortKeys lists the accepted values of the `sort` parameter.
func OutreachSortKeys() []string { return sortKeys(outreachSorts) }

const outreachInner = `
    SELECT l.id AS lead_id, l.campaign_id, cp.name AS campaign_name, c.id AS contact_id, c.email::text AS email,
           c.first_name, c.last_name, c.company, c.title, c.phone, c.website, l.status,
           l.interest_status, l.interest_label, ` + outcomeExpr + ` AS outcome,
           s.sending_account, s.sends_total, COALESCE(s.first_sent_at, l.last_contacted_at) AS first_contacted_at,
           GREATEST(l.last_contacted_at, s.last_sent_at) AS last_contacted_at,
           l.open_count, l.click_count, l.reply_count,
           GREATEST(l.last_replied_at, r.sent_at) AS last_replied_at,
           r.subject AS last_reply_subject, r.content_preview AS last_reply_preview, r.thread_id AS last_reply_thread
    FROM campaign_leads l
    JOIN contacts c ON c.id = l.contact_id
    JOIN campaigns cp ON cp.id = l.campaign_id
    LEFT JOIN LATERAL (
        SELECT count(*)::bigint AS sends_total, min(es.sent_at) AS first_sent_at, max(es.sent_at) AS last_sent_at,
               (array_agg(es.sending_account_email::text ORDER BY es.sent_at DESC))[1] AS sending_account
        FROM email_sends es WHERE es.campaign_lead_id = l.id
    ) s ON TRUE
    LEFT JOIN LATERAL (
        SELECT ie.subject, ie.content_preview, ie.thread_id, ie.sent_at
        FROM inbox_emails ie
        WHERE ie.direction = 'received' AND ie.lead_email = c.email
        ORDER BY (ie.campaign_id = l.campaign_id) DESC NULLS LAST, ie.sent_at DESC
        LIMIT 1
    ) r ON TRUE`

func buildOutreachWhere(f OutreachFilter, a *argSet) string {
	conds := []string{"TRUE"}
	if !f.IncludeNotContacted {
		conds = append(conds, "o.outcome <> 'not_contacted'")
	}
	if len(f.Outcomes) > 0 {
		conds = append(conds, "o.outcome = ANY("+a.add(f.Outcomes)+")")
	}
	if f.CampaignID != nil {
		conds = append(conds, "o.campaign_id = "+a.add(*f.CampaignID))
	}
	if f.Q != nil && strings.TrimSpace(*f.Q) != "" {
		p := a.add("%" + strings.TrimSpace(*f.Q) + "%")
		conds = append(conds, "(o.email ILIKE "+p+" OR o.company ILIKE "+p+" OR o.first_name ILIKE "+p+
			" OR o.last_name ILIKE "+p+" OR o.campaign_name ILIKE "+p+")")
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

func outreachQuery(f OutreachFilter, sort string, a *argSet) string {
	return "SELECT o.* FROM (" + outreachInner + ") o" + buildOutreachWhere(f, a) +
		lookupSort(outreachSorts, sort, "last_contacted_at:desc").orderBy("o.lead_id")
}

type rowScanner interface{ Scan(dest ...any) error }

func scanOutreach(row rowScanner) (OutreachRow, error) {
	var r OutreachRow
	err := row.Scan(&r.LeadID, &r.CampaignID, &r.CampaignName, &r.ContactID, &r.Email, &r.FirstName, &r.LastName,
		&r.Company, &r.Title, &r.Phone, &r.Website, &r.Status, &r.InterestStatus, &r.InterestLabel, &r.Outcome,
		&r.SendingAccount, &r.EmailsSent, &r.FirstContactedAt, &r.LastContactedAt, &r.OpenCount, &r.ClickCount,
		&r.ReplyCount, &r.LastRepliedAt, &r.LastReplySubject, &r.LastReplyPreview, &r.LastReplyThread)
	if err != nil {
		return OutreachRow{}, fmt.Errorf("db: scan outreach row: %w", err)
	}
	return r, nil
}

// ListOutreachRows returns one page of the outcomes table.
func (s *Store) ListOutreachRows(ctx context.Context, f OutreachFilter, sort string, limit, offset int) ([]OutreachRow, error) {
	var a argSet
	query := outreachQuery(f, sort, &a) + " LIMIT " + a.add(limit) + " OFFSET " + a.add(offset)
	rows, err := s.pool.Query(ctx, query, a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: list outreach: %w", err)
	}
	defer rows.Close()
	out := []OutreachRow{}
	for rows.Next() {
		r, err := scanOutreach(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountOutreachRows counts the leads a filter matches.
func (s *Store) CountOutreachRows(ctx context.Context, f OutreachFilter) (int64, error) {
	var a argSet
	var total int64
	err := s.pool.QueryRow(ctx, "SELECT count(*) FROM ("+outreachInner+") o"+buildOutreachWhere(f, &a),
		a.values()...).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("db: count outreach: %w", err)
	}
	return total, nil
}

// OutreachOutcomeCounts counts the leads per outcome, ignoring the outcome filter
// itself so the totals can label the filter's own options.
func (s *Store) OutreachOutcomeCounts(ctx context.Context, f OutreachFilter) (map[string]int64, error) {
	f.Outcomes = nil
	var a argSet
	rows, err := s.pool.Query(ctx, "SELECT o.outcome, count(*)::bigint FROM ("+outreachInner+") o"+
		buildOutreachWhere(f, &a)+" GROUP BY o.outcome", a.values()...)
	if err != nil {
		return nil, fmt.Errorf("db: count outreach outcomes: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var outcome string
		var n int64
		if err := rows.Scan(&outcome, &n); err != nil {
			return nil, fmt.Errorf("db: scan outreach outcome count: %w", err)
		}
		out[outcome] = n
	}
	return out, rows.Err()
}

// StreamOutreachRows calls fn for every row a filter matches, in order, without
// holding the whole result in memory. It is what the CSV export reads.
func (s *Store) StreamOutreachRows(ctx context.Context, f OutreachFilter, sort string, fn func(OutreachRow) error) error {
	var a argSet
	rows, err := s.pool.Query(ctx, outreachQuery(f, sort, &a), a.values()...)
	if err != nil {
		return fmt.Errorf("db: stream outreach: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanOutreach(rows)
		if err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}
