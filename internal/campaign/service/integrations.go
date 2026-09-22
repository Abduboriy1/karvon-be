package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/campaign/webhook"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// WebhookStatus describes a registered webhook.
type WebhookStatus struct {
	Registered bool
	URL        string
	ProviderID string
	Status     *int32
	Error      *string
}

// EventSummary counts stored provider events.
type EventSummary struct {
	Total          int64
	Unprocessed    int64
	Errored        int64
	LastReceivedAt *time.Time
}

// ProviderStatus is one provider's health as seen from here.
type ProviderStatus struct {
	SourceID     uuid.UUID
	HasKey       bool
	Enabled      bool
	LastTestedAt *time.Time
	LastTestOK   *bool
	Webhook      WebhookStatus
	Events       EventSummary
	LastSync     *dbgen.SyncRun
}

// IntegrationsStatus is the whole integrations page.
type IntegrationsStatus struct {
	PublicBaseURL string
	Instantly     ProviderStatus
	Mailchimp     ProviderStatus
	Audiences     int
	AI            AIProviderInfo
}

// TestResult is the outcome of a connection test.
type TestResult struct {
	OK            bool
	Detail        string
	AccountsCount int
}

// ProviderEventFilter narrows the event log.
type ProviderEventFilter struct {
	Provider   *string
	Processed  *bool
	HasError   *bool
	ContactID  *uuid.UUID
	CampaignID *uuid.UUID
}

// SendingAccountView is an account with its recent provider stats.
type SendingAccountView struct {
	Account      dbgen.SendingAccount
	Sent30d      int64
	Bounced30d   int64
	Replies30d   int64
	Campaigns    int64
	LastActivity *time.Time
	LocalSends   int64
	LocalBounces int64
	LocalReplies int64
}

// GetIntegrations reports the state of every integration.
func (s *Service) GetIntegrations(ctx context.Context) (IntegrationsStatus, error) {
	out := IntegrationsStatus{PublicBaseURL: s.cfg.PublicBaseURL, AI: s.AIProviderInfo()}
	settings, err := s.store.GetCampaignSettings(ctx)
	if err != nil {
		return out, apperr.Internal(err)
	}
	inst, err := s.instantlySource(ctx)
	if err != nil {
		return out, apperr.Internal(err)
	}
	out.Instantly = providerStatus(inst)
	out.Instantly.Webhook = WebhookStatus{
		Registered: settings.InstantlyWebhookID != nil, URL: campaign.Deref(settings.InstantlyWebhookUrl),
		ProviderID: campaign.Deref(settings.InstantlyWebhookID), Status: settings.InstantlyWebhookStatus, Error: settings.InstantlyWebhookError,
	}
	if out.Instantly.Events, err = s.eventSummary(ctx, campaign.ProviderInstantly); err != nil {
		return out, err
	}
	out.Instantly.LastSync = s.lastSync(ctx, campaign.SyncKindInstantlyCampaign)

	mc, err := s.mailchimpSource(ctx)
	if err != nil {
		return out, apperr.Internal(err)
	}
	out.Mailchimp = providerStatus(mc)
	if out.Mailchimp.Events, err = s.eventSummary(ctx, campaign.ProviderMailchimp); err != nil {
		return out, err
	}
	out.Mailchimp.LastSync = s.lastSync(ctx, campaign.SyncKindMailchimpMembers)
	audiences, err := s.store.ListNewsletterAudiences(ctx)
	if err != nil {
		return out, apperr.Internal(err)
	}
	out.Audiences = len(audiences)
	for _, a := range audiences {
		if a.WebhookID != nil {
			out.Mailchimp.Webhook = WebhookStatus{Registered: true, URL: campaign.Deref(a.WebhookUrl), ProviderID: *a.WebhookID}
			break
		}
	}
	return out, nil
}

func providerStatus(src dbgen.Source) ProviderStatus {
	return ProviderStatus{SourceID: src.ID, HasKey: len(src.ApiKeyEnc) > 0, Enabled: src.Enabled,
		LastTestedAt: src.LastTestedAt, LastTestOK: src.LastTestOk}
}

