package crawler

import (
	"net/url"
	"regexp"
	"strings"
)

// Network names a social platform a business profile lives on. The values are
// stored as-is in business_socials.network.
type Network string

// Supported networks. Only profile pages count: share buttons, tracking pixels,
// individual posts and videos are all rejected by SocialProfile.
const (
	NetworkFacebook  Network = "facebook"
	NetworkInstagram Network = "instagram"
	NetworkTikTok    Network = "tiktok"
	NetworkYouTube   Network = "youtube"
	NetworkLinkedIn  Network = "linkedin"
	NetworkX         Network = "x"
	NetworkThreads   Network = "threads"
	NetworkPinterest Network = "pinterest"
	NetworkYelp      Network = "yelp"
	NetworkLinktree  Network = "linktree"
)

// SocialLink is one social profile a business links to.
type SocialLink struct {
	Network Network
	// Handle is the profile's identifier on its network, e.g. "tahcrossfit" or
	// "company/acme-plumbing".
	Handle string
	// URL is the canonical profile URL, so the same profile linked as
	// m.facebook.com/Foo and https://www.facebook.com/foo/ is stored once.
	URL string
	// PageURL is the page the link was found on.
	PageURL string
}

// MaxSocialLinks bounds how many profiles one site may contribute. A real business
// links a handful; more than this is a directory or a template gone wrong.
const MaxSocialLinks = 20

// absoluteURLPattern finds absolute and protocol-relative URLs anywhere in a page:
// href attributes, JSON-LD "sameAs" arrays and inline scripts alike. JSON-escaped
// slashes (https:\/\/...) are unescaped before matching.
var absoluteURLPattern = regexp.MustCompile(`(?i)(?:https?:)?//[a-z0-9.-]+\.[a-z]{2,}(?:/[^\s"'<>\\()\[\]{}|^` + "`" + `]*)?`)

// ExtractSocialLinks returns the distinct social profiles a page links to, in the
// order they first appear, at most MaxSocialLinks.
func ExtractSocialLinks(page Page) []SocialLink {
	body := strings.ReplaceAll(string(page.Body), `\/`, `/`)
	pageURL := ""
	if page.URL != nil {
		pageURL = page.URL.String()
	}

	var (
		out  []SocialLink
		seen = make(map[string]struct{})
	)
	for _, raw := range absoluteURLPattern.FindAllString(body, -1) {
		if strings.HasPrefix(raw, "//") {
			raw = "https:" + raw
		}
		link, ok := SocialProfile(raw)
		if !ok {
			continue
		}
		if _, dup := seen[link.URL]; dup {
			continue
		}
		seen[link.URL] = struct{}{}
		link.PageURL = pageURL
		out = append(out, link)
		if len(out) >= MaxSocialLinks {
			break
		}
	}
	return out
}

// socialHosts maps every host a network serves profiles from, "www." already
// stripped, to that network.
var socialHosts = map[string]Network{
	"facebook.com":          NetworkFacebook,
	"m.facebook.com":        NetworkFacebook,
	"web.facebook.com":      NetworkFacebook,
	"mobile.facebook.com":   NetworkFacebook,
	"business.facebook.com": NetworkFacebook,
	"fb.com":                NetworkFacebook,
	"instagram.com":         NetworkInstagram,
	"m.instagram.com":       NetworkInstagram,
	"instagr.am":            NetworkInstagram,
	"tiktok.com":            NetworkTikTok,
	"m.tiktok.com":          NetworkTikTok,
	"youtube.com":           NetworkYouTube,
	"m.youtube.com":         NetworkYouTube,
	"linkedin.com":          NetworkLinkedIn,
	"twitter.com":           NetworkX,
	"mobile.twitter.com":    NetworkX,
	"x.com":                 NetworkX,
	"threads.net":           NetworkThreads,
	"threads.com":           NetworkThreads,
	"pinterest.com":         NetworkPinterest,
	"yelp.com":              NetworkYelp,
	"m.yelp.com":            NetworkYelp,
	"linktr.ee":             NetworkLinktree,
}

// canonicalHosts is the host each network's canonical profile URL uses.
var canonicalHosts = map[Network]string{
	NetworkFacebook:  "www.facebook.com",
	NetworkInstagram: "www.instagram.com",
	NetworkTikTok:    "www.tiktok.com",
	NetworkYouTube:   "www.youtube.com",
	NetworkLinkedIn:  "www.linkedin.com",
	NetworkX:         "x.com",
	NetworkThreads:   "www.threads.net",
	NetworkPinterest: "www.pinterest.com",
	NetworkYelp:      "www.yelp.com",
	NetworkLinktree:  "linktr.ee",
}

// reservedFirstSegments are paths on a network that are never a profile.
var reservedFirstSegments = map[Network]map[string]struct{}{
	NetworkFacebook: stringSet(
		"sharer", "sharer.php", "share", "share.php", "dialog", "plugins", "tr", "login",
		"login.php", "logout.php", "policies", "policy.php", "privacy", "legal", "terms",
		"help", "watch", "events", "groups", "hashtag", "photo", "photo.php", "photos",
		"story.php", "permalink.php", "l.php", "home.php", "ads", "business", "gaming",
		"marketplace", "search", "video.php", "videos", "reel", "reels", "stories",
		"messages", "notifications", "settings", "bookmarks", "friends", "pg", "v2.0",
		"recover", "r.php", "reg", "signup", "about", "careers", "fundraisers",
	),
	NetworkInstagram: stringSet(
		"p", "reel", "reels", "tv", "explore", "stories", "accounts", "direct", "about",
		"legal", "developer", "web", "emails", "static", "embed.js",
	),
	NetworkX: stringSet(
		"intent", "share", "home", "i", "search", "hashtag", "login", "logout", "signup",
		"tos", "privacy", "explore", "settings", "messages", "notifications", "widgets.js",
		"compose", "account", "oauth", "status",
	),
	NetworkPinterest: stringSet(
		"pin", "search", "ideas", "today", "login", "signup", "_", "business", "about",
		"settings", "board",
	),
	NetworkLinktree: stringSet("s", "discover", "marketplace", "blog", "help", "login", "register"),
}

