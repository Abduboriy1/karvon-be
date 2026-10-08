// Package fake is an in-memory Instantly for tests. It keeps campaigns, leads,
// accounts and webhooks in maps, records every call, and lets a test script
// failures per method so the jobs' retry paths can be exercised without a network.
package fake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// defaultPageSize mirrors Instantly's largest page.
const defaultPageSize = 100

// Request is one recorded call.
type Request struct {
	Method string
	Input  any
}

// Client is an in-memory instantly.Client. Its exported maps are the state a test
// seeds before use and inspects afterwards; every method takes the lock, so
// concurrent jobs are safe as long as the test itself does not mutate the maps
// while calls are in flight.
type Client struct {
	mu sync.Mutex

	// Fail queues errors per method name ("AddLeads", "CreateCampaign", ...); each
	// call pops one, so "fail twice then succeed" is a two-element slice.
	Fail map[string][]error

	Campaigns       map[string]instantly.Campaign
	Leads           map[string]instantly.Lead
	Accounts        []instantly.Account
	AccountDaily    []instantly.AccountDaily
	Analytics       map[string]instantly.CampaignAnalytics
	Daily           map[string][]instantly.DailyAnalytics
	Steps           map[string][]instantly.StepAnalytics
	Emails          []instantly.Email
	Webhooks        map[string]instantly.Webhook
	WebhookEvents   []instantly.WebhookEvent
	BackgroundJobs  map[string]instantly.BackgroundJob
	SendingStatuses map[string]instantly.SendingStatus
	// OAuthSessions are the started Google connections by session id; a test
	// finishes one with CompleteOAuth or FailOAuth.
	OAuthSessions map[string]instantly.OAuthStatus
	// WarmupEnabled holds every address EnableWarmup was called for.
	WarmupEnabled map[string]bool

	// InvalidEmails are counted as invalid_email_count and never created.
	InvalidEmails map[string]bool
	// Blocklisted are counted as in_blocklist and never created.
	Blocklisted map[string]bool

	// Now stamps created objects; swap it for a fixed clock.
	Now func() time.Time

	requests []Request
	counter  int
}

// New builds an empty fake.
func New() *Client {
	return &Client{
		Fail:            map[string][]error{},
		Campaigns:       map[string]instantly.Campaign{},
		Leads:           map[string]instantly.Lead{},
		Analytics:       map[string]instantly.CampaignAnalytics{},
		Daily:           map[string][]instantly.DailyAnalytics{},
		Steps:           map[string][]instantly.StepAnalytics{},
		Webhooks:        map[string]instantly.Webhook{},
		BackgroundJobs:  map[string]instantly.BackgroundJob{},
		SendingStatuses: map[string]instantly.SendingStatus{},
		OAuthSessions:   map[string]instantly.OAuthStatus{},
		WarmupEnabled:   map[string]bool{},
		InvalidEmails:   map[string]bool{},
		Blocklisted:     map[string]bool{},
		Now:             time.Now,
	}
}

// Requests returns a copy of every call made so far, in order.
func (c *Client) Requests() []Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Request, len(c.requests))
	copy(out, c.requests)
	return out
}

// Calls counts the recorded calls to one method.
func (c *Client) Calls(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.requests {
		if r.Method == method {
			n++
		}
	}
	return n
}

// record logs the call and pops the next scripted failure, if any. It expects
// the lock to be held.
func (c *Client) record(method string, input any) error {
	c.requests = append(c.requests, Request{Method: method, Input: input})
	queue := c.Fail[method]
	if len(queue) == 0 {
		return nil
	}
	err := queue[0]
	c.Fail[method] = queue[1:]
	return err
}

func (c *Client) nextID(prefix string) string {
	c.counter++
	return fmt.Sprintf("%s%d", prefix, c.counter)
}

/* ---------------------------------------------------------------- workspace */

// Ping implements instantly.Client.
func (c *Client) Ping(_ context.Context) (instantly.Workspace, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("Ping", nil); err != nil {
		return instantly.Workspace{}, err
	}
	return instantly.Workspace{AccountsCount: len(c.Accounts)}, nil
}

/* ---------------------------------------------------------------- campaigns */

