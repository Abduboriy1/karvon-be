package crawler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSocialProfileCanonicalizesProfiles(t *testing.T) {
	cases := []struct {
		raw     string
		network Network
		handle  string
		url     string
	}{
		{"https://www.facebook.com/tahcrossfit", NetworkFacebook, "tahcrossfit", "https://www.facebook.com/tahcrossfit"},
		{"http://m.facebook.com/TahCrossFit/?ref=page", NetworkFacebook, "tahcrossfit", "https://www.facebook.com/tahcrossfit"},
		{"https://fb.com/tahcrossfit#about", NetworkFacebook, "tahcrossfit", "https://www.facebook.com/tahcrossfit"},
		{"https://www.facebook.com/profile.php?id=100063", NetworkFacebook, "profile.php?id=100063", "https://www.facebook.com/profile.php?id=100063"},
		{"https://www.facebook.com/pages/Tah-CrossFit/123456", NetworkFacebook, "pages/Tah-CrossFit/123456", "https://www.facebook.com/pages/Tah-CrossFit/123456"},
		{"https://www.instagram.com/tahcrossfit/", NetworkInstagram, "tahcrossfit", "https://www.instagram.com/tahcrossfit"},
		{"https://www.tiktok.com/@TahCrossFit?lang=en", NetworkTikTok, "@tahcrossfit", "https://www.tiktok.com/@tahcrossfit"},
		{"https://www.youtube.com/@tahcrossfit/videos", NetworkYouTube, "@tahcrossfit", "https://www.youtube.com/@tahcrossfit"},
		{"https://youtube.com/channel/UCaBcD123", NetworkYouTube, "channel/UCaBcD123", "https://www.youtube.com/channel/UCaBcD123"},
		{"https://www.linkedin.com/company/Tah-CrossFit/about/", NetworkLinkedIn, "company/tah-crossfit", "https://www.linkedin.com/company/tah-crossfit"},
		{"https://twitter.com/tahcrossfit", NetworkX, "tahcrossfit", "https://x.com/tahcrossfit"},
		{"https://x.com/@tahcrossfit", NetworkX, "tahcrossfit", "https://x.com/tahcrossfit"},
		{"https://www.threads.net/@tahcrossfit", NetworkThreads, "@tahcrossfit", "https://www.threads.net/@tahcrossfit"},
		{"https://www.pinterest.com/tahcrossfit/", NetworkPinterest, "tahcrossfit", "https://www.pinterest.com/tahcrossfit"},
		{"https://www.yelp.com/biz/tah-crossfit-smithville", NetworkYelp, "biz/tah-crossfit-smithville", "https://www.yelp.com/biz/tah-crossfit-smithville"},
		{"https://linktr.ee/tahcrossfit", NetworkLinktree, "tahcrossfit", "https://linktr.ee/tahcrossfit"},
	}
	for _, tc := range cases {
		got, ok := SocialProfile(tc.raw)
		if !ok {
			t.Errorf("SocialProfile(%q) rejected a profile", tc.raw)
			continue
		}
		if got.Network != tc.network || got.Handle != tc.handle || got.URL != tc.url {
			t.Errorf("SocialProfile(%q) = %+v, want %s %q %q", tc.raw, got, tc.network, tc.handle, tc.url)
		}
	}
}

func TestSocialProfileRejectsNonProfileLinks(t *testing.T) {
	for _, raw := range []string{
		"https://www.facebook.com/",
		"https://www.facebook.com/sharer/sharer.php?u=https%3A%2F%2Fironworksgym.com",
		"https://www.facebook.com/tr?id=123&ev=PageView",
		"https://www.facebook.com/plugins/page.php?href=x",
		"https://www.facebook.com/profile.php",
		"https://connect.facebook.net/en_US/sdk.js",
		"https://www.instagram.com/p/Cabc123/",
		"https://www.instagram.com/reel/Cabc123/",
		"https://www.instagram.com/embed.js",
		"https://www.tiktok.com/embed/v2/123",
		"https://www.youtube.com/watch?v=abc",
		"https://www.youtube.com/embed/abc",
		"https://youtu.be/abc",
		"https://twitter.com/intent/tweet?text=hi",
		"https://twitter.com/share",
		"https://www.linkedin.com/shareArticle?url=x",
		"https://www.pinterest.com/pin/create/button/",
		"https://www.yelp.com/search?find_desc=gym",
		"https://ironworksgym.com/facebook",
		"mailto:someone@facebook.com",
	} {
		if got, ok := SocialProfile(raw); ok {
			t.Errorf("SocialProfile(%q) = %+v, want rejection", raw, got)
		}
	}
}

