// Package verify scores email addresses in two independent passes.
//
// Pass 1 is local and free: syntax, blocklists, DNS records and a few heuristics. It
// can award at most 85 points, so an address that has only been checked locally can
// never be tagged "Verified". Pass 2 calls a paid third-party API and is the only
// thing that can push a score to 100. The two passes are stored separately, so the
// UI can always show which pass contributed what.
package verify

import (
	"time"

	"github.com/google/uuid"
)

// Tag is the colour band a final score falls into. It is stored on the row and
// carried on the wire; the label and the icon are derived from it.
type Tag string

// The five tags, from best to worst.
const (
	TagGreen      Tag = "green"
	TagLightGreen Tag = "light_green"
	TagYellow     Tag = "yellow"
	TagOrange     Tag = "orange"
	TagRed        Tag = "red"
)

// Label is the human-readable name of a tag. Colour alone never conveys the result.
func (t Tag) Label() string {
	switch t {
	case TagGreen:
		return "Verified"
	case TagLightGreen:
		return "Likely valid"
	case TagYellow:
		return "Uncertain"
	case TagOrange:
		return "Risky"
	case TagRed:
		return "Invalid"
	default:
		return "Unknown"
	}
}

// Tags lists every tag in descending quality order.
var Tags = []Tag{TagGreen, TagLightGreen, TagYellow, TagOrange, TagRed}

// ValidTag reports whether a client-supplied value is one of the five tags.
func ValidTag(value string) bool {
	for _, tag := range Tags {
		if string(tag) == value {
			return true
		}
	}
	return false
}

// CheckStatus is the outcome of one Pass 1 check.
type CheckStatus string

// A check either earned its points, did not, or never ran.
const (
	CheckPass CheckStatus = "pass"
	CheckFail CheckStatus = "fail"
	CheckSkip CheckStatus = "skip"
)

// CheckResult is one line of the Pass 1 breakdown. Every check a pipeline knows about
// appears exactly once, so the UI can always explain a score in full.
type CheckResult struct {
	Key    string      `json:"key"`
	Label  string      `json:"label"`
	Status CheckStatus `json:"status"`
	Points int         `json:"points"`
	Max    int         `json:"max"`
	Detail string      `json:"detail,omitempty"`
}

// Check keys. They are persisted inside pass1_checks, so renaming one is a data
// migration rather than a refactor.
const (
	CheckSyntax       = "syntax"
	CheckRoleAccount  = "role_account"
	CheckDisposable   = "disposable_domain"
	CheckDNSResolves  = "dns_resolves"
	CheckStructure    = "structure"
	CheckTypo         = "typo"
	CheckHumanLocal   = "human_local"
	CheckFreeProvider = "not_free_provider"
	CheckMX           = "mx"
	CheckARecord      = "a_record"
	CheckSPF          = "spf"
	CheckDMARC        = "dmarc"
	CheckDomainAge    = "domain_age"
)

// Pass1Result is everything one local run learned about an address.
type Pass1Result struct {
	// Email is the normalized address the checks ran against.
	Email string
	// Domain is the lower-cased domain part.
	Domain string
	// Score is the final Pass 1 score after caps, between 0 and Pass1MaxScore.
	Score int
	// Checks is the full breakdown, in execution order.
	Checks []CheckResult
	// HardFail is the key of the check that zeroed the score, empty when none did.
	HardFail string
	// TypoSuggestion is the corrected address when the domain looks misspelled. It
	// is filled in even on a hard fail, so the UI can still offer the correction.
	TypoSuggestion string
}

// Pass2Status mirrors the provider verdicts the adapter maps every vendor onto.
type Pass2Status string

// The verdicts Pass 2 can produce.
const (
	Pass2Deliverable   Pass2Status = "deliverable"
	Pass2Risky         Pass2Status = "risky"
	Pass2Unknown       Pass2Status = "unknown"
	Pass2Undeliverable Pass2Status = "undeliverable"
	Pass2Error         Pass2Status = "error"
)

