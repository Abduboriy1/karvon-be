package exclusion

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

func TestDomainOfFoldsEverySpellingToOneHost(t *testing.T) {
	for _, raw := range []string{
		"example.com", "EXAMPLE.com", "www.example.com", "https://www.Example.com/contact?x=1",
		"http://example.com:8080", "@example.com", "Jane.Doe@Example.com", "example.com.", " example.com ",
	} {
		if got := domainOf(raw); got != "example.com" {
			t.Errorf("domainOf(%q) = %q, want example.com", raw, got)
		}
	}
	if got := domainOf("shop.example.co.uk"); got != "shop.example.co.uk" {
		t.Errorf("a subdomain lost its labels: %q", got)
	}
	for _, raw := range []string{"", "localhost", "127.0.0.1", "not a domain", "mailto:"} {
		if got := domainOf(raw); got != "" {
			t.Errorf("domainOf(%q) = %q, want nothing", raw, got)
		}
	}
}

func TestNormalizeProducesTheMatchKey(t *testing.T) {
	s := &Service{}
	ctx := context.Background()
	cases := []struct {
		kind, value, want string
	}{
		{KindEmail, "  Jane.Doe@Example.COM ", "jane.doe@example.com"},
		{KindEmailDomain, "jane@Mail.Example.com", "mail.example.com"},
		{KindDomain, "https://www.example.com/", "example.com"},
	}
	for _, tc := range cases {
		params, err := s.normalize(ctx, Input{Kind: tc.kind, Value: tc.value})
		if err != nil {
			t.Fatalf("%s %q: %v", tc.kind, tc.value, err)
		}
		if params.Value != tc.want || params.MatchMode != MatchExact || params.Source != SourceManual {
			t.Errorf("%s %q -> %+v, want value %q", tc.kind, tc.value, params, tc.want)
		}
		if params.DisplayValue != strings.TrimSpace(tc.value) {
			t.Errorf("the display value %q was not kept as entered", params.DisplayValue)
		}
	}
}

func TestNormalizeRejectsWhatCannotMatchReliably(t *testing.T) {
	s := &Service{}
	ctx := context.Background()
	reason := strings.Repeat("x", MaxReasonLen+1)
	cases := []struct {
		name  string
		in    Input
		field string
	}{
		{"empty value", Input{Kind: KindDomain, Value: "  "}, "value"},
		{"unknown kind", Input{Kind: "planet", Value: "x"}, "kind"},
		{"bad address", Input{Kind: KindEmail, Value: "not-an-address"}, "value"},
		{"public suffix", Input{Kind: KindDomain, Value: "co.uk"}, "value"},
		{"bare tld", Input{Kind: KindEmailDomain, Value: "@com"}, "value"},
		{"prefix on a domain", Input{Kind: KindDomain, Value: "example.com", MatchMode: MatchPrefix}, "match_mode"},
		{"unknown mode", Input{Kind: KindCompany, Value: "Acme", MatchMode: "fuzzy"}, "match_mode"},
		{"unknown source", Input{Kind: KindDomain, Value: "example.com", Source: "robot"}, "source"},
		{"long reason", Input{Kind: KindDomain, Value: "example.com", Reason: &reason}, "reason"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.normalize(ctx, tc.in)
			var appErr *apperr.Error
			if !errors.As(err, &appErr) || appErr.Code != apperr.CodeValidationFailed {
				t.Fatalf("err = %v, want a validation error", err)
			}
			found := false
			for _, f := range appErr.Fields {
				found = found || f.Field == tc.field
			}
			if !found {
				t.Errorf("fields = %+v, want one on %q", appErr.Fields, tc.field)
			}
		})
	}
}

func TestWarningsFlagRulesBroaderThanIntended(t *testing.T) {
	cases := []struct {
		name     string
		params   dbgen.CreateGlobalExclusionParams
		affected db.ExclusionAffected
		want     string
	}{
		{"free mail domain", dbgen.CreateGlobalExclusionParams{Kind: KindEmailDomain, Value: "gmail.com"}, db.ExclusionAffected{}, "shared mailbox"},
		{"many businesses", dbgen.CreateGlobalExclusionParams{Kind: KindDomain, Value: "big.com"}, db.ExclusionAffected{Businesses: broadRuleThreshold}, "covers"},
		{"one-word prefix", dbgen.CreateGlobalExclusionParams{Kind: KindCompany, Value: "apple", MatchMode: MatchPrefix}, db.ExclusionAffected{}, "one-word prefix"},
		{"ordinary rule", dbgen.CreateGlobalExclusionParams{Kind: KindCompany, Value: "planet fitness", MatchMode: MatchPrefix}, db.ExclusionAffected{Businesses: 12}, ""},
	}
	for _, tc := range cases {
		got := warningFor(tc.params, tc.affected)
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("%s: unexpected warning %q", tc.name, *got)
		case tc.want != "" && (got == nil || !strings.Contains(*got, tc.want)):
			t.Errorf("%s: warning = %v, want one containing %q", tc.name, got, tc.want)
		}
	}
}