// CreateCampaign implements instantly.Client.
func (c *Client) CreateCampaign(_ context.Context, in instantly.CreateCampaignInput) (instantly.Campaign, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("CreateCampaign", in); err != nil {
		return instantly.Campaign{}, err
	}
	now := c.Now()
	campaign := instantly.Campaign{
		ID:               c.nextID("ic_"),
		Name:             in.Name,
		Status:           instantly.CampaignStatusDraft,
		EmailList:        in.EmailList,
		Sequences:        in.Sequences,
		TimestampCreated: now,
		TimestampUpdated: now,
	}
	c.Campaigns[campaign.ID] = campaign
	return campaign, nil
}

// GetCampaign implements instantly.Client.
func (c *Client) GetCampaign(_ context.Context, id string) (instantly.Campaign, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("GetCampaign", id); err != nil {
		return instantly.Campaign{}, err
	}
	campaign, ok := c.Campaigns[id]
	if !ok {
		return instantly.Campaign{}, fmt.Errorf("fake instantly: campaign %s: %w", id, provider.ErrNotFound)
	}
	return campaign, nil
}

// ListCampaigns implements instantly.Client, paging by id.
func (c *Client) ListCampaigns(_ context.Context, startingAfter string) (instantly.CampaignPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("ListCampaigns", startingAfter); err != nil {
		return instantly.CampaignPage{}, err
	}
	campaigns := make([]instantly.Campaign, 0, len(c.Campaigns))
	for _, campaign := range c.Campaigns {
		campaigns = append(campaigns, campaign)
	}
	sort.Slice(campaigns, func(i, j int) bool { return campaigns[i].ID < campaigns[j].ID })
	items, next := paginate(campaigns, func(c instantly.Campaign) string { return c.ID }, 0, startingAfter)
	return instantly.CampaignPage{Items: items, NextStartingAfter: next}, nil
}

// PutCampaign stores a campaign as if it had been started in Instantly's own app,
// with its analytics, safely alongside a running sync.
func (c *Client) PutCampaign(campaign instantly.Campaign, analytics instantly.CampaignAnalytics) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Campaigns == nil {
		c.Campaigns = map[string]instantly.Campaign{}
	}
	if c.Analytics == nil {
		c.Analytics = map[string]instantly.CampaignAnalytics{}
	}
	c.Campaigns[campaign.ID] = campaign
	analytics.CampaignID = campaign.ID
	c.Analytics[campaign.ID] = analytics
}

// UpdateCampaign implements instantly.Client.
func (c *Client) UpdateCampaign(_ context.Context, id string, in instantly.UpdateCampaignInput) (instantly.Campaign, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("UpdateCampaign", in); err != nil {
		return instantly.Campaign{}, err
	}
	campaign, ok := c.Campaigns[id]
	if !ok {
		return instantly.Campaign{}, fmt.Errorf("fake instantly: campaign %s: %w", id, provider.ErrNotFound)
	}
	if in.Name != nil {
		campaign.Name = *in.Name
	}
	if in.Sequences != nil {
		campaign.Sequences = in.Sequences
	}
	if in.EmailList != nil {
		campaign.EmailList = in.EmailList
	}
	campaign.TimestampUpdated = c.Now()
	c.Campaigns[id] = campaign
	return campaign, nil
}

// ActivateCampaign implements instantly.Client.
func (c *Client) ActivateCampaign(_ context.Context, id string) error {
	return c.setCampaignStatus("ActivateCampaign", id, instantly.CampaignStatusActive)
}

// PauseCampaign implements instantly.Client.
func (c *Client) PauseCampaign(_ context.Context, id string) error {
	return c.setCampaignStatus("PauseCampaign", id, instantly.CampaignStatusPaused)
}

func (c *Client) setCampaignStatus(method, id string, status int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record(method, id); err != nil {
		return err
	}
	campaign, ok := c.Campaigns[id]
	if !ok {
		return fmt.Errorf("fake instantly: campaign %s: %w", id, provider.ErrNotFound)
	}
	campaign.Status = status
	campaign.TimestampUpdated = c.Now()
	c.Campaigns[id] = campaign
	return nil
}

// SendingStatus implements instantly.Client. An unseeded campaign reports "healthy".
func (c *Client) SendingStatus(_ context.Context, id string) (instantly.SendingStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("SendingStatus", id); err != nil {
		return instantly.SendingStatus{}, err
	}
	if _, ok := c.Campaigns[id]; !ok {
		return instantly.SendingStatus{}, fmt.Errorf("fake instantly: campaign %s: %w", id, provider.ErrNotFound)
	}
	if status, ok := c.SendingStatuses[id]; ok {
		return status, nil
	}
	return instantly.SendingStatus{Status: "healthy"}, nil
}