// Conclusive reports whether a verdict is worth caching and billing against. An
// unknown result or an error is retried later rather than stored for 90 days.
func (s Pass2Status) Conclusive() bool {
	switch s {
	case Pass2Deliverable, Pass2Risky, Pass2Undeliverable:
		return true
	default:
		return false
	}
}

// Pass is which of the two passes a run executes.
type Pass string

// The two run kinds.
const (
	PassSelf       Pass = "self"
	PassThirdParty Pass = "third_party"
)

// Run statuses mirror the verification_runs check constraint, which in turn mirrors
// the scrape job statuses the frontend already renders.
const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunDone      = "done"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
)

// Run item statuses mirror the verification_run_items check constraint.
const (
	ItemQueued  = "queued"
	ItemDone    = "done"
	ItemFailed  = "failed"
	ItemSkipped = "skipped"
)

// RunScope selects between "everything that matches the filter" and an explicit set.
type RunScope string

// The two scopes a run can have.
const (
	ScopeAll       RunScope = "all"
	ScopeSelection RunScope = "selection"
)

// RunFilter describes which addresses a run covers. It is stored verbatim on the run
// so the history page can explain what was submitted.
type RunFilter struct {
	Scope RunScope `json:"scope"`
	// IDs restricts a selection run to these verification rows.
	IDs []uuid.UUID `json:"ids,omitempty"`
	// BusinessIDs expands to every address of those businesses.
	BusinessIDs []uuid.UUID `json:"business_ids,omitempty"`
	// JobID expands to every address of the businesses a scrape job found.
	JobID *uuid.UUID `json:"job_id,omitempty"`
	// Tags keeps only addresses currently carrying one of these tags.
	Tags []string `json:"tags,omitempty"`
	// MinScore keeps only addresses at or above this final score.
	MinScore *int `json:"min_score,omitempty"`
	// IncludeSuppressed brings in addresses that only belong to suppressed businesses.
	IncludeSuppressed bool `json:"include_suppressed,omitempty"`
	// StaleAfterDays skips addresses verified more recently than this, for self runs.
	StaleAfterDays *int `json:"stale_after_days,omitempty"`
	// SkipScoreFloor lifts the paid band's minimum score. Only the single-address
	// action sets it: an operator who explicitly asks for one address to be checked
	// may pay for it however low the free checks scored it. It is never read from
	// the wire, so a bulk run cannot use it.
	SkipScoreFloor bool `json:"skip_score_floor,omitempty"`
	// Unscored keeps only addresses that have never been through the free stage. The
	// automatic sweep sets it, so a run covers what scrapes found since the last one
	// rather than re-scoring the whole list. Like SkipScoreFloor it is never read
	// from the wire.
	Unscored bool `json:"unscored,omitempty"`
}

// Estimate is what a run would cost before it is started.
type Estimate struct {
	// Emails is how many addresses the run would actually process.
	Emails int64
	// NeedsSelf is how many matching addresses have not completed Pass 1 yet, and so
	// are excluded from a third-party run.
	NeedsSelf int64
	// Cached is how many matching addresses have already been sent to a third party
	// once and are therefore skipped, permanently and without being billed.
	Cached int64
	// CreditsNeeded is one credit per address that would be sent to the provider.
	CreditsNeeded int64
	// CostPer1kCents is the configured price of the verifier source.
	CostPer1kCents int
	// EstCostCents is CreditsNeeded priced at CostPer1kCents.
	EstCostCents int64
	// BalanceCredits is the provider's reported balance, nil when unavailable.
	BalanceCredits *int64
}

// Stats is the payload behind GET /verification/stats.
type Stats struct {
	Total                  int64
	ByTag                  map[Tag]int64
	SelfVerified           int64
	ThirdPartyVerified     int64
	NeedsSelf              int64
	QualifyingForThird     int64
	QualifyingEstCostCents int64
	CreditsUsedTotal       int64
	CreditsUsed30d         int64
	BalanceCredits         *int64
	ActiveRuns             int64
	LastSelfRunAt          *time.Time
	LastThirdPartyRunAt    *time.Time
}

// CostCents prices a number of verifications at a per-1000 rate.
func CostCents(credits int64, costPer1kCents int) int64 {
	return credits * int64(costPer1kCents) / 1000
}
