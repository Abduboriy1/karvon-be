package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// Everything analytical is computed from email_sends and contact_events, which we
// own. Instantly's own numbers are kept beside ours (labelled) and never merged,
// because the two count differently and the difference is itself a signal.

// Metrics is one bucket of send-level counts and the rates derived from them.
type Metrics struct {
	Sends           int64   `json:"sends"`
	UniqueContacts  int64   `json:"unique_contacts"`
	Opened          int64   `json:"opened"`
	Clicked         int64   `json:"clicked"`
	Replied         int64   `json:"replied"`
	PositiveReplies int64   `json:"positive_replies"`
	Bounced         int64   `json:"bounced"`
	Unsubscribed    int64   `json:"unsubscribed"`
	OpenRate        float64 `json:"open_rate"`
	ClickRate       float64 `json:"click_rate"`
	ReplyRate       float64 `json:"reply_rate"`
	PositiveRate    float64 `json:"positive_reply_rate"`
	BounceRate      float64 `json:"bounce_rate"`
	UnsubscribeRate float64 `json:"unsubscribe_rate"`
}

func (m *Metrics) finish() {
	if m.Sends > 0 {
		m.OpenRate = ratio(m.Opened, m.Sends)
		m.ClickRate = ratio(m.Clicked, m.Sends)
		m.ReplyRate = ratio(m.Replied, m.Sends)
		m.PositiveRate = ratio(m.PositiveReplies, m.Sends)
		m.BounceRate = ratio(m.Bounced, m.Sends)
	}
	if m.UniqueContacts > 0 {
		m.UnsubscribeRate = ratio(m.Unsubscribed, m.UniqueContacts)
	}
}

func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(float64(a)/float64(b)*10000) / 10000
}

// Overview is the top of the Campaign area.
type Overview struct {
	Campaigns          map[string]int64
	ContactsByStage    map[string]int64
	Local              Metrics
	Interested         int64
	NewsletterEligible int64
	Subscribers        int64
	ConversionRate     float64
	Funnel             []FunnelStep
}

// FunnelStep is one bar of the cold → subscriber funnel.
type FunnelStep struct {
	Stage       string
	Label       string
	Current     int64
	EverReached int64
}

// CampaignAnalytics compares our numbers with Instantly's for one campaign.
type CampaignAnalytics struct {
	Local          Metrics
	Instantly      map[string]any
	Mismatch       []Mismatch
	Daily          []DailyPoint
	Funnel         []FunnelStep
	Interested     int64
	Eligible       int64
	Subscribers    int64
	ConversionRate float64
	LeadCounts     map[string]int64
}

// Mismatch is a metric where the two sources disagree beyond tolerance.
type Mismatch struct {
	Metric    string
	Local     int64
	Instantly int64
}

// DailyPoint is one day of local sends.
type DailyPoint struct {
	Day     string
	Sends   int64
	Opened  int64
	Clicked int64
	Replied int64
	Bounced int64
}

// VariantAnalytics is Metrics for one variant at one step.
type VariantAnalytics struct {
	VariantID   uuid.UUID
	VariantName string
	Step        int
	Assigned    int64
	Metrics
}

// ComponentAnalytics is Metrics for one component.
type ComponentAnalytics struct {
	ComponentID uuid.UUID
	Type        string
	Name        string
	Metrics
}

// AccountAnalytics is Metrics for one sending account, with Instantly's daily
// totals beside it.
type AccountAnalytics struct {
	SendingAccountID    uuid.NullUUID
	Email               string
	Local               Metrics
	InstantlySent       int64
	InstantlyBounced    int64
	InstantlyReplies    int64
	InstantlyBounceRate float64
}

const metricsSelect = `
    count(*)::bigint,
    count(DISTINCT s.campaign_lead_id)::bigint,
    count(*) FILTER (WHERE s.first_opened_at IS NOT NULL)::bigint,
    count(*) FILTER (WHERE s.first_clicked_at IS NOT NULL)::bigint,
    count(*) FILTER (WHERE s.replied_at IS NOT NULL AND COALESCE(s.reply_classification,'') NOT IN ('auto_reply','out_of_office'))::bigint,
    count(*) FILTER (WHERE s.reply_classification = 'positive')::bigint,
    count(*) FILTER (WHERE s.bounced_at IS NOT NULL)::bigint,
    count(*) FILTER (WHERE s.unsubscribed_at IS NOT NULL)::bigint`

