package registrar

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/registrar/cloudflare"
)

func TestNormalizeDomain(t *testing.T) {
	good := map[string]string{
		"Example.COM":                 "example.com",
		"  example.com.  ":            "example.com",
		"https://www.example.com/a?b": "example.com",
		"café.com":                    "xn--caf-dma.com",
		"xn--caf-dma.com":             "xn--caf-dma.com",
		"my-shop.co.uk":               "my-shop.co.uk",
	}
	for in, want := range good {
		got, err := NormalizeDomain(in)
		if err != nil || got != want {
			t.Errorf("NormalizeDomain(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "com", "exa mple.com", "-bad-.com", "a..com", strings.Repeat("a", 64) + ".com"} {
		if got, err := NormalizeDomain(in); err == nil {
			t.Errorf("NormalizeDomain(%q) = %q, want an error", in, got)
		}
	}
}

func purchaseOf(n int) PurchaseInput {
	in := PurchaseInput{Confirm: true}
	for i := 0; i < n; i++ {
		in.Domains = append(in.Domains, PurchaseDomain{Name: fmt.Sprintf("shop%d.com", i), ExpectedCostCents: 1011})
	}
	return in
}

func fieldsOf(t *testing.T, err error) map[string]string {
	t.Helper()
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Code != apperr.CodeValidationFailed {
		t.Fatalf("err = %v, want a validation error", err)
	}
	out := map[string]string{}
	for _, f := range appErr.Fields {
		out[f.Field] = f.Message
	}
	return out
}

func TestValidatePurchaseCapsAtTen(t *testing.T) {
	if _, err := validatePurchase(purchaseOf(MaxDomainsPerPurchase)); err != nil {
		t.Fatalf("ten domains were refused: %v", err)
	}
	_, err := validatePurchase(purchaseOf(MaxDomainsPerPurchase + 1))
	if msg := fieldsOf(t, err)["domains"]; !strings.Contains(msg, "at most 10") {
		t.Fatalf("eleven domains: message = %q", msg)
	}
}

func TestValidatePurchaseNeedsConfirmation(t *testing.T) {
	in := purchaseOf(1)
	in.Confirm = false
	if _, ok := fieldsOf(t, mustErr(validatePurchase(in)))["confirm"]; !ok {
		t.Fatal("an unconfirmed purchase was not refused on confirm")
	}
}

func TestValidatePurchaseRejectsRepeatsAndBadNames(t *testing.T) {
	in := PurchaseInput{Confirm: true, Domains: []PurchaseDomain{
		{Name: "shop.com", ExpectedCostCents: 100},
		{Name: "SHOP.com.", ExpectedCostCents: 100},
		{Name: "not a domain", ExpectedCostCents: 100},
		{Name: "ok.dev", ExpectedCostCents: -1},
	}}
	fields := fieldsOf(t, mustErr(validatePurchase(in)))
	for _, want := range []string{"domains[1]", "domains[2]", "domains[3].expected_cost_cents"} {
		if _, ok := fields[want]; !ok {
			t.Errorf("no error on %s; got %v", want, fields)
		}
	}
}

func mustErr(_ []string, err error) error { return err }

func TestRefusal(t *testing.T) {
	priced := func(cost string) Offer {
		return toOffer(cloudflare.Offer{Name: "a.com", Registrable: true, Tier: "standard",
			Pricing: &cloudflare.Pricing{Currency: "USD", RegistrationCost: cost, RenewalCost: cost}})
	}
	cases := []struct {
		name  string
		offer Offer
		found bool
		want  string
	}{
		{"same price", priced("10.11"), true, ""},
		{"cheaper", priced("9.00"), true, ""},
		{"dearer by a cent", priced("10.12"), true, CodePriceChanged},
		{"missing", Offer{}, false, CodeNotReturned},
		{"taken", toOffer(cloudflare.Offer{Name: "a.com", Reason: "domain_unavailable"}), true, "domain_unavailable"},
		{"premium", toOffer(cloudflare.Offer{Name: "a.com", Registrable: true, Tier: "premium",
			Pricing: &cloudflare.Pricing{Currency: "USD", RegistrationCost: "1", RenewalCost: "1"}}), true, CodePremium},
		{"unreadable price", toOffer(cloudflare.Offer{Name: "a.com", Registrable: true, Tier: "standard",
			Pricing: &cloudflare.Pricing{Currency: "USD", RegistrationCost: "ten", RenewalCost: "1"}}), true, CodeNoPrice},
	}
	for _, tc := range cases {
		code, message := refusal("a.com", tc.offer, tc.found, 1011)
		if code != tc.want {
			t.Errorf("%s: code = %q, want %q (%s)", tc.name, code, tc.want, message)
		}
		if tc.want != "" && message == "" {
			t.Errorf("%s: no message for the operator", tc.name)
		}
	}
}

func TestPurchasableNeedsAStandardPricedRegistrableName(t *testing.T) {
	offer := toOffer(cloudflare.Offer{Name: "a.com", Registrable: true, Tier: "standard",
		Pricing: &cloudflare.Pricing{Currency: "usd", RegistrationCost: "10.11", RenewalCost: "12.00"}})
	if !offer.Purchasable || offer.Pricing.RegistrationCostCents != 1011 || offer.Pricing.RenewalCostCents != 1200 ||
		offer.Pricing.Currency != "USD" {
		t.Fatalf("offer = %+v, pricing = %+v", offer, offer.Pricing)
	}
	if toOffer(cloudflare.Offer{Name: "a.com", Registrable: true, Tier: "standard"}).Purchasable {
		t.Fatal("an unpriced offer was purchasable")
	}
}
