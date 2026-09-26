package category

import (
	"errors"
	"strings"
	"testing"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/scraper"
)

func strPtr(s string) *string { return &s }

func TestInputNormalizeDedupesAndTrims(t *testing.T) {
	in := Input{Name: strPtr("  Coffee   shops "), Terms: []string{" cafe ", "Cafe", "", "espresso  bar"}}.normalize()
	if *in.Name != "Coffee shops" {
		t.Fatalf("name = %q", *in.Name)
	}
	if got := strings.Join(in.Terms, "|"); got != "cafe|espresso bar" {
		t.Fatalf("terms = %q", got)
	}
}

func TestInputValidate(t *testing.T) {
	tooMany := make([]string, scraper.MaxTerms+1)
	for i := range tooMany {
		tooMany[i] = strings.Repeat("x", i+1)
	}
	cases := []struct {
		name   string
		in     Input
		create bool
		ok     bool
	}{
		{"create ok", Input{Name: strPtr("Cafes"), Terms: []string{"cafe"}}, true, true},
		{"create missing name", Input{Terms: []string{"cafe"}}, true, false},
		{"create missing terms", Input{Name: strPtr("Cafes")}, true, false},
		{"blank terms", Input{Name: strPtr("Cafes"), Terms: []string{"  "}}, true, false},
		{"too many terms", Input{Name: strPtr("Cafes"), Terms: tooMany}, true, false},
		{"term too long", Input{Name: strPtr("Cafes"), Terms: []string{strings.Repeat("x", scraper.MaxTermLen+1)}}, true, false},
		{"name too long", Input{Name: strPtr(strings.Repeat("x", MaxNameLen+1))}, false, false},
		{"update empty is noop", Input{}, false, true},
		{"update blank name", Input{Name: strPtr("   ")}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.normalize().validate(tc.create)
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok {
				var appErr *apperr.Error
				if !errors.As(err, &appErr) || appErr.Code != apperr.CodeValidationFailed {
					t.Fatalf("want validation error, got %v", err)
				}
			}
		})
	}
}
