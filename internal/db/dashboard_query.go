package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

/* -------------------------------------------------------------- dashboard */

// The client report reads every module at once, so its queries live together here
// rather than beside each module's own. Every figure is bounded by a half-open
// [start, end) window on the timestamp that says when the thing happened:
//
//   - a business when it was first scraped (and "with email" means one of those
//     businesses has an address now), an address when it was found;
//   - a verification when its last pass ran (the paid pass if there was one);
//   - scraping spend when the provider run finished, verification spend when the
//     paid pass ran, a domain when it was registered;
//   - campaign figures by send cohort: opens, replies and bounces belong to the send
//     that drew them, the way /campaign-analytics counts them, so the rates in a
//     window never divide by sends from a different one;
//   - campaigns started in Instantly's app, which send nothing through Karvon, by
//     the calendar day Instantly reports each figure on (see importedDailySQL).
//
// Verification spend is priced at the verifier's current per-1k rate: the rate is
// not stored per check, so a rate change re-prices history.

// verifiedAtExpr is when an address's verdict was last set.
const verifiedAtExpr = `COALESCE(ev.pass2_verified_at, ev.pass1_verified_at)`

// scrapeCostAtExpr is when a provider run's spend was incurred.
const scrapeCostAtExpr = `COALESCE(jq.finished_at, jq.started_at, jq.created_at)`

// domainCostAtExpr is when a domain registration was billed.
const domainCostAtExpr = `COALESCE(di.registered_at, di.updated_at)`

// firstActivityExpr is when the operation started spending: the first job, campaign
// or mailbox. Recurring costs are not charged before it.
const firstActivityExpr = `(SELECT LEAST((SELECT min(created_at) FROM jobs),
                      (SELECT min(created_at) FROM campaigns),
                      (SELECT min(created_at) FROM workspace_mailboxes)))`

// mailboxStartExpr is when a mailbox started being paid for.
const mailboxStartExpr = `COALESCE(m.provisioned_at, m.created_at)`

// overlapDays is the length in days of [lo, hi), or 0 when it is empty.
func overlapDays(lo, hi string) string {
	return `GREATEST(0, EXTRACT(EPOCH FROM (` + hi + `) - (` + lo + `)) / 86400)::float8`
}

// successfulContactsSQL selects the contacts that became a successful lead in
// [$1, $2): a positive reply, marked interested, or a meeting booked. One contact
// counts once however many of those it did.
const successfulContactsSQL = `
    SELECT s.contact_id FROM email_sends s
     WHERE s.reply_classification = 'positive' AND s.replied_at >= $1 AND s.replied_at < $2
    UNION
    SELECT ce.contact_id FROM contact_events ce
     WHERE ce.type IN ('interested', 'meeting_booked') AND ce.occurred_at >= $1 AND ce.occurred_at < $2`

// importedDailySQL is Instantly's per-day figures for the campaigns started in
// Instantly's app. Their analytics snapshots are lifetime totals as of each fetch,
// so they cannot be windowed; these days can. A day is a calendar date, matched to
// the window's own calendar days rather than to its instants.
const importedDailySQL = `
    SELECT d.* FROM campaign_stats_daily d
    JOIN campaigns c ON c.id = d.campaign_id AND c.source = 'instantly'`

// windowDay is the calendar date t falls on where it was cut: the report's window
// opens and closes at local midnights, so this is the window's first day and the
// day after its last.
func windowDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// repliedFilter counts a human reply, the way campaign analytics does.
const repliedFilter = `s.replied_at IS NOT NULL AND COALESCE(s.reply_classification, '') NOT IN ('auto_reply', 'out_of_office')`

// DashboardSummary is every headline figure for one window.
type DashboardSummary struct {
	Businesses          int64
	BusinessesWithEmail int64
	EmailsFound         int64
	EmailsVerified      int64
	EmailsGreen         int64
	PaidChecks          int64

	ScrapingCents     int64
	VerificationCents int64
	DomainsCents      int64

	CampaignsLaunched int64
	Sends             int64
	LeadsContacted    int64
	Opened            int64
	Clicked           int64
	Replied           int64
	PositiveReplies   int64
	Bounced           int64
	Unsubscribed      int64
	Interested        int64
	MeetingsBooked    int64
	// SuccessfulLeads is the distinct contacts with a positive reply, an interested
	// mark or a booked meeting in the window.
	SuccessfulLeads int64

	// Imported is the campaigns started in Instantly, kept apart from the figures
	// above: none of them are in Sends, Replied, Interested or SuccessfulLeads.
	Imported DashboardImported

	// MailboxDays is the provisioned Workspace mailbox-days inside the window, up to
	// now; ActiveDays the days inside the window, up to now, since the first activity.
	// The service prices both at the configured monthly rates.
	MailboxDays float64
	ActiveDays  float64
}

