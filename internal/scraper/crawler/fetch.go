package crawler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNotHTML is returned when a response is not an HTML document.
var ErrNotHTML = errors.New("crawler: response is not html")

// maxRedirects bounds how far a site may bounce us.
const maxRedirects = 3

// Fetcher performs bounded, browser-like GET requests.
type Fetcher struct {
	client    *http.Client
	userAgent string
	maxBody   int64
}

// FetcherConfig configures a Fetcher.
type FetcherConfig struct {
	Timeout      time.Duration
	UserAgent    string
	MaxBodyBytes int64
	// Transport is injectable so tests can serve pages without a network.
	Transport http.RoundTripper
}

// NewFetcher builds a Fetcher with sane crawl defaults.
func NewFetcher(cfg FetcherConfig) *Fetcher {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 2 << 20
	}
	transport := cfg.Transport
	if transport == nil {
		transport = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: cfg.Timeout,
			MaxIdleConnsPerHost:   2,
			// A crawl touches thousands of distinct hosts once each, so idle
			// connections are capped and dropped quickly rather than piling up.
			MaxIdleConns:      200,
			IdleConnTimeout:   30 * time.Second,
			DisableKeepAlives: false,
		}
	}

	return &Fetcher{
		client: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: transport,
			CheckRedirect: func(_ *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return fmt.Errorf("crawler: stopped after %d redirects", maxRedirects)
				}
				return nil
			},
		},
		userAgent: cfg.UserAgent,
		maxBody:   cfg.MaxBodyBytes,
	}
}

// Get fetches one URL and returns the decoded page. Non-HTML and error statuses
// produce an error; callers treat that as "no emails here" rather than a job failure.
func (f *Fetcher) Get(ctx context.Context, target *url.URL) (Page, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Page{}, fmt.Errorf("crawler: build request: %w", err)
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := f.client.Do(req)
	if err != nil {
		return Page{}, fmt.Errorf("crawler: get %s: %w", target.Redacted(), err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return Page{}, fmt.Errorf("crawler: get %s: status %d", target.Redacted(), resp.StatusCode)
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType != "" && !strings.Contains(strings.ToLower(contentType), "html") {
		return Page{}, fmt.Errorf("%w (%s)", ErrNotHTML, contentType)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBody))
	if err != nil {
		return Page{}, fmt.Errorf("crawler: read %s: %w", target.Redacted(), err)
	}

	finalURL := resp.Request.URL
	if finalURL == nil {
		finalURL = target
	}
	return Page{URL: finalURL, Body: body}, nil
}

// GetRaw fetches a non-HTML resource such as robots.txt.
func (f *Fetcher) GetRaw(ctx context.Context, target *url.URL, limit int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", f.userAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}