// handlePattern bounds what a single-segment handle may look like, which also drops
// file names such as "sdk.js" that a network serves from its own domain.
var handlePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// SocialProfile reports whether raw is a link to a social profile and, if so,
// returns it in canonical form. PageURL is left empty.
func SocialProfile(raw string) (SocialLink, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return SocialLink{}, false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "":
	default:
		return SocialLink{}, false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	network, ok := socialHosts[host]
	if !ok {
		return SocialLink{}, false
	}

	var segments []string
	for _, s := range strings.Split(u.Path, "/") {
		if s = strings.TrimSpace(s); s != "" {
			segments = append(segments, s)
		}
	}
	if len(segments) == 0 {
		return SocialLink{}, false
	}
	if _, reserved := reservedFirstSegments[network][strings.ToLower(segments[0])]; reserved {
		return SocialLink{}, false
	}

	handle, ok := profileHandle(network, segments, u.Query())
	if !ok {
		return SocialLink{}, false
	}
	canonical := url.URL{Scheme: "https", Host: canonicalHosts[network], Path: "/" + handle}
	if network == NetworkFacebook && strings.HasPrefix(handle, "profile.php?id=") {
		canonical = url.URL{
			Scheme:   "https",
			Host:     canonicalHosts[network],
			Path:     "/profile.php",
			RawQuery: "id=" + strings.TrimPrefix(handle, "profile.php?id="),
		}
	}
	return SocialLink{Network: network, Handle: handle, URL: canonical.String()}, true
}

// profileHandle extracts the profile identifier from a path already known to be on
// network, or reports that the path is not a profile.
func profileHandle(network Network, segments []string, query url.Values) (string, bool) {
	first := segments[0]
	switch network {
	case NetworkFacebook:
		switch strings.ToLower(first) {
		case "profile.php":
			id := query.Get("id")
			if !isDigits(id) {
				return "", false
			}
			return "profile.php?id=" + id, true
		case "pages", "people":
			// /pages/Name/123456 and /people/Name/123456: the numeric id is the identity.
			if len(segments) >= 3 && isDigits(segments[2]) {
				return strings.ToLower(first) + "/" + segments[1] + "/" + segments[2], true
			}
			return "", false
		}
		return singleHandle(first, true)

	case NetworkInstagram, NetworkPinterest, NetworkLinktree:
		return singleHandle(first, true)

	case NetworkX:
		return singleHandle(strings.TrimPrefix(first, "@"), true)

	case NetworkTikTok, NetworkThreads:
		// Profiles are /@handle; everything else is a video, tag or page.
		if !strings.HasPrefix(first, "@") {
			return "", false
		}
		handle, ok := singleHandle(strings.TrimPrefix(first, "@"), true)
		return "@" + handle, ok

	case NetworkYouTube:
		if strings.HasPrefix(first, "@") {
			handle, ok := singleHandle(strings.TrimPrefix(first, "@"), true)
			return "@" + handle, ok
		}
		switch strings.ToLower(first) {
		case "channel":
			// Channel ids are case-sensitive.
			if len(segments) >= 2 {
				handle, ok := singleHandle(segments[1], false)
				return "channel/" + handle, ok
			}
		case "c", "user":
			if len(segments) >= 2 {
				handle, ok := singleHandle(segments[1], true)
				return strings.ToLower(first) + "/" + handle, ok
			}
		}
		return "", false

	case NetworkLinkedIn:
		switch strings.ToLower(first) {
		case "company", "in", "school", "showcase":
			if len(segments) >= 2 {
				handle, ok := singleHandle(segments[1], true)
				return strings.ToLower(first) + "/" + handle, ok
			}
		}
		return "", false

	case NetworkYelp:
		if strings.ToLower(first) == "biz" && len(segments) >= 2 {
			handle, ok := singleHandle(segments[1], true)
			return "biz/" + handle, ok
		}
		return "", false
	}
	return "", false
}

// singleHandle validates one path segment as a handle, lowercasing it when the
// network treats handles case-insensitively.
func singleHandle(segment string, lower bool) (string, bool) {
	if decoded, err := url.PathUnescape(segment); err == nil {
		segment = decoded
	}
	if !handlePattern.MatchString(segment) || strings.Trim(segment, ".") == "" {
		return "", false
	}
	// A handle never ends in a file extension such as ".js", ".php" or ".png".
	if dot := strings.LastIndex(segment, "."); dot > 0 {
		switch strings.ToLower(segment[dot+1:]) {
		case "js", "php", "png", "jpg", "jpeg", "gif", "svg", "css", "html", "htm", "ico", "xml", "json":
			return "", false
		}
	}
	if lower {
		segment = strings.ToLower(segment)
	}
	return segment, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func stringSet(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		out[v] = struct{}{}
	}
	return out
}
