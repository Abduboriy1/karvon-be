// Package instantly is the Instantly API v2 client. It covers exactly what the
// campaign module needs — campaigns, leads, sending accounts, emails, analytics,
// webhooks and background jobs — and nothing that Instantly does better on its own.
//
// Everything Instantly-specific (status codes, field names, pagination cursors) stays
// inside this package; the rest of the module talks to the Client interface.
package instantly

import (
	"context"
	"time"

	"github.com/bory/karvon-be/internal/db/dbgen"
)

// DefaultBaseURL is the production API root.
const DefaultBaseURL = "https://api.instantly.ai/api/v2"

// MaxLeadsPerAdd is Instantly's hard cap on POST /leads/add; the module sends far
// fewer per call (see config).
const MaxLeadsPerAdd = 1000

// Campaign status codes, as Instantly reports them.
const (
	CampaignStatusDraft               = 0
	CampaignStatusActive              = 1
	CampaignStatusPaused              = 2
	CampaignStatusCompleted           = 3
	CampaignStatusRunningSubsequences = 4
	CampaignStatusAccountsUnhealthy   = -1
	CampaignStatusBounceProtect       = -2
	CampaignStatusAccountSuspended    = -99
)

// Lead status codes.
const (
	LeadStatusActive       = 1
	LeadStatusPaused       = 2
	LeadStatusCompleted    = 3
	LeadStatusBounced      = -1
	LeadStatusUnsubscribed = -2
	LeadStatusSkipped      = -3
)

// Lead interest status codes (lt_interest_status).
const (
	InterestOutOfOffice      = 0
	InterestInterested       = 1
	InterestMeetingBooked    = 2
	InterestMeetingCompleted = 3
	InterestWon              = 4
	InterestNotInterested    = -1
	InterestWrongPerson      = -2
	InterestLost             = -3
	InterestNoShow           = -4
)

// Account status codes.
const (
	AccountStatusActive          = 1
	AccountStatusPaused          = 2
	AccountStatusMaintenance     = 3
	AccountStatusConnectionError = -1
	AccountStatusSoftBounceError = -2
	AccountStatusSendingError    = -3
)

// Email ue_type codes.
const (
	EmailTypeSentFromCampaign = 1
	EmailTypeReceived         = 2
	EmailTypeSent             = 3
	EmailTypeScheduled        = 4
)

// Webhook status codes.
const (
	WebhookStatusActive = 1
	WebhookStatusError  = -1
)

// Variant is one subject/body pair inside a sequence step.
type Variant struct {
	Subject  string `json:"subject"`
	Body     string `json:"body"`
	Disabled bool   `json:"v_disabled,omitempty"`
}

// Step is one email in the sequence.
type Step struct {
	Type      string    `json:"type"`
	Delay     int       `json:"delay"`
	DelayUnit string    `json:"delay_unit,omitempty"`
	Variants  []Variant `json:"variants"`
}

// Sequence wraps the steps; Instantly only reads sequences[0].
type Sequence struct {
	Steps []Step `json:"steps"`
}

// ScheduleWindow is one sending window.
type ScheduleWindow struct {
	Name     string          `json:"name"`
	Timing   map[string]any  `json:"timing"`
	Days     map[string]bool `json:"days"`
	Timezone string          `json:"timezone"`
}

// Schedule is Instantly's campaign_schedule object.
type Schedule struct {
	StartDate *string          `json:"start_date,omitempty"`
	EndDate   *string          `json:"end_date,omitempty"`
	Schedules []ScheduleWindow `json:"schedules"`
}

// CreateCampaignInput is the body of POST /campaigns.
type CreateCampaignInput struct {
	Name      string     `json:"name"`
	Schedule  Schedule   `json:"campaign_schedule"`
	Sequences []Sequence `json:"sequences"`
	EmailList []string   `json:"email_list"`
	// Settings carries the optional flags (daily_limit, stop_on_reply, ...) verbatim.
	Settings map[string]any `json:"-"`
}

