// Package cloudflare is the Cloudflare Registrar API client. It covers exactly what
// the domains module needs — search, the authoritative availability check,
// registration and its status, and the registrations the account already owns — plus
// the zone and DNS record calls the workspace module publishes mail records with, and
// keeps every Cloudflare-specific field name and status code inside this package.
//
// Registration is billed the moment it succeeds and is not refundable, so Register is
// the one call this client never repeats on its own: a lost response is reconciled
// by asking Cloudflare what happened, never by sending the request again.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the production API root.
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// Limits Cloudflare documents for the registrar endpoints.
const (
	// MaxCheckDomains is how many names one domain-check call accepts.
	MaxCheckDomains = 20
	// MaxSearchLimit is the most results one domain-search call returns.
	MaxSearchLimit = 50
	// MaxListPerPage is the largest page the registrations list serves.
	MaxListPerPage = 50
)

// Workflow states, as Cloudflare reports them for a registration or an update.
const (
	StatePending        = "pending"
	StateInProgress     = "in_progress"
	StateActionRequired = "action_required"
	StateBlocked        = "blocked"
	StateSucceeded      = "succeeded"
	StateFailed         = "failed"
)

// Pricing tiers. Premium names cannot be registered through the API.
const (
	TierStandard = "standard"
	TierPremium  = "premium"
)

// maxBodyBytes bounds how much of a response is read. A full page of registrations
// is a few kilobytes.
const maxBodyBytes = 1 << 20

// readRetries is how many times an idempotent call is repeated after a transient
// failure. Register and Update are never repeated.
const readRetries = 2

// Errors the service reacts to differently. Anything else is a transient failure.
var (
	// ErrAuth means the token was rejected or lacks the registrar permission.
	ErrAuth = errors.New("cloudflare: authentication failed")
	// ErrNotFound means Cloudflare has no such resource.
	ErrNotFound = errors.New("cloudflare: not found")
	// ErrRateLimited means Cloudflare throttled the account.
	ErrRateLimited = errors.New("cloudflare: rate limited")
)

// Message is one entry of Cloudflare's errors array.
type Message struct {
	Code    flexString `json:"code"`
	Message string     `json:"message"`
}

// APIError is a non-2xx response. It unwraps to one of the sentinels when the
// status maps to one.
type APIError struct {
	Status   int
	Messages []Message
	sentinel error
}

// Error implements error.
func (e *APIError) Error() string {
	parts := make([]string, 0, len(e.Messages))
	for _, m := range e.Messages {
		if m.Message == "" {
			continue
		}
		if m.Code != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", m.Message, m.Code))
		} else {
			parts = append(parts, m.Message)
		}
	}
	if len(parts) == 0 {
		return fmt.Sprintf("cloudflare: unexpected status %d", e.Status)
	}
	return fmt.Sprintf("cloudflare: status %d: %s", e.Status, strings.Join(parts, "; "))
}

// Unwrap lets errors.Is find the sentinel.
func (e *APIError) Unwrap() error { return e.sentinel }

// Detail is the first message Cloudflare sent, for showing to a person.
func (e *APIError) Detail() string {
	for _, m := range e.Messages {
		if m.Message != "" {
			return m.Message
		}
	}
	return ""
}

// Definite reports whether the response proves the request was refused. A 4xx other
// than 429 means Cloudflare looked at the request and rejected it; anything else —
// a 5xx, a timeout, a dropped connection — leaves open whether it was acted on.
func Definite(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != http.StatusTooManyRequests
}

// Pricing is what one domain costs, in the currency's major unit as a decimal string
// ("10.11"). Strings keep the registry's precision; see ParseCents.
type Pricing struct {
	Currency string `json:"currency"`
	// RegistrationCost is the first-year cost to register the domain.
	RegistrationCost string `json:"registration_cost"`
	// RenewalCost is the per-year renewal cost.
	RenewalCost string `json:"renewal_cost"`
}

// Offer is one domain returned by search or check.
type Offer struct {
	Name        string   `json:"name"`
	Registrable bool     `json:"registrable"`
	Tier        string   `json:"tier"`
	Reason      string   `json:"reason"`
	Pricing     *Pricing `json:"pricing"`
}

