package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

type brandCandidatePayload struct {
	GroupBy           string   `json:"group_by"`
	Key               string   `json:"key"`
	DisplayName       string   `json:"display_name"`
	TopDomain         *string  `json:"top_domain"`
	Locations         int64    `json:"locations"`
	Cities            int64    `json:"cities"`
	States            int64    `json:"states"`
	StateList         []string `json:"state_list"`
	DistinctNames     int64    `json:"distinct_names"`
	ExcludedLocations int64    `json:"excluded_locations"`
	ExistingExclusion *struct {
		ID string `json:"id"`
	} `json:"existing_exclusion"`
	Dismissal *struct {
		ID string `json:"id"`
	} `json:"dismissal"`
	SuggestedRule struct {
		Kind      string `json:"kind"`
		Value     string `json:"value"`
		MatchMode string `json:"match_mode"`
	} `json:"suggested_rule"`
	SampleBusinesses []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"sample_businesses"`
}

type brandScanPayload struct {
	Data []brandCandidatePayload `json:"data"`
	Meta struct {
		Total int64 `json:"total"`
	} `json:"meta"`
}

func (h *harness) brandScan(query string) brandScanPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodGet, "/api/v1/exclusions/brand-scan"+query, "", http.StatusOK)
	return decodeBody[brandScanPayload](h.t, rec)
}

func byKey(rows []brandCandidatePayload) map[string]brandCandidatePayload {
	out := make(map[string]brandCandidatePayload, len(rows))
	for _, r := range rows {
		out[r.Key] = r
	}
	return out
}

// seedBrands inserts chains, a platform shared by unrelated businesses and a small
// business with two locations.
func seedBrands(t *testing.T, h *harness) {
	t.Helper()
	type row struct{ name, city, state, website string }
	var rows []row
	for i, loc := range [][2]string{{"Austin", "TX"}, {"Dallas", "TX"}, {"Tulsa", "OK"}, {"Wichita", "KS"}} {
		rows = append(rows, row{"Planet Fitness", loc[0], loc[1], fmt.Sprintf("https://www.planetfitness.com/gyms/%d", i)})
	}
	for _, city := range []string{"Austin", "Dallas", "Waco"} {
		rows = append(rows, row{"HOTWORX - " + city, city, "TX", "https://hotworx.net/studio/" + city})
	}
	for _, city := range []string{"Austin", "Dallas", "Houston"} {
		rows = append(rows, row{"Crunch Fitness - " + city, city, "TX", "https://crunch" + city + ".com"})
	}
	for _, city := range []string{"Leeds", "York", "Bath"} {
		rows = append(rows, row{"Gym Group " + city, city, "", "https://" + city + ".gymgroup.co.uk"})
	}
	for i := range 5 {
		rows = append(rows, row{fmt.Sprintf("Small Gym %d", i), "Austin", "TX", fmt.Sprintf("https://facebook.com/smallgym%d", i)})
	}
	rows = append(rows, row{"Joe's Pizza", "Austin", "TX", "https://joespizza.com"}, row{"Joe's Pizza", "Round Rock", "TX", "https://joespizza.com"})

	pool := h.app.Store().Pool()
	for i, r := range rows {
		domain := hostOf(r.website)
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO businesses (id, place_id, name, city, state, website, domain, reviews)
			VALUES (gen_random_uuid(), $1, $2, $3, nullif($4, ''), $5, $6, $7)`,
			fmt.Sprintf("brand-%d", i), r.name, r.city, r.state, r.website, domain, 100+i); err != nil {
			t.Fatalf("could not seed %s: %v", r.name, err)
		}
	}
}

// hostOf is the host part of a seeded website, without "www.", as the scraper
// stores it.
func hostOf(website string) string {
	host := website[len("https://"):]
	for i, c := range host {
		if c == '/' {
			host = host[:i]
			break
		}
	}
	if len(host) > 4 && host[:4] == "www." {
		host = host[4:]
	}
	return host
}

