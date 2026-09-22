// Package crawler fetches business websites and extracts contact email addresses,
// respecting robots.txt and a per-host rate limit.
package crawler

import (
	"bytes"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/net/html"

	"github.com/bory/karvon-be/internal/business"
)

// Source records how an address was discovered.
type Source string

const (
	// SourceMailto is an address taken from a mailto: link.
	SourceMailto Source = "mailto"
	// SourceRegex is an address matched in page text or attributes.
	SourceRegex Source = "regex"
)

// Found is one accepted address with its provenance.
type Found struct {
	Email   string
	Source  Source
	PageURL string
}

// emailPattern is deliberately permissive; business.AcceptEmail does the filtering.
var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// DefaultContactWords mark links likely to lead to a page that carries an email
// address. Order is priority: a link matching an earlier word is fetched before one
// matching a later word, so "Contact Us" always beats "About". Matching is a
// case-insensitive substring test against both the href and the link text, so
// "contact" also covers "contact-us", "contactus" and "contact.html".
//
// Extend the list with KARVON_CRAWL_CONTACT_WORDS (comma-separated, appended).
var DefaultContactWords = []string{
	"contact",
	"get-in-touch", "getintouch", "get in touch", "in-touch",
	"reach-us", "reachus", "reach us", "reach out",
	"email", "e-mail", "mail us",
	"enquir", "inquir", "quote", "estimate",
	"support", "help",
	"about", "team", "staff", "our-people", "meet",
	"location", "find-us", "findus", "find us", "visit", "directions",
	"book", "appointment", "schedule",
	"impressum", "kontakt", "contacto", "contatti",
}

// Page is a fetched document ready for extraction.
type Page struct {
	URL  *url.URL
	Body []byte
}

// ExtractEmails pulls addresses out of a page. mailto: links are preferred, so they
// appear first in the result and win when the same address is found twice.
//
// Choosing which address becomes primary is the business package's job; this function
// only reports what the page contains.
func ExtractEmails(page Page) []Found {
	var (
		found []Found
		seen  = make(map[string]struct{})
	)
	add := func(raw string, source Source) {
		email, ok := business.AcceptEmail(raw)
		if !ok {
			return
		}
		if _, dup := seen[email]; dup {
			return
		}
		seen[email] = struct{}{}
		found = append(found, Found{Email: email, Source: source, PageURL: page.URL.String()})
	}

	var textBuf bytes.Buffer
	skipDepth := 0

	tokenizer := html.NewTokenizer(bytes.NewReader(page.Body))
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			// Second pass: regex over the visible text collected above.
			for _, match := range emailPattern.FindAllString(textBuf.String(), -1) {
				add(match, SourceRegex)
			}
			return sortMailtoFirst(found)

		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			switch token.Data {
			case "script", "style", "noscript":
				if token.Type == html.StartTagToken {
					skipDepth++
				}
			}
			for _, attr := range token.Attr {
				switch attr.Key {
				case "href", "src", "data-email", "data-mail", "content":
					value := strings.TrimSpace(attr.Val)
					if strings.HasPrefix(strings.ToLower(value), "mailto:") {
						add(decodeMailto(value), SourceMailto)
						continue
					}
					for _, match := range emailPattern.FindAllString(value, -1) {
						add(match, SourceRegex)
					}
				}
			}

		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			switch string(name) {
			case "script", "style", "noscript":
				if skipDepth > 0 {
					skipDepth--
				}
			}

		case html.TextToken:
			if skipDepth == 0 {
				textBuf.Write(tokenizer.Text())
				textBuf.WriteByte('\n')
			}
		}
	}
}

// decodeMailto strips the scheme, any query string and percent-encoding.
func decodeMailto(value string) string {
	value = value[len("mailto:"):]
	if idx := strings.IndexAny(value, "?#"); idx >= 0 {
		value = value[:idx]
	}
	if decoded, err := url.QueryUnescape(value); err == nil {
		value = decoded
	}
	// A mailto: may carry several comma-separated recipients; take the first.
	if idx := strings.Index(value, ","); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSpace(value)
}

