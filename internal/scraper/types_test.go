package scraper

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/apperr"
)

func TestConfigNormalize(t *testing.T) {
	cfg := Config{
		Terms:     []string{"  gyms  ", "GYMS", "cross   fit", "", "   "},
		Locations: []Location{{City: " Austin ", State: " TX "}, {City: "austin", State: "tx"}, {State: " tx "}, {City: "Dallas"}},
	}

	got := cfg.Normalize()

	wantTerms := []string{"gyms", "cross fit"}
	if len(got.Terms) != len(wantTerms) {
		t.Fatalf("Terms = %v, want %v", got.Terms, wantTerms)
	}
	for i := range wantTerms {
		if got.Terms[i] != wantTerms[i] {
			t.Fatalf("Terms = %v, want %v", got.Terms, wantTerms)
		}
	}

	wantLocs := []Location{{City: "Austin", State: "TX"}, {State: "TX"}, {City: "Dallas"}}
	if len(got.Locations) != len(wantLocs) {
		t.Fatalf("Locations = %+v, want %+v", got.Locations, wantLocs)
	}
	for i := range wantLocs {
		if got.Locations[i] != wantLocs[i] {
			t.Errorf("Locations[%d] = %+v, want %+v", i, got.Locations[i], wantLocs[i])
		}
	}

	if got.MaxPerQuery != DefaultMaxPerQuery {
		t.Errorf("MaxPerQuery = %d, want the default %d", got.MaxPerQuery, DefaultMaxPerQuery)
	}
	if got.Concurrency != DefaultConcurrency {
		t.Errorf("Concurrency = %d, want the default %d", got.Concurrency, DefaultConcurrency)
	}
}

func TestConfigNormalizeExpandsEmptyLocations(t *testing.T) {
	for name, locs := range map[string][]Location{
		"no locations":    nil,
		"blank location":  {{}},
		"whitespace only": {{City: "  ", State: " "}},
	} {
		t.Run(name, func(t *testing.T) {
			got := Config{Terms: []string{"gyms"}, Locations: locs}.Normalize()
			if len(got.Locations) != len(USStates) {
				t.Fatalf("Locations = %d, want one per state (%d)", len(got.Locations), len(USStates))
			}
			if got.Locations[0] != (Location{State: USStates[0]}) {
				t.Errorf("Locations[0] = %+v, want a bare state", got.Locations[0])
			}
		})
	}
}

func TestConfigNormalizeAllStatesKeepsNamedCities(t *testing.T) {
	got := Config{
		Terms:     []string{"gyms"},
		Locations: []Location{{City: "Austin", State: "TX"}, {}},
	}.Normalize()

	if len(got.Locations) != len(USStates)+1 {
		t.Fatalf("Locations = %d, want Austin/TX plus every state", len(got.Locations))
	}
	if got.Locations[0] != (Location{City: "Austin", State: "TX"}) {
		t.Errorf("Locations[0] = %+v, want the explicit city first", got.Locations[0])
	}
}

func TestConfigNormalizeKeepsExplicitValues(t *testing.T) {
	cfg := Config{
		Terms:       []string{"gyms"},
		Locations:   []Location{{City: "Austin"}},
		MaxPerQuery: 25,
		Concurrency: 2,
		CrawlEmails: true,
	}
	got := cfg.Normalize()
	if got.MaxPerQuery != 25 || got.Concurrency != 2 || !got.CrawlEmails {
		t.Fatalf("explicit values were overwritten: %+v", got)
	}
}