/* -------------------------------------------------------------------- leads */

// AddLeads implements instantly.Client.
func (c *Client) AddLeads(_ context.Context, in instantly.AddLeadsInput) (instantly.AddLeadsResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("AddLeads", in); err != nil {
		return instantly.AddLeadsResult{}, err
	}

	existing := map[string]bool{}
	for _, lead := range c.Leads {
		if lead.Campaign == in.CampaignID {
			existing[normalize(lead.Email)] = true
		}
	}

	result := instantly.AddLeadsResult{TotalSent: len(in.Leads), CreatedLeads: []instantly.CreatedLead{}}
	for _, input := range in.Leads {
		email := normalize(input.Email)
		switch {
		case c.InvalidEmails[email] || email == "" || !strings.Contains(email, "@"):
			result.InvalidEmailCount++
			result.InvalidEmails = append(result.InvalidEmails, input.Email)
			continue
		case c.Blocklisted[email]:
			result.InBlocklist++
			continue
		case in.SkipIfInCampaign && existing[email]:
			result.DuplicatedLeads++
			continue
		}

		lead := instantly.Lead{
			ID:       c.nextID("il_"),
			Email:    input.Email,
			Campaign: in.CampaignID,
			Status:   instantly.LeadStatusActive,
			Payload:  leadPayload(input),
		}
		c.Leads[lead.ID] = lead
		existing[email] = true
		result.LeadsUploaded++
		result.CreatedLeads = append(result.CreatedLeads, instantly.CreatedLead{ID: lead.ID, Email: lead.Email})
	}
	return result, nil
}

// leadPayload keeps the input fields the way Instantly echoes them back.
func leadPayload(in instantly.LeadInput) map[string]any {
	payload := map[string]any{}
	set := func(key, value string) {
		if value != "" {
			payload[key] = value
		}
	}
	set("first_name", in.FirstName)
	set("last_name", in.LastName)
	set("company_name", in.CompanyName)
	set("job_title", in.JobTitle)
	set("phone", in.Phone)
	set("website", in.Website)
	for key, value := range in.CustomVariables {
		payload[key] = value
	}
	return payload
}

// ListLeads implements instantly.Client. Pages are ordered by id and the cursor is
// the last id of the previous page, as with Instantly.
func (c *Client) ListLeads(_ context.Context, in instantly.ListLeadsInput) (instantly.LeadPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("ListLeads", in); err != nil {
		return instantly.LeadPage{}, err
	}

	wanted := map[string]bool{}
	for _, email := range in.Contacts {
		wanted[normalize(email)] = true
	}
	var matching []instantly.Lead
	for _, lead := range c.Leads {
		if in.CampaignID != "" && lead.Campaign != in.CampaignID {
			continue
		}
		if len(wanted) > 0 && !wanted[normalize(lead.Email)] {
			continue
		}
		matching = append(matching, lead)
	}
	sort.Slice(matching, func(i, j int) bool { return idLess(matching[i].ID, matching[j].ID) })

	items, next := paginate(matching, func(l instantly.Lead) string { return l.ID }, in.Limit, in.StartingAfter)
	return instantly.LeadPage{Items: items, NextStartingAfter: next}, nil
}

// DeleteLead implements instantly.Client.
func (c *Client) DeleteLead(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("DeleteLead", id); err != nil {
		return err
	}
	if _, ok := c.Leads[id]; !ok {
		return fmt.Errorf("fake instantly: lead %s: %w", id, provider.ErrNotFound)
	}
	delete(c.Leads, id)
	return nil
}

// UpdateInterestStatus implements instantly.Client. It updates every lead with that
// address (in the given campaign when one is named).
func (c *Client) UpdateInterestStatus(_ context.Context, in instantly.InterestInput) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("UpdateInterestStatus", in); err != nil {
		return err
	}
	now := c.Now()
	for id, lead := range c.Leads {
		if normalize(lead.Email) != normalize(in.LeadEmail) {
			continue
		}
		if in.CampaignID != "" && lead.Campaign != in.CampaignID {
			continue
		}
		lead.InterestStatus = in.InterestValue
		lead.LastInterestChange = &now
		c.Leads[id] = lead
	}
	return nil
}