// UpdateCampaignInput is the body of PATCH /campaigns/{id}; every field optional.
type UpdateCampaignInput struct {
	Name      *string        `json:"name,omitempty"`
	Schedule  *Schedule      `json:"campaign_schedule,omitempty"`
	Sequences []Sequence     `json:"sequences,omitempty"`
	EmailList []string       `json:"email_list,omitempty"`
	Settings  map[string]any `json:"-"`
}

// Campaign is what Instantly returns.
type Campaign struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Status           int        `json:"status"`
	NotSendingStatus *int       `json:"not_sending_status"`
	EmailList        []string   `json:"email_list"`
	Sequences        []Sequence `json:"sequences"`
	TimestampCreated time.Time  `json:"timestamp_created"`
	TimestampUpdated time.Time  `json:"timestamp_updated"`
}

// CampaignPage is one page of GET /campaigns.
type CampaignPage struct {
	Items             []Campaign `json:"items"`
	NextStartingAfter string     `json:"next_starting_after"`
}

// SendingStatus is GET /campaigns/{id}/sending-status.
type SendingStatus struct {
	Status            string     `json:"status"`
	Message           string     `json:"status_message"`
	IssueStartedAt    *time.Time `json:"issue_started_at"`
	LastHealthySendAt *time.Time `json:"last_healthy_send_at"`
}

// LeadInput is one lead in POST /leads/add.
type LeadInput struct {
	Email           string         `json:"email"`
	FirstName       string         `json:"first_name,omitempty"`
	LastName        string         `json:"last_name,omitempty"`
	CompanyName     string         `json:"company_name,omitempty"`
	JobTitle        string         `json:"job_title,omitempty"`
	Phone           string         `json:"phone,omitempty"`
	Website         string         `json:"website,omitempty"`
	CustomVariables map[string]any `json:"custom_variables,omitempty"`
}

// AddLeadsInput is the body of POST /leads/add.
type AddLeadsInput struct {
	CampaignID       string      `json:"campaign_id"`
	Leads            []LeadInput `json:"leads"`
	SkipIfInCampaign bool        `json:"skip_if_in_campaign"`
}

// CreatedLead is one entry of created_leads[].
type CreatedLead struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// AddLeadsResult summarises a bulk add.
type AddLeadsResult struct {
	TotalSent         int           `json:"total_sent"`
	LeadsUploaded     int           `json:"leads_uploaded"`
	DuplicatedLeads   int           `json:"duplicated_leads"`
	SkippedCount      int           `json:"skipped_count"`
	InvalidEmailCount int           `json:"invalid_email_count"`
	InBlocklist       int           `json:"in_blocklist"`
	CreatedLeads      []CreatedLead `json:"created_leads"`
	// InvalidEmails is filled by clients that report which addresses were
	// rejected; the documented response only carries a count.
	InvalidEmails []string `json:"-"`
}

// Lead is what Instantly returns for one lead.
type Lead struct {
	ID                 string         `json:"id"`
	Email              string         `json:"email"`
	Campaign           string         `json:"campaign"`
	Status             int            `json:"status"`
	InterestStatus     *int           `json:"lt_interest_status"`
	VerificationStatus *int           `json:"verification_status"`
	OpenCount          int            `json:"email_open_count"`
	ReplyCount         int            `json:"email_reply_count"`
	ClickCount         int            `json:"email_click_count"`
	LastContact        *time.Time     `json:"timestamp_last_contact"`
	LastOpen           *time.Time     `json:"timestamp_last_open"`
	LastClick          *time.Time     `json:"timestamp_last_click"`
	LastReply          *time.Time     `json:"timestamp_last_reply"`
	LastInterestChange *time.Time     `json:"timestamp_last_interest_change"`
	Payload            map[string]any `json:"payload"`
}

// ListLeadsInput is the body of POST /leads/list.
type ListLeadsInput struct {
	CampaignID    string   `json:"campaign,omitempty"`
	Contacts      []string `json:"contacts,omitempty"`
	Limit         int      `json:"limit,omitempty"`
	StartingAfter string   `json:"starting_after,omitempty"`
}

