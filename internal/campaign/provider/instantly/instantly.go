package instantly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bory/karvon-be/internal/campaign/provider"
)

// maxBodyBytes bounds how much of a response is read, so a misbehaving upstream
// cannot exhaust memory. A 100-item page of leads is well under 1 MiB.
const maxBodyBytes = 1 << 20

// defaultRetryAfter is the wait we assume on a 429 without a Retry-After header;
// Instantly does not document one.
const defaultRetryAfter = 30 * time.Second

// maxBackoff caps the delay between two attempts. A 429 whose Retry-After exceeds it
// is not retried here: the job snoozes for the full wait instead.
const maxBackoff = 5 * time.Second

// dateLayout is how Instantly's analytics endpoints take their date bounds.
const dateLayout = "2006-01-02"

// pageSize is the largest page Instantly serves.
const pageSize = 100

// ErrResponseTooLarge is returned when a response exceeds the body cap. Retrying
// would only download the same oversized payload again.
var ErrResponseTooLarge = errors.New("instantly: response body exceeds the size cap")

// Config configures the HTTP client.
type Config struct {
	// BaseURL is the API root, without a trailing slash. Empty means DefaultBaseURL.
	BaseURL string
	// APIKey is sent as a bearer token.
	APIKey string
	// Timeout bounds one call, including retries. Zero means 30s.
	Timeout time.Duration
	// BulkTimeout bounds the bulk lead calls (add, delete) instead: Instantly
	// takes well over 30s on a batch of a hundred leads and keeps processing after
	// the caller gives up. Zero means three minutes.
	BulkTimeout time.Duration
	// Retries is how many times a failed attempt is repeated. Zero means 3;
	// negative means one attempt.
	Retries int
	// Concurrency caps in-flight requests. Zero or less means 4.
	Concurrency int
	// BreakerThreshold is how many consecutive unavailability failures open the
	// circuit. Zero or less disables the breaker.
	BreakerThreshold int
	// BreakerCooldown is how long the circuit stays open. Zero means one minute.
	BreakerCooldown time.Duration

	HTTPClient *http.Client
	Log        *slog.Logger
	// Now is swappable so the breaker can be tested without sleeping.
	Now func() time.Time
}

// HTTPClient talks to Instantly over HTTPS. It is safe for concurrent use.
type HTTPClient struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger
	now  func() time.Time

	// sem bounds in-flight requests.
	sem chan struct{}

	mu       sync.Mutex
	failures int
	openTill time.Time
}

// New builds a client. It never fails: a misconfigured client fails every call
// rather than preventing the service from starting.
func New(cfg Config) *HTTPClient {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.BulkTimeout <= 0 {
		cfg.BulkTimeout = 3 * time.Minute
	}
	switch {
	case cfg.Retries == 0:
		cfg.Retries = 3
	case cfg.Retries < 0:
		cfg.Retries = 0
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.BreakerCooldown <= 0 {
		cfg.BreakerCooldown = time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		// No client-level timeout: each call carries its own context deadline, so
		// the deadline covers the retries as a whole rather than each attempt.
		httpClient = &http.Client{}
	}

	return &HTTPClient{
		cfg:  cfg,
		http: httpClient,
		log:  cfg.Log,
		now:  cfg.Now,
		sem:  make(chan struct{}, cfg.Concurrency),
	}
}

/* ---------------------------------------------------------------- workspace */

// Ping implements Client. It lists one account, which proves both the key and
// the accounts:read scope the sync jobs depend on.
func (c *HTTPClient) Ping(ctx context.Context) (Workspace, error) {
	page, err := c.listAccounts(ctx, 1, "")
	if err != nil {
		return Workspace{}, err
	}
	return Workspace{AccountsCount: len(page.Items)}, nil
}

/* ---------------------------------------------------------------- campaigns */

// CreateCampaign implements Client.
func (c *HTTPClient) CreateCampaign(ctx context.Context, in CreateCampaignInput) (Campaign, error) {
	body, err := overlay(in, in.Settings)
	if err != nil {
		return Campaign{}, err
	}
	var out Campaign
	if err := c.do(ctx, http.MethodPost, "/campaigns", nil, body, &out); err != nil {
		return Campaign{}, err
	}
	return out, nil
}

// GetCampaign implements Client.
func (c *HTTPClient) GetCampaign(ctx context.Context, id string) (Campaign, error) {
	var out Campaign
	if err := c.do(ctx, http.MethodGet, "/campaigns/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return Campaign{}, err
	}
	return out, nil
}

// campaignPageSize is smaller than pageSize: a campaign carries its whole
// sequence, so a full page of long sequences could exceed the body cap.
const campaignPageSize = 25

// ListCampaigns implements Client.
func (c *HTTPClient) ListCampaigns(ctx context.Context, startingAfter string) (CampaignPage, error) {
	query := url.Values{"limit": {strconv.Itoa(campaignPageSize)}}
	setIf(query, "starting_after", startingAfter)
	var out CampaignPage
	if err := c.do(ctx, http.MethodGet, "/campaigns", query, nil, &out); err != nil {
		return CampaignPage{}, err
	}
	return out, nil
}

// UpdateCampaign implements Client.
func (c *HTTPClient) UpdateCampaign(ctx context.Context, id string, in UpdateCampaignInput) (Campaign, error) {
	body, err := overlay(in, in.Settings)
	if err != nil {
		return Campaign{}, err
	}
	var out Campaign
	if err := c.do(ctx, http.MethodPatch, "/campaigns/"+url.PathEscape(id), nil, body, &out); err != nil {
		return Campaign{}, err
	}
	return out, nil
}

// ActivateCampaign implements Client.
func (c *HTTPClient) ActivateCampaign(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/campaigns/"+url.PathEscape(id)+"/activate", nil, nil, nil)
}

// PauseCampaign implements Client.
func (c *HTTPClient) PauseCampaign(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/campaigns/"+url.PathEscape(id)+"/pause", nil, nil, nil)
}

// SendingStatus implements Client. Only the summary is surfaced; the diagnostics
// block is free-form and changes between releases.
func (c *HTTPClient) SendingStatus(ctx context.Context, id string) (SendingStatus, error) {
	var out struct {
		Summary SendingStatus `json:"summary"`
	}
	path := "/campaigns/" + url.PathEscape(id) + "/sending-status"
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return SendingStatus{}, err
	}
	return out.Summary, nil
}