func scanMetrics(row interface{ Scan(...any) error }) (Metrics, error) {
	var m Metrics
	if err := row.Scan(&m.Sends, &m.UniqueContacts, &m.Opened, &m.Clicked, &m.Replied, &m.PositiveReplies, &m.Bounced, &m.Unsubscribed); err != nil {
		return Metrics{}, err
	}
	m.finish()
	return m, nil
}

func (s *Service) metrics(ctx context.Context, where string, args ...any) (Metrics, error) {
	m, err := scanMetrics(s.store.Pool().QueryRow(ctx, "SELECT "+metricsSelect+" FROM email_sends s "+where, args...))
	if err != nil {
		return Metrics{}, apperr.Internal(fmt.Errorf("analytics: metrics: %w", err))
	}
	return m, nil
}

// GetOverview builds the top-level dashboard.
func (s *Service) GetOverview(ctx context.Context) (Overview, error) {
	out := Overview{Campaigns: map[string]int64{}, ContactsByStage: map[string]int64{}}
	camps, err := s.store.CountCampaignsByStatus(ctx)
	if err != nil {
		return out, apperr.Internal(err)
	}
	for _, c := range camps {
		out.Campaigns[c.Status] = c.Total
	}
	if out.Local, err = s.metrics(ctx, ""); err != nil {
		return out, err
	}
	stages, err := s.store.CountContactsByStage(ctx)
	if err != nil {
		return out, apperr.Internal(err)
	}
	for _, st := range stages {
		out.ContactsByStage[st.LifecycleStage] = st.Total
	}
	reached, err := s.store.CountCampaignLeadsEverReached(ctx, uuid.NullUUID{})
	if err != nil {
		return out, apperr.Internal(err)
	}
	out.Funnel, out.Interested, out.NewsletterEligible, out.Subscribers, out.ConversionRate = buildFunnel(out.ContactsByStage, reached)
	return out, nil
}

func buildFunnel(byStage map[string]int64, reached []dbgen.CountCampaignLeadsEverReachedRow) ([]FunnelStep, int64, int64, int64, float64) {
	ever := map[string]int64{}
	for _, r := range reached {
		ever[r.Type] = r.Total
	}
	// "Ever reached" for a stage counts the event that enters it.
	enter := map[campaign.Stage]string{
		campaign.StageContacted: campaign.EventSent, campaign.StageEngaged: campaign.EventOpened,
		campaign.StageReplied: campaign.EventReplied, campaign.StageInterested: campaign.EventInterested,
		campaign.StagePermissionCaptured: campaign.EventConsentCaptured, campaign.StageNewsletterEligible: campaign.EventNewsletterEligible,
		campaign.StageMailchimpPending: campaign.EventNewsletterPending, campaign.StageMailchimpSubscribed: campaign.EventNewsletterSubscribed,
	}
	var funnel []FunnelStep
	var interested, eligible, subscribers, contacted int64
	for _, stage := range campaign.FunnelStages {
		step := FunnelStep{Stage: string(stage), Label: stage.Label(), Current: byStage[string(stage)]}
		if ev, ok := enter[stage]; ok {
			step.EverReached = ever[ev]
		}
		if step.EverReached < step.Current {
			step.EverReached = step.Current
		}
		funnel = append(funnel, step)
		switch stage {
		case campaign.StageInterested:
			interested = step.EverReached
		case campaign.StageNewsletterEligible:
			eligible = step.EverReached
		case campaign.StageMailchimpSubscribed:
			subscribers = step.EverReached
		case campaign.StageContacted:
			contacted = step.EverReached
		}
	}
	return funnel, interested, eligible, subscribers, ratio(subscribers, contacted)
}