func TestConfigQueries(t *testing.T) {
	cfg := Config{
		Terms:     []string{"gyms", "crossfit"},
		Locations: []Location{{City: "Austin", State: "TX"}, {City: "Dallas", State: "TX"}},
	}
	// One query per location, not one per term × location: the terms travel together
	// so a place matching both is inside one provider run and billed once.
	if got := cfg.QueryCount(); got != 2 {
		t.Fatalf("QueryCount = %d, want 2", got)
	}

	queries := cfg.Queries()
	if len(queries) != 2 {
		t.Fatalf("Queries returned %d rows", len(queries))
	}

	seen := map[string]bool{}
	for _, q := range queries {
		key := q.City + "|" + q.State
		if seen[key] {
			t.Fatalf("duplicate location %q", key)
		}
		seen[key] = true
		if len(q.Terms) != 2 || q.Terms[0] != "gyms" || q.Terms[1] != "crossfit" {
			t.Fatalf("query for %s carries terms %v, want every term", key, q.Terms)
		}
		if q.Label() != "gyms, crossfit" {
			t.Errorf("Label = %q", q.Label())
		}
	}
}

func TestConfigQueriesWithoutTerms(t *testing.T) {
	cfg := Config{Locations: []Location{{State: "TX"}}}
	if got := cfg.QueryCount(); got != 0 {
		t.Fatalf("QueryCount = %d, want 0 when there are no terms", got)
	}
	if got := cfg.Queries(); len(got) != 0 {
		t.Fatalf("Queries returned %d rows, want none", len(got))
	}
}

func TestUnlimitedMaxPerQuery(t *testing.T) {
	cfg := Config{
		Terms:       []string{"gyms"},
		Locations:   []Location{{State: "TX"}},
		MaxPerQuery: MaxPerQueryUnlimited,
		Concurrency: 1,
	}.Normalize()

	if !cfg.IsUnlimited() {
		t.Fatalf("MaxPerQuery = %d, want the unlimited sentinel to survive Normalize", cfg.MaxPerQuery)
	}
	if err := cfg.Validate(500); err != nil {
		t.Fatalf("an unlimited config was rejected: %v", err)
	}
	// Nothing knows the real count in advance, so the estimate uses the ceiling.
	if got := cfg.EstimatedListings(); got != EstimateUnlimitedPerQuery {
		t.Errorf("EstimatedListings = %d, want %d", got, EstimateUnlimitedPerQuery)
	}
}

func fieldErrors(t *testing.T, err error) map[string]string {
	t.Helper()
	appErr := apperr.From(err)
	if appErr == nil {
		t.Fatal("expected an error")
	}
	if appErr.Code != apperr.CodeValidationFailed {
		t.Fatalf("code = %q, want validation_failed", appErr.Code)
	}
	out := map[string]string{}
	for _, f := range appErr.Fields {
		out[f.Field] = f.Message
	}
	return out
}

func TestConfigValidate(t *testing.T) {
	valid := Config{
		Terms:       []string{"gyms"},
		Locations:   []Location{{City: "Austin", State: "TX"}},
		MaxPerQuery: 100,
		Concurrency: 8,
	}
	if err := valid.Validate(500); err != nil {
		t.Fatalf("a valid config was rejected: %v", err)
	}

	t.Run("no terms", func(t *testing.T) {
		cfg := valid
		cfg.Terms = nil
		if _, ok := fieldErrors(t, cfg.Validate(500))["config.terms"]; !ok {
			t.Fatal("expected a config.terms error")
		}
	})

	t.Run("too many terms", func(t *testing.T) {
		cfg := valid
		cfg.Terms = make([]string, MaxTerms+1)
		for i := range cfg.Terms {
			cfg.Terms[i] = "term"
		}
		if _, ok := fieldErrors(t, cfg.Validate(10_000))["config.terms"]; !ok {
			t.Fatal("expected a config.terms error")
		}
	})

	t.Run("too many locations", func(t *testing.T) {
		cfg := valid
		cfg.Locations = make([]Location, MaxLocations+1)
		for i := range cfg.Locations {
			cfg.Locations[i] = Location{City: "Austin"}
		}
		if _, ok := fieldErrors(t, cfg.Validate(10_000))["config.locations"]; !ok {
			t.Fatal("expected a config.locations error")
		}
	})

	t.Run("query budget", func(t *testing.T) {
		cfg := valid
		cfg.Terms = []string{"a", "b", "c"}
		cfg.Locations = []Location{{City: "1"}, {City: "2"}, {City: "3"}}
		message := fieldErrors(t, cfg.Validate(2))["config"]
		if !strings.Contains(message, "3 queries") {
			t.Fatalf("message = %q, want it to state the query count", message)
		}
	})

	t.Run("bounds", func(t *testing.T) {
		cfg := valid
		cfg.MaxPerQuery = -5
		cfg.Concurrency = 999
		fields := fieldErrors(t, cfg.Validate(500))
		if _, ok := fields["config.max_per_query"]; !ok {
			t.Error("expected a max_per_query error")
		}
		if _, ok := fields["config.concurrency"]; !ok {
			t.Error("expected a concurrency error")
		}
	})

	t.Run("term too long", func(t *testing.T) {
		cfg := valid
		cfg.Terms = []string{strings.Repeat("x", MaxTermLen+1)}
		if _, ok := fieldErrors(t, cfg.Validate(500))["config.terms[0]"]; !ok {
			t.Fatal("expected an indexed term error")
		}
	})
}

