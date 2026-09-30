package business

import (
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// NormalizeWebsite canonicalises a provider-supplied website value. It returns an
// absolute https/http URL and the bare registrable host without a "www." prefix.
// ok is false for anything that is not a usable public web address.
func NormalizeWebsite(raw string) (website string, domain string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	switch {
	case strings.Contains(raw, "://"):
		// Keep the scheme; it is validated below.
	case hasNonWebScheme(raw):
		// mailto:, tel:, javascript: and friends are not websites.
		return "", "", false
	default:
		raw = "https://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	// Credentials in a stored website value are never legitimate here.
	if u.User != nil {
		return "", "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", "", false
	}

	host := strings.ToLower(u.Hostname())
	if host == "" || !strings.Contains(host, ".") {
		return "", "", false
	}
	if host == "localhost" || net.ParseIP(host) != nil {
		return "", "", false
	}
	host = strings.TrimPrefix(host, "www.")
	if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return "", "", false
	}

	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	return u.String(), host, true
}

// SameSite reports whether two hosts belong to the same registrable domain, ignoring
// a leading "www.".
func SameSite(a, b string) bool {
	return strings.EqualFold(strings.TrimPrefix(strings.ToLower(a), "www."),
		strings.TrimPrefix(strings.ToLower(b), "www."))
}

// nonWebSchemes are prefixes that sometimes end up in a "website" field but can
// never be crawled.
var nonWebSchemes = []string{"mailto:", "tel:", "sms:", "javascript:", "data:", "file:", "ftp:"}

// hasNonWebScheme reports whether raw starts with a scheme we refuse outright.
func hasNonWebScheme(raw string) bool {
	lower := strings.ToLower(raw)
	for _, scheme := range nonWebSchemes {
		if strings.HasPrefix(lower, scheme) {
			return true
		}
	}
	return false
}

// websiteKeyPrefix and websiteKeyQuery mirror the regular expressions of the
// ListFreshCrawledSiblings query; the two must stay in step.
var (
	websiteKeyQuery  = regexp.MustCompile(`[?#].*$`)
	websiteKeyPrefix = regexp.MustCompile(`(?i)^https?://(www\.)?|/+$`)
)

// trackingParams are query parameters that record where a click came from rather
// than which page it opens. Any "utm_" parameter is one too.
var trackingParams = map[string]struct{}{
	"fbclid": {}, "gclid": {}, "gbraid": {}, "wbraid": {}, "dclid": {}, "msclkid": {},
	"yclid": {}, "mibextid": {}, "igshid": {}, "igsh": {}, "y_source": {},
	"mc_cid": {}, "mc_eid": {}, "_ga": {}, "wt.mc_id": {},
}

// WebsitePathKey is the part of WebsiteKey that SQL can compute: the address without
// its scheme, a leading "www.", the query string and trailing slashes.
func WebsitePathKey(website string) string {
	key := websiteKeyQuery.ReplaceAllString(strings.TrimSpace(website), "")
	return strings.ToLower(websiteKeyPrefix.ReplaceAllString(key, ""))
}

// WebsiteKey identifies a website for crawl reuse: two listings with the same key
// point at the same page. It ignores the scheme, a leading "www.", the fragment,
// trailing slashes and tracking parameters, and keeps the path and the rest of the
// query string, so one page per location on a shared domain stays distinct whether
// the path ("/stores/12") or the query ("profile.php?id=12") names it.
func WebsiteKey(website string) string {
	key := WebsitePathKey(website)
	raw, _, _ := strings.Cut(strings.TrimSpace(website), "#")
	if _, query, ok := strings.Cut(raw, "?"); ok {
		if params := pageParams(query); params != "" {
			key += "?" + params
		}
	}
	return key
}

// pageParams keeps the parameters of a query string that pick the page, lowercased,
// deduplicated and sorted so their order does not matter.
func pageParams(query string) string {
	seen := make(map[string]struct{})
	var kept []string
	for _, param := range strings.Split(strings.ToLower(query), "&") {
		name, _, _ := strings.Cut(param, "=")
		if name == "" || strings.HasPrefix(name, "utm_") {
			continue
		}
		if _, tracking := trackingParams[name]; tracking {
			continue
		}
		if _, dup := seen[param]; dup {
			continue
		}
		seen[param] = struct{}{}
		kept = append(kept, param)
	}
	sort.Strings(kept)
	return strings.Join(kept, "&")
}
