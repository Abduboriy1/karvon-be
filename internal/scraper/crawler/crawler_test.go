package crawler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// testSite is the domain the crawler believes it is visiting. Requests for it are
// redirected to the local test server by rewriteTransport, so the crawler sees a
// realistic hostname instead of an IP literal.
const testSite = "https://ironworksgym.com"

// rewriteTransport sends every request to the test server, whatever host was asked for.
type rewriteTransport struct {
	host string
	base http.RoundTripper
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = t.host
	return t.base.RoundTrip(clone)
}

// newTestCrawler points a crawler at a test server serving the testSite domain.
func newTestCrawler(t *testing.T, handler http.Handler) (*Crawler, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	crawler := New(Config{
		UserAgent:       testAgent,
		Timeout:         2 * time.Second,
		MaxBodyBytes:    1 << 20,
		PerHostInterval: time.Millisecond,
		MaxExtraPages:   3,
		Transport:       rewriteTransport{host: target.Host, base: http.DefaultTransport},
	})
	return crawler, testSite
}

func TestCrawlSiteFindsEmailOnTheHomepage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<a href="mailto:info@ironworksgym.com">mail</a>`))
	})

	crawler, base := newTestCrawler(t, mux)
	result, err := crawler.CrawlSite(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped != SkipNone {
		t.Fatalf("Skipped = %q, want none", result.Skipped)
	}
	if len(result.Emails) != 1 || result.Emails[0].Email != "info@ironworksgym.com" {
		t.Fatalf("Emails = %+v", result.Emails)
	}
	if result.PagesFetched != 1 {
		t.Fatalf("PagesFetched = %d, want 1: the homepage was enough", result.PagesFetched)
	}
}

func TestCrawlSiteFollowsAContactLink(t *testing.T) {
	var fetched []string
	var mu sync.Mutex

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fetched = append(fetched, r.URL.Path)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<a href="/contact">Contact us</a><p>no address here</p>`))
		case "/contact":
			_, _ = w.Write([]byte(`<p>Reach us at hello@ironworksgym.com</p>`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	crawler, base := newTestCrawler(t, mux)
	result, err := crawler.CrawlSite(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Emails) != 1 || result.Emails[0].Email != "hello@ironworksgym.com" {
		t.Fatalf("Emails = %+v", result.Emails)
	}
	if result.PagesFetched != 2 {
		t.Fatalf("PagesFetched = %d, want 2", result.PagesFetched)
	}
	if !strings.HasSuffix(result.Emails[0].PageURL, "/contact") {
		t.Errorf("PageURL = %q, want the contact page", result.Emails[0].PageURL)
	}
}

func TestCrawlSiteTriesFallbackPathsAndStopsAtTheLimit(t *testing.T) {
	var mu sync.Mutex
	var fetched []string

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fetched = append(fetched, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<p>nothing to see</p>`))
	})

	crawler, base := newTestCrawler(t, mux)
	result, err := crawler.CrawlSite(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Emails) != 0 {
		t.Fatalf("Emails = %+v, want none", result.Emails)
	}
	// Homepage plus the first three conventional fallbacks, and no more.
	if result.PagesFetched != 4 {
		t.Fatalf("PagesFetched = %d, want 4", result.PagesFetched)
	}

	mu.Lock()
	defer mu.Unlock()
	want := map[string]bool{"/": true}
	for _, path := range DefaultContactPaths[:3] {
		want[path] = true
	}
	for _, path := range fetched {
		if path == "/robots.txt" {
			continue
		}
		if !want[path] {
			t.Errorf("unexpected fetch of %q", path)
		}
	}
}

func TestCrawlSiteHonoursRobotsDisallowAll(t *testing.T) {
	var homepageHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		homepageHits++
		_, _ = w.Write([]byte(`<a href="mailto:info@ironworksgym.com">mail</a>`))
	})

	crawler, base := newTestCrawler(t, mux)
	result, err := crawler.CrawlSite(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped != SkipRobots {
		t.Fatalf("Skipped = %q, want %q", result.Skipped, SkipRobots)
	}
	if homepageHits != 0 {
		t.Fatalf("the homepage was fetched %d times despite Disallow: /", homepageHits)
	}
	if len(result.Emails) != 0 {
		t.Fatal("no address may be collected from a disallowed site")
	}
}

func TestCrawlSiteSkipsPlatformDomainsWithoutAnyRequest(t *testing.T) {
	crawler := New(Config{UserAgent: testAgent, Timeout: time.Second})
	result, err := crawler.CrawlSite(context.Background(), "https://someshop.wixpress.com")
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped != SkipPlatform {
		t.Fatalf("Skipped = %q, want %q", result.Skipped, SkipPlatform)
	}
}

func TestCrawlSiteSkipsUnusableWebsiteValues(t *testing.T) {
	crawler := New(Config{UserAgent: testAgent, Timeout: time.Second})
	for _, website := range []string{"", "mailto:a@b.com", "not a url at all"} {
		result, err := crawler.CrawlSite(context.Background(), website)
		if err != nil {
			t.Fatalf("CrawlSite(%q): %v", website, err)
		}
		if result.Skipped != SkipInvalidURL {
			t.Errorf("CrawlSite(%q) Skipped = %q, want %q", website, result.Skipped, SkipInvalidURL)
		}
	}
}

func TestCrawlSiteIgnoresNonHTMLResponses(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4 info@ironworksgym.com"))
	})

	crawler, base := newTestCrawler(t, mux)
	result, err := crawler.CrawlSite(context.Background(), base)
	if err == nil {
		t.Fatal("a non-HTML homepage should be reported as an error")
	}
	if len(result.Emails) != 0 {
		t.Fatalf("Emails = %+v, want none", result.Emails)
	}
}

func TestCrawlSiteRespectsContextCancellation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<p>nothing</p>`))
	})

	crawler, base := newTestCrawler(t, mux)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := crawler.CrawlSite(ctx, base); err == nil {
		t.Fatal("a cancelled context must stop the crawl")
	}
}