// DashboardImported is Instantly's own figures for the campaigns started in
// Instantly's app, over the window's calendar days. They are counted on the day
// Instantly reports them — a reply on the day it came in, not with the send that
// drew it — and Instantly reports no bounces or unsubscribes per day.
type DashboardImported struct {
	// Campaigns is the imported campaigns that sent or heard back in the window.
	Campaigns int64
	Sends     int64
	// LeadsContacted is the leads emailed for the first time.
	LeadsContacted int64
	Opened         int64
	Clicked        int64
	Replied        int64
	// Interested is Instantly's opportunity count.
	Interested int64
}

// DashboardSummary computes the headline figures for [start, end).
func (s *Store) DashboardSummary(ctx context.Context, start, end time.Time) (DashboardSummary, error) {
	q := `
SELECT
    (SELECT count(*) FROM businesses b
      WHERE NOT b.suppressed AND b.created_at >= $1 AND b.created_at < $2)::bigint,
    (SELECT count(*) FROM businesses b
      WHERE NOT b.suppressed AND b.created_at >= $1 AND b.created_at < $2
        AND EXISTS (SELECT 1 FROM business_emails be WHERE be.business_id = b.id))::bigint,
    (SELECT count(*) FROM business_emails be
      WHERE be.found_at >= $1 AND be.found_at < $2)::bigint,
    (SELECT count(*) FROM email_verifications ev
      WHERE ` + verifiedAtExpr + ` >= $1 AND ` + verifiedAtExpr + ` < $2)::bigint,
    (SELECT count(*) FROM email_verifications ev
      WHERE ev.verification_tag = 'green' AND ` + verifiedAtExpr + ` >= $1 AND ` + verifiedAtExpr + ` < $2)::bigint,
    (SELECT count(*) FROM email_verifications ev
      WHERE ev.pass2_credits > 0 AND ev.pass2_verified_at >= $1 AND ev.pass2_verified_at < $2)::bigint,

    (SELECT COALESCE(sum(jq.cost_cents), 0) FROM job_queries jq
      WHERE ` + scrapeCostAtExpr + ` >= $1 AND ` + scrapeCostAtExpr + ` < $2)::bigint,
    (SELECT COALESCE(sum(ev.pass2_credits::bigint * src.cost_per_1k_cents), 0) / 1000
       FROM email_verifications ev JOIN sources src ON src.id = ev.pass2_source_id
      WHERE ev.pass2_credits > 0 AND ev.pass2_verified_at >= $1 AND ev.pass2_verified_at < $2)::bigint,
    (SELECT COALESCE(sum(COALESCE(di.cost_cents, di.quoted_cost_cents)), 0) FROM domain_purchase_items di
      WHERE di.status = 'succeeded' AND ` + domainCostAtExpr + ` >= $1 AND ` + domainCostAtExpr + ` < $2)::bigint,

    (SELECT count(*) FROM campaigns c WHERE c.launched_at >= $1 AND c.launched_at < $2)::bigint,
    m.sends, m.contacted, m.opened, m.clicked, m.replied, m.positive, m.bounced, m.unsubscribed,

    (SELECT count(DISTINCT ce.contact_id) FROM contact_events ce
      WHERE ce.type = 'interested' AND ce.occurred_at >= $1 AND ce.occurred_at < $2)::bigint,
    (SELECT count(DISTINCT ce.contact_id) FROM contact_events ce
      WHERE ce.type = 'meeting_booked' AND ce.occurred_at >= $1 AND ce.occurred_at < $2)::bigint,
    (SELECT count(*) FROM (` + successfulContactsSQL + `) w)::bigint,

    im.campaigns, im.sends, im.contacted, im.opened, im.clicked, im.replied, im.interested,

    (SELECT COALESCE(sum(` + overlapDays("GREATEST($1, "+mailboxStartExpr+")", "LEAST($2, now())") + `), 0)
       FROM workspace_mailboxes m WHERE m.status = 'created')::float8,
    (SELECT CASE WHEN f.at IS NULL THEN 0
                 ELSE ` + overlapDays("GREATEST($1, f.at)", "LEAST($2, now())") + ` END
       FROM (SELECT ` + firstActivityExpr + ` AS at) f)::float8
FROM (
    SELECT count(*)::bigint                                                AS sends,
           count(DISTINCT s.contact_id)::bigint                            AS contacted,
           count(*) FILTER (WHERE s.first_opened_at IS NOT NULL)::bigint   AS opened,
           count(*) FILTER (WHERE s.first_clicked_at IS NOT NULL)::bigint  AS clicked,
           count(*) FILTER (WHERE ` + repliedFilter + `)::bigint            AS replied,
           count(*) FILTER (WHERE s.reply_classification = 'positive')::bigint AS positive,
           count(*) FILTER (WHERE s.bounced_at IS NOT NULL)::bigint        AS bounced,
           count(*) FILTER (WHERE s.unsubscribed_at IS NOT NULL)::bigint   AS unsubscribed
    FROM email_sends s
    WHERE s.sent_at >= $1 AND s.sent_at < $2
) m, (
    SELECT count(DISTINCT d.campaign_id) FILTER (WHERE d.sent > 0 OR d.replies > 0 OR d.opportunities > 0)::bigint AS campaigns,
           COALESCE(sum(d.sent), 0)::bigint                AS sends,
           COALESCE(sum(d.new_leads_contacted), 0)::bigint AS contacted,
           COALESCE(sum(d.unique_opened), 0)::bigint       AS opened,
           COALESCE(sum(d.unique_clicks), 0)::bigint       AS clicked,
           COALESCE(sum(d.unique_replies), 0)::bigint      AS replied,
           COALESCE(sum(d.opportunities), 0)::bigint       AS interested
    FROM (` + importedDailySQL + `) d
    WHERE d.day >= $3::date AND d.day < $4::date
) im`

	var out DashboardSummary
	im := &out.Imported
	err := s.pool.QueryRow(ctx, q, start, end, windowDay(start), windowDay(end)).Scan(
		&out.Businesses, &out.BusinessesWithEmail, &out.EmailsFound, &out.EmailsVerified, &out.EmailsGreen, &out.PaidChecks,
		&out.ScrapingCents, &out.VerificationCents, &out.DomainsCents,
		&out.CampaignsLaunched,
		&out.Sends, &out.LeadsContacted, &out.Opened, &out.Clicked, &out.Replied, &out.PositiveReplies, &out.Bounced, &out.Unsubscribed,
		&out.Interested, &out.MeetingsBooked, &out.SuccessfulLeads,
		&im.Campaigns, &im.Sends, &im.LeadsContacted, &im.Opened, &im.Clicked, &im.Replied, &im.Interested,
		&out.MailboxDays, &out.ActiveDays,
	)
	if err != nil {
		return DashboardSummary{}, fmt.Errorf("db: dashboard summary: %w", err)
	}
	return out, nil
}

