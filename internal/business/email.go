// Package business holds the rules that turn raw provider listings and crawled pages
// into the deduplicated master list: domain normalisation, email filtering and
// ranking, and CSV export.
package business

import (
	"strings"
)

// rolePrefixes are mailbox names that never belong to a person we want to reach.
var rolePrefixes = map[string]struct{}{
	"noreply":       {},
	"no-reply":      {},
	"donotreply":    {},
	"do-not-reply":  {},
	"postmaster":    {},
	"abuse":         {},
	"mailer-daemon": {},
	"bounce":        {},
	"bounces":       {},
}

// platformDomains are sites whose addresses belong to a vendor, not the business.
var platformDomains = map[string]struct{}{
	"wixpress.com":             {},
	"wix.com":                  {},
	"sentry.io":                {},
	"sentry-next.wixpress.com": {},
	"godaddy.com":              {},
	"secureserver.net":         {},
	"example.com":              {},
	"example.org":              {},
	"example.net":              {},
	"squarespace.com":          {},
	"wordpress.com":            {},
	"shopify.com":              {},
	"weebly.com":               {},
	"sentry.wixpress.com":      {},
	"googleapis.com":           {},
	"gstatic.com":              {},
	"schema.org":               {},
	"w3.org":                   {},
	"cloudflare.com":           {},
	"jquery.com":               {},
	"facebook.com":             {},
	"instagram.com":            {},
	"youtube.com":              {},
	"twitter.com":              {},
	"x.com":                    {},
	"linkedin.com":             {},
	"tiktok.com":               {},
	"yelp.com":                 {},
	"tripadvisor.com":          {},
	"mindbodyonline.com":       {},
	"mailchimp.com":            {},
	"list-manage.com":          {},
	"hubspot.com":              {},
	"salesforce.com":           {},
	"adobe.com":                {},
	"fontawesome.com":          {},
}

// imageExtensions catch regex matches like "logo.png@2x" that are not addresses.
var imageExtensions = []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".ico", ".bmp", ".avif"}

// NormalizeEmail lowercases and trims an address, returning ok=false if it is not a
// plausible address at all.
func NormalizeEmail(raw string) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(raw))
	e = strings.Trim(e, ".,;:<>()[]\"'")
	local, domain, found := strings.Cut(e, "@")
	if !found || local == "" || domain == "" {
		return "", false
	}
	if strings.Contains(local, "@") || !strings.Contains(domain, ".") {
		return "", false
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return "", false
	}
	tld := domain[strings.LastIndex(domain, ".")+1:]
	if len(tld) < 2 || !isAlpha(tld) {
		return "", false
	}
	return e, true
}

func isAlpha(s string) bool {
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return s != ""
}

// IsRoleAddress reports whether the mailbox is an automated or abuse address.
func IsRoleAddress(email string) bool {
	local, _, found := strings.Cut(email, "@")
	if !found {
		return false
	}
	// Strip plus-addressing before matching, e.g. "noreply+tag@".
	if plus := strings.Index(local, "+"); plus > 0 {
		local = local[:plus]
	}
	_, ok := rolePrefixes[local]
	return ok
}

// IsPlatformDomain reports whether a domain belongs to a website builder, CDN or social
// network rather than to a business we can contact.
func IsPlatformDomain(domain string) bool {
	domain = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(domain), "www."))
	if domain == "" {
		return false
	}
	if _, ok := platformDomains[domain]; ok {
		return true
	}
	// Match subdomains of a denylisted apex, e.g. "mail.wixpress.com".
	for candidate := domain; ; {
		_, rest, found := strings.Cut(candidate, ".")
		if !found {
			return false
		}
		if _, ok := platformDomains[rest]; ok {
			return true
		}
		candidate = rest
	}
}

// looksLikeImage filters regex hits such as "sprite.png@2x.png".
func looksLikeImage(email string) bool {
	local, domain, _ := strings.Cut(email, "@")
	for _, ext := range imageExtensions {
		if strings.HasSuffix(local, ext) || strings.HasSuffix(domain, ext) {
			return true
		}
	}
	return false
}

// AcceptEmail applies every insert-time rule. It returns the normalized address and
// whether it should be stored at all.
func AcceptEmail(raw string) (string, bool) {
	email, ok := NormalizeEmail(raw)
	if !ok {
		return "", false
	}
	if looksLikeImage(email) {
		return "", false
	}
	if IsRoleAddress(email) {
		return "", false
	}
	_, domain, _ := strings.Cut(email, "@")
	if IsPlatformDomain(domain) {
		return "", false
	}
	return email, true
}

// preferredLocals rank generic business mailboxes from best to worst.
var preferredLocals = []string{"info", "hello", "contact"}

// Rank scores an address for primary selection; lower is better. Addresses on the
// business's own domain always beat addresses on a third-party domain.
func Rank(email, siteDomain string) int {
	local, domain, _ := strings.Cut(email, "@")
	if plus := strings.Index(local, "+"); plus > 0 {
		local = local[:plus]
	}

	score := 100
	for i, want := range preferredLocals {
		if local == want {
			score = i
			break
		}
	}
	if siteDomain != "" && !strings.EqualFold(domain, siteDomain) {
		score += 1000
	}
	return score
}

// PickPrimary returns the index of the best address in emails, or -1 when empty.
// Ties are broken by discovery order, which is the order of the slice.
func PickPrimary(emails []string, siteDomain string) int {
	best, bestScore := -1, 0
	for i, e := range emails {
		score := Rank(e, siteDomain)
		if best == -1 || score < bestScore {
			best, bestScore = i, score
		}
	}
	return best
}