func TestCrawlSiteFollowsAStaticContactPageLink(t *testing.T) {
	// Mirrors a hand-built site: nav links are relative "contact.html" style and the
	// homepage carries no address at all.
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`
				<a class="nav-link" href="index.html">Home</a>
				<a class="nav-link" href="#">Trainers</a>
				<a class="nav-link" href="contact.html">Contact Us</a>
				<a href="#" class="btn">Get In Touch</a>`))
		case "/contact.html":
			_, _ = w.Write([]byte(`<p>Email: <a href="mailto:ironworks@gmail.com">ironworks@gmail.com</a></p>`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	crawler, base := newTestCrawler(t, mux)
	result, err := crawler.CrawlSite(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Emails) != 1 || result.Emails[0].Email != "ironworks@gmail.com" {
		t.Fatalf("Emails = %+v", result.Emails)
	}
	if result.PagesFetched != 2 {
		t.Fatalf("PagesFetched = %d, want 2: the contact link must be tried first", result.PagesFetched)
	}
	if !strings.HasSuffix(result.Emails[0].PageURL, "/contact.html") {
		t.Errorf("PageURL = %q, want the contact page", result.Emails[0].PageURL)
	}
}

func TestCrawlSitePrefersContactLinksOverAboutLinks(t *testing.T) {
	var mu sync.Mutex
	var fetched []string

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fetched = append(fetched, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<a href="/about-us">About</a><a href="/team">Team</a><a href="/reach-us">Contact</a>`))
		case "/reach-us":
			_, _ = w.Write([]byte(`<p>hello@ironworksgym.com</p>`))
		default:
			_, _ = w.Write([]byte(`<p>nothing here</p>`))
		}
	})

	crawler, base := newTestCrawler(t, mux)
	result, err := crawler.CrawlSite(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Emails) != 1 {
		t.Fatalf("Emails = %+v", result.Emails)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fetched) < 2 || fetched[1] != "/reach-us" {
		t.Fatalf("fetch order = %v, want the contact link second", fetched)
	}
}

func TestCrawlSiteTriesFallbackPathsWhenTheHomepageIsBlocked(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusForbidden)
		case "/contact-us":
			_, _ = w.Write([]byte(`<p>owner@ironworksgym.com</p>`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	crawler, base := newTestCrawler(t, mux)
	result, err := crawler.CrawlSite(context.Background(), base)
	if err != nil {
		t.Fatalf("a reachable contact page must clear the homepage error, got %v", err)
	}
	if len(result.Emails) != 1 || result.Emails[0].Email != "owner@ironworksgym.com" {
		t.Fatalf("Emails = %+v", result.Emails)
	}
}

func TestCrawlSiteUsesCustomContactWordsAndPaths(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte(`<a href="/holler">Holler at us</a>`))
		case "/holler":
			_, _ = w.Write([]byte(`<p>yo@ironworksgym.com</p>`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	target, _ := url.Parse(server.URL)

	crawler := New(Config{
		UserAgent:       testAgent,
		Timeout:         2 * time.Second,
		PerHostInterval: time.Millisecond,
		MaxExtraPages:   1,
		ContactWords:    []string{"holler"},
		ContactPaths:    []string{},
		Transport:       rewriteTransport{host: target.Host, base: http.DefaultTransport},
	})
	result, err := crawler.CrawlSite(context.Background(), testSite)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Emails) != 1 || result.Emails[0].Email != "yo@ironworksgym.com" {
		t.Fatalf("Emails = %+v", result.Emails)
	}
}
