package verify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeLookup answers DNS from a map, so no unit test touches a resolver.
type fakeLookup struct {
	facts map[string]DomainFacts
	errs  map[string]error
	calls map[string]int
}

func (f *fakeLookup) Lookup(_ context.Context, domain string) (DomainFacts, error) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[domain]++
	if err, ok := f.errs[domain]; ok {
		return DomainFacts{}, err
	}
	facts, ok := f.facts[domain]
	if !ok {
		// An unknown domain resolves to nothing, which is a hard fail.
		return DomainFacts{}, nil
	}
	return facts, nil
}

// fakeAge answers RDAP from a map.
type fakeAge struct {
	dates map[string]time.Time
	errs  map[string]error
}

func (f *fakeAge) RegisteredAt(_ context.Context, domain string) (*time.Time, error) {
	if err, ok := f.errs[domain]; ok {
		return nil, err
	}
	if date, ok := f.dates[domain]; ok {
		return &date, nil
	}
	return nil, nil
}

const goodDomain = "ironworksgym.com"

var fixedNow = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

// fullyHealthy is a domain that earns every DNS and age point.
func fullyHealthy() (*fakeLookup, *fakeAge) {
	return &fakeLookup{
			facts: map[string]DomainFacts{
				goodDomain: {
					HasMX: true, HasA: true, HasSPF: true, HasDMARC: true,
					MXHosts: []string{"aspmx.l.google.com"},
				},
			},
		}, &fakeAge{
			dates: map[string]time.Time{goodDomain: fixedNow.AddDate(-5, 0, 0)},
		}
}

func newTestPipeline(t *testing.T, dns DomainLookup, age AgeLookup, mode RoleMode) *Pipeline {
	t.Helper()
	lists, err := LoadLists("")
	if err != nil {
		t.Fatalf("could not load the embedded lists: %v", err)
	}
	return NewPipeline(PipelineConfig{
		Lists:        lists,
		DNS:          dns,
		Age:          age,
		RoleSoftMode: mode,
		Now:          func() time.Time { return fixedNow },
	})
}

// checkByKey finds one line of the breakdown.
func checkByKey(t *testing.T, result Pass1Result, key string) CheckResult {
	t.Helper()
	for _, check := range result.Checks {
		if check.Key == key {
			return check
		}
	}
	t.Fatalf("check %q is missing from the breakdown", key)
	return CheckResult{}
}

// The breakdown must always add up to the stored score, because the UI renders the
// points column and a mismatch would be unexplainable.
func assertBreakdownSumsToScore(t *testing.T, result Pass1Result) {
	t.Helper()
	total := 0
	for _, check := range result.Checks {
		total += check.Points
	}
	if total != result.Score {
		t.Fatalf("breakdown sums to %d but the score is %d", total, result.Score)
	}
	if len(result.Checks) != len(checkCatalog) {
		t.Fatalf("breakdown has %d checks, want all %d", len(result.Checks), len(checkCatalog))
	}
}

func TestPass1AwardsTheFullLocalScore(t *testing.T) {
	dns, age := fullyHealthy()
	pipeline := newTestPipeline(t, dns, age, RoleModeHard)

	result := pipeline.Run(context.Background(), "  Jane.Doe@IronWorksGym.com  ")

	if result.Email != "jane.doe@ironworksgym.com" {
		t.Errorf("email = %q, want the normalized address", result.Email)
	}
	if result.Domain != goodDomain {
		t.Errorf("domain = %q, want %q", result.Domain, goodDomain)
	}
	if result.Score != Pass1MaxScore {
		t.Errorf("score = %d, want %d\nbreakdown: %+v", result.Score, Pass1MaxScore, result.Checks)
	}
	if result.HardFail != "" {
		t.Errorf("hard fail = %q, want none", result.HardFail)
	}
	if result.TypoSuggestion != "" {
		t.Errorf("typo suggestion = %q, want none", result.TypoSuggestion)
	}
	if _, tag := Finalize(result.Score, nil); tag != TagLightGreen {
		t.Errorf("tag = %q, want %q: a local run must never reach green", tag, TagLightGreen)
	}
	assertBreakdownSumsToScore(t, result)
}