// DashboardPoint is one bucket of the report's time series.
type DashboardPoint struct {
	// BucketStart is the first local day of the bucket, as a calendar date.
	BucketStart       time.Time
	Businesses        int64
	EmailsFound       int64
	EmailsGreen       int64
	Sends             int64
	Replied           int64
	PositiveReplies   int64
	SuccessfulLeads   int64
	ScrapingCents     int64
	VerificationCents int64
	DomainsCents      int64
	MailboxDays       float64
	ActiveDays        float64
	// The imported campaigns' sends, replies and opportunities, by the day
	// Instantly reports them; not in the figures above.
	ImportedSends      int64
	ImportedReplied    int64
	ImportedInterested int64
}

// DashboardSeries buckets the report's figures over [start, end). bucket must be one
// of "day", "week" or "month" — the caller validates it — and tz an IANA zone the
// buckets are cut in. Every bucket in the window is returned, empty ones as zeros.
func (s *Store) DashboardSeries(ctx context.Context, start, end time.Time, bucket, tz string) ([]DashboardPoint, error) {
	// $3 is the date_trunc unit and $4 the zone; neither is interpolated. A bucket's
	// own bounds (lo, hi) are clamped to the window for the recurring-cost days.
	q := `
WITH buckets AS (
    SELECT gs AS k,
           GREATEST($1::timestamptz, gs AT TIME ZONE $4) AS lo,
           LEAST($2::timestamptz, (gs + ('1 ' || $3)::interval) AT TIME ZONE $4) AS hi
    FROM generate_series(date_trunc($3, $1::timestamptz AT TIME ZONE $4),
                         ($2::timestamptz AT TIME ZONE $4) - interval '1 microsecond',
                         ('1 ' || $3)::interval) gs
),
won AS (
    SELECT date_trunc($3, w.at AT TIME ZONE $4) k, count(*) n
    FROM (
        SELECT x.contact_id, min(x.at) AS at FROM (
            SELECT s.contact_id, s.replied_at AS at FROM email_sends s
             WHERE s.reply_classification = 'positive' AND s.replied_at >= $1 AND s.replied_at < $2
            UNION ALL
            SELECT ce.contact_id, ce.occurred_at FROM contact_events ce
             WHERE ce.type IN ('interested', 'meeting_booked') AND ce.occurred_at >= $1 AND ce.occurred_at < $2
        ) x GROUP BY x.contact_id
    ) w GROUP BY 1
),
mbox AS (
    SELECT b.k, sum(` + overlapDays("GREATEST(b.lo, "+mailboxStartExpr+")", "LEAST(b.hi, now())") + `) n
    FROM buckets b JOIN workspace_mailboxes m ON m.status = 'created'
    GROUP BY b.k
),
active AS (
    SELECT b.k, CASE WHEN f.at IS NULL THEN 0
                     ELSE ` + overlapDays("GREATEST(b.lo, f.at)", "LEAST(b.hi, now())") + ` END n
    FROM buckets b CROSS JOIN (SELECT ` + firstActivityExpr + ` AS at) f
),
biz AS (
    SELECT date_trunc($3, b.created_at AT TIME ZONE $4) k, count(*) n
    FROM businesses b
    WHERE NOT b.suppressed AND b.created_at >= $1 AND b.created_at < $2 GROUP BY 1
),
found AS (
    SELECT date_trunc($3, be.found_at AT TIME ZONE $4) k, count(*) n
    FROM business_emails be
    WHERE be.found_at >= $1 AND be.found_at < $2 GROUP BY 1
),
green AS (
    SELECT date_trunc($3, ` + verifiedAtExpr + ` AT TIME ZONE $4) k, count(*) n
    FROM email_verifications ev
    WHERE ev.verification_tag = 'green' AND ` + verifiedAtExpr + ` >= $1 AND ` + verifiedAtExpr + ` < $2 GROUP BY 1
),
sends AS (
    SELECT date_trunc($3, s.sent_at AT TIME ZONE $4) k,
           count(*) n,
           count(*) FILTER (WHERE ` + repliedFilter + `) replied,
           count(*) FILTER (WHERE s.reply_classification = 'positive') positive
    FROM email_sends s
    WHERE s.sent_at >= $1 AND s.sent_at < $2 GROUP BY 1
),
scrape AS (
    SELECT date_trunc($3, ` + scrapeCostAtExpr + ` AT TIME ZONE $4) k, sum(jq.cost_cents) n
    FROM job_queries jq
    WHERE ` + scrapeCostAtExpr + ` >= $1 AND ` + scrapeCostAtExpr + ` < $2 GROUP BY 1
),
verify AS (
    SELECT date_trunc($3, ev.pass2_verified_at AT TIME ZONE $4) k,
           sum(ev.pass2_credits::bigint * src.cost_per_1k_cents) / 1000 n
    FROM email_verifications ev JOIN sources src ON src.id = ev.pass2_source_id
    WHERE ev.pass2_credits > 0 AND ev.pass2_verified_at >= $1 AND ev.pass2_verified_at < $2 GROUP BY 1
),
domains AS (
    SELECT date_trunc($3, ` + domainCostAtExpr + ` AT TIME ZONE $4) k,
           sum(COALESCE(di.cost_cents, di.quoted_cost_cents)) n
    FROM domain_purchase_items di
    WHERE di.status = 'succeeded' AND ` + domainCostAtExpr + ` >= $1 AND ` + domainCostAtExpr + ` < $2 GROUP BY 1
),
imported AS (
    SELECT date_trunc($3, d.day::timestamp) k,
           sum(d.sent) sends, sum(d.unique_replies) replied, sum(d.opportunities) interested
    FROM (` + importedDailySQL + `) d
    WHERE d.day >= ($1::timestamptz AT TIME ZONE $4)::date AND d.day < ($2::timestamptz AT TIME ZONE $4)::date
    GROUP BY 1
)
SELECT b.k::date,
       COALESCE(biz.n, 0)::bigint, COALESCE(found.n, 0)::bigint, COALESCE(green.n, 0)::bigint,
       COALESCE(sends.n, 0)::bigint, COALESCE(sends.replied, 0)::bigint, COALESCE(sends.positive, 0)::bigint,
       COALESCE(won.n, 0)::bigint,
       COALESCE(scrape.n, 0)::bigint, COALESCE(verify.n, 0)::bigint, COALESCE(domains.n, 0)::bigint,
       COALESCE(mbox.n, 0)::float8, COALESCE(active.n, 0)::float8,
       COALESCE(imported.sends, 0)::bigint, COALESCE(imported.replied, 0)::bigint,
       COALESCE(imported.interested, 0)::bigint
FROM buckets b
LEFT JOIN biz     ON biz.k = b.k
LEFT JOIN found   ON found.k = b.k
LEFT JOIN green   ON green.k = b.k
LEFT JOIN sends   ON sends.k = b.k
LEFT JOIN scrape  ON scrape.k = b.k
LEFT JOIN verify  ON verify.k = b.k
LEFT JOIN domains ON domains.k = b.k
LEFT JOIN won     ON won.k = b.k
LEFT JOIN mbox    ON mbox.k = b.k
LEFT JOIN active  ON active.k = b.k
LEFT JOIN imported ON imported.k = b.k
ORDER BY b.k`

	var out []DashboardPoint
	err := s.InTxRaw(ctx, func(tx pgx.Tx) error {
		// The planner cannot estimate the bucket joins and prices this tens-of-rows
		// query in the millions, which switches JIT on: compiling it then costs about
		// a second against milliseconds of execution.
		if _, err := tx.Exec(ctx, "SET LOCAL jit = off"); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, q, start, end, bucket, tz)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p DashboardPoint
			if err := rows.Scan(&p.BucketStart, &p.Businesses, &p.EmailsFound, &p.EmailsGreen,
				&p.Sends, &p.Replied, &p.PositiveReplies, &p.SuccessfulLeads,
				&p.ScrapingCents, &p.VerificationCents, &p.DomainsCents,
				&p.MailboxDays, &p.ActiveDays,
				&p.ImportedSends, &p.ImportedReplied, &p.ImportedInterested); err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("db: dashboard series: %w", err)
	}
	return out, nil
}