// LeadPage is one page of leads.
type LeadPage struct {
	Items             []Lead `json:"items"`
	NextStartingAfter string `json:"next_starting_after"`
}

// InterestInput is the body of POST /leads/update-interest-status.
type InterestInput struct {
	LeadEmail     string `json:"lead_email"`
	InterestValue *int   `json:"interest_value"`
	CampaignID    string `json:"campaign_id,omitempty"`
}

// BackgroundJob is GET /background-jobs/{id}.
type BackgroundJob struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Type     string `json:"type"`
	Progress int    `json:"progress"`
}

// Account is one sending account.
type Account struct {
	Email                string         `json:"email"`
	FirstName            string         `json:"first_name"`
	LastName             string         `json:"last_name"`
	ProviderCode         *int           `json:"provider_code"`
	Status               int            `json:"status"`
	WarmupStatus         *int           `json:"warmup_status"`
	DailyLimit           *int           `json:"daily_limit"`
	SendingGap           *int           `json:"sending_gap"`
	WarmupScore          *int           `json:"stat_warmup_score"`
	StatusMessage        *string        `json:"status_message"`
	TrackingDomainName   *string        `json:"tracking_domain_name"`
	TrackingDomainStatus *string        `json:"tracking_domain_status"`
	SetupPending         bool           `json:"setup_pending"`
	IsManagedAccount     bool           `json:"is_managed_account"`
	TimestampCreated     *time.Time     `json:"timestamp_created"`
	Raw                  map[string]any `json:"-"`
}

// AccountPage is one page of accounts.
type AccountPage struct {
	Items             []Account `json:"items"`
	NextStartingAfter string    `json:"next_starting_after"`
}

// AccountDaily is one row of GET /accounts/analytics/daily.
type AccountDaily struct {
	Date              string `json:"date"`
	EmailAccount      string `json:"email_account"`
	Sent              int    `json:"sent"`
	Bounced           int    `json:"bounced"`
	Contacted         int    `json:"contacted"`
	NewLeadsContacted int    `json:"new_leads_contacted"`
	Opened            int    `json:"opened"`
	UniqueOpened      int    `json:"unique_opened"`
	Replies           int    `json:"replies"`
	UniqueReplies     int    `json:"unique_replies"`
	Clicks            int    `json:"clicks"`
	UniqueClicks      int    `json:"unique_clicks"`
}

// CampaignAnalytics is one row of GET /campaigns/analytics.
type CampaignAnalytics struct {
	CampaignID            string  `json:"campaign_id"`
	CampaignName          string  `json:"campaign_name"`
	CampaignStatus        int     `json:"campaign_status"`
	LeadsCount            int     `json:"leads_count"`
	ContactedCount        int     `json:"contacted_count"`
	EmailsSentCount       int     `json:"emails_sent_count"`
	NewLeadsContacted     int     `json:"new_leads_contacted_count"`
	OpenCount             int     `json:"open_count"`
	OpenCountUnique       int     `json:"open_count_unique"`
	ReplyCount            int     `json:"reply_count"`
	ReplyCountUnique      int     `json:"reply_count_unique"`
	ReplyCountAutomatic   int     `json:"reply_count_automatic"`
	LinkClickCount        int     `json:"link_click_count"`
	LinkClickCountUnique  int     `json:"link_click_count_unique"`
	BouncedCount          int     `json:"bounced_count"`
	UnsubscribedCount     int     `json:"unsubscribed_count"`
	CompletedCount        int     `json:"completed_count"`
	TotalOpportunities    int     `json:"total_opportunities"`
	TotalOpportunityValue float64 `json:"total_opportunity_value"`
}

// DailyAnalytics is one row of GET /campaigns/analytics/daily.
type DailyAnalytics struct {
	Date              string `json:"date"`
	Sent              int    `json:"sent"`
	Contacted         int    `json:"contacted"`
	NewLeadsContacted int    `json:"new_leads_contacted"`
	Opened            int    `json:"opened"`
	UniqueOpened      int    `json:"unique_opened"`
	Replies           int    `json:"replies"`
	UniqueReplies     int    `json:"unique_replies"`
	Clicks            int    `json:"clicks"`
	UniqueClicks      int    `json:"unique_clicks"`
	Opportunities     int    `json:"opportunities"`
}