/* -------------------------------------------------------------------- leads */

// AddLeads implements Client.
func (c *HTTPClient) AddLeads(ctx context.Context, in AddLeadsInput) (AddLeadsResult, error) {
	var out AddLeadsResult
	if err := c.doWithin(ctx, c.cfg.BulkTimeout, http.MethodPost, "/leads/add", nil, in, &out); err != nil {
		return AddLeadsResult{}, err
	}
	return out, nil
}

// ListLeads implements Client.
func (c *HTTPClient) ListLeads(ctx context.Context, in ListLeadsInput) (LeadPage, error) {
	var out LeadPage
	if err := c.do(ctx, http.MethodPost, "/leads/list", nil, in, &out); err != nil {
		return LeadPage{}, err
	}
	return out, nil
}

// DeleteLead implements Client. A lead Instantly no longer has is provider.ErrNotFound.
func (c *HTTPClient) DeleteLead(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/leads/"+url.PathEscape(id), nil, nil, nil)
}

// DeleteLeads implements Client.
func (c *HTTPClient) DeleteLeads(ctx context.Context, in DeleteLeadsInput) (int, error) {
	var out struct {
		Count int `json:"count"`
	}
	if err := c.doWithin(ctx, c.cfg.BulkTimeout, http.MethodDelete, "/leads", nil, in, &out); err != nil {
		return 0, err
	}
	return out.Count, nil
}

// UpdateInterestStatus implements Client. Instantly answers 202 with no useful body.
func (c *HTTPClient) UpdateInterestStatus(ctx context.Context, in InterestInput) error {
	return c.do(ctx, http.MethodPost, "/leads/update-interest-status", nil, in, nil)
}

// GetBackgroundJob implements Client.
func (c *HTTPClient) GetBackgroundJob(ctx context.Context, id string) (BackgroundJob, error) {
	var out BackgroundJob
	if err := c.do(ctx, http.MethodGet, "/background-jobs/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return BackgroundJob{}, err
	}
	return out, nil
}

/* ----------------------------------------------------------------- accounts */

// ListAccounts implements Client. Each Account also carries the undecoded object
// in Raw, so the accounts sync can persist fields this package does not model.
func (c *HTTPClient) ListAccounts(ctx context.Context, startingAfter string) (AccountPage, error) {
	return c.listAccounts(ctx, pageSize, startingAfter)
}

