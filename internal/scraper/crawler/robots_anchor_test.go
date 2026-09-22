package crawler

import "testing"

// "$" anchors a robots.txt pattern to the end of the path, so "Disallow: /private$"
// forbids exactly "/private" and nothing else.
//
// matchRobotsPattern walks the pattern's literal segments to prove the prefix
// matches, but then answers the "$" question with a bare strings.HasSuffix against
// the LAST segment, throwing away the position it just computed. Any path that both
// starts with the pattern and happens to end with its last segment matches, so
// "/private/secret/private" is treated as disallowed and the crawler silently skips
// pages it is allowed to fetch.
func TestRobotsDollarAnchorsToTheEndOfThePath(t *testing.T) {
	policy := ParseRobots([]byte("User-agent: *\nDisallow: /private$\n"))

	tests := map[string]bool{
		"/private":                false, // exactly the pattern: disallowed
		"/private/secret/private": true,  // longer than the pattern: allowed
		"/private/secret":         true,
		"/privatex":               true,
		"/a/private":              true,
	}
	for path, want := range tests {
		if got := policy.Allowed("KarvonBot", path); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", path, got, want)
		}
	}
}

// The same mistake with a wildcard in front of the anchor: "/*.pdf$" must forbid a
// path that ends in ".pdf", and "/reports.pdf/index.html" does not.
func TestRobotsDollarAnchorWithAWildcard(t *testing.T) {
	policy := ParseRobots([]byte("User-agent: *\nDisallow: /*.pdf$\n"))

	tests := map[string]bool{
		"/reports.pdf":            false,
		"/a/b/reports.pdf":        false,
		"/reports.pdf/index.html": true,
		"/reports.pdfx":           true,
	}
	for path, want := range tests {
		if got := policy.Allowed("KarvonBot", path); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", path, got, want)
		}
	}
}
