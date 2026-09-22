package business

import "testing"

func TestAcceptEmail(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{name: "lowercases and trims", input: "  Info@IronworksGym.com  ", want: "info@ironworksgym.com", ok: true},
		{name: "plain address", input: "hello@ironworksgym.com", want: "hello@ironworksgym.com", ok: true},
		{name: "uppercase", input: "Hello@IronworksGym.com", want: "hello@ironworksgym.com", ok: true},
		{name: "strips punctuation", input: "(hello@ironworksgym.com),", want: "hello@ironworksgym.com", ok: true},
		{name: "role address noreply", input: "noreply@ironworksgym.com", ok: false},
		{name: "role address with plus tag", input: "no-reply+news@ironworksgym.com", ok: false},
		{name: "role address postmaster", input: "postmaster@ironworksgym.com", ok: false},
		{name: "role address abuse", input: "abuse@ironworksgym.com", ok: false},
		{name: "platform domain", input: "someone@wixpress.com", ok: false},
		{name: "platform subdomain", input: "a@mail.sentry.io", ok: false},
		{name: "example domain", input: "a@example.com", ok: false},
		{name: "image artefact", input: "logo.png@2x.png", ok: false},
		{name: "missing at", input: "not-an-email", ok: false},
		{name: "missing domain dot", input: "a@localhost", ok: false},
		{name: "numeric tld", input: "a@b.12", ok: false},
		{name: "double dot", input: "a@b..com", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := AcceptEmail(tt.input)
			if ok != tt.ok {
				t.Fatalf("AcceptEmail(%q) ok = %v, want %v (got %q)", tt.input, ok, tt.ok, got)
			}
			if ok && got != tt.want {
				t.Fatalf("AcceptEmail(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeEmailKeepsCaseInsensitiveIdentity(t *testing.T) {
	first, ok1 := NormalizeEmail("Info@Example.Org")
	second, ok2 := NormalizeEmail("info@example.org")
	if !ok1 || !ok2 {
		t.Fatal("both addresses should normalize")
	}
	if first != second {
		t.Fatalf("%q != %q: addresses differing only in case must normalize identically", first, second)
	}
}

func TestIsPlatformDomain(t *testing.T) {
	tests := map[string]bool{
		"wixpress.com":     true,
		"www.wixpress.com": true,
		"cdn.wixpress.com": true,
		"sentry.io":        true,
		"godaddy.com":      true,
		"ironworksgym.com": false,
		"gym.co.uk":        false,
		"":                 false,
	}
	for domain, want := range tests {
		if got := IsPlatformDomain(domain); got != want {
			t.Errorf("IsPlatformDomain(%q) = %v, want %v", domain, got, want)
		}
	}
}

func TestRankPrefersGenericMailboxesOnTheOwnDomain(t *testing.T) {
	const site = "ironworksgym.com"

	cases := []struct {
		better string
		worse  string
	}{
		{better: "info@" + site, worse: "hello@" + site},
		{better: "hello@" + site, worse: "contact@" + site},
		{better: "contact@" + site, worse: "trainer@" + site},
		{better: "trainer@" + site, worse: "info@some-other-agency.com"},
	}

	for _, c := range cases {
		if Rank(c.better, site) >= Rank(c.worse, site) {
			t.Errorf("Rank(%q)=%d should be lower than Rank(%q)=%d",
				c.better, Rank(c.better, site), c.worse, Rank(c.worse, site))
		}
	}
}

func TestPickPrimary(t *testing.T) {
	const site = "ironworksgym.com"

	tests := []struct {
		name   string
		emails []string
		want   int
	}{
		{
			name:   "info wins over hello and contact",
			emails: []string{"contact@ironworksgym.com", "hello@ironworksgym.com", "info@ironworksgym.com"},
			want:   2,
		},
		{
			name:   "first found wins when nothing is generic",
			emails: []string{"amy@ironworksgym.com", "ben@ironworksgym.com"},
			want:   0,
		},
		{
			name:   "own domain beats a generic third-party address",
			emails: []string{"info@marketing-agency.com", "amy@ironworksgym.com"},
			want:   1,
		},
		{name: "empty", emails: nil, want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PickPrimary(tt.emails, site); got != tt.want {
				t.Fatalf("PickPrimary = %d, want %d", got, tt.want)
			}
		})
	}
}
