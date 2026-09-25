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
