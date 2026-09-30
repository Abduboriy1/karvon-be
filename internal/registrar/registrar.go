// Package registrar is the domains module: it searches for domain names, buys them
// through the Cloudflare Registrar API, and manages the ones the account owns.
//
// Buying is the only irreversible, billed action in the module, and it is guarded
// three ways:
//
//   - a purchase is explicitly confirmed and carries the price the operator saw for
//     every domain; nothing is registered above that price;
//   - a purchase holds at most MaxDomainsPerPurchase domains, and only one purchase
//     runs at a time, both enforced by the database as well as here;
//   - each registration is recorded as started before Cloudflare is called, so a
//     worker that dies mid-call is reconciled by asking Cloudflare what happened
//     rather than by registering again.
package registrar

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"golang.org/x/net/idna"

	"github.com/bory/karvon-be/internal/queue"
	"github.com/bory/karvon-be/internal/registrar/cloudflare"
)

// The source row holding the Cloudflare API token.
const (
	RoleRegistrar  = "registrar"
	KindCloudflare = "cloudflare"
)

// MaxDomainsPerPurchase is the most domains one purchase may buy. It is a hard rule,
// not a setting: the schema allows item positions 0-9 only.
const MaxDomainsPerPurchase = 10

// MaxTokenLength bounds what a client may send as an API token.
const MaxTokenLength = 500

// Purchase statuses, mirroring the domain_purchases.status check constraint.
const (
	PurchaseQueued     = "queued"
	PurchaseProcessing = "processing"
	PurchaseSucceeded  = "succeeded"
	PurchasePartial    = "partial"
	PurchaseFailed     = "failed"
)

// Item statuses, mirroring the domain_purchase_items.status check constraint.
const (
	ItemPending        = "pending"
	ItemRegistering    = "registering"
	ItemSucceeded      = "succeeded"
	ItemFailed         = "failed"
	ItemActionRequired = "action_required"
)

// Item error codes this module sets. Cloudflare's own codes (its availability
// reasons and workflow error codes) are passed through as they come.
const (
	CodePriceChanged     = "price_changed"
	CodeNoPrice          = "no_price"
	CodeCurrencyMismatch = "currency_mismatch"
	CodeNotReturned      = "not_returned"
	CodePremium          = "domain_premium"
	CodeUnavailable      = "domain_unavailable"
	CodeRejected         = "registration_rejected"
	CodeFailed           = "registration_failed"
	CodeNotConfigured    = "not_configured"
	CodeAuthFailed       = "auth_failed"
	CodeUnconfirmed      = "unconfirmed"
	CodeTimedOut         = "timed_out"
	CodeAborted          = "aborted"
)

// KindPurchase is the River job kind. It is persisted in the queue table, so
// renaming it is a migration rather than a refactor.
const KindPurchase = "domain_purchase"

// PurchaseArgs drives one purchase from queued to settled.
type PurchaseArgs struct {
	PurchaseID uuid.UUID `json:"purchase_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (PurchaseArgs) Kind() string { return KindPurchase }

// InsertOpts implements river.JobArgsWithInsertOpts. Snoozes while Cloudflare
// finishes a registration do not count as attempts.
func (PurchaseArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       queue.QueueDomains,
		MaxAttempts: 8,
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// idnaProfile is the lookup profile browsers use: it lowercases, maps full-width
// characters and validates labels, so "Café.COM" and "xn--caf-dma.com" agree.
var idnaProfile = idna.New(
	idna.MapForLookup(),
	idna.BidiRule(),
	idna.Transitional(false),
	idna.StrictDomainName(true),
)

// NormalizeDomain turns what an operator typed into the ASCII name Cloudflare
// expects: lower case, punycode for non-ASCII labels, no scheme, path or trailing dot.
func NormalizeDomain(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if i := strings.Index(name, "://"); i >= 0 {
		name = name[i+3:]
	}
	if i := strings.IndexAny(name, "/?#"); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimSuffix(name, ".")
	if name == "" {
		return "", fmt.Errorf("is empty")
	}
	ascii, err := idnaProfile.ToASCII(name)
	if err != nil {
		return "", fmt.Errorf("is not a valid domain name")
	}
	if strings.HasPrefix(ascii, "www.") && strings.Count(ascii, ".") >= 2 {
		ascii = strings.TrimPrefix(ascii, "www.")
	}
	labels := strings.Split(ascii, ".")
	if len(labels) < 2 || len(ascii) > 253 {
		return "", fmt.Errorf("must be a full domain name such as example.com")
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return "", fmt.Errorf("is not a valid domain name")
		}
	}
	return ascii, nil
}

// NormalizeExtension turns ".COM", "com" or "co.uk" into the bare lower-case
// extension Cloudflare filters search results by.
func NormalizeExtension(raw string) (string, error) {
	ext := strings.TrimPrefix(strings.TrimSpace(raw), ".")
	if ext == "" {
		return "", fmt.Errorf("is empty")
	}
	ascii, err := idnaProfile.ToASCII(ext)
	if err != nil || len(ascii) > 63 {
		return "", fmt.Errorf("is not a valid extension")
	}
	return ascii, nil
}

// Offer is one domain from a search or a check, with its prices in cents.
type Offer struct {
	Name        string
	Registrable bool
	Tier        string
	Reason      *string
	Pricing     *Pricing
	// Purchasable is Registrable narrowed to what this module will actually buy:
	// a standard-tier name with a price that reads exactly.
	Purchasable bool
}

// Pricing is an offer's price, as Cloudflare wrote it and in cents.
type Pricing struct {
	Currency              string
	RegistrationCost      string
	RenewalCost           string
	RegistrationCostCents int64
	RenewalCostCents      int64
}

// Registration is a domain the Cloudflare account owns.
type Registration = cloudflare.Registration

func toOffer(in cloudflare.Offer) Offer {
	out := Offer{Name: in.Name, Registrable: in.Registrable, Tier: in.Tier}
	if in.Reason != "" {
		reason := in.Reason
		out.Reason = &reason
	}
	if in.Pricing != nil {
		registration, regErr := cloudflare.ParseCents(in.Pricing.RegistrationCost)
		renewal, renErr := cloudflare.ParseCents(in.Pricing.RenewalCost)
		if regErr == nil && renErr == nil {
			out.Pricing = &Pricing{
				Currency:              strings.ToUpper(in.Pricing.Currency),
				RegistrationCost:      in.Pricing.RegistrationCost,
				RenewalCost:           in.Pricing.RenewalCost,
				RegistrationCostCents: registration,
				RenewalCostCents:      renewal,
			}
		}
	}
	out.Purchasable = out.Registrable && out.Tier != cloudflare.TierPremium && out.Pricing != nil
	return out
}