// GetBackgroundJob implements instantly.Client.
func (c *Client) GetBackgroundJob(_ context.Context, id string) (instantly.BackgroundJob, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("GetBackgroundJob", id); err != nil {
		return instantly.BackgroundJob{}, err
	}
	job, ok := c.BackgroundJobs[id]
	if !ok {
		return instantly.BackgroundJob{}, fmt.Errorf("fake instantly: background job %s: %w", id, provider.ErrNotFound)
	}
	return job, nil
}

/* ----------------------------------------------------------------- accounts */

// ListAccounts implements instantly.Client, paging by email.
func (c *Client) ListAccounts(_ context.Context, startingAfter string) (instantly.AccountPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("ListAccounts", startingAfter); err != nil {
		return instantly.AccountPage{}, err
	}
	accounts := make([]instantly.Account, len(c.Accounts))
	copy(accounts, c.Accounts)
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].Email < accounts[j].Email })
	items, next := paginate(accounts, func(a instantly.Account) string { return a.Email }, 0, startingAfter)
	return instantly.AccountPage{Items: items, NextStartingAfter: next}, nil
}

// StartGoogleOAuth implements instantly.Client with a pending session.
func (c *Client) StartGoogleOAuth(_ context.Context) (instantly.OAuthSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("StartGoogleOAuth", nil); err != nil {
		return instantly.OAuthSession{}, err
	}
	id := c.nextID("oauth-")
	c.OAuthSessions[id] = instantly.OAuthStatus{Status: instantly.OAuthPending}
	return instantly.OAuthSession{
		SessionID: id,
		AuthURL:   "https://accounts.google.com/o/oauth2/v2/auth?client_id=instantly&state=api_session:" + id,
		// The caller compares the expiry with its own clock, as it does Instantly's, so
		// this one is stamped with the wall clock rather than c.Now.
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}, nil
}

// OAuthSessionStatus implements instantly.Client.
func (c *Client) OAuthSessionStatus(_ context.Context, sessionID string) (instantly.OAuthStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("OAuthSessionStatus", sessionID); err != nil {
		return instantly.OAuthStatus{}, err
	}
	status, ok := c.OAuthSessions[sessionID]
	if !ok {
		return instantly.OAuthStatus{}, fmt.Errorf("fake instantly: oauth session %s: %w", sessionID, provider.ErrNotFound)
	}
	return status, nil
}

// CompleteOAuth finishes a session the way a person signing in as email would,
// and adds the account.
func (c *Client) CompleteOAuth(sessionID, email string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.OAuthSessions[sessionID] = instantly.OAuthStatus{Status: instantly.OAuthSuccess, Email: email, AccountID: email}
	c.Accounts = append(c.Accounts, instantly.Account{Email: email, Status: 1})
}

// FailOAuth ends a session with an error code.
func (c *Client) FailOAuth(sessionID, code, description string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.OAuthSessions[sessionID] = instantly.OAuthStatus{Status: instantly.OAuthError, Error: code, ErrorDescription: description}
}

// EnableWarmup implements instantly.Client.
func (c *Client) EnableWarmup(_ context.Context, emails []string) (instantly.BackgroundJob, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("EnableWarmup", emails); err != nil {
		return instantly.BackgroundJob{}, err
	}
	for _, email := range emails {
		c.WarmupEnabled[email] = true
	}
	job := instantly.BackgroundJob{ID: c.nextID("job-"), Status: "pending", Type: "warmup_enable"}
	c.BackgroundJobs[job.ID] = job
	return job, nil
}