func (c *HTTPClient) listAccounts(ctx context.Context, limit int, startingAfter string) (AccountPage, error) {
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if startingAfter != "" {
		query.Set("starting_after", startingAfter)
	}
	var raw struct {
		Items             []json.RawMessage `json:"items"`
		NextStartingAfter string            `json:"next_starting_after"`
	}
	if err := c.do(ctx, http.MethodGet, "/accounts", query, nil, &raw); err != nil {
		return AccountPage{}, err
	}

	page := AccountPage{
		Items:             make([]Account, 0, len(raw.Items)),
		NextStartingAfter: raw.NextStartingAfter,
	}
	for _, item := range raw.Items {
		var account Account
		if err := json.Unmarshal(item, &account); err != nil {
			return AccountPage{}, fmt.Errorf("instantly: decode account: %w", err)
		}
		if err := json.Unmarshal(item, &account.Raw); err != nil {
			return AccountPage{}, fmt.Errorf("instantly: decode account: %w", err)
		}
		page.Items = append(page.Items, account)
	}
	return page, nil
}

// StartGoogleOAuth implements Client. A repeat after a lost answer only opens a
// second session, which expires unused, so it is safe to retry.
func (c *HTTPClient) StartGoogleOAuth(ctx context.Context) (OAuthSession, error) {
	var out OAuthSession
	if err := c.do(ctx, http.MethodPost, "/oauth/google/init", nil, map[string]any{}, &out); err != nil {
		return OAuthSession{}, err
	}
	if out.SessionID == "" || out.AuthURL == "" {
		return OAuthSession{}, errors.New("instantly: the OAuth session came back without an id or URL")
	}
	return out, nil
}

// OAuthSessionStatus implements Client.
func (c *HTTPClient) OAuthSessionStatus(ctx context.Context, sessionID string) (OAuthStatus, error) {
	var out OAuthStatus
	if err := c.do(ctx, http.MethodGet, "/oauth/session/status/"+url.PathEscape(sessionID), nil, nil, &out); err != nil {
		return OAuthStatus{}, err
	}
	return out, nil
}

// EnableWarmup implements Client.
func (c *HTTPClient) EnableWarmup(ctx context.Context, emails []string) (BackgroundJob, error) {
	var out BackgroundJob
	if err := c.do(ctx, http.MethodPost, "/accounts/warmup/enable", nil, map[string]any{"emails": emails}, &out); err != nil {
		return BackgroundJob{}, err
	}
	return out, nil
}

