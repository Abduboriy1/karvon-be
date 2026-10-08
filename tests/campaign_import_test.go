package integration_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
)

type sourcedCampaignPayload struct {
	campaignPayload
	Source     string `json:"source"`
	SendsTotal int64  `json:"sends_total"`
}

// listCampaigns reads the campaign list with a raw query string.
func (h *harness) listCampaigns(query string) []sourcedCampaignPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodGet, "/api/v1/campaigns"+query, "", http.StatusOK)
	var page struct {
		Data []sourcedCampaignPayload `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		h.t.Fatalf("could not decode the campaign list: %v", err)
	}
	return page.Data
}

// waitForImported syncs with Instantly and waits until the imported campaign
// matches.
func (h *harness) waitForImported(instantlyID string, ok func(sourcedCampaignPayload) bool) sourcedCampaignPayload {
	h.t.Helper()
	h.mustRequest(http.MethodPost, "/api/v1/campaigns/sync", "", http.StatusAccepted)
	deadline := time.Now().Add(30 * time.Second)
	var last sourcedCampaignPayload
	for time.Now().Before(deadline) {
		for _, c := range h.listCampaigns("?source=instantly") {
			if c.InstantlyCampaignID != nil && *c.InstantlyCampaignID == instantlyID {
				last = c
				if ok(c) {
					return c
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("the Instantly campaign %s never synced as expected; last seen %+v", instantlyID, last)
	return last
}

// A campaign started in Instantly's own app shows up in the campaign list marked
// as such, with Instantly's numbers, follows its renames and status changes, and
// cannot be edited from here. A campaign launched from Karvon is not imported a
// second time.
func TestACampaignStartedInInstantlyIsImported(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.campaign = c.launchCampaign(c.campaign.ID)

	native := instantly.Campaign{
		ID: "native-1", Name: "Dallas dentists", Status: instantly.CampaignStatusActive,
		Sequences:        []instantly.Sequence{{Steps: []instantly.Step{{Type: "email"}, {Type: "email"}, {Type: "email"}}}},
		TimestampCreated: time.Now().Add(-48 * time.Hour).UTC(),
	}
	c.instantly.PutCampaign(native, instantly.CampaignAnalytics{
		LeadsCount: 40, ContactedCount: 25, ReplyCountUnique: 3, EmailsSentCount: 60, BouncedCount: 1,
		TotalOpportunities: 2,
	})

	imported := c.waitForImported("native-1", func(p sourcedCampaignPayload) bool { return p.Contacted == 25 })
	if imported.Source != "instantly" || imported.Name != "Dallas dentists" || imported.Status != "active" || imported.Steps != 3 {
		t.Fatalf("imported campaign = %+v", imported)
	}
	if imported.LeadsTotal != 40 || imported.Replied != 3 || imported.Interested != 2 || imported.Bounced != 1 || imported.SendsTotal != 60 {
		t.Fatalf("imported counters = %+v", imported)
	}

	all := c.listCampaigns("")
	if len(all) != 2 {
		t.Fatalf("want the Karvon campaign and the import, got %d campaigns", len(all))
	}
	for _, p := range all {
		if p.ID == c.campaign.ID && p.Source != "karvon" {
			t.Fatalf("the launched campaign has source %q", p.Source)
		}
	}
	if karvon := c.listCampaigns("?source=karvon"); len(karvon) != 1 || karvon[0].ID != c.campaign.ID {
		t.Fatalf("source=karvon returned %+v", karvon)
	}

	c.mustRequest(http.MethodPatch, "/api/v1/campaigns/"+imported.ID, `{"name":"Renamed here"}`, http.StatusConflict)
	c.mustRequest(http.MethodPost, "/api/v1/campaigns/"+imported.ID+"/launch", "", http.StatusConflict)

	native.Name = "Dallas dentists v2"
	native.Status = instantly.CampaignStatusPaused
	c.instantly.PutCampaign(native, instantly.CampaignAnalytics{LeadsCount: 40, ContactedCount: 30})
	c.waitForImported("native-1", func(p sourcedCampaignPayload) bool {
		return p.Name == "Dallas dentists v2" && p.Status == "paused"
	})
}

// An imported campaign sends nothing through Karvon, so its local numbers are
// all zero. Analytics still carries Instantly's figures but must not report that
// as a mismatch: no sync could ever clear it.
func TestAnImportedCampaignReportsNoMismatch(t *testing.T) {
	c := newCampaignHarness(t, 1)

	c.instantly.PutCampaign(instantly.Campaign{
		ID: "native-2", Name: "Austin plumbers", Status: instantly.CampaignStatusActive,
		Sequences:        []instantly.Sequence{{Steps: []instantly.Step{{Type: "email"}}}},
		TimestampCreated: time.Now().Add(-24 * time.Hour).UTC(),
	}, instantly.CampaignAnalytics{
		LeadsCount: 50, ContactedCount: 45, EmailsSentCount: 120, ReplyCountUnique: 6, BouncedCount: 4,
		OpenCountUnique: 30, LinkClickCountUnique: 5,
	})
	imported := c.waitForImported("native-2", func(p sourcedCampaignPayload) bool { return p.SendsTotal == 120 })

	rec := c.mustRequest(http.MethodGet, "/api/v1/campaign-analytics/campaigns/"+imported.ID, "", http.StatusOK)
	var analytics struct {
		Local     metricsPayload `json:"local"`
		Instantly map[string]any `json:"instantly"`
		Mismatch  []any          `json:"mismatch"`
	}
	decodeInto(t, rec, &analytics)

	if analytics.Local.Sends != 0 {
		t.Fatalf("an imported campaign has %d local sends", analytics.Local.Sends)
	}
	if sent, _ := analytics.Instantly["emails_sent_count"].(float64); sent != 120 {
		t.Fatalf("Instantly's figures were not returned: %v", analytics.Instantly)
	}
	if analytics.Mismatch == nil || len(analytics.Mismatch) != 0 {
		t.Fatalf("mismatch = %v, want []", analytics.Mismatch)
	}
}