// Registration is a domain the account owns.
type Registration struct {
	DomainName  string     `json:"domain_name"`
	Status      string     `json:"status"`
	CreatedAt   *time.Time `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	AutoRenew   bool       `json:"auto_renew"`
	Locked      bool       `json:"locked"`
	PrivacyMode string     `json:"privacy_mode"`
}

// WorkflowError is why a workflow failed.
type WorkflowError struct {
	Code    flexString `json:"code"`
	Message string     `json:"message"`
}

// Workflow is the status of a registration or an update. Cloudflare answers with the
// same shape whether the work finished inside its wait window or is still running.
type Workflow struct {
	DomainName string     `json:"domain_name"`
	State      string     `json:"state"`
	Completed  bool       `json:"completed"`
	CreatedAt  *time.Time `json:"created_at"`
	UpdatedAt  *time.Time `json:"updated_at"`
	Context    struct {
		DomainName   string        `json:"domain_name"`
		Registration *Registration `json:"registration"`
	} `json:"context"`
	Error *WorkflowError `json:"error"`
	Links struct {
		Self     string `json:"self"`
		Resource string `json:"resource"`
	} `json:"links"`
}

// RegistrationPage is one page of the account's registrations.
type RegistrationPage struct {
	Registrations []Registration
	// NextCursor is empty on the last page.
	NextCursor string
}

// RegisterInput is the body of a registration. Everything not set here takes the
// account default: the default registrant contact, the default payment method,
// WHOIS redaction where the extension allows it, and the extension's minimum term.
type RegisterInput struct {
	DomainName string `json:"domain_name"`
	AutoRenew  bool   `json:"auto_renew"`
}

// Config configures the client.
type Config struct {
	// BaseURL is the API root, without a trailing slash. Empty means DefaultBaseURL.
	BaseURL   string
	AccountID string
	Token     string
	// Timeout bounds one HTTP attempt. Registration waits up to ten seconds on
	// Cloudflare's side, so this must stay well above that. Zero means 30s.
	Timeout    time.Duration
	HTTPClient *http.Client
}

// Client talks to the Cloudflare Registrar API. It is safe for concurrent use.
type Client struct {
	base    string
	account string
	token   string
	http    *http.Client
}

// New builds a client.
func New(cfg Config) *Client {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	return &Client{base: base, account: cfg.AccountID, token: cfg.Token, http: httpClient}
}

// Search suggests domains for a keyword. It is fast and reads cached data, so it is
// for discovery only; Check is the answer to "can this be bought, and for how much".
func (c *Client) Search(ctx context.Context, q string, limit int, extensions []string) ([]Offer, error) {
	query := url.Values{"q": {q}}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	for _, ext := range extensions {
		query.Add("extensions", ext)
	}
	var out struct {
		Domains []Offer `json:"domains"`
	}
	if _, err := c.do(ctx, http.MethodGet, c.accountPath("/registrar/domain-search"), query, nil, true, &out); err != nil {
		return nil, err
	}
	return out.Domains, nil
}

// Check asks the registries directly whether each name can be registered and at what
// price. It reserves nothing.
func (c *Client) Check(ctx context.Context, domains []string) ([]Offer, error) {
	if len(domains) == 0 || len(domains) > MaxCheckDomains {
		return nil, fmt.Errorf("cloudflare: check takes 1 to %d domains, got %d", MaxCheckDomains, len(domains))
	}
	body := map[string][]string{"domains": domains}
	var out struct {
		Domains []Offer `json:"domains"`
	}
	// Check is a POST but reads only, so it is safe to repeat.
	if _, err := c.do(ctx, http.MethodPost, c.accountPath("/registrar/domain-check"), nil, body, true, &out); err != nil {
		return nil, err
	}
	return out.Domains, nil
}

// Register registers one domain and charges the account's default payment method.
// It is sent exactly once: whatever goes wrong, the caller finds out what happened
// through RegistrationStatus rather than by calling this again.
func (c *Client) Register(ctx context.Context, in RegisterInput) (Workflow, error) {
	var out Workflow
	_, err := c.do(ctx, http.MethodPost, c.accountPath("/registrar/registrations"), nil, in, false, &out)
	return out, err
}

// RegistrationStatus reports how a registration workflow is going.
func (c *Client) RegistrationStatus(ctx context.Context, domain string) (Workflow, error) {
	var out Workflow
	_, err := c.do(ctx, http.MethodGet,
		c.accountPath("/registrar/registrations/"+url.PathEscape(domain)+"/registration-status"), nil, nil, true, &out)
	return out, err
}

// GetRegistration returns one domain the account owns.
func (c *Client) GetRegistration(ctx context.Context, domain string) (Registration, error) {
	var out Registration
	_, err := c.do(ctx, http.MethodGet,
		c.accountPath("/registrar/registrations/"+url.PathEscape(domain)), nil, nil, true, &out)
	return out, err
}

// ListRegistrations returns one page of the account's domains, oldest first.
func (c *Client) ListRegistrations(ctx context.Context, cursor string, perPage int) (RegistrationPage, error) {
	if perPage <= 0 || perPage > MaxListPerPage {
		perPage = MaxListPerPage
	}
	query := url.Values{"per_page": {strconv.Itoa(perPage)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	var out []Registration
	info, err := c.do(ctx, http.MethodGet, c.accountPath("/registrar/registrations"), query, nil, true, &out)
	if err != nil {
		return RegistrationPage{}, err
	}
	return RegistrationPage{Registrations: out, NextCursor: info.Cursor}, nil
}

// UpdateAutoRenew switches automatic renewal. Turning it on authorises Cloudflare to
// charge the renewal up to 30 days before expiry. It is sent exactly once.
func (c *Client) UpdateAutoRenew(ctx context.Context, domain string, autoRenew bool) (Workflow, error) {
	body := map[string]bool{"auto_renew": autoRenew}
	var out Workflow
	_, err := c.do(ctx, http.MethodPatch,
		c.accountPath("/registrar/registrations/"+url.PathEscape(domain)), nil, body, false, &out)
	return out, err
}

func (c *Client) accountPath(suffix string) string {
	return "/accounts/" + url.PathEscape(c.account) + suffix
}

// envelope is Cloudflare's v4 response wrapper.
type envelope struct {
	Success    bool            `json:"success"`
	Errors     []Message       `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo resultInfo      `json:"result_info"`
}