// GetCampaignAnalytics compares local and provider numbers for one campaign.
func (s *Service) GetCampaignAnalytics(ctx context.Context, id uuid.UUID, days int) (CampaignAnalytics, error) {
	if _, err := s.campaign(ctx, id); err != nil {
		return CampaignAnalytics{}, err
	}
	if days <= 0 {
		days = 30
	}
	out := CampaignAnalytics{Instantly: map[string]any{}, Mismatch: []Mismatch{}, Daily: []DailyPoint{}, LeadCounts: map[string]int64{}}
	var err error
	if out.Local, err = s.metrics(ctx, "WHERE s.campaign_id = $1", id); err != nil {
		return out, err
	}
	if snap, err := s.store.LatestCampaignAnalyticsSnapshot(ctx, dbgen.LatestCampaignAnalyticsSnapshotParams{CampaignID: id, Source: campaign.SnapshotInstantly}); err == nil {
		_ = json.Unmarshal(snap.Metrics, &out.Instantly)
		out.Mismatch = mismatches(out.Local, out.Instantly)
	}
	rows, err := s.store.Pool().Query(ctx, `
		SELECT to_char(date_trunc('day', s.sent_at), 'YYYY-MM-DD'), count(*)::bigint,
		       count(*) FILTER (WHERE s.first_opened_at IS NOT NULL)::bigint,
		       count(*) FILTER (WHERE s.first_clicked_at IS NOT NULL)::bigint,
		       count(*) FILTER (WHERE s.replied_at IS NOT NULL)::bigint,
		       count(*) FILTER (WHERE s.bounced_at IS NOT NULL)::bigint
		FROM email_sends s WHERE s.campaign_id = $1 AND s.sent_at >= $2
		GROUP BY 1 ORDER BY 1`, id, s.now().AddDate(0, 0, -days))
	if err != nil {
		return out, apperr.Internal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p DailyPoint
		if err := rows.Scan(&p.Day, &p.Sends, &p.Opened, &p.Clicked, &p.Replied, &p.Bounced); err != nil {
			return out, apperr.Internal(err)
		}
		out.Daily = append(out.Daily, p)
	}
	if err := rows.Err(); err != nil {
		return out, apperr.Internal(err)
	}
	stages, err := s.store.CountCampaignContactsByStage(ctx, id)
	if err != nil {
		return out, apperr.Internal(err)
	}
	byStage := map[string]int64{}
	for _, st := range stages {
		byStage[st.LifecycleStage] = st.Total
	}
	reached, err := s.store.CountCampaignLeadsEverReached(ctx, uuid.NullUUID{UUID: id, Valid: true})
	if err != nil {
		return out, apperr.Internal(err)
	}
	out.Funnel, out.Interested, out.Eligible, out.Subscribers, out.ConversionRate = buildFunnel(byStage, reached)
	leadCounts, err := s.store.CountCampaignLeadsByStatus(ctx, id)
	if err != nil {
		return out, apperr.Internal(err)
	}
	for _, c := range leadCounts {
		out.LeadCounts[c.Status] = c.Total
	}
	return out, nil
}

func mismatches(local Metrics, remote map[string]any) []Mismatch {
	pairs := []struct {
		name   string
		local  int64
		remote string
	}{
		{"sends", local.Sends, "emails_sent_count"},
		{"replied", local.Replied, "reply_count_unique"},
		{"bounced", local.Bounced, "bounced_count"},
		{"opened", local.Opened, "open_count_unique"},
		{"clicked", local.Clicked, "link_click_count_unique"},
	}
	var out []Mismatch
	for _, p := range pairs {
		raw, ok := remote[p.remote]
		if !ok {
			continue
		}
		var r int64
		switch v := raw.(type) {
		case float64:
			r = int64(v)
		case int64:
			r = v
		default:
			continue
		}
		diff := p.local - r
		if diff < 0 {
			diff = -diff
		}
		tolerance := int64(math.Max(2, 0.05*float64(r)))
		if diff > tolerance {
			out = append(out, Mismatch{Metric: p.name, Local: p.local, Instantly: r})
		}
	}
	if out == nil {
		out = []Mismatch{}
	}
	return out
}