func TestPass1HardFails(t *testing.T) {
	tests := []struct {
		name     string
		email    string
		wantKey  string
		wantCall bool // whether DNS should have been consulted at all
	}{
		{name: "not an address", email: "not-an-email", wantKey: CheckSyntax},
		{name: "display name is not an address", email: "gym <a@b.com>", wantKey: CheckSyntax},
		{name: "two at signs", email: "a@b@c.com", wantKey: CheckSyntax},
		{name: "automated mailbox", email: "noreply@" + goodDomain, wantKey: CheckRoleAccount},
		{name: "automated mailbox with a tag", email: "no-reply+news@" + goodDomain, wantKey: CheckRoleAccount},
		{name: "abuse mailbox", email: "postmaster@" + goodDomain, wantKey: CheckRoleAccount},
		{name: "shared mailbox in hard mode", email: "info@" + goodDomain, wantKey: CheckRoleAccount},
		{name: "disposable domain", email: "jane@mailinator.com", wantKey: CheckDisposable},
		{name: "disposable subdomain", email: "jane@inbox.mailinator.com", wantKey: CheckDisposable},
		{name: "domain does not resolve", email: "jane@nowhere.example", wantKey: CheckDNSResolves, wantCall: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dns, age := fullyHealthy()
			pipeline := newTestPipeline(t, dns, age, RoleModeHard)

			result := pipeline.Run(context.Background(), tt.email)

			if result.Score != 0 {
				t.Errorf("score = %d, want 0 for a hard fail", result.Score)
			}
			if result.HardFail != tt.wantKey {
				t.Errorf("hard fail = %q, want %q", result.HardFail, tt.wantKey)
			}
			if _, tag := Finalize(result.Score, nil); tag != TagRed {
				t.Errorf("tag = %q, want %q", tag, TagRed)
			}
			failed := checkByKey(t, result, tt.wantKey)
			if failed.Status != CheckFail {
				t.Errorf("%s status = %q, want %q", tt.wantKey, failed.Status, CheckFail)
			}
			if failed.Detail == "" {
				t.Errorf("%s has no detail explaining the failure", tt.wantKey)
			}
			assertBreakdownSumsToScore(t, result)

			if !tt.wantCall && len(dns.calls) > 0 {
				t.Errorf("DNS was queried for an address killed by a local check: %v", dns.calls)
			}
		})
	}
}

// A hard fail must leave every later check marked "skip" rather than silently
// missing, so the UI always renders the same thirteen rows.
func TestPass1HardFailSkipsTheRest(t *testing.T) {
	dns, age := fullyHealthy()
	pipeline := newTestPipeline(t, dns, age, RoleModeHard)

	result := pipeline.Run(context.Background(), "noreply@"+goodDomain)

	for _, key := range []string{CheckStructure, CheckTypo, CheckHumanLocal, CheckFreeProvider,
		CheckDNSResolves, CheckMX, CheckARecord, CheckSPF, CheckDMARC, CheckDomainAge} {
		check := checkByKey(t, result, key)
		if check.Status != CheckSkip {
			t.Errorf("%s status = %q, want %q after a hard fail", key, check.Status, CheckSkip)
		}
		if check.Points != 0 {
			t.Errorf("%s awarded %d points after a hard fail", key, check.Points)
		}
		if !strings.Contains(check.Detail, "not evaluated") {
			t.Errorf("%s detail = %q, want it to say the check never ran", key, check.Detail)
		}
	}
}