// sortMailtoFirst is a stable partition that keeps mailto hits ahead of regex hits.
func sortMailtoFirst(found []Found) []Found {
	if len(found) < 2 {
		return found
	}
	out := make([]Found, 0, len(found))
	for _, f := range found {
		if f.Source == SourceMailto {
			out = append(out, f)
		}
	}
	for _, f := range found {
		if f.Source != SourceMailto {
			out = append(out, f)
		}
	}
	return out
}

// ExtractContactLinks returns same-host URLs that look like contact or about pages,
// best candidates first, deduplicated. It uses DefaultContactWords; see
// ExtractContactLinksWith to supply a custom list.
func ExtractContactLinks(page Page, limit int) []*url.URL {
	return ExtractContactLinksWith(page, DefaultContactWords, limit)
}

// ExtractContactLinksWith is ExtractContactLinks with an explicit, priority-ordered
// word list. Links are ranked by the index of the first word they match; ties keep
// document order. At most limit links are returned.
func ExtractContactLinksWith(page Page, words []string, limit int) []*url.URL {
	if limit <= 0 || len(words) == 0 {
		return nil
	}
	lowerWords := make([]string, 0, len(words))
	for _, w := range words {
		if w = strings.ToLower(strings.TrimSpace(w)); w != "" {
			lowerWords = append(lowerWords, w)
		}
	}

	type candidate struct {
		link     *url.URL
		priority int
		order    int
	}
	var (
		found []candidate
		seen  = make(map[string]int) // url -> index into found
	)

	consider := func(href, text string) {
		if href == "" {
			return
		}
		lowerHref, lowerText := strings.ToLower(href), strings.ToLower(text)
		priority := -1
		for i, word := range lowerWords {
			if strings.Contains(lowerHref, word) || strings.Contains(lowerText, word) {
				priority = i
				break
			}
		}
		if priority < 0 {
			return
		}
		resolved, err := page.URL.Parse(href)
		if err != nil {
			return
		}
		resolved.Fragment = ""
		if resolved.Scheme != "http" && resolved.Scheme != "https" {
			return
		}
		if !business.SameSite(resolved.Hostname(), page.URL.Hostname()) {
			return
		}
		if resolved.String() == page.URL.String() {
			return
		}
		key := resolved.String()
		if idx, dup := seen[key]; dup {
			// The same page linked twice keeps its best priority.
			if priority < found[idx].priority {
				found[idx].priority = priority
			}
			return
		}
		seen[key] = len(found)
		found = append(found, candidate{link: resolved, priority: priority, order: len(found)})
	}

	tokenizer := html.NewTokenizer(bytes.NewReader(page.Body))
	var pendingHref string
	var textBuf bytes.Buffer

scan:
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			consider(pendingHref, textBuf.String())
			break scan

		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			if token.Data != "a" {
				continue
			}
			consider(pendingHref, textBuf.String())
			pendingHref, textBuf = "", bytes.Buffer{}
			for _, attr := range token.Attr {
				if attr.Key == "href" {
					pendingHref = strings.TrimSpace(attr.Val)
				}
			}
			if token.Type == html.SelfClosingTagToken {
				consider(pendingHref, "")
				pendingHref = ""
			}

		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			if string(name) == "a" {
				consider(pendingHref, textBuf.String())
				pendingHref, textBuf = "", bytes.Buffer{}
			}

		case html.TextToken:
			if pendingHref != "" {
				textBuf.Write(tokenizer.Text())
			}
		}
	}

	sort.SliceStable(found, func(i, j int) bool {
		if found[i].priority != found[j].priority {
			return found[i].priority < found[j].priority
		}
		return found[i].order < found[j].order
	})
	if len(found) > limit {
		found = found[:limit]
	}
	out := make([]*url.URL, 0, len(found))
	for _, c := range found {
		out = append(out, c.link)
	}
	return out
}