func TestExtractSocialLinksFindsLinksJSONLDAndDeduplicates(t *testing.T) {
	page := Page{
		URL: mustURL(t, "https://ironworksgym.com/"),
		Body: []byte(`<html><head>
<script type="application/ld+json">{"@type":"Gym","sameAs":["https:\/\/www.instagram.com\/ironworks\/","https://www.yelp.com/biz/ironworks-gym"]}</script>
<script src="https://connect.facebook.net/en_US/sdk.js"></script>
</head><body>
<a href="https://www.facebook.com/IronWorksGym">Facebook</a>
<a href="https://www.facebook.com/sharer/sharer.php?u=x">Share</a>
<a href="//m.facebook.com/ironworksgym/">Facebook again</a>
<a href="https://www.instagram.com/ironworks">Instagram</a>
<a href="/contact">Contact</a>
</body></html>`),
	}
	got := ExtractSocialLinks(page)
	want := []string{
		"https://www.instagram.com/ironworks",
		"https://www.yelp.com/biz/ironworks-gym",
		"https://www.facebook.com/ironworksgym",
	}
	if len(got) != len(want) {
		t.Fatalf("ExtractSocialLinks = %+v, want %v", got, want)
	}
	for i, link := range got {
		if link.URL != want[i] {
			t.Errorf("link %d = %q, want %q", i, link.URL, want[i])
		}
		if link.PageURL != "https://ironworksgym.com/" {
			t.Errorf("link %d PageURL = %q", i, link.PageURL)
		}
	}
}

func TestCrawlSiteCollectsSocialLinksEvenWhenTheHomepageHasAnEmail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<a href="mailto:info@ironworksgym.com">mail</a>
<a href="https://www.facebook.com/ironworksgym">fb</a>`))
	})

	crawler, base := newTestCrawler(t, mux)
	result, err := crawler.CrawlSite(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Socials) != 1 || result.Socials[0].URL != "https://www.facebook.com/ironworksgym" {
		t.Fatalf("Socials = %+v", result.Socials)
	}
}

func TestCrawlSiteCollectsSocialLinksAcrossPages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/contact-ironworks-in-smithville", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<a href="https://www.facebook.com/ironworksgym">fb</a>
<a href="https://www.tiktok.com/@ironworksgym">tiktok</a>`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<a href="/contact-ironworks-in-smithville">Contact</a>
<a href="https://www.facebook.com/ironworksgym">fb</a>`))
	})

	crawler, base := newTestCrawler(t, mux)
	result, _ := crawler.CrawlSite(context.Background(), base)
	if len(result.Emails) != 0 {
		t.Fatalf("Emails = %+v, want none", result.Emails)
	}
	if len(result.Socials) != 2 {
		t.Fatalf("Socials = %+v, want facebook and tiktok once each", result.Socials)
	}
	if strings.Contains(result.Socials[0].PageURL, "contact") {
		t.Errorf("facebook PageURL = %q, want the homepage that linked it first", result.Socials[0].PageURL)
	}
	if result.Socials[1].Network != NetworkTikTok {
		t.Errorf("second social = %+v, want tiktok", result.Socials[1])
	}
}

func TestCrawlSiteKeepsASocialProfileUsedAsTheWebsite(t *testing.T) {
	crawler := New(Config{UserAgent: testAgent, Timeout: time.Second})
	result, err := crawler.CrawlSite(context.Background(), "https://www.facebook.com/tahcrossfit")
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped != SkipPlatform {
		t.Fatalf("Skipped = %q, want %q", result.Skipped, SkipPlatform)
	}
	if result.PagesFetched != 0 {
		t.Fatalf("PagesFetched = %d, want 0: social profiles are never fetched", result.PagesFetched)
	}
	if len(result.Socials) != 1 || result.Socials[0].URL != "https://www.facebook.com/tahcrossfit" || result.Socials[0].PageURL != "" {
		t.Fatalf("Socials = %+v", result.Socials)
	}
}