func TestBrandScanFindsChainsAndSkipsSharedPlatforms(t *testing.T) {
	h := newHarness(t, nil)
	seedBrands(t, h)

	// The default grouping is the registrable domain: every location of a brand
	// links to it even when each has its own name.
	scan := h.brandScan("")
	got := byKey(scan.Data)
	if got["planetfitness.com"].Locations != 4 || got["planetfitness.com"].States != 3 {
		t.Errorf("planetfitness.com is %+v", got["planetfitness.com"])
	}
	hotworx := got["hotworx.net"]
	if hotworx.Locations != 3 || hotworx.DistinctNames != 3 || hotworx.SuggestedRule.Kind != "domain" ||
		hotworx.SuggestedRule.Value != "hotworx.net" || hotworx.SuggestedRule.MatchMode != "exact" {
		t.Errorf("hotworx.net is %+v", hotworx)
	}
	if len(hotworx.SampleBusinesses) != 3 {
		t.Errorf("hotworx.net has %d samples", len(hotworx.SampleBusinesses))
	}
	if got["gymgroup.co.uk"].Locations != 3 {
		t.Errorf("a brand under a two-label suffix is %+v", got["gymgroup.co.uk"])
	}
	if _, ok := got["facebook.com"]; ok {
		t.Error("a social network was reported as a brand")
	}
	if _, ok := got["joespizza.com"]; ok {
		t.Error("a two-location business passed the default threshold")
	}
	if scan.Meta.Total != int64(len(scan.Data)) || scan.Data[0].Key != "planetfitness.com" {
		t.Errorf("the scan is ordered %v with total %d", scan.Data, scan.Meta.Total)
	}

	// Thresholds and the name groupings.
	if rows := h.brandScan("?min_states=2").Data; len(rows) != 1 || rows[0].Key != "planetfitness.com" {
		t.Errorf("min_states=2 returned %+v", rows)
	}
	if rows := byKey(h.brandScan("?min_locations=2").Data); rows["joespizza.com"].Locations != 2 {
		t.Errorf("min_locations=2 missed the two-location business: %+v", rows)
	}
	names := byKey(h.brandScan("?group_by=name").Data)
	if pf := names["planet fitness"]; pf.Locations != 4 || pf.SuggestedRule.Kind != "company" ||
		pf.SuggestedRule.Value != "Planet Fitness" || pf.SuggestedRule.MatchMode != "exact" {
		t.Errorf("the planet fitness name group is %+v", pf)
	}
	if _, ok := names["crunch fitness austin"]; ok {
		t.Error("a single listing was reported as a name group")
	}
	prefixes := byKey(h.brandScan("?group_by=name_prefix").Data)
	if cf := prefixes["crunch fitness"]; cf.Locations != 3 || cf.SuggestedRule.MatchMode != "prefix" {
		t.Errorf("the crunch fitness prefix group is %+v", cf)
	}
	if rows := h.brandScan("?q=hotw").Data; len(rows) != 1 || rows[0].Key != "hotworx.net" {
		t.Errorf("q=hotw returned %+v", rows)
	}
	if rows := h.brandScan("?state=ok").Data; len(rows) != 0 {
		t.Errorf("state=ok left one location per group, yet returned %+v", rows)
	}

	rec := h.request(http.MethodGet, "/api/v1/exclusions/brand-scan?min_locations=1", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("min_locations=1 returned %d, want 422", rec.Code)
	}
}

