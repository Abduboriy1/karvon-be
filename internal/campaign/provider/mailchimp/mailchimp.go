package mailchimp

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

// DefaultBaseURL is the Marketing API root. The literal {dc} is replaced with the
// datacenter derived from the API key.
const DefaultBaseURL = "https://{dc}.api.mailchimp.com/3.0"

// MaxConcurrency is Mailchimp's documented ceiling of simultaneous connections
// per key. Config.Concurrency is clamped to it.
const MaxConcurrency = 10

// maxBodyBytes bounds how much of a response is read. A page of 1000 members is
// well under this; anything larger is a misbehaving server.
const maxBodyBytes = 1 << 20

// defaultRetryAfter is used when a 429 carries no Retry-After header.
const defaultRetryAfter = 30 * time.Second

// Config configures the client.
type Config struct {
	// BaseURL is the API root. It may contain the literal {dc}, which is
	// substituted with the datacenter from the key. Empty means DefaultBaseURL.
	BaseURL string
	// APIKey is the Mailchimp key, "<hex>-<dc>".
	APIKey string
	// Timeout bounds one call, including its retries. Zero means 30s.
	Timeout time.Duration
	// Retries is how many times a failed attempt is repeated. 0 means one attempt.
	Retries int
	// Concurrency caps in-flight requests. Zero or less means 4; more than
	// MaxConcurrency is clamped.
	Concurrency int
	// BreakerThreshold is how many consecutive transport or 5xx failures open the
	// circuit. Zero or less disables the breaker.
	BreakerThreshold int
	// BreakerCooldown is how long the circuit stays open. Zero means one minute.
	BreakerCooldown time.Duration

	HTTPClient *http.Client
	Log        *slog.Logger
	// Now is swappable so the breaker can be tested without sleeping.
	Now func() time.Time
}

// HTTPClient talks to the Marketing API. It is safe for concurrent use.
type HTTPClient struct {
	cfg     Config
	baseURL string
	dc      string
	http    *http.Client
	log     *slog.Logger
	now     func() time.Time

	// sem bounds in-flight requests.
	sem chan struct{}

	mu       sync.Mutex
	failures int
	openTill time.Time
}

// DataCenter is the suffix after the last '-' of a key ("xxxx-us6" → "us6"), or ""
// when the key has none.
func DataCenter(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	i := strings.LastIndex(apiKey, "-")
	if i < 0 {
		return ""
	}
	return apiKey[i+1:]
}

// New builds a client. It never fails: a key without a datacenter produces a client
// whose every call fails with provider.ErrAuth, which is what the settings page
// should show.
func New(cfg Config) *HTTPClient {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.Concurrency > MaxConcurrency {
		cfg.Concurrency = MaxConcurrency
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
		// No client-level timeout: each call carries its own context deadline.
		httpClient = &http.Client{}
	}

	dc := DataCenter(cfg.APIKey)
	return &HTTPClient{
		cfg:     cfg,
		baseURL: strings.TrimSuffix(strings.ReplaceAll(cfg.BaseURL, "{dc}", dc), "/"),
		dc:      dc,
		http:    httpClient,
		log:     cfg.Log,
		now:     cfg.Now,
		sem:     make(chan struct{}, cfg.Concurrency),
	}
}

// BaseURL is the resolved API root, with the datacenter substituted.
func (c *HTTPClient) BaseURL() string { return c.baseURL }

// DC is the datacenter derived from the key.
func (c *HTTPClient) DC() string { return c.dc }

/* ------------------------------------------------------------------- calls */

// Ping implements Client with GET /?fields=account_name,email, which is
// authenticated and so proves the key, where /ping does not.
func (c *HTTPClient) Ping(ctx context.Context) (Account, error) {
	var out Account
	q := url.Values{"fields": {"account_name,email"}}
	if err := c.do(ctx, http.MethodGet, "/", q, nil, &out); err != nil {
		return Account{}, err
	}
	out.DC = c.dc
	return out, nil
}

// ListAudiences implements Client.
func (c *HTTPClient) ListAudiences(ctx context.Context) ([]Audience, error) {
	var out struct {
		Lists []Audience `json:"lists"`
	}
	q := url.Values{
		"count": {"1000"},
		"fields": {"lists.id,lists.web_id,lists.name,lists.double_optin," +
			"lists.stats.member_count,lists.stats.unsubscribe_count,lists.stats.cleaned_count"},
	}
	if err := c.do(ctx, http.MethodGet, "/lists", q, nil, &out); err != nil {
		return nil, err
	}
	return out.Lists, nil
}

