package verify

import (
	"context"
	"fmt"
	"time"
)

// RoleMode decides what a shared business mailbox costs.
type RoleMode string

// The two role-account policies.
const (
	// RoleModeHard follows the brief: info@, sales@ and friends score 0 and never
	// reach the paid pass.
	RoleModeHard RoleMode = "hard"
	// RoleModePenalty caps them at RoleSoftScoreCap instead. For small businesses a
	// shared mailbox is often the only published address, and the scraper's own
	// ranking prefers info@ over everything else, so this keeps those addresses
	// usable.
	RoleModePenalty RoleMode = "penalty"
)

// checkCatalog is every Pass 1 check in execution order, with the label the UI
// shows. The breakdown always lists all of them, so a score is always explainable.
var checkCatalog = []struct {
	Key   string
	Label string
}{
	{CheckSyntax, "Valid syntax (RFC 5322)"},
	{CheckRoleAccount, "Not a role account"},
	{CheckDisposable, "Not a disposable domain"},
	{CheckStructure, "Length and structure"},
	{CheckTypo, "No typo in the domain"},
	{CheckHumanLocal, "Local part looks human"},
	{CheckFreeProvider, "Not a free mail provider"},
	{CheckDNSResolves, "Domain resolves"},
	{CheckMX, "MX records present"},
	{CheckARecord, "A/AAAA records present"},
	{CheckSPF, "SPF record present"},
	{CheckDMARC, "DMARC record present"},
	{CheckDomainAge, "Domain older than one year"},
}

// PipelineConfig wires the pipeline's dependencies. Every one of them is an
// interface, so the scorer runs in unit tests with no network and no database.
type PipelineConfig struct {
	Lists        *Lists
	DNS          DomainLookup
	Age          AgeLookup
	RoleSoftMode RoleMode
	Now          func() time.Time
}

// Pipeline runs the local checks in cheapest-first order and short-circuits the
// moment a hard check fails.
type Pipeline struct {
	lists    *Lists
	dns      DomainLookup
	age      AgeLookup
	roleMode RoleMode
	now      func() time.Time
}

