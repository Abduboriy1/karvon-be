// Package fbscrape talks to the fb-scrape service (services/fb-scrape), which reads
// the public Intro / Details box of a Facebook Page without logging in.
//
// The service owns everything that makes that work at volume: the Webshare proxy
// pool, the per-IP page quota and cooldown, and the retry of a failed page on
// another proxy. Its proxy state is shared across requests, so this client sends
// one page per request and leaves the queueing to the service.
package fbscrape

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is where the Compose stack publishes the service.
const DefaultBaseURL = "http://localhost:8000"

// APIKeyHeader carries the service's optional API_KEY.
const APIKeyHeader = "X-API-Key" //nolint:gosec // G101: a header name, not a credential

// maxBodyBytes bounds how much of a response is read. One page's details box is a
// few kilobytes.
const maxBodyBytes = 1 << 20

// defaultTimeout is used when Config.Timeout is zero. A page can wait in the
// service's queue for a free proxy, and a proxy rests for a minute after its quota.
const defaultTimeout = 3 * time.Minute

// Config configures the client.
type Config struct {
	// BaseURL is the root of the service, without a trailing slash.
	BaseURL string
	// APIKey is sent as X-API-Key. Empty when the service has no API_KEY set.
	APIKey string
	// Timeout bounds one request, including the time the page waits in the
	// service's queue.
	Timeout time.Duration
	// HTTPClient replaces the default client, for tests.
	HTTPClient *http.Client
}

// Page is what the service read from one Facebook Page. Fields the page does not
// show are empty.
type Page struct {
	URL      string   `json:"url"`
	Name     string   `json:"name"`
	Category string   `json:"category"`
	Email    string   `json:"email"`
	Phone    string   `json:"phone"`
	Website  string   `json:"website"`
	Lines    []string `json:"lines"`
	// Error is set, and the fields above are empty, when the service could not
	// read the page: a login wall, a page that hides its details, or every proxy
	// it tried failing.
	Error string `json:"error"`
}

// Client calls the service.
type Client struct {
	baseURL string
	apiKey  string
	timeout time.Duration
	http    *http.Client
}

// New builds a client.
func New(cfg Config) *Client {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{baseURL: baseURL, apiKey: cfg.APIKey, timeout: timeout, http: httpClient}
}

// Scrape reads one page. A page the service could not read is not an error: it
// comes back with Page.Error set. The error return is kept for the service itself
// failing — unreachable, rejecting the key, or answering with something else — which
// is worth retrying later, unlike a page that has no details to show.
func (c *Client) Scrape(ctx context.Context, pageURL string) (Page, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	body, err := json.Marshal(struct {
		Pages []string `json:"pages"`
	}{Pages: []string{pageURL}})
	if err != nil {
		return Page{}, fmt.Errorf("fbscrape: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/scrape", bytes.NewReader(body))
	if err != nil {
		return Page{}, fmt.Errorf("fbscrape: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set(APIKeyHeader, c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return Page{}, fmt.Errorf("fbscrape: call %s: %w", c.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return Page{}, fmt.Errorf("fbscrape: read response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return Page{}, errors.New("fbscrape: the service rejected the API key (KARVON_FB_SCRAPE_API_KEY)")
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return Page{}, fmt.Errorf("fbscrape: service answered %d: %s", resp.StatusCode, snippet(raw))
	}

	var decoded struct {
		Results []Page `json:"results"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Page{}, fmt.Errorf("fbscrape: decode response: %w", err)
	}
	if len(decoded.Results) != 1 {
		return Page{}, fmt.Errorf("fbscrape: expected 1 result, got %d", len(decoded.Results))
	}
	return decoded.Results[0], nil
}

// snippet keeps an error message readable when the service answers with a page of HTML.
func snippet(raw []byte) string {
	const limit = 200
	s := strings.TrimSpace(string(raw))
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}
