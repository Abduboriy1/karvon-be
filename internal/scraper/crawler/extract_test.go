package crawler

import (
	"net/url"
	"testing"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestExtractEmailsPrefersMailtoLinks(t *testing.T) {
	html := `
	<html><body>
	  <p>Write to us at hello@ironworksgym.com</p>
	  <a href="mailto:info@ironworksgym.com?subject=Hi">Email us</a>
	</body></html>`

	found := ExtractEmails(Page{URL: mustURL(t, "https://ironworksgym.com/"), Body: []byte(html)})
	if len(found) != 2 {
		t.Fatalf("got %d addresses, want 2: %+v", len(found), found)
	}
	if found[0].Email != "info@ironworksgym.com" || found[0].Source != SourceMailto {
		t.Fatalf("first hit = %+v, want the mailto address first", found[0])
	}
	if found[1].Email != "hello@ironworksgym.com" || found[1].Source != SourceRegex {
		t.Fatalf("second hit = %+v", found[1])
	}
	if found[0].PageURL != "https://ironworksgym.com/" {
		t.Errorf("PageURL = %q", found[0].PageURL)
	}
}

func TestExtractEmailsDecodesAndTrimsMailto(t *testing.T) {
	html := `<a href="mailto:Info%40ironworksgym.com,second@ironworksgym.com?subject=x">mail</a>`
	found := ExtractEmails(Page{URL: mustURL(t, "https://ironworksgym.com/"), Body: []byte(html)})
	if len(found) != 1 || found[0].Email != "info@ironworksgym.com" {
		t.Fatalf("got %+v, want the first, percent-decoded recipient", found)
	}
}

func TestExtractEmailsIgnoresScriptsAndDenylistedAddresses(t *testing.T) {
	html := `
	<html><head>
	  <script>var support = "noreply@ironworksgym.com"; var t = "tracker@sentry.io";</script>
	  <style>.a{background:url(logo.png@2x.png)}</style>
	</head><body>
	  <p>noreply@ironworksgym.com and someone@wixpress.com</p>
	  <a href="mailto:info@ironworksgym.com">ok</a>
	</body></html>`

	found := ExtractEmails(Page{URL: mustURL(t, "https://ironworksgym.com/"), Body: []byte(html)})
	if len(found) != 1 {
		t.Fatalf("got %+v, want only the one usable address", found)
	}
	if found[0].Email != "info@ironworksgym.com" {
		t.Fatalf("Email = %q", found[0].Email)
	}
}

func TestExtractEmailsDeduplicatesAcrossSources(t *testing.T) {
	html := `
	<a href="mailto:info@ironworksgym.com">a</a>
	<p>info@ironworksgym.com</p>
	<a href="mailto:INFO@ironworksgym.com">b</a>`

	found := ExtractEmails(Page{URL: mustURL(t, "https://ironworksgym.com/"), Body: []byte(html)})
	if len(found) != 1 {
		t.Fatalf("got %d addresses, want 1: %+v", len(found), found)
	}
	if found[0].Source != SourceMailto {
		t.Fatalf("Source = %q, want the mailto provenance to win", found[0].Source)
	}
}

func TestExtractEmailsFindsAddressesInAttributes(t *testing.T) {
	html := `<div data-email="amy@ironworksgym.com"></div><meta content="ben@ironworksgym.com">`
	found := ExtractEmails(Page{URL: mustURL(t, "https://ironworksgym.com/"), Body: []byte(html)})
	if len(found) != 2 {
		t.Fatalf("got %+v, want both attribute addresses", found)
	}
}

func TestExtractContactLinks(t *testing.T) {
	html := `
	<a href="/about-us">About us</a>
	<a href="/pricing">Pricing</a>
	<a href="/contact">Get in touch</a>
	<a href="https://facebook.com/contact">Facebook</a>
	<a href="/schedule">Book a class</a>
	<a href="/contact">Get in touch (again)</a>`

	page := Page{URL: mustURL(t, "https://ironworksgym.com/"), Body: []byte(html)}
	links := ExtractContactLinks(page, 5)

	var got []string
	for _, link := range links {
		got = append(got, link.String())
	}
	// Ranked by word priority: the contact link first, then about, then the booking
	// link, regardless of where each sat in the document.
	want := []string{
		"https://ironworksgym.com/contact",
		"https://ironworksgym.com/about-us",
		"https://ironworksgym.com/schedule",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestExtractContactLinksRespectsTheLimit(t *testing.T) {
	html := `<a href="/contact-1">contact</a><a href="/contact-2">contact</a><a href="/contact-3">contact</a>`
	page := Page{URL: mustURL(t, "https://ironworksgym.com/"), Body: []byte(html)}
	if links := ExtractContactLinks(page, 2); len(links) != 2 {
		t.Fatalf("got %d links, want 2", len(links))
	}
}

func TestExtractContactLinksRanksContactAboveAboutAndKeepsDocumentOrderForTies(t *testing.T) {
	html := `
	<a href="/about">About</a>
	<a href="/team">Team</a>
	<a href="/contact-us">Contact</a>
	<a href="/contact.html">Contact Us</a>`
	page := Page{URL: mustURL(t, "https://ironworksgym.com/"), Body: []byte(html)}

	var got []string
	for _, link := range ExtractContactLinks(page, 10) {
		got = append(got, link.Path)
	}
	want := []string{"/contact-us", "/contact.html", "/about", "/team"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestExtractContactLinksWithCustomWords(t *testing.T) {
	html := `<a href="/contact">Contact</a><a href="/holler">Holler</a>`
	page := Page{URL: mustURL(t, "https://ironworksgym.com/"), Body: []byte(html)}
	links := ExtractContactLinksWith(page, []string{"holler"}, 10)
	if len(links) != 1 || links[0].Path != "/holler" {
		t.Fatalf("got %v, want only /holler", links)
	}
}