func TestPass1SoftSignals(t *testing.T) {
	tests := []struct {
		name      string
		email     string
		facts     DomainFacts
		wantCheck string
		wantScore int
	}{
		{
			name:      "no MX records",
			email:     "jane@" + goodDomain,
			facts:     DomainFacts{HasA: true, HasSPF: true, HasDMARC: true},
			wantCheck: CheckMX,
			wantScore: Pass1MaxScore - PointsMX,
		},
		{
			name:      "no address records",
			email:     "jane@" + goodDomain,
			facts:     DomainFacts{HasMX: true, HasSPF: true, HasDMARC: true, MXHosts: []string{"mx.example.com"}},
			wantCheck: CheckARecord,
			wantScore: Pass1MaxScore - PointsARecord,
		},
		{
			name:      "no SPF",
			email:     "jane@" + goodDomain,
			facts:     DomainFacts{HasMX: true, HasA: true, HasDMARC: true, MXHosts: []string{"mx.example.com"}},
			wantCheck: CheckSPF,
			wantScore: Pass1MaxScore - PointsSPF,
		},
		{
			name:      "no DMARC",
			email:     "jane@" + goodDomain,
			facts:     DomainFacts{HasMX: true, HasA: true, HasSPF: true, MXHosts: []string{"mx.example.com"}},
			wantCheck: CheckDMARC,
			wantScore: Pass1MaxScore - PointsDMARC,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dns := &fakeLookup{facts: map[string]DomainFacts{goodDomain: tt.facts}}
			age := &fakeAge{dates: map[string]time.Time{goodDomain: fixedNow.AddDate(-5, 0, 0)}}
			pipeline := newTestPipeline(t, dns, age, RoleModeHard)

			result := pipeline.Run(context.Background(), tt.email)

			if result.Score != tt.wantScore {
				t.Errorf("score = %d, want %d\nbreakdown: %+v", result.Score, tt.wantScore, result.Checks)
			}
			if check := checkByKey(t, result, tt.wantCheck); check.Status != CheckFail {
				t.Errorf("%s status = %q, want %q", tt.wantCheck, check.Status, CheckFail)
			}
			assertBreakdownSumsToScore(t, result)
		})
	}
}

func TestPass1FreeProviderLosesItsBonus(t *testing.T) {
	const gmail = "gmail.com"
	dns := &fakeLookup{facts: map[string]DomainFacts{
		gmail: {HasMX: true, HasA: true, HasSPF: true, HasDMARC: true, MXHosts: []string{"gmail-smtp-in.l.google.com"}},
	}}
	age := &fakeAge{dates: map[string]time.Time{gmail: fixedNow.AddDate(-20, 0, 0)}}
	pipeline := newTestPipeline(t, dns, age, RoleModeHard)

	result := pipeline.Run(context.Background(), "jane.doe@gmail.com")

	if check := checkByKey(t, result, CheckFreeProvider); check.Status != CheckFail {
		t.Errorf("free provider check = %q, want %q", check.Status, CheckFail)
	}
	if result.Score != Pass1MaxScore-PointsNotFreeProvider {
		t.Errorf("score = %d, want %d", result.Score, Pass1MaxScore-PointsNotFreeProvider)
	}
	if result.TypoSuggestion != "" {
		t.Errorf("a real provider was flagged as a typo of %q", result.TypoSuggestion)
	}
	assertBreakdownSumsToScore(t, result)
}

func TestPass1TypoCapsTheScoreAndSuggestsACorrection(t *testing.T) {
	const typo = "gmial.com"
	dns := &fakeLookup{facts: map[string]DomainFacts{
		typo: {HasMX: true, HasA: true, HasSPF: true, HasDMARC: true, MXHosts: []string{"mx.example.com"}},
	}}
	age := &fakeAge{dates: map[string]time.Time{typo: fixedNow.AddDate(-5, 0, 0)}}
	pipeline := newTestPipeline(t, dns, age, RoleModeHard)

	result := pipeline.Run(context.Background(), "jane.doe@gmial.com")

	if result.Score != TypoScoreCap {
		t.Errorf("score = %d, want the typo cap %d", result.Score, TypoScoreCap)
	}
	if result.TypoSuggestion != "jane.doe@gmail.com" {
		t.Errorf("suggestion = %q, want %q", result.TypoSuggestion, "jane.doe@gmail.com")
	}
	if _, tag := Finalize(result.Score, nil); tag != TagOrange {
		t.Errorf("tag = %q, want %q", tag, TagOrange)
	}
	if result.Score >= MinScoreYellow {
		t.Errorf("a typo'd address scored %d and would be sent to the paid pass", result.Score)
	}
	if check := checkByKey(t, result, CheckTypo); !strings.Contains(check.Detail, "gmail.com") {
		t.Errorf("typo detail = %q, want the suggestion in it", check.Detail)
	}
}