func (s *Service) eventSummary(ctx context.Context, prov string) (EventSummary, error) {
	row, err := s.store.CountProviderEventsSummary(ctx, prov)
	if err != nil {
		return EventSummary{}, apperr.Internal(err)
	}
	summary := EventSummary{Total: row.Total, Unprocessed: row.Unprocessed, Errored: row.Errored}
	if !row.LastReceivedAt.IsZero() {
		t := row.LastReceivedAt
		summary.LastReceivedAt = &t
	}
	return summary, nil
}

func (s *Service) lastSync(ctx context.Context, kind string) *dbgen.SyncRun {
	row, err := s.store.LastSyncRun(ctx, kind)
	if err != nil {
		return nil
	}
	return &row
}

// TestInstantly proves the stored key works.
func (s *Service) TestInstantly(ctx context.Context) (TestResult, error) {
	source, err := s.instantlySource(ctx)
	if err != nil {
		return TestResult{}, apperr.Internal(err)
	}
	source.Enabled = true
	client, err := s.instantly.For(ctx, source)
	if err != nil {
		s.recordTest(ctx, source.ID, false)
		return TestResult{}, err
	}
	ws, err := client.Ping(ctx)
	if err != nil {
		s.recordTest(ctx, source.ID, false)
		return TestResult{}, providerErr("Instantly rejected the connection test", err)
	}
	s.recordTest(ctx, source.ID, true)
	return TestResult{OK: true, AccountsCount: ws.AccountsCount, Detail: "key accepted; accounts:read scope confirmed"}, nil
}

// TestMailchimp proves the stored key works.
func (s *Service) TestMailchimp(ctx context.Context) (TestResult, error) {
	source, err := s.mailchimpSource(ctx)
	if err != nil {
		return TestResult{}, apperr.Internal(err)
	}
	source.Enabled = true
	client, err := s.mailchimp.For(ctx, source)
	if err != nil {
		s.recordTest(ctx, source.ID, false)
		return TestResult{}, err
	}
	account, err := client.Ping(ctx)
	if err != nil {
		s.recordTest(ctx, source.ID, false)
		return TestResult{}, providerErr("Mailchimp rejected the connection test", err)
	}
	s.recordTest(ctx, source.ID, true)
	return TestResult{OK: true, Detail: strings.TrimSpace(account.AccountName + " " + account.DC)}, nil
}

func (s *Service) recordTest(ctx context.Context, id uuid.UUID, ok bool) {
	if err := s.store.SetSourceTestResult(ctx, dbgen.SetSourceTestResultParams{ID: id, Ok: &ok}); err != nil {
		s.log.Warn("could not record source test result", "source_id", id, "error", err)
	}
}

// RegisterInstantlyWebhook creates (or rotates) the workspace webhook. The secret
// header Instantly will send is generated here and stored encrypted.
func (s *Service) RegisterInstantlyWebhook(ctx context.Context, rotate bool) (WebhookStatus, error) {
	if s.cfg.PublicBaseURL == "" {
		return WebhookStatus{}, apperr.Conflict("KARVON_PUBLIC_BASE_URL must be set before a webhook can be registered")
	}
	settings, err := s.store.GetCampaignSettings(ctx)
	if err != nil {
		return WebhookStatus{}, apperr.Internal(err)
	}
	client, err := s.Instantly(ctx)
	if err != nil {
		return WebhookStatus{}, err
	}
	if settings.InstantlyWebhookID != nil && !rotate {
		if hook, err := client.GetWebhook(ctx, *settings.InstantlyWebhookID); err == nil {
			_ = s.store.SetInstantlyWebhookStatus(ctx, dbgen.SetInstantlyWebhookStatusParams{Status: hookStatus(hook.Status)})
			if hook.Status != nil && *hook.Status < 0 {
				_ = client.ResumeWebhook(ctx, hook.ID)
			}
			return WebhookStatus{Registered: true, URL: hook.TargetHookURL, ProviderID: hook.ID, Status: hookStatus(hook.Status)}, nil
		}
	}
	if settings.InstantlyWebhookID != nil {
		if err := client.DeleteWebhook(ctx, *settings.InstantlyWebhookID); err != nil && !errors.Is(err, provider.ErrNotFound) {
			return WebhookStatus{}, providerErr("Instantly could not remove the old webhook", err)
		}
	}
	token, secret := randomToken(), randomToken()
	url := strings.TrimSuffix(s.cfg.PublicBaseURL, "/") + "/api/v1/webhooks/instantly/" + token
	hook, err := client.CreateWebhook(ctx, instantly.CreateWebhookInput{
		TargetHookURL: url, EventType: "all_events", Name: "Karvon",
		Headers: map[string]string{webhook.SecretHeader: secret},
	})
	if err != nil {
		return WebhookStatus{}, providerErr("Instantly could not create the webhook", err)
	}
	secretEnc, err := s.cipher.EncryptString(secret)
	if err != nil {
		return WebhookStatus{}, apperr.Internal(err)
	}
	if _, err := s.store.SetInstantlyWebhook(ctx, dbgen.SetInstantlyWebhookParams{
		Token: &token, SecretEnc: secretEnc, WebhookID: &hook.ID, WebhookUrl: &url, Status: hookStatus(hook.Status),
	}); err != nil {
		return WebhookStatus{}, apperr.Internal(err)
	}
	return WebhookStatus{Registered: true, URL: url, ProviderID: hook.ID, Status: hookStatus(hook.Status)}, nil
}

