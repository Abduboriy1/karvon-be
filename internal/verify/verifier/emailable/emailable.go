// Package emailable implements the third-party verifier interface against
// Emailable's REST API.
//
// Its verdict vocabulary maps one-to-one onto ours, it bills one credit per
// non-cached address, and it exposes a balance endpoint that doubles as the
// connection test on the Sources page.
package emailable

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bory/karvon-be/internal/verify/verifier"
)

// DefaultBaseURL is the production API root.
const DefaultBaseURL = "https://api.emailable.com"

// maxBodyBytes bounds how much of a response is read before giving up, so a
// misbehaving endpoint cannot exhaust memory.
const maxBodyBytes = 1 << 20

// statusPending is Emailable's non-standard "accepted, come back later" code.
const statusPending = 249

// Client talks to Emailable.
type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// New builds a client from the shared provider options.
func New(opts verifier.Options) *Client {
	base := strings.TrimSuffix(opts.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	return &Client{apiKey: opts.APIKey, baseURL: base, http: opts.Client()}
}

// Name implements verifier.Verifier.
func (c *Client) Name() string { return "Emailable" }

// verifyResponse is the part of the payload we act on. The whole body is stored
// separately as Result.Raw.
type verifyResponse struct {
	State     string `json:"state"`
	Reason    string `json:"reason"`
	AcceptAll bool   `json:"accept_all"`
	Role      bool   `json:"role"`
	Free      bool   `json:"free"`
	Disposabl bool   `json:"disposable"`
	Message   string `json:"message"`
}

type accountResponse struct {
	AvailableCredits int64 `json:"available_credits"`
}

// Verify implements verifier.Verifier.
func (c *Client) Verify(ctx context.Context, email string) (verifier.Result, error) {
	query := url.Values{}
	query.Set("email", email)
	query.Set("api_key", c.apiKey)
	// Ask the vendor to wait for a definitive answer rather than returning 249 on
	// the first call; it still gives up eventually, which we handle as pending.
	query.Set("smtp", "true")

	body, err := c.get(ctx, "/v1/verify", query)
	if err != nil {
		return verifier.Result{}, err
	}

	var payload verifyResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return verifier.Result{}, fmt.Errorf("emailable: decode response: %w", err)
	}

	status := mapState(payload.State, payload.AcceptAll)
	return verifier.Result{
		Status:      status,
		Reason:      reasonOf(payload),
		Raw:         json.RawMessage(body),
		CreditsUsed: creditsFor(status),
	}, nil
}

// Balance implements verifier.Verifier and backs the Sources connection test.
func (c *Client) Balance(ctx context.Context) (verifier.Balance, error) {
	query := url.Values{}
	query.Set("api_key", c.apiKey)

	body, err := c.get(ctx, "/v1/account", query)
	if err != nil {
		return verifier.Balance{}, err
	}

	var payload accountResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return verifier.Balance{}, fmt.Errorf("emailable: decode account response: %w", err)
	}
	return verifier.Balance{Credits: payload.AvailableCredits}, nil
}

// get performs one request and turns every non-200 into a classified error.
func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	endpoint := c.baseURL + path + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("emailable: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("emailable: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err := classify(resp, body); err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, fmt.Errorf("emailable: read response: %w", readErr)
	}
	return body, nil
}

// classify maps HTTP status codes onto the sentinels the pipeline reacts to.
func classify(resp *http.Response, body []byte) error {
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == statusPending:
		return verifier.ErrPending
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w (status %d)", verifier.ErrAuth, resp.StatusCode)
	case resp.StatusCode == http.StatusPaymentRequired:
		return fmt.Errorf("%w (status %d)", verifier.ErrInsufficientCredits, resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return &verifier.RetryAfterError{
			Err:   verifier.ErrRateLimited,
			After: verifier.ParseRetryAfter(resp.Header.Get("Retry-After"), 30*time.Second),
		}
	case looksLikeCreditExhaustion(resp.StatusCode, body):
		return verifier.ErrInsufficientCredits
	default:
		return fmt.Errorf("emailable: unexpected status %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
}

// looksLikeCreditExhaustion catches the vendor reporting an empty balance as a
// generic 4xx with a message rather than a 402.
func looksLikeCreditExhaustion(status int, body []byte) bool {
	if status < 400 || status >= 500 {
		return false
	}
	message := strings.ToLower(string(body))
	return strings.Contains(message, "insufficient") && strings.Contains(message, "credit")
}

// mapState translates Emailable's vocabulary. A catch-all domain is reported as
// deliverable with accept_all set, which is exactly what "risky" means here: the
// domain accepts everything, so the mailbox itself is unproven.
func mapState(state string, acceptAll bool) verifier.Status {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "deliverable":
		if acceptAll {
			return verifier.StatusRisky
		}
		return verifier.StatusDeliverable
	case "undeliverable":
		return verifier.StatusUndeliverable
	case "risky":
		return verifier.StatusRisky
	default:
		return verifier.StatusUnknown
	}
}

func reasonOf(payload verifyResponse) string {
	if payload.Reason != "" {
		return payload.Reason
	}
	return payload.Message
}

// creditsFor reports the billable cost. An inconclusive answer is not charged for,
// which keeps the credit counter honest against the vendor's invoice.
func creditsFor(status verifier.Status) int {
	if status == verifier.StatusUnknown {
		return 0
	}
	return 1
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// compile-time proof that the client satisfies the interface.
var _ verifier.Verifier = (*Client)(nil)