// GetVariantAnalytics breaks a campaign down per variant and step.
func (s *Service) GetVariantAnalytics(ctx context.Context, id uuid.UUID) ([]VariantAnalytics, error) {
	if _, err := s.campaign(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.store.Pool().Query(ctx, `
		SELECT v.id, v.name, va.step, count(DISTINCT va.id)::bigint,
		       count(s.id)::bigint, count(DISTINCT s.campaign_lead_id)::bigint,
		       count(s.id) FILTER (WHERE s.first_opened_at IS NOT NULL)::bigint,
		       count(s.id) FILTER (WHERE s.first_clicked_at IS NOT NULL)::bigint,
		       count(s.id) FILTER (WHERE s.replied_at IS NOT NULL AND COALESCE(s.reply_classification,'') NOT IN ('auto_reply','out_of_office'))::bigint,
		       count(s.id) FILTER (WHERE s.reply_classification = 'positive')::bigint,
		       count(s.id) FILTER (WHERE s.bounced_at IS NOT NULL)::bigint,
		       count(s.id) FILTER (WHERE s.unsubscribed_at IS NOT NULL)::bigint
		FROM variant_assignments va
		JOIN campaign_leads cl ON cl.id = va.campaign_lead_id
		JOIN email_variants v ON v.id = va.variant_id
		LEFT JOIN email_sends s ON s.assignment_id = va.id
		WHERE cl.campaign_id = $1
		GROUP BY v.id, v.name, va.step ORDER BY va.step, v.name`, id)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	defer rows.Close()
	out := []VariantAnalytics{}
	for rows.Next() {
		var v VariantAnalytics
		if err := rows.Scan(&v.VariantID, &v.VariantName, &v.Step, &v.Assigned, &v.Sends, &v.UniqueContacts, &v.Opened, &v.Clicked,
			&v.Replied, &v.PositiveReplies, &v.Bounced, &v.Unsubscribed); err != nil {
			return nil, apperr.Internal(err)
		}
		v.finish()
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetComponentAnalytics answers "which subject / hook / CTA works best".
func (s *Service) GetComponentAnalytics(ctx context.Context, componentType string, campaignID *uuid.UUID, since *time.Time) ([]ComponentAnalytics, error) {
	if componentType != "" && !campaign.ValidComponentType(componentType) {
		return nil, apperr.Validation("invalid filter", apperr.FieldError{Field: "type", Message: "must be one of: " + strings.Join(campaign.ComponentTypes, ", ")})
	}
	conds := []string{"TRUE"}
	args := []any{}
	if componentType != "" {
		args = append(args, componentType)
		conds = append(conds, fmt.Sprintf("c.type = $%d", len(args)))
	}
	if campaignID != nil {
		args = append(args, *campaignID)
		conds = append(conds, fmt.Sprintf("s.campaign_id = $%d", len(args)))
	}
	if since != nil {
		args = append(args, *since)
		conds = append(conds, fmt.Sprintf("s.sent_at >= $%d", len(args)))
	}
	rows, err := s.store.Pool().Query(ctx, `
		SELECT c.id, c.type, c.name,
		       count(*)::bigint, count(DISTINCT s.campaign_lead_id)::bigint,
		       count(*) FILTER (WHERE s.first_opened_at IS NOT NULL)::bigint,
		       count(*) FILTER (WHERE s.first_clicked_at IS NOT NULL)::bigint,
		       count(*) FILTER (WHERE s.replied_at IS NOT NULL AND COALESCE(s.reply_classification,'') NOT IN ('auto_reply','out_of_office'))::bigint,
		       count(*) FILTER (WHERE s.reply_classification = 'positive')::bigint,
		       count(*) FILTER (WHERE s.bounced_at IS NOT NULL)::bigint,
		       count(*) FILTER (WHERE s.unsubscribed_at IS NOT NULL)::bigint
		FROM email_sends s
		JOIN variant_assignments va ON va.id = s.assignment_id
		JOIN LATERAL unnest(va.component_ids) AS cid ON TRUE
		JOIN email_components c ON c.id = cid
		WHERE `+strings.Join(conds, " AND ")+`
		GROUP BY c.id, c.type, c.name
		ORDER BY c.type, count(*) FILTER (WHERE s.reply_classification = 'positive') DESC, count(*) DESC`, args...)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	defer rows.Close()
	out := []ComponentAnalytics{}
	for rows.Next() {
		var c ComponentAnalytics
		if err := rows.Scan(&c.ComponentID, &c.Type, &c.Name, &c.Sends, &c.UniqueContacts, &c.Opened, &c.Clicked, &c.Replied,
			&c.PositiveReplies, &c.Bounced, &c.Unsubscribed); err != nil {
			return nil, apperr.Internal(err)
		}
		c.finish()
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetSendingAccountAnalytics answers "which account bounces least".
func (s *Service) GetSendingAccountAnalytics(ctx context.Context) ([]AccountAnalytics, error) {
	rows, err := s.store.Pool().Query(ctx, `
		SELECT s.sending_account_id, COALESCE(s.sending_account_email::text, '(unknown)'), `+metricsSelect+`
		FROM email_sends s
		GROUP BY s.sending_account_id, s.sending_account_email
		ORDER BY count(*) DESC`)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	defer rows.Close()
	out := []AccountAnalytics{}
	for rows.Next() {
		var a AccountAnalytics
		m := &a.Local
		if err := rows.Scan(&a.SendingAccountID, &a.Email, &m.Sends, &m.UniqueContacts, &m.Opened, &m.Clicked, &m.Replied,
			&m.PositiveReplies, &m.Bounced, &m.Unsubscribed); err != nil {
			return nil, apperr.Internal(err)
		}
		m.finish()
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internal(err)
	}
	stats, err := s.store.SumSendingAccountStats(ctx, dateOf(s.now().AddDate(0, 0, -30)))
	if err != nil {
		return nil, apperr.Internal(err)
	}
	byID := map[uuid.UUID]dbgen.SumSendingAccountStatsRow{}
	for _, st := range stats {
		byID[st.SendingAccountID] = st
	}
	for i := range out {
		if !out[i].SendingAccountID.Valid {
			continue
		}
		if st, ok := byID[out[i].SendingAccountID.UUID]; ok {
			out[i].InstantlySent, out[i].InstantlyBounced, out[i].InstantlyReplies = st.Sent, st.Bounced, st.UniqueReplies
			out[i].InstantlyBounceRate = ratio(st.Bounced, st.Sent)
		}
	}
	return out, nil
}

// GetFunnel returns the cold → subscriber funnel, optionally for one campaign.
func (s *Service) GetFunnel(ctx context.Context, campaignID *uuid.UUID) ([]FunnelStep, error) {
	byStage := map[string]int64{}
	if campaignID != nil {
		rows, err := s.store.CountCampaignContactsByStage(ctx, *campaignID)
		if err != nil {
			return nil, apperr.Internal(err)
		}
		for _, r := range rows {
			byStage[r.LifecycleStage] = r.Total
		}
	} else {
		rows, err := s.store.CountContactsByStage(ctx)
		if err != nil {
			return nil, apperr.Internal(err)
		}
		for _, r := range rows {
			byStage[r.LifecycleStage] = r.Total
		}
	}
	reached, err := s.store.CountCampaignLeadsEverReached(ctx, campaign.NullUUID(campaignID))
	if err != nil {
		return nil, apperr.Internal(err)
	}
	funnel, _, _, _, _ := buildFunnel(byStage, reached)
	return funnel, nil
}

type localAccountStat struct {
	sends, bounces, replies int64
	last                    *time.Time
}

func (s *Service) localAccountStats(ctx context.Context, since time.Time) (map[string]localAccountStat, error) {
	rows, err := s.store.Pool().Query(ctx, `
		SELECT lower(s.sending_account_email::text), count(*)::bigint,
		       count(*) FILTER (WHERE s.bounced_at IS NOT NULL)::bigint,
		       count(*) FILTER (WHERE s.replied_at IS NOT NULL)::bigint,
		       max(s.sent_at)
		FROM email_sends s WHERE s.sending_account_email IS NOT NULL AND s.sent_at >= $1
		GROUP BY 1`, since)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	defer rows.Close()
	out := map[string]localAccountStat{}
	for rows.Next() {
		var email string
		var st localAccountStat
		if err := rows.Scan(&email, &st.sends, &st.bounces, &st.replies, &st.last); err != nil {
			return nil, apperr.Internal(err)
		}
		out[email] = st
	}
	return out, rows.Err()
}

func dateOf(t time.Time) pgtype.Date {
	return pgtype.Date{Time: t.UTC().Truncate(24 * time.Hour), Valid: true}
}