// NewPipeline builds the Pass 1 scorer.
func NewPipeline(cfg PipelineConfig) *Pipeline {
	if cfg.RoleSoftMode == "" {
		cfg.RoleSoftMode = RoleModeHard
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Age == nil {
		cfg.Age = DisabledAgeLookup()
	}
	return &Pipeline{
		lists:    cfg.Lists,
		dns:      cfg.DNS,
		age:      cfg.Age,
		roleMode: cfg.RoleSoftMode,
		now:      cfg.Now,
	}
}

// MaxScore is the highest score this pipeline can award. It drops below
// Pass1MaxScore when the domain-age lookup is switched off.
func (p *Pipeline) MaxScore() int {
	if _, disabled := p.age.(disabledAgeLookup); disabled {
		return Pass1MaxScore - PointsDomainAge
	}
	return Pass1MaxScore
}

// Run scores one address. It never returns an error: an address that cannot be
// checked is a result with skipped checks, not a failed operation.
func (p *Pipeline) Run(ctx context.Context, raw string) Pass1Result {
	email := NormalizeAddress(raw)
	result := Pass1Result{Email: email}
	b := newBreakdown()

	local, domain, split := SplitAddress(email)
	domain = normalizeDomain(domain)
	result.Domain = domain

	if !split || !validSyntax(email) {
		b.hard(CheckSyntax, "the address is not a valid RFC 5322 address")
		return b.finish(result)
	}
	b.pass(CheckSyntax, "")

	// The suggestion is computed before anything can short-circuit, so a misspelled
	// domain that does not resolve still tells the operator what to correct it to.
	if suggested := p.lists.TypoSuggestion(domain); suggested != "" {
		result.TypoSuggestion = SuggestAddress(local, suggested)
	}

	mailbox := baseLocal(local)
	switch {
	case p.lists.IsRoleHard(mailbox):
		b.hard(CheckRoleAccount, fmt.Sprintf("%q is an automated or abuse mailbox", mailbox))
		return b.finish(result)
	case p.lists.IsRoleSoft(mailbox) && p.roleMode == RoleModeHard:
		b.hard(CheckRoleAccount, fmt.Sprintf("%q is a shared role mailbox", mailbox))
		return b.finish(result)
	case p.lists.IsRoleSoft(mailbox):
		b.failCapped(CheckRoleAccount, RoleSoftScoreCap,
			fmt.Sprintf("%q is a shared role mailbox, so the score is capped at %d",
				mailbox, RoleSoftScoreCap))
	default:
		b.pass(CheckRoleAccount, "")
	}

	if p.lists.IsDisposable(domain) {
		b.hard(CheckDisposable, fmt.Sprintf("%s is a throwaway mail provider", domain))
		return b.finish(result)
	}
	b.pass(CheckDisposable, "")

	if ok, why := checkStructure(local, domain); ok {
		b.pass(CheckStructure, "")
	} else {
		b.fail(CheckStructure, why)
	}

	if result.TypoSuggestion != "" {
		b.failCapped(CheckTypo, TypoScoreCap, "did you mean "+result.TypoSuggestion+"?")
	} else {
		b.pass(CheckTypo, "")
	}

	if ok, why := checkHumanLocal(local); ok {
		b.pass(CheckHumanLocal, "")
	} else {
		b.fail(CheckHumanLocal, why)
	}

	if p.lists.IsFreeProvider(domain) {
		b.fail(CheckFreeProvider, domain+" is a consumer mail provider, so its records say nothing about this mailbox")
	} else {
		b.pass(CheckFreeProvider, "")
	}

	facts, err := p.dns.Lookup(ctx, domain)
	switch {
	case err != nil:
		// The domain's existence is unknown rather than disproven, so nothing is
		// awarded and nothing is failed.
		reason := "the domain could not be resolved: " + err.Error()
		b.skip(CheckDNSResolves, reason)
		b.skip(CheckMX, reason)
		b.skip(CheckARecord, reason)
		b.skip(CheckSPF, reason)
		b.skip(CheckDMARC, reason)
	case !facts.Resolves():
		b.hard(CheckDNSResolves, "the domain has neither MX nor address records")
		return b.finish(result)
	default:
		b.pass(CheckDNSResolves, "")
		if facts.HasMX {
			b.pass(CheckMX, mxDetail(facts.MXHosts))
		} else {
			b.fail(CheckMX, "the domain has no MX records")
		}
		if facts.HasA {
			b.pass(CheckARecord, "")
		} else {
			b.fail(CheckARecord, "the domain has no A or AAAA records")
		}
		if facts.HasSPF {
			b.pass(CheckSPF, "")
		} else {
			b.fail(CheckSPF, "no SPF record was published")
		}
		if facts.HasDMARC {
			b.pass(CheckDMARC, "")
		} else {
			b.fail(CheckDMARC, "no DMARC record was published")
		}
	}

	p.scoreDomainAge(ctx, domain, b)
	return b.finish(result)
}

// scoreDomainAge is separated because it is the only check that can be switched off
// entirely, and because every failure mode degrades to a skip.
func (p *Pipeline) scoreDomainAge(ctx context.Context, domain string, b *breakdown) {
	registeredAt, err := p.age.RegisteredAt(ctx, domain)
	switch {
	case err != nil:
		b.skip(CheckDomainAge, "the registration date is unavailable")
	case registeredAt == nil:
		b.skip(CheckDomainAge, "the registry did not publish a registration date")
	case p.now().Sub(*registeredAt) >= DomainAgeThreshold:
		b.pass(CheckDomainAge, "registered "+registeredAt.Format(time.DateOnly))
	default:
		b.fail(CheckDomainAge, "registered "+registeredAt.Format(time.DateOnly)+", less than a year ago")
	}
}

func mxDetail(hosts []string) string {
	switch len(hosts) {
	case 0:
		return ""
	case 1:
		return hosts[0]
	default:
		return fmt.Sprintf("%s and %d more", hosts[0], len(hosts)-1)
	}
}

// breakdown accumulates check results and the caps they impose, then renders the
// full ordered list exactly once.
type breakdown struct {
	recorded map[string]CheckResult
	order    []string
	caps     []int
	hardFail string
}

func newBreakdown() *breakdown {
	return &breakdown{recorded: make(map[string]CheckResult, len(checkCatalog))}
}

func (b *breakdown) record(key string, status CheckStatus, points int, detail string) {
	if _, seen := b.recorded[key]; seen {
		return
	}
	b.order = append(b.order, key)
	b.recorded[key] = CheckResult{
		Key:    key,
		Label:  labelFor(key),
		Status: status,
		Points: points,
		Max:    pass1Weights[key],
		Detail: detail,
	}
}

func (b *breakdown) pass(key, detail string) { b.record(key, CheckPass, pass1Weights[key], detail) }
func (b *breakdown) fail(key, detail string) { b.record(key, CheckFail, 0, detail) }
func (b *breakdown) skip(key, detail string) { b.record(key, CheckSkip, 0, detail) }

// hard marks the check that zeroes the whole score.
func (b *breakdown) hard(key, detail string) {
	b.record(key, CheckFail, 0, detail)
	if b.hardFail == "" {
		b.hardFail = key
	}
}

// failCapped is a soft failure that also puts a ceiling on the total.
func (b *breakdown) failCapped(key string, ceiling int, detail string) {
	b.record(key, CheckFail, 0, detail)
	b.caps = append(b.caps, ceiling)
}

// finish renders every catalogued check in order, applies the caps and returns the
// completed result.
func (b *breakdown) finish(result Pass1Result) Pass1Result {
	hardFailed := b.hardFail != ""

	checks := make([]CheckResult, 0, len(checkCatalog))
	score := 0
	for _, entry := range checkCatalog {
		check, ok := b.recorded[entry.Key]
		if !ok {
			check = CheckResult{
				Key:    entry.Key,
				Label:  entry.Label,
				Status: CheckSkip,
				Max:    pass1Weights[entry.Key],
			}
			if hardFailed {
				check.Detail = "not evaluated: " + labelFor(b.hardFail) + " failed"
			}
		}
		// A hard fail zeroes the score, so no check may still show points against
		// it; the breakdown must always add up to the stored score.
		if hardFailed {
			check.Points = 0
		}
		score += check.Points
		checks = append(checks, check)
	}

	for _, ceiling := range b.caps {
		if !hardFailed && score > ceiling {
			score = ceiling
		}
	}
	if score > Pass1MaxScore {
		score = Pass1MaxScore
	}
	if score < 0 {
		score = 0
	}

	result.Checks = checks
	result.Score = score
	result.HardFail = b.hardFail
	return result
}

func labelFor(key string) string {
	for _, entry := range checkCatalog {
		if entry.Key == key {
			return entry.Label
		}
	}
	return key
}