// The correction has to survive a hard fail, otherwise a misspelled domain that does
// not resolve is a dead end for the operator.
func TestPass1KeepsTheTypoSuggestionThroughAHardFail(t *testing.T) {
	dns := &fakeLookup{} // gmial.com resolves to nothing
	pipeline := newTestPipeline(t, dns, &fakeAge{}, RoleModeHard)

	result := pipeline.Run(context.Background(), "jane@gmial.com")

	if result.HardFail != CheckDNSResolves {
		t.Fatalf("hard fail = %q, want %q", result.HardFail, CheckDNSResolves)
	}
	if result.Score != 0 {
		t.Errorf("score = %d, want 0", result.Score)
	}
	if result.TypoSuggestion != "jane@gmail.com" {
		t.Errorf("suggestion = %q, want it kept despite the hard fail", result.TypoSuggestion)
	}
}

func TestPass1RoleSoftModes(t *testing.T) {
	dns, age := fullyHealthy()

	t.Run("hard mode zeroes a shared mailbox", func(t *testing.T) {
		pipeline := newTestPipeline(t, dns, age, RoleModeHard)
		result := pipeline.Run(context.Background(), "info@"+goodDomain)
		if result.Score != 0 || result.HardFail != CheckRoleAccount {
			t.Fatalf("score = %d, hard fail = %q, want 0 and %q",
				result.Score, result.HardFail, CheckRoleAccount)
		}
	})

	t.Run("penalty mode caps it instead", func(t *testing.T) {
		pipeline := newTestPipeline(t, dns, age, RoleModePenalty)
		result := pipeline.Run(context.Background(), "info@"+goodDomain)
		if result.HardFail != "" {
			t.Fatalf("hard fail = %q, want none in penalty mode", result.HardFail)
		}
		if result.Score != RoleSoftScoreCap {
			t.Fatalf("score = %d, want the role cap %d", result.Score, RoleSoftScoreCap)
		}
		if result.Score < MinScoreYellow {
			t.Fatalf("penalty mode scored %d, below the paid gate %d: the mode would be pointless",
				result.Score, MinScoreYellow)
		}
		if check := checkByKey(t, result, CheckRoleAccount); check.Status != CheckFail {
			t.Fatalf("role check = %q, want %q", check.Status, CheckFail)
		}
	})

	t.Run("an automated mailbox is hard in both modes", func(t *testing.T) {
		pipeline := newTestPipeline(t, dns, age, RoleModePenalty)
		result := pipeline.Run(context.Background(), "postmaster@"+goodDomain)
		if result.HardFail != CheckRoleAccount {
			t.Fatalf("hard fail = %q, want %q even in penalty mode",
				result.HardFail, CheckRoleAccount)
		}
	})
}

func TestPass1LocalPartHeuristic(t *testing.T) {
	tests := []struct {
		local     string
		wantHuman bool
	}{
		{"jane.doe", true},
		{"jsmith", true},
		{"bookings2024", true},
		{"j", true},
		{"first.last.jr", true},
		{"jane-doe", true},
		{"a7f3c9d21b04e8", false},
		{"qwertyuiop", false},
		{"asdfgh", false},
		{"123456789", false},
		{"xkcdvbnmzpqrst", false},
	}

	dns, age := fullyHealthy()
	pipeline := newTestPipeline(t, dns, age, RoleModeHard)

	for _, tt := range tests {
		t.Run(tt.local, func(t *testing.T) {
			result := pipeline.Run(context.Background(), tt.local+"@"+goodDomain)
			check := checkByKey(t, result, CheckHumanLocal)
			gotHuman := check.Status == CheckPass
			if gotHuman != tt.wantHuman {
				t.Fatalf("%q human = %v (%s), want %v", tt.local, gotHuman, check.Detail, tt.wantHuman)
			}
			if gotHuman && check.Points != PointsHumanLocal {
				t.Fatalf("a human local part earned %d points, want %d", check.Points, PointsHumanLocal)
			}
		})
	}
}