// StepAnalytics is one row of GET /campaigns/analytics/steps.
type StepAnalytics struct {
	Step          string `json:"step"`
	Variant       string `json:"variant"`
	Sent          int    `json:"sent"`
	Opened        int    `json:"opened"`
	UniqueOpened  int    `json:"unique_opened"`
	Replies       int    `json:"replies"`
	UniqueReplies int    `json:"unique_replies"`
	Clicks        int    `json:"clicks"`
	UniqueClicks  int    `json:"unique_clicks"`
}

// Email is one unibox email.
type Email struct {
	ID               string    `json:"id"`
	MessageID        string    `json:"message_id"`
	ThreadID         string    `json:"thread_id"`
	CampaignID       string    `json:"campaign_id"`
	LeadEmail        string    `json:"lead"`
	LeadID           string    `json:"lead_id"`
	EAccount         string    `json:"eaccount"`
	FromAddress      string    `json:"from_address_email"`
	ToAddressList    string    `json:"to_address_email_list"`
	CCAddressList    string    `json:"cc_address_email_list"`
	Subject          string    `json:"subject"`
	Body             EmailBody `json:"body"`
	ContentPreview   string    `json:"content_preview"`
	Step             string    `json:"step"`
	UEType           int       `json:"ue_type"`
	IsUnread         *int      `json:"is_unread"`
	IsAutoReply      int       `json:"is_auto_reply"`
	InterestStatus   *int      `json:"i_status"`
	AIInterestValue  *float64  `json:"ai_interest_value"`
	TimestampEmail   time.Time `json:"timestamp_email"`
	TimestampCreated time.Time `json:"timestamp_created"`
}

// EmailBody is an email's content in both forms Instantly keeps.
type EmailBody struct {
	Text string `json:"text"`
	HTML string `json:"html"`
}

// ListEmailsInput filters GET /emails.
type ListEmailsInput struct {
	CampaignID    string
	EmailType     string // received | sent | manual
	Lead          string
	Limit         int
	StartingAfter string
	SortOrder     string
	// Search is a lead address, or "thread:<thread_id>" for one conversation.
	Search string
	// MinTimestampCreated keeps emails Instantly stored after this instant.
	MinTimestampCreated *time.Time
}

// ThreadSearch is the Search value that lists one thread.
func ThreadSearch(threadID string) string { return "thread:" + threadID }

// EmailPage is one page of emails.
type EmailPage struct {
	Items             []Email `json:"items"`
	NextStartingAfter string  `json:"next_starting_after"`
}

