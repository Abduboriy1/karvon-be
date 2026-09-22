package business

import "testing"

func TestNormalizeWebsite(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantWebsite string
		wantDomain  string
		ok          bool
	}{
		{name: "adds scheme", input: "ironworksgym.com", wantWebsite: "https://ironworksgym.com", wantDomain: "ironworksgym.com", ok: true},
		{name: "strips www", input: "https://www.ironworksgym.com/", wantWebsite: "https://www.ironworksgym.com/", wantDomain: "ironworksgym.com", ok: true},
		{name: "lowercases host", input: "HTTPS://IronworksGym.com/Contact", wantWebsite: "https://ironworksgym.com/Contact", wantDomain: "ironworksgym.com", ok: true},
		{name: "drops fragment", input: "https://gym.com/a#top", wantWebsite: "https://gym.com/a", wantDomain: "gym.com", ok: true},
		{name: "keeps port", input: "http://gym.com:8080/x", wantWebsite: "http://gym.com:8080/x", wantDomain: "gym.com", ok: true},
		{name: "rejects empty", input: "   ", ok: false},
		{name: "rejects mailto", input: "mailto:a@b.com", ok: false},
		{name: "rejects localhost", input: "http://localhost:3000", ok: false},
		{name: "rejects ip literal", input: "http://127.0.0.1/", ok: false},
		{name: "rejects hostless", input: "https://", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			website, domain, ok := NormalizeWebsite(tt.input)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if domain != tt.wantDomain {
				t.Errorf("domain = %q, want %q", domain, tt.wantDomain)
			}
			if website != tt.wantWebsite {
				t.Errorf("website = %q, want %q", website, tt.wantWebsite)
			}
		})
	}
}

func TestSameSite(t *testing.T) {
	if !SameSite("www.gym.com", "gym.com") {
		t.Error("www and apex should be the same site")
	}
	if SameSite("gym.com", "other.com") {
		t.Error("different hosts should not match")
	}
}

func TestNormalizeWebsiteRejectsCredentialsAndOddSchemes(t *testing.T) {
	for _, input := range []string{
		"mailto:a@b.com",
		"MAILTO:a@b.com",
		"tel:+15125550100",
		"javascript:alert(1)",
		"https://user:pass@gym.com/",
		"ftp://files.gym.com",
	} {
		if _, _, ok := NormalizeWebsite(input); ok {
			t.Errorf("NormalizeWebsite(%q) should have been rejected", input)
		}
	}
}

func TestNormalizeWebsiteAcceptsHostWithExplicitPortAndNoScheme(t *testing.T) {
	_, domain, ok := NormalizeWebsite("gym.com:8080")
	if !ok {
		t.Fatal("host:port without a scheme should be accepted")
	}
	if domain != "gym.com" {
		t.Fatalf("domain = %q, want gym.com", domain)
	}
}