func TestPass1Structure(t *testing.T) {
	tests := []struct {
		name  string
		email string
	}{
		{"leading dot", ".jane@" + goodDomain},
		{"trailing dot", "jane.@" + goodDomain},
		{"consecutive dots", "ja..ne@" + goodDomain},
		{"quoted local part", `"jane doe"@` + goodDomain},
	}

	dns, age := fullyHealthy()
	pipeline := newTestPipeline(t, dns, age, RoleModeHard)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := pipeline.Run(context.Background(), tt.email)
			// Either the RFC parser rejects it outright or the structure check does;
			// what matters is that it never scores as a clean address.
			if result.Score >= Pass1MaxScore {
				t.Fatalf("%q scored %d, want less than a clean address", tt.email, result.Score)
			}
			if result.HardFail == "" {
				if check := checkByKey(t, result, CheckStructure); check.Status != CheckFail {
					t.Fatalf("structure check = %q, want %q", check.Status, CheckFail)
				}
			}
		})
	}
}

func TestPass1DomainAge(t *testing.T) {
	dns, _ := fullyHealthy()

	t.Run("a young domain fails the check", func(t *testing.T) {
		age := &fakeAge{dates: map[string]time.Time{goodDomain: fixedNow.AddDate(0, -2, 0)}}
		pipeline := newTestPipeline(t, dns, age, RoleModeHard)
		result := pipeline.Run(context.Background(), "jane@"+goodDomain)
		if check := checkByKey(t, result, CheckDomainAge); check.Status != CheckFail {
			t.Fatalf("age check = %q, want %q", check.Status, CheckFail)
		}
		if result.Score != Pass1MaxScore-PointsDomainAge {
			t.Fatalf("score = %d, want %d", result.Score, Pass1MaxScore-PointsDomainAge)
		}
	})

	t.Run("an unavailable registry skips the check", func(t *testing.T) {
		age := &fakeAge{errs: map[string]error{goodDomain: errors.New("rdap is down")}}
		pipeline := newTestPipeline(t, dns, age, RoleModeHard)
		result := pipeline.Run(context.Background(), "jane@"+goodDomain)
		if check := checkByKey(t, result, CheckDomainAge); check.Status != CheckSkip {
			t.Fatalf("age check = %q, want %q when the registry is unreachable", check.Status, CheckSkip)
		}
		if result.Score != Pass1MaxScore-PointsDomainAge {
			t.Fatalf("score = %d, want %d", result.Score, Pass1MaxScore-PointsDomainAge)
		}
		assertBreakdownSumsToScore(t, result)
	})

	t.Run("disabling RDAP lowers the maximum", func(t *testing.T) {
		pipeline := newTestPipeline(t, dns, DisabledAgeLookup(), RoleModeHard)
		if got := pipeline.MaxScore(); got != Pass1MaxScore-PointsDomainAge {
			t.Fatalf("MaxScore() = %d, want %d with RDAP off", got, Pass1MaxScore-PointsDomainAge)
		}
		result := pipeline.Run(context.Background(), "jane@"+goodDomain)
		if result.Score != pipeline.MaxScore() {
			t.Fatalf("score = %d, want %d", result.Score, pipeline.MaxScore())
		}
		if _, tag := Finalize(result.Score, nil); tag != TagLightGreen {
			t.Fatalf("tag = %q, want %q with RDAP off", tag, TagLightGreen)
		}
	})
}

// A resolver outage must not look like a non-existent domain: the checks are skipped
// and the score stays too low to be billed.
func TestPass1ResolverOutageSkipsRatherThanFails(t *testing.T) {
	dns := &fakeLookup{errs: map[string]error{goodDomain: errors.New("i/o timeout")}}
	age := &fakeAge{dates: map[string]time.Time{goodDomain: fixedNow.AddDate(-5, 0, 0)}}
	pipeline := newTestPipeline(t, dns, age, RoleModeHard)

	result := pipeline.Run(context.Background(), "jane.doe@"+goodDomain)

	if result.HardFail != "" {
		t.Fatalf("hard fail = %q, want none: an outage does not disprove the domain", result.HardFail)
	}
	for _, key := range []string{CheckDNSResolves, CheckMX, CheckARecord, CheckSPF, CheckDMARC} {
		if check := checkByKey(t, result, key); check.Status != CheckSkip {
			t.Errorf("%s = %q, want %q during an outage", key, check.Status, CheckSkip)
		}
	}
	if result.Score >= MinScoreYellow {
		t.Errorf("score = %d during a DNS outage, which would send it to the paid pass", result.Score)
	}
	assertBreakdownSumsToScore(t, result)
}