// DeleteInstantlyWebhook removes the workspace webhook.
func (s *Service) DeleteInstantlyWebhook(ctx context.Context) error {
	settings, err := s.store.GetCampaignSettings(ctx)
	if err != nil {
		return apperr.Internal(err)
	}
	if settings.InstantlyWebhookID != nil {
		client, err := s.Instantly(ctx)
		if err != nil {
			return err
		}
		if err := client.DeleteWebhook(ctx, *settings.InstantlyWebhookID); err != nil && !errors.Is(err, provider.ErrNotFound) {
			return providerErr("Instantly could not delete the webhook", err)
		}
	}
	if _, err := s.store.SetInstantlyWebhook(ctx, dbgen.SetInstantlyWebhookParams{}); err != nil {
		return apperr.Internal(err)
	}
	return nil
}

func hookStatus(v *int) *int32 {
	if v == nil {
		return nil
	}
	return campaign.Ptr(campaign.Int32(*v))
}

// ListProviderEvents pages the raw event log.
func (s *Service) ListProviderEvents(ctx context.Context, f ProviderEventFilter, page, perPage int) (Page[dbgen.ProviderEvent], error) {
	rows, err := s.store.ListProviderEvents(ctx, dbgen.ListProviderEventsParams{Provider: f.Provider, Processed: f.Processed, HasError: f.HasError,
		ContactID: campaign.NullUUID(f.ContactID), CampaignID: campaign.NullUUID(f.CampaignID),
		Lim: campaign.Int32(perPage), Off: campaign.Int32((page - 1) * perPage)})
	if err != nil {
		return Page[dbgen.ProviderEvent]{}, apperr.Internal(err)
	}
	total, err := s.store.CountProviderEvents(ctx, dbgen.CountProviderEventsParams{Provider: f.Provider, Processed: f.Processed, HasError: f.HasError,
		ContactID: campaign.NullUUID(f.ContactID), CampaignID: campaign.NullUUID(f.CampaignID)})
	if err != nil {
		return Page[dbgen.ProviderEvent]{}, apperr.Internal(err)
	}
	return Page[dbgen.ProviderEvent]{Rows: rows, Total: total}, nil
}

// ReprocessProviderEvent clears an event's outcome and queues it again.
func (s *Service) ReprocessProviderEvent(ctx context.Context, id uuid.UUID) (dbgen.ProviderEvent, error) {
	row, err := s.store.ResetProviderEvent(ctx, id)
	if err != nil {
		return dbgen.ProviderEvent{}, notFound("provider event", err)
	}
	if err := s.enqueue(ctx, campaign.ProcessEventArgs{ProviderEventID: id}); err != nil {
		return dbgen.ProviderEvent{}, err
	}
	return row, nil
}