func TestCostArithmetic(t *testing.T) {
	// 400 listings at 450 cents per 1000.
	if got := EstimateCostCents(400, 450); got != 180 {
		t.Errorf("EstimateCostCents = %d, want 180", got)
	}
	if got := ActualCostCents(37, 450); got != 16 {
		t.Errorf("ActualCostCents = %d, want 16 (integer cents)", got)
	}
	if got := ActualCostCents(0, 450); got != 0 {
		t.Errorf("ActualCostCents(0) = %d, want 0", got)
	}
	if got := EstimateCostCents(1000, 0); got != 0 {
		t.Errorf("a free source should estimate 0, got %d", got)
	}

	// Two terms in one location still bill per place returned, so the ceiling
	// counts each term's cap.
	cfg := Config{
		Terms:       []string{"gyms", "crossfit"},
		Locations:   []Location{{State: "TX"}, {State: "CA"}},
		MaxPerQuery: 100,
	}
	if got := cfg.EstimatedListings(); got != 400 {
		t.Errorf("EstimatedListings = %d, want 400", got)
	}
}

func TestDecodeConfigAndStats(t *testing.T) {
	cfg, err := DecodeConfig([]byte(`{"terms":["gyms"],"locations":[{"city":"Austin","state":"TX"}],"max_per_query":25,"crawl_emails":false,"concurrency":4}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Terms) != 1 || cfg.MaxPerQuery != 25 || cfg.CrawlEmails {
		t.Fatalf("decoded config = %+v", cfg)
	}

	stats, err := DecodeStats([]byte(`{"queries_total":4,"emails_found":9,"cost_cents":1234}`))
	if err != nil {
		t.Fatal(err)
	}
	if stats.QueriesTotal != 4 || stats.EmailsFound != 9 || stats.CostCents != 1234 {
		t.Fatalf("decoded stats = %+v", stats)
	}

	// Empty documents are valid: a brand-new job has `{}` stats.
	if _, err := DecodeStats(nil); err != nil {
		t.Fatalf("nil stats should decode: %v", err)
	}
	if _, err := DecodeConfig([]byte(`not json`)); err == nil {
		t.Fatal("invalid JSON should fail")
	}
}

func TestStatsJSONKeysMatchTheBumpKeys(t *testing.T) {
	raw, err := json.Marshal(Stats{})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	// The atomic counter updates address these keys by name in SQL, so a rename in
	// the struct tag without a matching constant change would silently break stats.
	for _, key := range []string{
		StatKeyQueriesDone, StatKeyQueriesFailed, StatKeyListingsFound,
		StatKeySitesCrawled, StatKeyEmailsFound, StatKeyCostCents,
	} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("stats JSON has no %q key", key)
		}
	}
}

func TestRerunName(t *testing.T) {
	if got := rerunName("Gyms TX"); got != "Gyms TX (re-run)" {
		t.Errorf("rerunName = %q", got)
	}
	if got := rerunName("Gyms TX (re-run)"); got != "Gyms TX (re-run)" {
		t.Errorf("a re-run of a re-run should not stack suffixes: %q", got)
	}
	long := strings.Repeat("x", MaxNameLen)
	if got := rerunName(long); len(got) > MaxNameLen {
		t.Errorf("rerunName produced %d characters, the column allows %d", len(got), MaxNameLen)
	}
}

func TestSuffixedNameSwapsDerivedMarkers(t *testing.T) {
	cases := map[string]string{
		"Gyms":                          "Gyms (re-crawl)",
		"Gyms (re-crawl)":               "Gyms (re-crawl)",
		"Gyms (re-run)":                 "Gyms (re-crawl)",
		strings.Repeat("x", MaxNameLen): strings.Repeat("x", MaxNameLen-len(recrawlSuffix)) + recrawlSuffix,
	}
	for in, want := range cases {
		if got := suffixedName(in, recrawlSuffix); got != want {
			t.Errorf("suffixedName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := suffixedName("Gyms (re-crawl)", rerunSuffix); got != "Gyms (re-run)" {
		t.Errorf("re-running a re-crawl = %q, want %q", got, "Gyms (re-run)")
	}
}

func TestConfigRoundTripsRecrawlOf(t *testing.T) {
	origin := uuid.New()
	cfg := Config{Terms: []string{"gyms"}, Locations: []Location{{City: "Austin", State: "TX"}}, RecrawlOf: &origin}
	raw, err := json.Marshal(cfg.Normalize())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.IsRecrawl() || *decoded.RecrawlOf != origin {
		t.Fatalf("RecrawlOf = %v, want %s", decoded.RecrawlOf, origin)
	}
	plain, _ := json.Marshal(Config{Terms: []string{"gyms"}}.Normalize())
	if strings.Contains(string(plain), "recrawl_of") {
		t.Fatalf("a plain config must omit recrawl_of: %s", plain)
	}
}

func TestNormalizeSocialNetworks(t *testing.T) {
	enabled := map[string]bool{SocialNetworkFacebook: true}

	got, err := NormalizeSocialNetworks([]string{" Facebook ", "facebook"}, enabled)
	if err != nil || len(got) != 1 || got[0] != SocialNetworkFacebook {
		t.Fatalf("got %v, %v; want one facebook", got, err)
	}
	if fields := fieldErrors(t, mustErr(NormalizeSocialNetworks(nil, enabled))); fields["networks"] == "" {
		t.Errorf("empty networks: fields = %v", fields)
	}
	if fields := fieldErrors(t, mustErr(NormalizeSocialNetworks([]string{"myspace"}, enabled))); fields["networks[0]"] == "" {
		t.Errorf("unknown network: fields = %v", fields)
	}

	// A network the server knows but whose scraper is not running is a conflict,
	// not a validation error: the request is fine, the server is not set up for it.
	_, err = NormalizeSocialNetworks([]string{"facebook"}, nil)
	if appErr := apperr.From(err); appErr == nil || appErr.Code != apperr.CodeConflict {
		t.Fatalf("disabled scraper: err = %v, want a conflict", err)
	}
}

func mustErr(_ []string, err error) error { return err }

func TestSocialScrapeConfigSurvivesNormalize(t *testing.T) {
	cfg := Config{SocialNetworks: []string{SocialNetworkFacebook}, SocialMissingEmailOnly: true}.Normalize()
	if !cfg.IsSocialScrape() || !cfg.SocialMissingEmailOnly {
		t.Fatalf("normalized config lost the social scrape: %+v", cfg)
	}
	if cfg.IsRecrawl() {
		t.Error("a social media scrape must not read as a re-crawl")
	}
}
