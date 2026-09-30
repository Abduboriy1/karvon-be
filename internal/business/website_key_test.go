package business

import "testing"

func TestWebsiteKeyKeepsThePathAndIgnoresNoise(t *testing.T) {
	same := []string{
		"https://www.CityOfCape.org/Parks/Aquatics",
		"http://cityofcape.org/parks/aquatics/",
		"https://cityofcape.org/parks/aquatics?utm_source=google#hours",
	}
	for _, website := range same {
		if got := WebsiteKey(website); got != "cityofcape.org/parks/aquatics" {
			t.Errorf("WebsiteKey(%q) = %q", website, got)
		}
	}
	if WebsiteKey("https://cityofcape.org/parks/aquatics") == WebsiteKey("https://cityofcape.org/parks/arena") {
		t.Error("two pages on one domain must have different keys")
	}
	if got := WebsiteKey("https://www.ironworksgym.com/"); got != "ironworksgym.com" {
		t.Errorf("homepage key = %q", got)
	}
}

func TestWebsiteKeyKeepsTheQueryThatNamesThePage(t *testing.T) {
	// Every Facebook page without a vanity name lives on one path; its id is the page.
	if WebsiteKey("https://www.facebook.com/profile.php?id=61551337991356") ==
		WebsiteKey("https://www.facebook.com/profile.php?id=61585270274495") {
		t.Error("two Facebook profiles must have different keys")
	}
	if WebsiteKey("https://www.hy-vee.com/stores/detail.aspx?s=12") ==
		WebsiteKey("https://www.hy-vee.com/stores/detail.aspx?s=34") {
		t.Error("two store pages that differ by query must have different keys")
	}

	same := []string{
		"https://www.facebook.com/profile.php?id=61567420741356&mibextid=wwXIfr&mibextid=wwXIfr",
		"https://facebook.com/profile.php?utm_source=google&id=61567420741356#about",
		"http://www.facebook.com/profile.php/?fbclid=abc&id=61567420741356",
	}
	cvs := "https://www.cvs.com/store-locator/visalia-ca-pharmacies/3619-w-caldwell-ave-visalia-ca-93277/storeid=9271"
	if WebsiteKey(cvs+"?WT.mc_id=LS_GOOGLE_RX_9271") != WebsiteKey(cvs+"?WT.mc_id=LS_GOOGLE_FS_9271") {
		t.Error("a WebTrends campaign tag must not tell two listings of one store apart")
	}
	for _, website := range same {
		if got := WebsiteKey(website); got != "facebook.com/profile.php?id=61567420741356" {
			t.Errorf("WebsiteKey(%q) = %q", website, got)
		}
	}
	if got := WebsiteKey("https://x.com/p?b=2&a=1"); got != WebsiteKey("https://x.com/p?a=1&b=2") {
		t.Errorf("parameter order must not matter, got %q", got)
	}
}

func TestWebsitePathKeyDropsTheQuery(t *testing.T) {
	if got := WebsitePathKey("https://www.facebook.com/profile.php?id=1#about"); got != "facebook.com/profile.php" {
		t.Errorf("WebsitePathKey = %q", got)
	}
}