// AccountDailyAnalytics implements instantly.Client.
func (c *Client) AccountDailyAnalytics(_ context.Context, from, to time.Time, emails []string) ([]instantly.AccountDaily, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("AccountDailyAnalytics", emails); err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, email := range emails {
		wanted[normalize(email)] = true
	}
	fromDay, toDay := from.Format("2006-01-02"), to.Format("2006-01-02")
	var out []instantly.AccountDaily
	for _, row := range c.AccountDaily {
		if len(wanted) > 0 && !wanted[normalize(row.EmailAccount)] {
			continue
		}
		if row.Date != "" && (row.Date < fromDay || row.Date > toDay) {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

/* ---------------------------------------------------------------- analytics */

// CampaignAnalytics implements instantly.Client.
func (c *Client) CampaignAnalytics(_ context.Context, ids []string) ([]instantly.CampaignAnalytics, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("CampaignAnalytics", ids); err != nil {
		return nil, err
	}
	var out []instantly.CampaignAnalytics
	for _, id := range ids {
		if row, ok := c.Analytics[id]; ok {
			out = append(out, row)
		}
	}
	return out, nil
}

// CampaignDailyAnalytics implements instantly.Client.
func (c *Client) CampaignDailyAnalytics(_ context.Context, id string, from, to time.Time) ([]instantly.DailyAnalytics, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("CampaignDailyAnalytics", id); err != nil {
		return nil, err
	}
	fromDay, toDay := from.Format("2006-01-02"), to.Format("2006-01-02")
	var out []instantly.DailyAnalytics
	for _, row := range c.Daily[id] {
		if row.Date != "" && (row.Date < fromDay || row.Date > toDay) {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// CampaignStepAnalytics implements instantly.Client.
func (c *Client) CampaignStepAnalytics(_ context.Context, id string) ([]instantly.StepAnalytics, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("CampaignStepAnalytics", id); err != nil {
		return nil, err
	}
	return append([]instantly.StepAnalytics(nil), c.Steps[id]...), nil
}

/* ------------------------------------------------------------------- emails */

// ListEmails implements instantly.Client. email_type narrows on ue_type the way
// Instantly does: received is 2, sent is a campaign send (1), manual is 3.
func (c *Client) ListEmails(_ context.Context, in instantly.ListEmailsInput) (instantly.EmailPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("ListEmails", in); err != nil {
		return instantly.EmailPage{}, err
	}
	var matching []instantly.Email
	for _, email := range c.Emails {
		if in.CampaignID != "" && email.CampaignID != in.CampaignID {
			continue
		}
		if in.Lead != "" && normalize(email.LeadEmail) != normalize(in.Lead) {
			continue
		}
		if !emailTypeMatches(in.EmailType, email.UEType) {
			continue
		}
		if thread, ok := strings.CutPrefix(in.Search, "thread:"); ok {
			if email.ThreadID != thread {
				continue
			}
		} else if in.Search != "" && normalize(email.LeadEmail) != normalize(in.Search) {
			continue
		}
		if in.MinTimestampCreated != nil && !email.TimestampCreated.After(*in.MinTimestampCreated) {
			continue
		}
		matching = append(matching, email)
	}
	sort.Slice(matching, func(i, j int) bool {
		if in.SortOrder == "desc" {
			i, j = j, i
		}
		if !matching[i].TimestampEmail.Equal(matching[j].TimestampEmail) {
			return matching[i].TimestampEmail.Before(matching[j].TimestampEmail)
		}
		return idLess(matching[i].ID, matching[j].ID)
	})
	items, next := paginate(matching, func(e instantly.Email) string { return e.ID }, in.Limit, in.StartingAfter)
	return instantly.EmailPage{Items: items, NextStartingAfter: next}, nil
}

func emailTypeMatches(want string, ueType int) bool {
	switch want {
	case "":
		return true
	case "received":
		return ueType == instantly.EmailTypeReceived
	case "sent":
		return ueType == instantly.EmailTypeSentFromCampaign
	case "manual":
		return ueType == instantly.EmailTypeSent
	default:
		return false
	}
}

// AddEmails puts emails in the fake Unibox, safely alongside a running sync.
func (c *Client) AddEmails(emails ...instantly.Email) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Emails = append(c.Emails, emails...)
}

// MarkThreadRead implements instantly.Client.
func (c *Client) MarkThreadRead(_ context.Context, threadID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("MarkThreadRead", threadID); err != nil {
		return err
	}
	unread := 0
	for i := range c.Emails {
		if c.Emails[i].ThreadID == threadID {
			c.Emails[i].IsUnread = &unread
		}
	}
	return nil
}

/* ----------------------------------------------------------------- webhooks */

// CreateWebhook implements instantly.Client.
func (c *Client) CreateWebhook(_ context.Context, in instantly.CreateWebhookInput) (instantly.Webhook, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("CreateWebhook", in); err != nil {
		return instantly.Webhook{}, err
	}
	status := instantly.WebhookStatusActive
	hook := instantly.Webhook{
		ID:            c.nextID("iw_"),
		Name:          in.Name,
		TargetHookURL: in.TargetHookURL,
		EventType:     in.EventType,
		Status:        &status,
	}
	c.Webhooks[hook.ID] = hook
	return hook, nil
}

// GetWebhook implements instantly.Client.
func (c *Client) GetWebhook(_ context.Context, id string) (instantly.Webhook, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("GetWebhook", id); err != nil {
		return instantly.Webhook{}, err
	}
	hook, ok := c.Webhooks[id]
	if !ok {
		return instantly.Webhook{}, fmt.Errorf("fake instantly: webhook %s: %w", id, provider.ErrNotFound)
	}
	return hook, nil
}

// DeleteWebhook implements instantly.Client.
func (c *Client) DeleteWebhook(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("DeleteWebhook", id); err != nil {
		return err
	}
	if _, ok := c.Webhooks[id]; !ok {
		return fmt.Errorf("fake instantly: webhook %s: %w", id, provider.ErrNotFound)
	}
	delete(c.Webhooks, id)
	return nil
}

// TestWebhook implements instantly.Client.
func (c *Client) TestWebhook(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("TestWebhook", id); err != nil {
		return err
	}
	if _, ok := c.Webhooks[id]; !ok {
		return fmt.Errorf("fake instantly: webhook %s: %w", id, provider.ErrNotFound)
	}
	return nil
}

// ResumeWebhook implements instantly.Client. It clears the error state.
func (c *Client) ResumeWebhook(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("ResumeWebhook", id); err != nil {
		return err
	}
	hook, ok := c.Webhooks[id]
	if !ok {
		return fmt.Errorf("fake instantly: webhook %s: %w", id, provider.ErrNotFound)
	}
	status := instantly.WebhookStatusActive
	hook.Status = &status
	hook.TimestampError = nil
	c.Webhooks[id] = hook
	return nil
}

// ListWebhookEvents implements instantly.Client.
func (c *Client) ListWebhookEvents(_ context.Context, in instantly.WebhookEventsInput) (instantly.WebhookEventPage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.record("ListWebhookEvents", in); err != nil {
		return instantly.WebhookEventPage{}, err
	}
	var matching []instantly.WebhookEvent
	for _, event := range c.WebhookEvents {
		if in.Success != nil && event.Success != *in.Success {
			continue
		}
		day := event.TimestampCreated.Format("2006-01-02")
		if in.From != "" && day < in.From {
			continue
		}
		if in.To != "" && day > in.To {
			continue
		}
		matching = append(matching, event)
	}
	sort.Slice(matching, func(i, j int) bool { return idLess(matching[i].ID, matching[j].ID) })
	items, next := paginate(matching, func(e instantly.WebhookEvent) string { return e.ID }, in.Limit, in.StartingAfter)
	return instantly.WebhookEventPage{Items: items, NextStartingAfter: next}, nil
}

/* ------------------------------------------------------------------ factory */

// StaticFactory hands out one client for every source. Err, when set, is returned
// instead so a test can force the factory to fail.
type StaticFactory struct {
	Client instantly.Client
	Err    error
}

// For implements instantly.Factory.
func (f StaticFactory) For(_ context.Context, _ dbgen.Source) (instantly.Client, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Client, nil
}

/* ------------------------------------------------------------------- helpers */

// paginate returns the page after the cursor and the cursor for the next one,
// empty when this was the last page.
func paginate[T any](all []T, key func(T) string, limit int, startingAfter string) ([]T, string) {
	if limit <= 0 || limit > defaultPageSize {
		limit = defaultPageSize
	}
	start := 0
	if startingAfter != "" {
		for i, item := range all {
			if key(item) == startingAfter {
				start = i + 1
				break
			}
		}
	}
	if start >= len(all) {
		return []T{}, ""
	}
	end := start + limit
	if end > len(all) {
		end = len(all)
	}
	page := make([]T, end-start)
	copy(page, all[start:end])
	next := ""
	if end < len(all) {
		next = key(page[len(page)-1])
	}
	return page, next
}

// idLess orders generated ids ("il_2" before "il_10") numerically by length first,
// so pagination follows creation order.
func idLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func normalize(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// compile-time proof that the fake satisfies the interfaces.
var (
	_ instantly.Client  = (*Client)(nil)
	_ instantly.Factory = StaticFactory{}
)