// AccountDailyAnalytics implements Client.
func (c *HTTPClient) AccountDailyAnalytics(ctx context.Context, from, to time.Time, emails []string) ([]AccountDaily, error) {
	query := url.Values{
		"start_date": {from.Format(dateLayout)},
		"end_date":   {to.Format(dateLayout)},
	}
	for _, email := range emails {
		query.Add("emails", email)
	}
	var out []AccountDaily
	if err := c.do(ctx, http.MethodGet, "/accounts/analytics/daily", query, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

/* ---------------------------------------------------------------- analytics */

// CampaignAnalytics implements Client.
func (c *HTTPClient) CampaignAnalytics(ctx context.Context, ids []string) ([]CampaignAnalytics, error) {
	query := url.Values{}
	for _, id := range ids {
		query.Add("ids", id)
	}
	var out []CampaignAnalytics
	if err := c.do(ctx, http.MethodGet, "/campaigns/analytics", query, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CampaignDailyAnalytics implements Client.
func (c *HTTPClient) CampaignDailyAnalytics(ctx context.Context, id string, from, to time.Time) ([]DailyAnalytics, error) {
	query := url.Values{
		"campaign_id": {id},
		"start_date":  {from.Format(dateLayout)},
		"end_date":    {to.Format(dateLayout)},
	}
	var out []DailyAnalytics
	if err := c.do(ctx, http.MethodGet, "/campaigns/analytics/daily", query, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CampaignStepAnalytics implements Client.
func (c *HTTPClient) CampaignStepAnalytics(ctx context.Context, id string) ([]StepAnalytics, error) {
	query := url.Values{"campaign_id": {id}}
	var out []StepAnalytics
	if err := c.do(ctx, http.MethodGet, "/campaigns/analytics/steps", query, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

/* ------------------------------------------------------------------- emails */

// ListEmails implements Client.
func (c *HTTPClient) ListEmails(ctx context.Context, in ListEmailsInput) (EmailPage, error) {
	query := url.Values{}
	setIf(query, "campaign_id", in.CampaignID)
	setIf(query, "email_type", in.EmailType)
	setIf(query, "lead", in.Lead)
	setIf(query, "starting_after", in.StartingAfter)
	setIf(query, "sort_order", in.SortOrder)
	setIf(query, "search", in.Search)
	if in.MinTimestampCreated != nil {
		query.Set("min_timestamp_created", in.MinTimestampCreated.UTC().Format(time.RFC3339Nano))
	}
	if in.Limit > 0 {
		query.Set("limit", strconv.Itoa(in.Limit))
	}
	var out EmailPage
	if err := c.do(ctx, http.MethodGet, "/emails", query, nil, &out); err != nil {
		return EmailPage{}, err
	}
	return out, nil
}

// MarkThreadRead implements Client.
func (c *HTTPClient) MarkThreadRead(ctx context.Context, threadID string) error {
	return c.do(ctx, http.MethodPost, "/emails/threads/"+url.PathEscape(threadID)+"/mark-as-read", nil, nil, nil)
}

/* ----------------------------------------------------------------- webhooks */

// CreateWebhook implements Client.
func (c *HTTPClient) CreateWebhook(ctx context.Context, in CreateWebhookInput) (Webhook, error) {
	var out Webhook
	if err := c.do(ctx, http.MethodPost, "/webhooks", nil, in, &out); err != nil {
		return Webhook{}, err
	}
	return out, nil
}

// GetWebhook implements Client.
func (c *HTTPClient) GetWebhook(ctx context.Context, id string) (Webhook, error) {
	var out Webhook
	if err := c.do(ctx, http.MethodGet, "/webhooks/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return Webhook{}, err
	}
	return out, nil
}

// DeleteWebhook implements Client.
func (c *HTTPClient) DeleteWebhook(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/webhooks/"+url.PathEscape(id), nil, nil, nil)
}

// TestWebhook implements Client.
func (c *HTTPClient) TestWebhook(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/webhooks/"+url.PathEscape(id)+"/test", nil, nil, nil)
}

// ResumeWebhook implements Client.
func (c *HTTPClient) ResumeWebhook(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/webhooks/"+url.PathEscape(id)+"/resume", nil, nil, nil)
}

// ListWebhookEvents implements Client.
func (c *HTTPClient) ListWebhookEvents(ctx context.Context, in WebhookEventsInput) (WebhookEventPage, error) {
	query := url.Values{}
	if in.Success != nil {
		query.Set("success", strconv.FormatBool(*in.Success))
	}
	setIf(query, "from", in.From)
	setIf(query, "to", in.To)
	setIf(query, "starting_after", in.StartingAfter)
	if in.Limit > 0 {
		query.Set("limit", strconv.Itoa(in.Limit))
	}
	var out WebhookEventPage
	if err := c.do(ctx, http.MethodGet, "/webhook-events", query, nil, &out); err != nil {
		return WebhookEventPage{}, err
	}
	return out, nil
}

/* --------------------------------------------------------------------- wire */

func (c *HTTPClient) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	return c.doWithin(ctx, c.cfg.Timeout, method, path, query, body, out)
}

// do performs one API call: a slot from the semaphore, a deadline that covers every
// attempt, retries on transient failures, and the status-to-sentinel mapping. A
// nil body sends no payload; a nil out discards the response.
func (c *HTTPClient) doWithin(ctx context.Context, timeout time.Duration, method, path string, query url.Values, body, out any) error {
	if open, until := c.circuitOpen(); open {
		return fmt.Errorf("instantly: %w (until %s)", provider.ErrCircuitOpen, until.Format(time.TimeOnly))
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Hold a slot for the whole call. Waiting for one is part of the timeout.
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return fmt.Errorf("instantly: waiting for a request slot: %w", ctx.Err())
	}

	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("instantly: encode %s %s: %w", method, path, err)
		}
		payload = encoded
	}

	var lastErr error
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, delayBefore(attempt, lastErr)); err != nil {
				return err
			}
		}
		err := c.attempt(ctx, method, path, query, payload, out)
		if err == nil {
			c.recordSuccess()
			return nil
		}
		lastErr = err
		if !retryable(err) {
			break
		}
		c.log.Debug("retrying an Instantly request",
			"method", method, "path", path, "attempt", attempt+1, "error", err)
	}
	if unavailable(lastErr) {
		c.recordFailure(lastErr)
	}
	return lastErr
}

// attempt performs exactly one request.
func (c *HTTPClient) attempt(ctx context.Context, method, path string, query url.Values, payload []byte, out any) error {
	target := c.cfg.BaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("instantly: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Surface the context cause rather than the wrapped transport error, so
		// the caller can tell a timeout from a refused connection.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("instantly: %s %s: %w", method, path, ctxErr)
		}
		return fmt.Errorf("instantly: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read one byte past the cap so an oversized body is detected rather than
	// silently truncated into a decode error.
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if len(raw) > maxBodyBytes {
		return fmt.Errorf("instantly: %s %s: %w", method, path, ErrResponseTooLarge)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapStatus(method, path, resp, raw)
	}
	if readErr != nil {
		return fmt.Errorf("instantly: read %s %s: %w", method, path, readErr)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("instantly: decode %s %s: %w", method, path, err)
	}
	return nil
}

// mapStatus turns a non-2xx response into the sentinel the jobs react to.
func mapStatus(method, path string, resp *http.Response, raw []byte) error {
	detail := provider.Truncate(strings.TrimSpace(string(raw)), 300)
	wrap := func(sentinel error) error {
		if detail == "" {
			return fmt.Errorf("instantly: %s %s: %w", method, path, sentinel)
		}
		return fmt.Errorf("instantly: %s %s: %w: %s", method, path, sentinel, detail)
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return wrap(provider.ErrAuth)
	case http.StatusPaymentRequired:
		return wrap(provider.ErrPaymentRequired)
	case http.StatusNotFound:
		return wrap(provider.ErrNotFound)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return wrap(provider.ErrInvalid)
	case http.StatusTooManyRequests:
		return &provider.RetryAfterError{
			Err:   provider.ErrRateLimited,
			After: provider.ParseRetryAfter(resp.Header.Get("Retry-After"), defaultRetryAfter),
		}
	default:
		return &provider.StatusError{Provider: "instantly", Code: resp.StatusCode, Body: detail}
	}
}

/* ------------------------------------------------------------------ breaker */

// circuitOpen reports whether the breaker is currently short-circuiting calls.
func (c *HTTPClient) circuitOpen() (bool, time.Time) {
	if c.cfg.BreakerThreshold <= 0 {
		return false, time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.openTill.IsZero() || !c.now().Before(c.openTill) {
		return false, time.Time{}
	}
	return true, c.openTill
}

func (c *HTTPClient) recordFailure(err error) {
	if c.cfg.BreakerThreshold <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures++
	if c.failures < c.cfg.BreakerThreshold {
		return
	}
	c.openTill = c.now().Add(c.cfg.BreakerCooldown)
	c.failures = 0
	c.log.Warn("Instantly keeps failing; pausing calls",
		"cooldown", c.cfg.BreakerCooldown, "error", err)
}

func (c *HTTPClient) recordSuccess() {
	if c.cfg.BreakerThreshold <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures = 0
	c.openTill = time.Time{}
}

/* ------------------------------------------------------------------- helpers */

// overlay marshals a body struct and lays the free-form settings over it at the
// top level, which is where Instantly expects the optional campaign flags.
func overlay(body any, settings map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("instantly: encode campaign: %w", err)
	}
	merged := map[string]any{}
	if err := json.Unmarshal(encoded, &merged); err != nil {
		return nil, fmt.Errorf("instantly: encode campaign: %w", err)
	}
	for key, value := range settings {
		merged[key] = value
	}
	return merged, nil
}

func setIf(query url.Values, key, value string) {
	if value != "" {
		query.Set(key, value)
	}
}

// retryable reports whether repeating the request could plausibly help. A 4xx other
// than 429 is our mistake and will fail identically every time; a 429 is only worth
// waiting out here when the suggested wait is short.
func retryable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, ErrResponseTooLarge) {
		return false
	}
	if wait, ok := provider.RetryAfter(err); ok {
		return wait <= maxBackoff
	}
	return provider.Retryable(err)
}

// unavailable reports whether a failure says Instantly itself is unreachable or
// broken, which is the only kind worth opening the circuit over. Being throttled
// or making a bad request is not.
func unavailable(err error) bool {
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		return false
	case errors.Is(err, context.DeadlineExceeded):
		return true
	case errors.Is(err, provider.ErrAuth), errors.Is(err, provider.ErrPaymentRequired),
		errors.Is(err, provider.ErrNotFound), errors.Is(err, provider.ErrInvalid),
		errors.Is(err, provider.ErrRateLimited), errors.Is(err, ErrResponseTooLarge):
		return false
	}
	var status *provider.StatusError
	if errors.As(err, &status) {
		return status.Code >= 500
	}
	// A transport or decode failure.
	return true
}

// delayBefore is the wait before attempt n (1-based): exponential from 250ms,
// capped, and never shorter than what a Retry-After asked for.
func delayBefore(attempt int, lastErr error) time.Duration {
	delay := backoff(attempt)
	if wait, ok := provider.RetryAfter(lastErr); ok && wait > delay {
		delay = wait
	}
	return delay
}

// backoff is the delay before attempt n (1-based), doubling from 250ms.
func backoff(attempt int) time.Duration {
	delay := 250 * time.Millisecond << (attempt - 1)
	if delay > maxBackoff {
		delay = maxBackoff
	}
	return delay
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("instantly: backoff interrupted: %w", ctx.Err())
	}
}

// compile-time proof that the client satisfies the interface.
var _ Client = (*HTTPClient)(nil)