// CreateWebhookInput is the body of POST /webhooks.
type CreateWebhookInput struct {
	TargetHookURL string            `json:"target_hook_url"`
	EventType     string            `json:"event_type"`
	Name          string            `json:"name,omitempty"`
	CampaignID    *string           `json:"campaign,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
}

// Webhook is what Instantly returns.
type Webhook struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	TargetHookURL  string     `json:"target_hook_url"`
	EventType      string     `json:"event_type"`
	Status         *int       `json:"status"`
	TimestampError *time.Time `json:"timestamp_error"`
}

// WebhookEventsInput filters GET /webhook-events.
type WebhookEventsInput struct {
	Success       *bool
	From          string // YYYY-MM-DD
	To            string
	Limit         int
	StartingAfter string
}

// WebhookEvent is one delivery attempt record.
type WebhookEvent struct {
	ID               string         `json:"id"`
	TimestampCreated time.Time      `json:"timestamp_created"`
	Success          bool           `json:"success"`
	RetryCount       int            `json:"retry_count"`
	WillRetry        bool           `json:"will_retry"`
	WebhookURL       string         `json:"webhook_url"`
	StatusCode       *int           `json:"status_code"`
	LeadEmail        string         `json:"lead_email"`
	Payload          map[string]any `json:"payload"`
}

// WebhookEventPage is one page of webhook events.
type WebhookEventPage struct {
	Items             []WebhookEvent `json:"items"`
	NextStartingAfter string         `json:"next_starting_after"`
}

// Workspace is what Ping reports: enough to prove the key and show who we are.
type Workspace struct {
	AccountsCount int
}

// OAuth session states, as GET /oauth/session/status reports them.
const (
	OAuthPending = "pending"
	OAuthSuccess = "success"
	OAuthError   = "error"
	OAuthExpired = "expired"
)

// OAuthSession is a started Google sign-in that connects a mailbox to Instantly.
// A person opens AuthURL and signs in as the mailbox; the session lives ten minutes.
type OAuthSession struct {
	SessionID string    `json:"session_id"`
	AuthURL   string    `json:"auth_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// OAuthStatus is how a session is going. Email and AccountID are set on success,
// Error and ErrorDescription on error ("account_exists", ...).
type OAuthStatus struct {
	Status           string `json:"status"`
	Email            string `json:"email"`
	Name             string `json:"name"`
	AccountID        string `json:"account_id"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Client is everything the module asks Instantly for.
type Client interface {
	Ping(ctx context.Context) (Workspace, error)

	CreateCampaign(ctx context.Context, in CreateCampaignInput) (Campaign, error)
	GetCampaign(ctx context.Context, id string) (Campaign, error)
	// ListCampaigns pages through every campaign in the workspace, including
	// the ones started in Instantly's own app.
	ListCampaigns(ctx context.Context, startingAfter string) (CampaignPage, error)
	UpdateCampaign(ctx context.Context, id string, in UpdateCampaignInput) (Campaign, error)
	ActivateCampaign(ctx context.Context, id string) error
	PauseCampaign(ctx context.Context, id string) error
	SendingStatus(ctx context.Context, id string) (SendingStatus, error)

	AddLeads(ctx context.Context, in AddLeadsInput) (AddLeadsResult, error)
	ListLeads(ctx context.Context, in ListLeadsInput) (LeadPage, error)
	DeleteLead(ctx context.Context, id string) error
	UpdateInterestStatus(ctx context.Context, in InterestInput) error
	GetBackgroundJob(ctx context.Context, id string) (BackgroundJob, error)

	ListAccounts(ctx context.Context, startingAfter string) (AccountPage, error)
	AccountDailyAnalytics(ctx context.Context, from, to time.Time, emails []string) ([]AccountDaily, error)
	// StartGoogleOAuth begins connecting a Google Workspace mailbox (accounts:create).
	StartGoogleOAuth(ctx context.Context) (OAuthSession, error)
	// OAuthSessionStatus reports a started connection (accounts:read).
	OAuthSessionStatus(ctx context.Context, sessionID string) (OAuthStatus, error)
	// EnableWarmup starts warmup for up to 100 accounts, as a background job
	// (accounts:update).
	EnableWarmup(ctx context.Context, emails []string) (BackgroundJob, error)

	CampaignAnalytics(ctx context.Context, ids []string) ([]CampaignAnalytics, error)
	CampaignDailyAnalytics(ctx context.Context, id string, from, to time.Time) ([]DailyAnalytics, error)
	CampaignStepAnalytics(ctx context.Context, id string) ([]StepAnalytics, error)

	ListEmails(ctx context.Context, in ListEmailsInput) (EmailPage, error)
	MarkThreadRead(ctx context.Context, threadID string) error

	CreateWebhook(ctx context.Context, in CreateWebhookInput) (Webhook, error)
	GetWebhook(ctx context.Context, id string) (Webhook, error)
	DeleteWebhook(ctx context.Context, id string) error
	TestWebhook(ctx context.Context, id string) error
	ResumeWebhook(ctx context.Context, id string) error
	ListWebhookEvents(ctx context.Context, in WebhookEventsInput) (WebhookEventPage, error)
}

// Factory builds a client from a stored source row, decrypting the key. Tests swap
// in their own so nothing ever reaches Instantly.
type Factory interface {
	For(ctx context.Context, source dbgen.Source) (Client, error)
}
