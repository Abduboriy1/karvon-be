package crawler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

const testAgent = "KarvonBot/1.0"

func TestRobotsPolicyAllowed(t *testing.T) {
	policy := ParseRobots([]byte(`
User-agent: *
Disallow: /private
Disallow: /tmp/
Allow: /private/public-page

User-agent: KarvonBot
Disallow: /nope
`))

	tests := []struct {
		agent string
		path  string
		want  bool
	}{
		// The KarvonBot group is more specific, so the wildcard rules do not apply.
		{agent: testAgent, path: "/nope", want: false},
		{agent: testAgent, path: "/private", want: true},
		{agent: "SomeOtherBot", path: "/private", want: false},
		{agent: "SomeOtherBot", path: "/private/public-page", want: true},
		{agent: "SomeOtherBot", path: "/tmp/x", want: false},
		{agent: "SomeOtherBot", path: "/", want: true},
	}

	for _, tt := range tests {
		if got := policy.Allowed(tt.agent, tt.path); got != tt.want {
			t.Errorf("Allowed(%q, %q) = %v, want %v", tt.agent, tt.path, got, tt.want)
		}
	}
}

func TestRobotsPolicyDisallowAll(t *testing.T) {
	policy := ParseRobots([]byte("User-agent: *\nDisallow: /\n"))
	if policy.Allowed(testAgent, "/") {
		t.Error("Disallow: / must block the homepage")
	}
	if policy.Allowed(testAgent, "/contact") {
		t.Error("Disallow: / must block every path")
	}
}

func TestRobotsPolicyEmptyDisallowAllowsEverything(t *testing.T) {
	policy := ParseRobots([]byte("User-agent: *\nDisallow:\n"))
	if !policy.Allowed(testAgent, "/anything") {
		t.Error("an empty Disallow means no restriction")
	}
}

func TestRobotsPolicyWildcards(t *testing.T) {
	policy := ParseRobots([]byte("User-agent: *\nDisallow: /*.pdf$\nDisallow: /a/*/secret\n"))

	cases := map[string]bool{
		"/brochure.pdf":   false,
		"/brochure.pdf?x": true,
		"/a/b/secret":     false,
		"/a/b/public":     true,
	}
	for path, want := range cases {
		if got := policy.Allowed(testAgent, path); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestRobotsPolicyIgnoresComments(t *testing.T) {
	policy := ParseRobots([]byte("# a comment\nUser-agent: * # inline\nDisallow: /x # why\n"))
	if policy.Allowed(testAgent, "/x") {
		t.Error("the rule before the comment should still apply")
	}
}

func TestRobotsCacheFetchesOncePerHost(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hits++
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /private\n"))
	}))
	defer server.Close()

	cache := NewRobotsCache(NewFetcher(FetcherConfig{Timeout: 2 * time.Second, UserAgent: testAgent}), testAgent)
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if !cache.Allowed(ctx, base.JoinPath("/")) {
		t.Error("the homepage should be allowed")
	}
	if cache.Allowed(ctx, base.JoinPath("/private")) {
		t.Error("/private should be disallowed")
	}
	if hits != 1 {
		t.Fatalf("robots.txt fetched %d times, want 1", hits)
	}
}

func TestRobotsCacheTreatsMissingFileAsAllowAll(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cache := NewRobotsCache(NewFetcher(FetcherConfig{Timeout: 2 * time.Second, UserAgent: testAgent}), testAgent)
	base, _ := url.Parse(server.URL)
	if !cache.Allowed(context.Background(), base.JoinPath("/anything")) {
		t.Error("a 404 robots.txt means no restrictions")
	}
}