// GetAudience implements Client.
func (c *HTTPClient) GetAudience(ctx context.Context, listID string) (Audience, error) {
	var out Audience
	if err := c.do(ctx, http.MethodGet, "/lists/"+url.PathEscape(listID), nil, nil, &out); err != nil {
		return Audience{}, err
	}
	return out, nil
}

// UpsertMember implements Client with PUT /lists/{id}/members/{hash}, the
// idempotent form: a new address is created with StatusIfNew, an existing one is
// updated with the fields given.
func (c *HTTPClient) UpsertMember(ctx context.Context, listID string, in MemberInput) (Member, error) {
	var out Member
	path := "/lists/" + url.PathEscape(listID) + "/members/" + SubscriberHash(in.EmailAddress)
	if err := c.do(ctx, http.MethodPut, path, nil, in, &out); err != nil {
		return Member{}, err
	}
	return out, nil
}

// GetMember implements Client.
func (c *HTTPClient) GetMember(ctx context.Context, listID, hash string) (Member, error) {
	var out Member
	path := "/lists/" + url.PathEscape(listID) + "/members/" + url.PathEscape(hash)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return Member{}, err
	}
	return out, nil
}

// ArchiveMember implements Client. Mailchimp's DELETE archives; the address can be
// re-added later, unlike a permanent delete.
func (c *HTTPClient) ArchiveMember(ctx context.Context, listID, hash string) error {
	path := "/lists/" + url.PathEscape(listID) + "/members/" + url.PathEscape(hash)
	return c.do(ctx, http.MethodDelete, path, nil, nil, nil)
}

// ListMembers implements Client.
func (c *HTTPClient) ListMembers(ctx context.Context, listID string, in ListMembersInput) (MemberPage, error) {
	q := url.Values{}
	if in.Count > 0 {
		q.Set("count", strconv.Itoa(in.Count))
	}
	if in.Offset > 0 {
		q.Set("offset", strconv.Itoa(in.Offset))
	}
	if in.SinceLastChanged != nil {
		q.Set("since_last_changed", in.SinceLastChanged.UTC().Format(time.RFC3339))
	}
	if in.Status != "" {
		q.Set("status", in.Status)
	}
	var out MemberPage
	if err := c.do(ctx, http.MethodGet, "/lists/"+url.PathEscape(listID)+"/members", q, nil, &out); err != nil {
		return MemberPage{}, err
	}
	return out, nil
}

// AddTags implements Client. Mailchimp answers 204 with no body.
func (c *HTTPClient) AddTags(ctx context.Context, listID, hash string, tags []Tag) error {
	body := struct {
		Tags []Tag `json:"tags"`
	}{Tags: tags}
	path := "/lists/" + url.PathEscape(listID) + "/members/" + url.PathEscape(hash) + "/tags"
	return c.do(ctx, http.MethodPost, path, nil, body, nil)
}

// CreateWebhook implements Client.
func (c *HTTPClient) CreateWebhook(ctx context.Context, listID string, in WebhookInput) (Webhook, error) {
	var out Webhook
	if err := c.do(ctx, http.MethodPost, "/lists/"+url.PathEscape(listID)+"/webhooks", nil, in, &out); err != nil {
		return Webhook{}, err
	}
	return out, nil
}

// DeleteWebhook implements Client.
func (c *HTTPClient) DeleteWebhook(ctx context.Context, listID, webhookID string) error {
	path := "/lists/" + url.PathEscape(listID) + "/webhooks/" + url.PathEscape(webhookID)
	return c.do(ctx, http.MethodDelete, path, nil, nil, nil)
}

// ListWebhooks implements Client.
func (c *HTTPClient) ListWebhooks(ctx context.Context, listID string) ([]Webhook, error) {
	var out struct {
		Webhooks []Webhook `json:"webhooks"`
	}
	if err := c.do(ctx, http.MethodGet, "/lists/"+url.PathEscape(listID)+"/webhooks", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Webhooks, nil
}

/* --------------------------------------------------------------- transport */

// apiError is Mailchimp's problem+json body.
type apiError struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail"`
	Instance string `json:"instance"`
}