type resultInfo struct {
	Cursor string `json:"cursor"`
}

// do sends one request. An idempotent request is repeated after a transient failure;
// any other is sent once and its failure returned as it stands.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, idempotent bool, out any) (resultInfo, error) {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return resultInfo{}, fmt.Errorf("cloudflare: encode request: %w", err)
		}
		payload = encoded
	}

	attempts := 1
	if idempotent {
		attempts += readRetries
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			wait := time.Duration(attempt) * 500 * time.Millisecond
			select {
			case <-ctx.Done():
				return resultInfo{}, ctx.Err()
			case <-time.After(wait):
			}
		}
		info, err := c.attempt(ctx, method, path, query, payload, out)
		if err == nil {
			return info, nil
		}
		lastErr = err
		if !transient(err) || ctx.Err() != nil {
			break
		}
	}
	return resultInfo{}, lastErr
}

func (c *Client) attempt(ctx context.Context, method, path string, query url.Values, payload []byte, out any) (resultInfo, error) {
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return resultInfo{}, fmt.Errorf("cloudflare: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return resultInfo{}, fmt.Errorf("cloudflare: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return resultInfo{}, fmt.Errorf("cloudflare: read response: %w", err)
	}
	if len(raw) > maxBodyBytes {
		return resultInfo{}, errors.New("cloudflare: response body exceeds the size cap")
	}

	var env envelope
	decodeErr := json.Unmarshal(raw, &env)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{Status: resp.StatusCode, Messages: env.Errors}
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			apiErr.sentinel = ErrAuth
		case http.StatusNotFound:
			apiErr.sentinel = ErrNotFound
		case http.StatusTooManyRequests:
			apiErr.sentinel = ErrRateLimited
		}
		return resultInfo{}, apiErr
	}
	if decodeErr != nil {
		return resultInfo{}, fmt.Errorf("cloudflare: decode response: %w", decodeErr)
	}
	if !env.Success && len(env.Errors) > 0 {
		return resultInfo{}, &APIError{Status: resp.StatusCode, Messages: env.Errors}
	}
	if out != nil && len(env.Result) > 0 && string(env.Result) != "null" {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return resultInfo{}, fmt.Errorf("cloudflare: decode result: %w", err)
		}
	}
	return env.ResultInfo, nil
}

// transient reports whether repeating a read could help.
func transient(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500
	}
	return !errors.Is(err, context.Canceled)
}

// ParseCents converts a decimal price string ("10.11", "8", "12.5") into cents. It
// rejects negatives, more than two decimal places and anything that is not a number,
// because a price that cannot be read exactly must never be charged.
func ParseCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" || (hasFrac && (frac == "" || len(frac) > 2)) {
		return 0, fmt.Errorf("cloudflare: %q is not a price", s)
	}
	for _, part := range []string{whole, frac} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return 0, fmt.Errorf("cloudflare: %q is not a price", s)
			}
		}
	}
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || units > 1_000_000_000 {
		return 0, fmt.Errorf("cloudflare: %q is not a price", s)
	}
	cents := int64(0)
	if hasFrac {
		if len(frac) == 1 {
			frac += "0"
		}
		cents, _ = strconv.ParseInt(frac, 10, 64)
	}
	return units*100 + cents, nil
}

// flexString decodes a JSON string or number. Cloudflare's v4 error codes are
// numbers while the registrar workflow's are strings.
type flexString string

// UnmarshalJSON implements json.Unmarshaler.
func (f *flexString) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*f = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

// String returns the code as text.
func (f flexString) String() string { return string(f) }