// DashboardCampaign is one campaign's figures for the window.
type DashboardCampaign struct {
	ID     uuid.UUID
	Name   string
	Status string
	// Source is "karvon" or "instantly". A campaign started in Instantly is read from
	// Instantly's per-day figures: PositiveReplies is its opportunities, and Bounced
	// is nil because Instantly reports no bounces per day.
	Source          string
	LaunchedAt      *time.Time
	LeadsTotal      int64
	Sends           int64
	LeadsContacted  int64
	Opened          int64
	Replied         int64
	PositiveReplies int64
	Bounced         *int64
}

// DashboardCampaigns lists the campaigns that sent in [start, end) or are running
// now, busiest first. A campaign started in Instantly counts on the window's
// calendar days, from Instantly's per-day figures.
func (s *Store) DashboardCampaigns(ctx context.Context, start, end time.Time, limit int) ([]DashboardCampaign, error) {
	q := `
SELECT c.id, c.name, c.status, c.source, c.launched_at, c.leads_total::bigint,
       COALESCE(m.sends, im.sends, 0), COALESCE(m.contacted, im.contacted, 0), COALESCE(m.opened, im.opened, 0),
       COALESCE(m.replied, im.replied, 0), COALESCE(m.positive, im.positive, 0),
       CASE WHEN c.source = 'instantly' THEN NULL ELSE COALESCE(m.bounced, 0) END
FROM campaigns c
LEFT JOIN (
    SELECT s.campaign_id,
           count(*)::bigint                                                     AS sends,
           count(DISTINCT s.contact_id)::bigint                                 AS contacted,
           count(*) FILTER (WHERE s.first_opened_at IS NOT NULL)::bigint        AS opened,
           count(*) FILTER (WHERE ` + repliedFilter + `)::bigint                 AS replied,
           count(*) FILTER (WHERE s.reply_classification = 'positive')::bigint  AS positive,
           count(*) FILTER (WHERE s.bounced_at IS NOT NULL)::bigint             AS bounced
    FROM email_sends s
    WHERE s.sent_at >= $1 AND s.sent_at < $2
    GROUP BY s.campaign_id
) m ON m.campaign_id = c.id
LEFT JOIN (
    SELECT d.campaign_id,
           sum(d.sent)::bigint                AS sends,
           sum(d.new_leads_contacted)::bigint AS contacted,
           sum(d.unique_opened)::bigint       AS opened,
           sum(d.unique_replies)::bigint      AS replied,
           sum(d.opportunities)::bigint       AS positive
    FROM (` + importedDailySQL + `) d
    WHERE d.day >= $4::date AND d.day < $5::date
    GROUP BY d.campaign_id
) im ON im.campaign_id = c.id
WHERE m.campaign_id IS NOT NULL OR im.sends > 0 OR c.status IN ('launching', 'active')
ORDER BY COALESCE(m.sends, im.sends, 0) DESC, c.launched_at DESC NULLS LAST, c.id
LIMIT $3`

	rows, err := s.pool.Query(ctx, q, start, end, limit, windowDay(start), windowDay(end))
	if err != nil {
		return nil, fmt.Errorf("db: dashboard campaigns: %w", err)
	}
	defer rows.Close()

	var out []DashboardCampaign
	for rows.Next() {
		var c DashboardCampaign
		if err := rows.Scan(&c.ID, &c.Name, &c.Status, &c.Source, &c.LaunchedAt, &c.LeadsTotal,
			&c.Sends, &c.LeadsContacted, &c.Opened, &c.Replied, &c.PositiveReplies, &c.Bounced); err != nil {
			return nil, fmt.Errorf("db: dashboard campaigns: scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: dashboard campaigns: %w", err)
	}
	return out, nil
}

// DashboardCount is one labelled count in a breakdown.
type DashboardCount struct {
	Label string
	Count int64
}

// DashboardBreakdowns is the "where did the leads come from" side of the report.
type DashboardBreakdowns struct {
	Categories      []DashboardCount
	States          []DashboardCount
	VerificationMix []DashboardCount
	CampaignStatus  []DashboardCount
}

// DashboardBreakdowns groups the window's businesses by category and state, its
// verified addresses by tag, and every campaign by its current status.
func (s *Store) DashboardBreakdowns(ctx context.Context, start, end time.Time, limit int) (DashboardBreakdowns, error) {
	var out DashboardBreakdowns
	var err error

	if out.Categories, err = s.dashboardCounts(ctx, `
SELECT COALESCE(NULLIF(trim(b.category), ''), 'Uncategorised'), count(*)::bigint
FROM businesses b
WHERE NOT b.suppressed AND b.created_at >= $1 AND b.created_at < $2
GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT $3`, start, end, limit); err != nil {
		return out, fmt.Errorf("db: dashboard categories: %w", err)
	}

	if out.States, err = s.dashboardCounts(ctx, `
SELECT COALESCE(NULLIF(trim(b.state), ''), 'Unknown'), count(*)::bigint
FROM businesses b
WHERE NOT b.suppressed AND b.created_at >= $1 AND b.created_at < $2
GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT $3`, start, end, limit); err != nil {
		return out, fmt.Errorf("db: dashboard states: %w", err)
	}

	if out.VerificationMix, err = s.dashboardCounts(ctx, `
SELECT ev.verification_tag, count(*)::bigint
FROM email_verifications ev
WHERE `+verifiedAtExpr+` >= $1 AND `+verifiedAtExpr+` < $2
GROUP BY 1
ORDER BY array_position(ARRAY['green', 'light_green', 'yellow', 'orange', 'red'], ev.verification_tag)
LIMIT $3`, start, end, limit); err != nil {
		return out, fmt.Errorf("db: dashboard verification mix: %w", err)
	}

	// Campaign status is a snapshot of now, not of the window: a status has no history.
	if out.CampaignStatus, err = s.dashboardCounts(ctx, `
SELECT c.status, count(*)::bigint FROM campaigns c GROUP BY 1 ORDER BY 2 DESC, 1`); err != nil {
		return out, fmt.Errorf("db: dashboard campaign status: %w", err)
	}
	return out, nil
}

func (s *Store) dashboardCounts(ctx context.Context, q string, args ...any) ([]DashboardCount, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DashboardCount{}
	for rows.Next() {
		var c DashboardCount
		if err := rows.Scan(&c.Label, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
