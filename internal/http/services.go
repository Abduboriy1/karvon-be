package httpapi

import (
	"context"
	"io"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/business"
	"github.com/bory/karvon-be/internal/campaign/ai"
	campaignsvc "github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/category"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/exclusion"
	"github.com/bory/karvon-be/internal/registrar"
	"github.com/bory/karvon-be/internal/scraper"
	"github.com/bory/karvon-be/internal/source"
	"github.com/bory/karvon-be/internal/stats"
	"github.com/bory/karvon-be/internal/verify"
	"github.com/bory/karvon-be/internal/workspace"
)

// The HTTP layer depends on these interfaces rather than on the concrete services, so
// handlers can be exercised with stubs and the transport stays free of business logic.

// JobService is the behaviour behind /jobs.
type JobService interface {
	Estimate(ctx context.Context, in scraper.CreateInput) (scraper.Estimate, error)
	Create(ctx context.Context, in scraper.CreateInput) (db.JobRow, error)
	Get(ctx context.Context, id uuid.UUID) (db.JobRow, error)
	List(ctx context.Context, filter db.JobFilter, sort string, page, perPage int) (scraper.ListResult, error)
	Cancel(ctx context.Context, id uuid.UUID) (db.JobRow, error)
	Rerun(ctx context.Context, id uuid.UUID) (db.JobRow, error)
	Recrawl(ctx context.Context, id uuid.UUID, targets []string) (db.JobRow, error)
	RecrawlBusinesses(ctx context.Context, filter db.BusinessFilter, targets []string) (db.JobRow, error)
	SocialScrapeBusinesses(ctx context.Context, in scraper.SocialScrapeInput) (db.JobRow, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// BusinessService is the behaviour behind /businesses.
type BusinessService interface {
	List(ctx context.Context, filter db.BusinessFilter, sort string, page, perPage int) (business.ListResult, error)
	Get(ctx context.Context, id uuid.UUID) (business.Detail, error)
	Update(ctx context.Context, id uuid.UUID, in business.UpdateInput) (business.Detail, error)
	Bulk(ctx context.Context, ids []uuid.UUID, action business.BulkAction) (int64, error)
	Export(ctx context.Context, filter db.BusinessFilter, dst io.Writer, flush func()) (int, error)
}

// SourceService is the behaviour behind /sources.
type SourceService interface {
	List(ctx context.Context) ([]dbgen.Source, error)
	Get(ctx context.Context, id uuid.UUID) (dbgen.Source, error)
	Update(ctx context.Context, id uuid.UUID, in source.UpdateInput) (dbgen.Source, error)
	Test(ctx context.Context, id uuid.UUID) (source.TestResult, error)
}

// CategoryService is the behaviour behind /scrape-categories.
type CategoryService interface {
	List(ctx context.Context) ([]dbgen.ScrapeCategory, error)
	Get(ctx context.Context, id uuid.UUID) (dbgen.ScrapeCategory, error)
	Create(ctx context.Context, in category.Input) (dbgen.ScrapeCategory, error)
	Update(ctx context.Context, id uuid.UUID, in category.Input) (dbgen.ScrapeCategory, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// ExclusionService is the behaviour behind /exclusions.
type ExclusionService interface {
	List(ctx context.Context, f db.ExclusionFilter, sort string, page, perPage int) (exclusion.Page, error)
	Get(ctx context.Context, id uuid.UUID) (exclusion.Rule, error)
	Create(ctx context.Context, in exclusion.Input) (exclusion.Rule, error)
	Preview(ctx context.Context, in exclusion.Input) (exclusion.Preview, error)
	Remove(ctx context.Context, id uuid.UUID, note string) error
	Check(ctx context.Context, subject exclusion.Subject) (*db.ExclusionRef, error)
	CreateBulk(ctx context.Context, in []exclusion.Input) ([]exclusion.BulkResult, error)
	BrandScan(ctx context.Context, in exclusion.BrandScanInput) (exclusion.BrandScanPage, error)
	Dismiss(ctx context.Context, in exclusion.DismissalInput) (dbgen.BrandScanDismissal, error)
	Undismiss(ctx context.Context, id uuid.UUID) error
	ListDismissals(ctx context.Context, groupBy *string, page, perPage int) (exclusion.DismissalPage, error)
}

// VerificationService is the behaviour behind /verification.
type VerificationService interface {
	Config() verify.ServiceConfig
	Settings(ctx context.Context) verify.Settings
	SettingsView(ctx context.Context) (verify.SettingsView, error)
	SaveSettings(ctx context.Context, in verify.Settings) (verify.SettingsView, error)
	Stats(ctx context.Context) (verify.Stats, error)
	List(ctx context.Context, filter db.VerificationFilter, sort string, page, perPage int) (verify.ListResult, error)
	Get(ctx context.Context, id uuid.UUID) (verify.Detail, error)
	VerifyOne(ctx context.Context, id uuid.UUID, pass verify.Pass) (dbgen.VerificationRun, error)
	ApplyTypo(ctx context.Context, id uuid.UUID) (verify.Detail, error)
	EstimateRun(ctx context.Context, pass verify.Pass, filter verify.RunFilter) (verify.Estimate, error)
	CreateRun(ctx context.Context, in verify.CreateRunInput) (dbgen.VerificationRun, error)
	GetRun(ctx context.Context, id uuid.UUID) (dbgen.VerificationRun, error)
	ListRuns(ctx context.Context, pass, status *string, page, perPage int) (verify.RunListResult, error)
	CancelRun(ctx context.Context, id uuid.UUID) (dbgen.VerificationRun, error)
}

// DomainService is the behaviour behind /domains: the Cloudflare connection, search
// and check, purchases, and the domains the account owns.
type DomainService interface {
	Settings(ctx context.Context) (registrar.Settings, error)
	SaveSettings(ctx context.Context, in registrar.SettingsInput) (registrar.Settings, error)
	TestConnection(ctx context.Context) (registrar.TestResult, error)
	Search(ctx context.Context, in registrar.SearchInput) ([]registrar.Offer, error)
	Check(ctx context.Context, names []string) ([]registrar.Offer, error)
	CreatePurchase(ctx context.Context, in registrar.PurchaseInput) (registrar.Purchase, error)
	GetPurchase(ctx context.Context, id uuid.UUID) (registrar.Purchase, error)
	ListPurchases(ctx context.Context, page, perPage int) (registrar.PurchasePage, error)
	ListRegistrations(ctx context.Context) ([]registrar.Registration, error)
	GetRegistration(ctx context.Context, domain string) (registrar.Registration, error)
	UpdateRegistration(ctx context.Context, domain string, autoRenew bool) (registrar.Registration, bool, error)
}

// MailboxService is the behaviour behind /workspace: the Google Workspace connection,
// domain setups, DKIM, and mailbox credentials.
type MailboxService interface {
	Settings(ctx context.Context) (workspace.Settings, error)
	SaveSettings(ctx context.Context, in workspace.SettingsInput) (workspace.Settings, error)
	TestConnection(ctx context.Context) (workspace.TestResult, error)
	CreateSetup(ctx context.Context, in workspace.SetupInput) (workspace.Setup, error)
	GetSetup(ctx context.Context, domain string) (workspace.Setup, error)
	ListSetups(ctx context.Context, page, perPage int) (workspace.SetupPage, error)
	RetrySetup(ctx context.Context, domain string) (workspace.Setup, error)
	PublishDKIM(ctx context.Context, domain, selector, value string) (workspace.Setup, error)
	MailboxCredentials(ctx context.Context, id uuid.UUID) (workspace.Credentials, error)
	AddMailboxes(ctx context.Context, domain string, in []workspace.MailboxInput, confirm bool) (workspace.Setup, error)
	DeleteMailbox(ctx context.Context, id uuid.UUID) error
	ConnectInstantly(ctx context.Context, id uuid.UUID, warmup bool) (workspace.InstantlyConnection, error)
}

// StatsService is the behaviour behind /stats/scraper and /dashboard/report.
type StatsService interface {
	Scraper(ctx context.Context) (stats.Scraper, error)
	Report(ctx context.Context, in stats.ReportInput) (stats.Report, error)
}

// EventStore is the read side the SSE handler needs.
type EventStore interface {
	ListRecentJobEvents(ctx context.Context, arg dbgen.ListRecentJobEventsParams) ([]dbgen.JobEvent, error)
	ListJobEventsAfter(ctx context.Context, arg dbgen.ListJobEventsAfterParams) ([]dbgen.JobEvent, error)
	GetJobStatus(ctx context.Context, id uuid.UUID) (string, error)
}

// CampaignService is the behaviour behind the whole /campaigns area: campaigns and
// their leads, contacts and their consent, reusable content, the AI generator, the
// sending-account mirror, the newsletter stage, analytics, and the inbound webhooks.
//
// It is one interface rather than eight because one service implements all of it;
// the handlers are split by resource.
type CampaignService interface {
	// Campaigns.
	CreateCampaign(ctx context.Context, in campaignsvc.CampaignInput) (db.CampaignRow, error)
	GetCampaign(ctx context.Context, id uuid.UUID) (db.CampaignRow, error)
	GetCampaignDetail(ctx context.Context, id uuid.UUID) (campaignsvc.CampaignDetail, error)
	ListCampaigns(ctx context.Context, f db.CampaignFilter, sort string, page, perPage int) (campaignsvc.Page[db.CampaignRow], error)
	UpdateCampaign(ctx context.Context, id uuid.UUID, in campaignsvc.CampaignInput) (db.CampaignRow, error)
	ArchiveCampaign(ctx context.Context, id uuid.UUID) (db.CampaignRow, error)
	GetChecklist(ctx context.Context, id uuid.UUID) (campaignsvc.Checklist, error)
	LaunchCampaign(ctx context.Context, id uuid.UUID, scheduledAt *time.Time) (db.CampaignRow, error)
	UnscheduleCampaign(ctx context.Context, id uuid.UUID) (db.CampaignRow, error)
	PauseCampaign(ctx context.Context, id uuid.UUID) (db.CampaignRow, error)
	ResumeCampaign(ctx context.Context, id uuid.UUID) (db.CampaignRow, error)
	SyncCampaign(ctx context.Context, id uuid.UUID) error
	SyncCampaigns(ctx context.Context) error
	SetSendingAccounts(ctx context.Context, id uuid.UUID, accountIDs []uuid.UUID) ([]dbgen.SendingAccount, error)
	ListCampaignVariants(ctx context.Context, id uuid.UUID) ([]dbgen.ListCampaignVariantsRow, error)
	SetCampaignVariants(ctx context.Context, id uuid.UUID, items []campaignsvc.VariantWeight) ([]dbgen.ListCampaignVariantsRow, error)

	// Leads.
	EstimateImport(ctx context.Context, campaignID uuid.UUID, f db.ImportFilter) (campaignsvc.ImportResult, error)
	ImportLeads(ctx context.Context, campaignID uuid.UUID, f db.ImportFilter) (campaignsvc.ImportResult, error)
	ListLeads(ctx context.Context, f db.LeadFilter, sort string, page, perPage int) (campaignsvc.Page[db.LeadRow], error)
	GetLead(ctx context.Context, campaignID, leadID uuid.UUID) (campaignsvc.LeadDetail, error)
	RemoveLead(ctx context.Context, campaignID, leadID uuid.UUID) error
	ListCampaignActivity(ctx context.Context, campaignID uuid.UUID, types []string, page, perPage int) (campaignsvc.Page[dbgen.ContactEvent], error)
	ListActivity(ctx context.Context, f campaignsvc.ActivityFilter, page, perPage int) (campaignsvc.Page[dbgen.ContactEvent], error)

	// Contacts and consent.
	ListContacts(ctx context.Context, f db.ContactFilter, sort string, page, perPage int) (campaignsvc.Page[db.ContactRow], error)
	GetContact(ctx context.Context, id uuid.UUID) (campaignsvc.ContactDetail, error)
	UpdateContact(ctx context.Context, id uuid.UUID, in campaignsvc.ContactUpdate) (campaignsvc.ContactDetail, error)
	ContactTimeline(ctx context.Context, id uuid.UUID, page, perPage int) (campaignsvc.Page[dbgen.ContactEvent], error)
	RequestPermission(ctx context.Context, id uuid.UUID, note string) (campaignsvc.ContactDetail, error)
	CaptureConsent(ctx context.Context, id uuid.UUID, in campaignsvc.ConsentInput) (dbgen.ContactConsent, error)
	RevokeConsent(ctx context.Context, contactID, consentID uuid.UUID, reason string) (campaignsvc.ContactDetail, error)
	SuppressContact(ctx context.Context, id uuid.UUID, reason, note string) (campaignsvc.ContactDetail, error)
	LiftSuppression(ctx context.Context, contactID, suppressionID uuid.UUID, note string) (campaignsvc.ContactDetail, error)

	// Content.
	CreateComponent(ctx context.Context, in campaignsvc.ComponentInput) (dbgen.EmailComponent, error)
	GetComponent(ctx context.Context, id uuid.UUID) (dbgen.EmailComponent, error)
	ListComponents(ctx context.Context, f campaignsvc.ComponentFilter, page, perPage int) (campaignsvc.Page[dbgen.EmailComponent], error)
	UpdateComponent(ctx context.Context, id uuid.UUID, in campaignsvc.ComponentInput) (dbgen.EmailComponent, error)
	SetComponentStatus(ctx context.Context, id uuid.UUID, status string) (dbgen.EmailComponent, error)
	GetComponentUsage(ctx context.Context, id uuid.UUID) (campaignsvc.ComponentUsage, error)
	CreateVariant(ctx context.Context, in campaignsvc.VariantInput) (campaignsvc.VariantDetail, error)
	GetVariant(ctx context.Context, id uuid.UUID) (campaignsvc.VariantDetail, error)
	ListVariants(ctx context.Context, f campaignsvc.VariantFilter, page, perPage int) (campaignsvc.Page[dbgen.EmailVariant], error)
	UpdateVariant(ctx context.Context, id uuid.UUID, in campaignsvc.VariantInput) (campaignsvc.VariantDetail, error)
	SetVariantStatus(ctx context.Context, id uuid.UUID, status string) (campaignsvc.VariantDetail, error)
	PreviewVariant(ctx context.Context, id uuid.UUID, contactID *uuid.UUID) (campaignsvc.Preview, error)
	PreviewAssembly(ctx context.Context, refs []campaignsvc.SlotRef, contactID *uuid.UUID) (campaignsvc.Preview, error)

	// AI generator.
	AIProviderInfo(ctx context.Context) (campaignsvc.AIProviderInfo, error)
	StartChatGPTSignIn(ctx context.Context) (string, error)
	FinishChatGPTSignIn(ctx context.Context, state, code, oauthErr string) error
	DisconnectChatGPT(ctx context.Context) error
	CreateGeneration(ctx context.Context, brief ai.Brief, campaignID *uuid.UUID, manual bool) (campaignsvc.GenerationView, error)
	GetGeneration(ctx context.Context, id uuid.UUID) (campaignsvc.GenerationView, error)
	ListGenerations(ctx context.Context, campaignID *uuid.UUID, page, perPage int) (campaignsvc.Page[campaignsvc.GenerationView], error)
	ParseGeneration(ctx context.Context, id uuid.UUID, raw string) (campaignsvc.GenerationView, error)
	ImportGeneration(ctx context.Context, id uuid.UUID, sel campaignsvc.ImportSelection) (campaignsvc.ImportOutcome, error)

	// Sending accounts and integrations.
	ListSendingAccounts(ctx context.Context, status *int, q *string) ([]campaignsvc.SendingAccountView, error)
	GetSendingAccount(ctx context.Context, id uuid.UUID) (campaignsvc.SendingAccountView, []dbgen.SendingAccountStatsDaily, []dbgen.Campaign, error)
	SyncSendingAccounts(ctx context.Context) error
	GetIntegrations(ctx context.Context) (campaignsvc.IntegrationsStatus, error)
	TestInstantly(ctx context.Context) (campaignsvc.TestResult, error)
	TestMailchimp(ctx context.Context) (campaignsvc.TestResult, error)
	RegisterInstantlyWebhook(ctx context.Context, rotate bool) (campaignsvc.WebhookStatus, error)
	DeleteInstantlyWebhook(ctx context.Context) error
	ListProviderEvents(ctx context.Context, f campaignsvc.ProviderEventFilter, page, perPage int) (campaignsvc.Page[dbgen.ProviderEvent], error)
	ReprocessProviderEvent(ctx context.Context, id uuid.UUID) (dbgen.ProviderEvent, error)
	ListSyncRuns(ctx context.Context, kind *string, page, perPage int) (campaignsvc.Page[dbgen.SyncRun], error)

	// Instantly cleanup.
	GetCleanupOverview(ctx context.Context) (campaignsvc.CleanupOverview, error)
	UpdateCleanupSettings(ctx context.Context, in campaignsvc.CleanupSettings) (campaignsvc.CleanupSettings, error)
	PreviewCleanup(ctx context.Context, in campaignsvc.CleanupRequest) (campaignsvc.CleanupPreview, error)
	StartCleanup(ctx context.Context, in campaignsvc.CleanupRequest, trigger string) (dbgen.InstantlyCleanupRun, error)
	ListCleanupRuns(ctx context.Context, page, perPage int) (campaignsvc.Page[dbgen.InstantlyCleanupRun], error)
	GetCleanupRun(ctx context.Context, id uuid.UUID) (dbgen.InstantlyCleanupRun, error)

	// Newsletter.
	ListAudiences(ctx context.Context) ([]dbgen.NewsletterAudience, error)
	SyncAudiences(ctx context.Context) error
	UpdateAudience(ctx context.Context, id uuid.UUID, in campaignsvc.AudienceUpdate) (dbgen.NewsletterAudience, error)
	RegisterMailchimpWebhook(ctx context.Context, id uuid.UUID) (dbgen.NewsletterAudience, error)
	DeleteMailchimpWebhook(ctx context.Context, id uuid.UUID) (dbgen.NewsletterAudience, error)
	ListEligible(ctx context.Context, campaignID, audienceID *uuid.UUID, page, perPage int) (campaignsvc.Page[campaignsvc.EligibleContact], error)
	PushSubscriptions(ctx context.Context, contactIDs []uuid.UUID, audienceID *uuid.UUID) (campaignsvc.PushResult, error)
	ListSubscriptions(ctx context.Context, f campaignsvc.SubscriptionFilter, page, perPage int) (campaignsvc.Page[dbgen.ListNewsletterSubscriptionsRow], error)
	RetrySubscription(ctx context.Context, id uuid.UUID) (dbgen.NewsletterSubscription, error)
	GetNewsletterStats(ctx context.Context) (campaignsvc.NewsletterStats, error)

	// Analytics.
	GetOverview(ctx context.Context) (campaignsvc.Overview, error)
	GetCampaignAnalytics(ctx context.Context, id uuid.UUID, days int) (campaignsvc.CampaignAnalytics, error)
	GetVariantAnalytics(ctx context.Context, id uuid.UUID) ([]campaignsvc.VariantAnalytics, error)
	GetComponentAnalytics(ctx context.Context, componentType string, campaignID *uuid.UUID, since *time.Time) ([]campaignsvc.ComponentAnalytics, error)
	GetSendingAccountAnalytics(ctx context.Context) ([]campaignsvc.AccountAnalytics, error)
	GetFunnel(ctx context.Context, campaignID *uuid.UUID) ([]campaignsvc.FunnelStep, error)

	// Inbox and outreach outcomes.
	ListInbox(ctx context.Context, f db.InboxFilter, sort string, page, perPage int) (campaignsvc.Page[db.InboxRow], error)
	InboxUnreadCount(ctx context.Context) (int64, error)
	GetThread(ctx context.Context, threadID string, refresh bool) (campaignsvc.Thread, error)
	MarkThreadRead(ctx context.Context, threadID string) error
	SyncInbox(ctx context.Context) error
	ListOutreach(ctx context.Context, f db.OutreachFilter, sort string, page, perPage int) (campaignsvc.OutreachPage, error)
	ExportOutreach(ctx context.Context, f db.OutreachFilter, sort string, dst io.Writer, flush func()) (int, error)

	// Inbound webhooks.
	AuthenticateInstantlyWebhook(ctx context.Context, token, secret string) error
	IngestInstantly(ctx context.Context, body []byte, source string) (campaignsvc.IngestResult, error)
	AuthenticateMailchimpWebhook(ctx context.Context, token, signature string, body []byte, tolerance time.Duration) (dbgen.NewsletterAudience, error)
	IngestMailchimp(ctx context.Context, audience dbgen.NewsletterAudience, form url.Values, source string) (campaignsvc.IngestResult, error)
}
