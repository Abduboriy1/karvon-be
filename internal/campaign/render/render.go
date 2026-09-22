// Package render assembles reusable email components into a subject and a body,
// substitutes {{placeholder}} variables for one contact, and converts between the
// plain-text form we edit and the HTML form Instantly sends.
//
// Everything here is pure: no I/O, no clock, no provider knowledge beyond the
// component order defined by the campaign package.
package render

import (
	"html"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/campaign"
)

// Slot is one component chosen for one step: its type, its body text and the
// component it came from.
type Slot struct {
	Type        string
	Body        string
	ComponentID uuid.UUID
}

// Assemble orders the slots by campaign.ComponentTypes (subject first) and returns
// the subject (the body of the "subject" slot, "" if none) and the body: every
// non-subject slot body, trimmed, joined by a blank line. Slots of unknown type
// keep their relative order after the known ones; empty slots are skipped.
func Assemble(slots []Slot) (subject, body string) {
	ordered := make([]Slot, len(slots))
	copy(ordered, slots)
	sort.SliceStable(ordered, func(i, j int) bool {
		return componentRank(ordered[i].Type) < componentRank(ordered[j].Type)
	})

	parts := make([]string, 0, len(ordered))
	for _, s := range ordered {
		text := strings.TrimSpace(s.Body)
		if s.Type == campaign.ComponentSubject {
			if subject == "" {
				subject = text
			}
			continue
		}
		if text == "" {
			continue
		}
		parts = append(parts, text)
	}
	return subject, strings.Join(parts, "\n\n")
}

func componentRank(t string) int {
	for i, ct := range campaign.ComponentTypes {
		if ct == t {
			return i
		}
	}
	return len(campaign.ComponentTypes)
}

// placeholderRE matches {{name}}, {{ name }} and {{name|fallback text}}. Group 1 is
// the name, group 2 the optional fallback.
var placeholderRE = regexp.MustCompile(`\{\{\s*([^{}|]*?)\s*(?:\|([^{}]*))?\}\}`)

// Render substitutes placeholders in template. Names are case-insensitive and
// matched against the vars keys (lower-cased). A known var with a non-empty value
// wins; otherwise the fallback if one is given; otherwise the placeholder renders
// as "". Unknown placeholders without a fallback also render as "".
func Render(template string, vars map[string]string) string {
	lowered := make(map[string]string, len(vars))
	for k, v := range vars {
		lowered[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return placeholderRE.ReplaceAllStringFunc(template, func(match string) string {
		m := placeholderRE.FindStringSubmatch(match)
		name := strings.ToLower(strings.TrimSpace(m[1]))
		if v, ok := lowered[name]; ok && v != "" {
			return v
		}
		return strings.TrimSpace(m[2])
	})
}

// Placeholders returns the distinct placeholder names in template, lower-cased and
// without their fallbacks, in order of first appearance.
func Placeholders(template string) []string {
	seen := map[string]bool{}
	// Never nil: the value is stored in a NOT NULL text[] column, and a body with
	// no placeholders is perfectly ordinary.
	names := []string{}
	for _, m := range placeholderRE.FindAllStringSubmatch(template, -1) {
		name := strings.ToLower(strings.TrimSpace(m[1]))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// ContactInput is the contact data the standard placeholders are built from.
type ContactInput struct {
	FirstName, LastName, Company, Title, Email, Domain, Phone, Website, City, State string
}

// ContactVars builds the standard var map: first_name, last_name, full_name,
// company, title, email, domain, phone, website, city and state.
func ContactVars(c ContactInput) map[string]string {
	first := strings.TrimSpace(c.FirstName)
	last := strings.TrimSpace(c.LastName)
	return map[string]string{
		"first_name": first,
		"last_name":  last,
		"full_name":  strings.TrimSpace(first + " " + last),
		"company":    strings.TrimSpace(c.Company),
		"title":      strings.TrimSpace(c.Title),
		"email":      strings.TrimSpace(c.Email),
		"domain":     strings.TrimSpace(c.Domain),
		"phone":      strings.TrimSpace(c.Phone),
		"website":    strings.TrimSpace(c.Website),
		"city":       strings.TrimSpace(c.City),
		"state":      strings.TrimSpace(c.State),
	}
}

var blankLineRE = regexp.MustCompile(`\n[ \t]*\n`)

// HTMLBody converts rendered plain text into the HTML Instantly expects: the text
// is HTML-escaped, paragraphs (separated by blank lines) are wrapped in <p>…</p>,
// and single newlines inside a paragraph become <br/>. Returns "" for empty input.
func HTMLBody(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if strings.TrimSpace(text) == "" {
		return ""
	}
	var paragraphs []string
	for _, para := range blankLineRE.Split(text, -1) {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		lines := strings.Split(para, "\n")
		for i, line := range lines {
			lines[i] = html.EscapeString(strings.TrimSpace(line))
		}
		paragraphs = append(paragraphs, "<p>"+strings.Join(lines, "<br/>")+"</p>")
	}
	return strings.Join(paragraphs, "\n")
}

var (
	brRE          = regexp.MustCompile(`(?i)<br\s*/?>`)
	paragraphEnd  = regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|tr)\s*>`)
	tagRE         = regexp.MustCompile(`<[^>]*>`)
	manyNewlines  = regexp.MustCompile(`\n[ \t]*\n[\s]*\n`)
	trailingSpace = regexp.MustCompile(`[ \t]+\n`)
)

// TextBody strips simple HTML back to plain text, for previews of stored bodies:
// <br/> becomes a newline, a closing paragraph becomes a blank line, every other
// tag is removed and entities are unescaped.
func TextBody(htmlBody string) string {
	s := strings.ReplaceAll(htmlBody, "\r\n", "\n")
	s = brRE.ReplaceAllString(s, "\n")
	s = paragraphEnd.ReplaceAllString(s, "\n\n")
	s = tagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = trailingSpace.ReplaceAllString(s, "\n")
	for manyNewlines.MatchString(s) {
		s = manyNewlines.ReplaceAllString(s, "\n\n")
	}
	return strings.TrimSpace(s)
}