func TestBrandScanSuggestionsExcludeInBulkAndDismissalsHideGroups(t *testing.T) {
	h := newHarness(t, nil)
	seedBrands(t, h)
	got := byKey(h.brandScan("").Data)

	// The suggested rules go straight into the bulk endpoint; a duplicate is
	// reported on its item without failing the rest.
	body := fmt.Sprintf(`{"items":[%s,%s,%s,{"kind":"domain","value":"com"}]}`,
		ruleJSON(got["planetfitness.com"]), ruleJSON(got["hotworx.net"]), ruleJSON(got["hotworx.net"]))
	rec := h.mustRequest(http.MethodPost, "/api/v1/exclusions/bulk", body, http.StatusOK)
	bulk := decodeBody[struct {
		Created int `json:"created"`
		Results []struct {
			Index     int              `json:"index"`
			Status    string           `json:"status"`
			Exclusion exclusionPayload `json:"exclusion"`
		} `json:"results"`
	}](t, rec)
	if bulk.Created != 2 || len(bulk.Results) != 4 {
		t.Fatalf("the bulk result is %+v", bulk)
	}
	for i, want := range []string{"created", "created", "duplicate", "invalid"} {
		if bulk.Results[i].Status != want || bulk.Results[i].Index != i {
			t.Errorf("item %d is %+v, want %s", i, bulk.Results[i], want)
		}
	}
	if bulk.Results[0].Exclusion.Affected.Businesses != 4 {
		t.Errorf("the planetfitness.com rule covers %+v", bulk.Results[0].Exclusion.Affected)
	}

	// Fully excluded groups leave the scan, and come back on request with the rule.
	after := byKey(h.brandScan("").Data)
	if _, ok := after["planetfitness.com"]; ok {
		t.Error("an excluded brand is still listed")
	}
	all := byKey(h.brandScan("?include_excluded=true").Data)
	pf := all["planetfitness.com"]
	if pf.ExcludedLocations != 4 || pf.ExistingExclusion == nil || pf.ExistingExclusion.ID != bulk.Results[0].Exclusion.ID {
		t.Errorf("the excluded group is %+v", pf)
	}

	// A dismissed group is hidden from that grouping only.
	rec = h.mustRequest(http.MethodPost, "/api/v1/exclusions/brand-scan/dismissals",
		`{"group_by":"domain","key":"gymgroup.co.uk","note":"local chain, keep"}`, http.StatusCreated)
	dismissal := decodeBody[struct {
		ID string `json:"id"`
	}](t, rec)
	if rec := h.request(http.MethodPost, "/api/v1/exclusions/brand-scan/dismissals",
		`{"group_by":"domain","key":"gymgroup.co.uk"}`); rec.Code != http.StatusConflict {
		t.Errorf("a second dismissal returned %d, want 409", rec.Code)
	}
	if _, ok := byKey(h.brandScan("").Data)["gymgroup.co.uk"]; ok {
		t.Error("a dismissed group is still listed")
	}
	shown := byKey(h.brandScan("?include_dismissed=true").Data)["gymgroup.co.uk"]
	if shown.Dismissal == nil || shown.Dismissal.ID != dismissal.ID {
		t.Errorf("include_dismissed lists the group as %+v", shown)
	}
	rec = h.mustRequest(http.MethodGet, "/api/v1/exclusions/brand-scan/dismissals?group_by=domain", "", http.StatusOK)
	if list := decodeBody[brandScanPayload](t, rec); list.Meta.Total != 1 {
		t.Errorf("the dismissal list holds %d", list.Meta.Total)
	}

	h.mustRequest(http.MethodDelete, "/api/v1/exclusions/brand-scan/dismissals/"+dismissal.ID, "", http.StatusNoContent)
	h.mustRequest(http.MethodDelete, "/api/v1/exclusions/brand-scan/dismissals/"+dismissal.ID, "", http.StatusNotFound)
	if _, ok := byKey(h.brandScan("").Data)["gymgroup.co.uk"]; !ok {
		t.Error("an undismissed group did not come back")
	}
}

func ruleJSON(c brandCandidatePayload) string {
	return fmt.Sprintf(`{"kind":%q,"value":%q,"match_mode":%q}`, c.SuggestedRule.Kind, c.SuggestedRule.Value, c.SuggestedRule.MatchMode)
}