// do performs one API call with the timeout, the semaphore, the breaker and the
// retries. out may be nil when no body is expected.
func (c *HTTPClient) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	if c.dc == "" {
		return fmt.Errorf("mailchimp: %w: the API key has no datacenter suffix", provider.ErrAuth)
	}
	if open, until := c.circuitOpen(); open {
		return &provider.RetryAfterError{Err: provider.ErrCircuitOpen, After: until.Sub(c.now())}
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return fmt.Errorf("mailchimp: waiting for a request slot: %w", ctx.Err())
	}

	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("mailchimp: encode request: %w", err)
		}
	}

	var lastErr error
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, backoff(attempt)); err != nil {
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
		c.recordFailure(err)
		c.log.Debug("retrying a Mailchimp call",
			"method", method, "path", path, "attempt", attempt+1, "error", err)
	}
	return lastErr
}

// attempt performs exactly one request.
func (c *HTTPClient) attempt(ctx context.Context, method, path string, query url.Values, payload []byte, out any) error {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("mailchimp: build request: %w", err)
	}
	req.SetBasicAuth("anystring", c.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("mailchimp: request: %w", ctxErr)
		}
		return fmt.Errorf("mailchimp: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return mapError(resp.StatusCode, resp.Header.Get("Retry-After"), raw)
	}
	if readErr != nil {
		return fmt.Errorf("mailchimp: read response: %w", readErr)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("mailchimp: decode response: %w", err)
	}
	return nil
}

// mapError turns a non-2xx response into the sentinel the jobs react to.
func mapError(status int, retryAfter string, raw []byte) error {
	var problem apiError
	_ = json.Unmarshal(raw, &problem) // a non-JSON body just leaves it empty
	detail := strings.TrimSpace(problem.Detail)
	if detail == "" {
		detail = strings.TrimSpace(problem.Title)
	}
	if detail == "" {
		detail = provider.Truncate(strings.TrimSpace(string(raw)), 300)
	}

	switch status {
	case http.StatusTooManyRequests:
		return &provider.RetryAfterError{
			Err:   fmt.Errorf("mailchimp: %w: %s", provider.ErrRateLimited, detail),
			After: provider.ParseRetryAfter(retryAfter, defaultRetryAfter),
		}
	case http.StatusUnauthorized:
		return fmt.Errorf("mailchimp: %w: %s", provider.ErrAuth, detail)
	case http.StatusNotFound:
		return fmt.Errorf("mailchimp: %w: %s", provider.ErrNotFound, detail)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		lowerTitle := strings.ToLower(problem.Title)
		lowerDetail := strings.ToLower(problem.Detail)
		switch {
		case strings.Contains(lowerTitle, "member exists"):
			return fmt.Errorf("mailchimp: %w: %s", provider.ErrMemberExists, detail)
		case strings.Contains(lowerTitle, "compliance state"), strings.Contains(lowerDetail, "compliance state"):
			return fmt.Errorf("mailchimp: %w: %s", provider.ErrComplianceState, detail)
		default:
			return fmt.Errorf("mailchimp: %w: %s", provider.ErrInvalid, detail)
		}
	default:
		return &provider.StatusError{Provider: "mailchimp", Code: status, Body: provider.Truncate(string(raw), 300)}
	}
}

// retryable reports whether repeating the request could plausibly help. Sentinels
// and context errors never do; a 5xx or a transport failure might. A 429 is left
// to the queue, which snoozes the whole job rather than spinning here.
func retryable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, provider.ErrRateLimited) {
		return false
	}
	return provider.Retryable(err)
}

// backoff is the delay before attempt n (1-based), doubling from 250ms.
func backoff(attempt int) time.Duration {
	delay := 250 * time.Millisecond << (attempt - 1)
	if delay > 5*time.Second {
		delay = 5 * time.Second
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
		return fmt.Errorf("mailchimp: backoff interrupted: %w", ctx.Err())
	}
}

/* ----------------------------------------------------------------- breaker */

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

// recordFailure counts a transport or 5xx failure; 4xx answers are the caller's
// problem and say nothing about Mailchimp's health.
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
	c.log.Warn("Mailchimp keeps failing; pausing calls",
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

// compile-time proof that the client satisfies the interface.
var _ Client = (*HTTPClient)(nil)