// ListSyncRuns pages the reconciliation log.
func (s *Service) ListSyncRuns(ctx context.Context, kind *string, page, perPage int) (Page[dbgen.SyncRun], error) {
	rows, err := s.store.ListSyncRuns(ctx, dbgen.ListSyncRunsParams{Kind: kind, Lim: campaign.Int32(perPage), Off: campaign.Int32((page - 1) * perPage)})
	if err != nil {
		return Page[dbgen.SyncRun]{}, apperr.Internal(err)
	}
	total, err := s.store.CountSyncRuns(ctx, kind)
	if err != nil {
		return Page[dbgen.SyncRun]{}, apperr.Internal(err)
	}
	return Page[dbgen.SyncRun]{Rows: rows, Total: total}, nil
}

/* ----------------------------------------------------- sending accounts */

// ListSendingAccounts lists the mirrored accounts with 30-day stats.
func (s *Service) ListSendingAccounts(ctx context.Context, status *int, q *string) ([]SendingAccountView, error) {
	var st *int32
	if status != nil {
		st = campaign.Ptr(campaign.Int32(*status))
	}
	rows, err := s.store.ListSendingAccounts(ctx, dbgen.ListSendingAccountsParams{Status: st, Q: q})
	if err != nil {
		return nil, apperr.Internal(err)
	}
	since := s.now().AddDate(0, 0, -30)
	stats, err := s.store.SumSendingAccountStats(ctx, dateOf(since))
	if err != nil {
		return nil, apperr.Internal(err)
	}
	byID := map[uuid.UUID]dbgen.SumSendingAccountStatsRow{}
	for _, r := range stats {
		byID[r.SendingAccountID] = r
	}
	local, err := s.localAccountStats(ctx, since)
	if err != nil {
		return nil, err
	}
	out := make([]SendingAccountView, 0, len(rows))
	for _, a := range rows {
		v := SendingAccountView{Account: a}
		if st, ok := byID[a.ID]; ok {
			v.Sent30d, v.Bounced30d, v.Replies30d = st.Sent, st.Bounced, st.UniqueReplies
			if st.LastDay.Valid {
				t := st.LastDay.Time
				v.LastActivity = &t
			}
		}
		if l, ok := local[strings.ToLower(a.Email)]; ok {
			v.LocalSends, v.LocalBounces, v.LocalReplies = l.sends, l.bounces, l.replies
			if l.last != nil && (v.LastActivity == nil || l.last.After(*v.LastActivity)) {
				v.LastActivity = l.last
			}
		}
		camps, err := s.store.ListCampaignsForSendingAccount(ctx, a.ID)
		if err != nil {
			return nil, apperr.Internal(err)
		}
		v.Campaigns = int64(len(camps))
		out = append(out, v)
	}
	return out, nil
}

// GetSendingAccount loads one account with daily stats and its campaigns.
func (s *Service) GetSendingAccount(ctx context.Context, id uuid.UUID) (SendingAccountView, []dbgen.SendingAccountStatsDaily, []dbgen.Campaign, error) {
	account, err := s.store.GetSendingAccount(ctx, id)
	if err != nil {
		return SendingAccountView{}, nil, nil, notFound("sending account", err)
	}
	views, err := s.ListSendingAccounts(ctx, nil, campaign.Ptr(account.Email))
	if err != nil {
		return SendingAccountView{}, nil, nil, err
	}
	view := SendingAccountView{Account: account}
	for _, v := range views {
		if v.Account.ID == id {
			view = v
		}
	}
	daily, err := s.store.ListSendingAccountStatsDaily(ctx, dbgen.ListSendingAccountStatsDailyParams{SendingAccountID: id, Since: dateOf(s.now().AddDate(0, 0, -30))})
	if err != nil {
		return SendingAccountView{}, nil, nil, apperr.Internal(err)
	}
	camps, err := s.store.ListCampaignsForSendingAccount(ctx, id)
	if err != nil {
		return SendingAccountView{}, nil, nil, apperr.Internal(err)
	}
	return view, daily, camps, nil
}

// SyncSendingAccounts queues an account refresh.
func (s *Service) SyncSendingAccounts(ctx context.Context) error {
	return s.enqueue(ctx, campaign.SyncAccountsArgs{RequestID: ids.New()})
}

var _ = pgx.ErrNoRows
