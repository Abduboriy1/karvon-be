package crawler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bory/karvon-be/internal/business"
)

// Config tunes a Crawler.
type Config struct {
	UserAgent       string
	Timeout         time.Duration
	MaxBodyBytes    int64
	PerHostInterval time.Duration
	// MaxExtraPages bounds the contact-page hunt after the homepage.
	MaxExtraPages int
	// ContactWords rank the homepage links worth following; nil means
	// DefaultContactWords. See ExtractContactLinksWith.
	ContactWords []string
	// ContactPaths are tried on the site root when the homepage links nowhere
	// useful (or could not be fetched); nil means DefaultContactPaths.
	ContactPaths []string
	Transport    http.RoundTripper
}

// DefaultMaxExtraPages is how many pages beyond the homepage a crawl may fetch.
const DefaultMaxExtraPages = 5

// DefaultContactPaths are the conventional contact-page locations, tried in order
// when the homepage does not link to a contact page. Static sites (contact.html),
// PHP sites (contact.php) and Shopify stores (/pages/contact) are all covered.
//
// Extend the list with KARVON_CRAWL_CONTACT_PATHS (comma-separated, appended).
var DefaultContactPaths = []string{
	"/contact",
	"/contact-us",
	"/contact.html",
	"/contact-us.html",
	"/contactus",
	"/contact.php",
	"/contact.htm",
	"/pages/contact",
	"/pages/contact-us",
	"/get-in-touch",
	"/about",
	"/about-us",
	"/about.html",
	"/about-us.html",
	"/team",
	"/support",
}

// Crawler visits one business website at a time and returns the addresses it finds.
type Crawler struct {
	fetcher *Fetcher
	robots  *RobotsCache
	limiter *HostLimiter
	cfg     Config
}

// SkipReason explains why a site was not fetched at all.
type SkipReason string

const (
	// SkipNone means the site was crawled.
	SkipNone SkipReason = ""
	// SkipInvalidURL means the stored website could not be parsed.
	SkipInvalidURL SkipReason = "invalid_url"
	// SkipPlatform means the domain belongs to a website builder or social network.
	SkipPlatform SkipReason = "platform_domain"
	// SkipRobots means robots.txt disallows the homepage.
	SkipRobots SkipReason = "robots_disallow"
)

// SiteResult is the outcome of crawling one website.
type SiteResult struct {
	Domain       string
	Emails       []Found
	PagesFetched int
	Skipped      SkipReason
}

// New builds a Crawler. Fetcher, robots cache and rate limiter are shared across all
// sites so the per-host limit holds across concurrent workers in this process.
func New(cfg Config) *Crawler {
	if cfg.MaxExtraPages <= 0 {
		cfg.MaxExtraPages = DefaultMaxExtraPages
	}
	if cfg.ContactWords == nil {
		cfg.ContactWords = DefaultContactWords
	}
	if cfg.ContactPaths == nil {
		cfg.ContactPaths = DefaultContactPaths
	}
	if cfg.PerHostInterval <= 0 {
		cfg.PerHostInterval = time.Second
	}
	fetcher := NewFetcher(FetcherConfig{
		Timeout:      cfg.Timeout,
		UserAgent:    cfg.UserAgent,
		MaxBodyBytes: cfg.MaxBodyBytes,
		Transport:    cfg.Transport,
	})
	return &Crawler{
		fetcher: fetcher,
		robots:  NewRobotsCache(fetcher, cfg.UserAgent),
		limiter: NewHostLimiter(cfg.PerHostInterval),
		cfg:     cfg,
	}
}

// CrawlSite fetches a website's homepage and, if no address turns up, up to
// MaxExtraPages contact-like pages on the same host: first the links the homepage
// itself advertises (ranked by ContactWords), then the conventional ContactPaths.
//
// A failure to fetch any single page is not fatal: whatever was found so far is
// returned together with the error, and callers record it as a warning. When the
// homepage itself cannot be fetched the conventional paths are still tried, because a
// blocked or broken landing page often sits next to a perfectly reachable contact page.
func (c *Crawler) CrawlSite(ctx context.Context, website string) (SiteResult, error) {
	normalized, domain, ok := business.NormalizeWebsite(website)
	if !ok {
		return SiteResult{Skipped: SkipInvalidURL}, nil
	}
	if business.IsPlatformDomain(domain) {
		return SiteResult{Domain: domain, Skipped: SkipPlatform}, nil
	}

	root, err := url.Parse(normalized)
	if err != nil {
		// An unparseable website is a data problem, not a crawl failure: report it
		// as a skip so the job keeps going.
		return SiteResult{Skipped: SkipInvalidURL}, nil //nolint:nilerr // skip, do not fail the job
	}
	result := SiteResult{Domain: domain}

	if !c.robots.Allowed(ctx, root) {
		result.Skipped = SkipRobots
		return result, nil
	}

	var (
		firstErr error
		homepage *Page
	)
	page, err := c.fetchPage(ctx, root)
	if err != nil {
		if ctx.Err() != nil {
			return result, err
		}
		firstErr = err
	} else {
		result.PagesFetched++
		result.Emails = ExtractEmails(page)
		if len(result.Emails) > 0 {
			return result, nil
		}
		homepage = &page
	}

	for _, candidate := range c.candidatePages(homepage, root) {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !c.robots.Allowed(ctx, candidate) {
			continue
		}
		next, err := c.fetchPage(ctx, candidate)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		result.PagesFetched++
		if found := ExtractEmails(next); len(found) > 0 {
			result.Emails = found
			// A contact page that answered makes an unreachable homepage moot.
			return result, nil
		}
	}
	return result, firstErr
}

// candidatePages merges contact-ish links found on the homepage (when it was
// fetched) with the conventional fallback paths, best candidates first, deduplicated
// and capped at MaxExtraPages. The homepage's final URL is the base for relative
// links, so a site that redirects to www. or to https is followed correctly.
func (c *Crawler) candidatePages(homepage *Page, root *url.URL) []*url.URL {
	seen := map[string]struct{}{root.String(): {}}
	var out []*url.URL

	base := root
	if homepage != nil {
		if homepage.URL != nil {
			base = homepage.URL
			seen[base.String()] = struct{}{}
		}
		for _, link := range ExtractContactLinksWith(*homepage, c.cfg.ContactWords, c.cfg.MaxExtraPages) {
			if _, dup := seen[link.String()]; dup {
				continue
			}
			seen[link.String()] = struct{}{}
			out = append(out, link)
		}
	}
	for _, path := range c.cfg.ContactPaths {
		if len(out) >= c.cfg.MaxExtraPages {
			break
		}
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		candidate := &url.URL{Scheme: base.Scheme, Host: base.Host, Path: path}
		if _, dup := seen[candidate.String()]; dup {
			continue
		}
		seen[candidate.String()] = struct{}{}
		out = append(out, candidate)
	}
	if len(out) > c.cfg.MaxExtraPages {
		out = out[:c.cfg.MaxExtraPages]
	}
	return out
}

func (c *Crawler) fetchPage(ctx context.Context, target *url.URL) (Page, error) {
	if err := c.limiter.Wait(ctx, target.Host); err != nil {
		return Page{}, err
	}
	page, err := c.fetcher.Get(ctx, target)
	if err != nil {
		if errors.Is(err, ErrNotHTML) {
			return Page{}, fmt.Errorf("skipping %s: %w", target.Redacted(), err)
		}
		return Page{}, err
	}
	return page, nil
}
