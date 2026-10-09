package integration_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
)

type cleanupLeadPayload struct {
	ID                    string  `json:"id"`
	Email                 string  `json:"email"`
	Status                string  `json:"status"`
	ProviderRemovedAt     *string `json:"provider_removed_at"`
	ProviderRemovedReason *string `json:"provider_removed_reason"`
}

type cleanupRunPayload struct {
	ID          string  `json:"id"`
	Status      string  `json:"status"`
	Trigger     string  `json:"trigger"`
	Selected    int     `json:"selected"`
	Removed     int     `json:"removed"`
	AlreadyGone int     `json:"already_gone"`
	Failed      int     `json:"failed"`
	Error       *string `json:"error"`
}

type cleanupPreviewPayload struct {
	Eligible  int64 `json:"eligible"`
	Campaigns []struct {
		Campaign struct {
			ID string `json:"id"`
		} `json:"campaign"`
		InInstantly int64 `json:"in_instantly"`
		Eligible    int64 `json:"eligible"`
	} `json:"campaigns"`
	Capacity struct {
		Limit       *int   `json:"limit"`
		InUse       int64  `json:"in_use"`
		Available   *int64 `json:"available"`
		KarvonLeads int64  `json:"karvon_leads"`
	} `json:"capacity"`
}

func (c *campaignHarness) cleanupPreview(query string) cleanupPreviewPayload {
	c.t.Helper()
	rec := c.mustRequest(http.MethodGet, "/api/v1/integrations/instantly/cleanup/preview?"+query, "", http.StatusOK)
	return decodeBody[cleanupPreviewPayload](c.t, rec)
}

func (c *campaignHarness) waitForCleanupRun(id string) cleanupRunPayload {
	c.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last cleanupRunPayload
	for time.Now().Before(deadline) {
		rec := c.mustRequest(http.MethodGet, "/api/v1/integrations/instantly/cleanup/runs/"+id, "", http.StatusOK)
		last = decodeBody[cleanupRunPayload](c.t, rec)
		if last.Status == campaign.CleanupDone || last.Status == campaign.CleanupFailed {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("cleanup run %s never finished, last state: %+v", id, last)
	return last
}

func (c *campaignHarness) cleanupLead(email string) cleanupLeadPayload {
	c.t.Helper()
	rec := c.mustRequest(http.MethodGet, "/api/v1/campaigns/"+c.campaign.ID+"/leads?per_page=200", "", http.StatusOK)
	for _, lead := range decodeBody[struct {
		Data []cleanupLeadPayload `json:"data"`
	}](c.t, rec).Data {
		if strings.EqualFold(lead.Email, email) {
			return lead
		}
	}
	c.t.Fatalf("no lead for %s", email)
	return cleanupLeadPayload{}
}

func TestCleanupDeletesFinishedLeadsFromInstantlyAndKeepsTheirHistory(t *testing.T) {
	c := newCampaignHarness(t, 1)

	// With the plan's cap known, the checklist warns (without blocking) when the
	// campaign would not fit.
	c.mustRequest(http.MethodPut, "/api/v1/integrations/instantly/cleanup/settings",
		`{"scope":"finished","min_idle_days":3,"include_replied":false,"auto_enabled":false,"contact_limit":1}`, http.StatusOK)
	checklist := c.checklist(t, c.campaign.ID)
	capacityItem := false
	for _, item := range checklist.Items {
		if item.Key == "instantly_capacity" {
			capacityItem = true
			if item.OK || item.Blocking {
				t.Errorf("instantly_capacity is ok=%v blocking=%v, want a non-blocking warning", item.OK, item.Blocking)
			}
		}
	}
	if !capacityItem || !checklist.Ready {
		t.Fatalf("want a ready checklist carrying an instantly_capacity warning, got %+v", checklist)
	}

	c.campaign = c.launchCampaign(c.campaign.ID)
	if c.campaign.Status != campaign.CampaignActive {
		t.Fatalf("the campaign is %s, want active", c.campaign.Status)
	}
	leads := c.leads(c.campaign.ID)
	if len(leads) < 2 {
		t.Fatalf("want at least two leads, got %d", len(leads))
	}
	target, other := leads[0], leads[1]
	c.event(t, campaign.InstantlyEmailSent, target.Email, nil)

	// Emailed but mid-sequence: only the "emailed" scope takes it.
	if got := c.cleanupPreview("scope=finished&min_idle_days=0").Eligible; got != 0 {
		t.Errorf("finished scope previews %d, want 0 while the sequence runs", got)
	}
	if got := c.cleanupPreview("scope=emailed&min_idle_days=0").Eligible; got != 1 {
		t.Errorf("emailed scope previews %d, want 1", got)
	}

	// Instantly finishes the sequence; the sync mirrors it.
	lead := c.instantly.Leads[*target.InstantlyLeadID]
	lead.Status = instantly.LeadStatusCompleted
	c.instantly.Leads[*target.InstantlyLeadID] = lead
	c.mustRequest(http.MethodPost, "/api/v1/campaigns/"+c.campaign.ID+"/sync", "", http.StatusAccepted)
	deadline := time.Now().Add(30 * time.Second)
	for c.leadByEmail(c.campaign.ID, target.Email).Status != campaign.LeadCompleted {
		if time.Now().After(deadline) {
			t.Fatal("the completed lead was never mirrored")
		}
		time.Sleep(100 * time.Millisecond)
	}

	before := c.cleanupPreview("scope=finished&min_idle_days=0")
	if before.Eligible != 1 {
		t.Fatalf("finished scope previews %d, want 1", before.Eligible)
	}
	if got := c.cleanupPreview("scope=finished&min_idle_days=3").Eligible; got != 0 {
		t.Errorf("a 3-day idle window previews %d, want 0 for a lead emailed just now", got)
	}

	rec := c.mustRequest(http.MethodPost, "/api/v1/integrations/instantly/cleanup/runs",
		`{"scope":"finished","min_idle_days":0,"include_replied":false}`, http.StatusAccepted)
	run := c.waitForCleanupRun(decodeBody[cleanupRunPayload](t, rec).ID)
	if run.Status != campaign.CleanupDone || run.Removed != 1 || run.Selected != 1 || run.Trigger != campaign.CleanupTriggerManual {
		t.Fatalf("run finished as %+v, want done with one removal", run)
	}

	// Gone from Instantly, kept here.
	if _, still := c.instantly.Leads[*target.InstantlyLeadID]; still {
		t.Error("the lead is still in Instantly")
	}
	if _, still := c.instantly.Leads[*other.InstantlyLeadID]; !still {
		t.Error("the lead that was never emailed was deleted from Instantly")
	}
	removed := c.cleanupLead(target.Email)
	if removed.ProviderRemovedAt == nil || removed.ProviderRemovedReason == nil || *removed.ProviderRemovedReason != campaign.ProviderRemovedCleanup {
		t.Errorf("the lead is not stamped as cleaned up: %+v", removed)
	}
	contact := c.contactByEmail(target.Email)
	if contact.LifecycleStage != string(campaign.StageContacted) {
		t.Errorf("the contact is %s, want contacted still", contact.LifecycleStage)
	}
	found := false
	for _, ev := range c.timeline(contact.ID) {
		if ev.Type == campaign.EventRemovedFromProvider {
			found = true
		}
	}
	if !found {
		t.Error("the timeline has no removed_from_provider event")
	}
	after := c.cleanupPreview("scope=finished&min_idle_days=0")
	if after.Capacity.KarvonLeads != before.Capacity.KarvonLeads-1 {
		t.Errorf("karvon_leads went %d → %d, want one fewer", before.Capacity.KarvonLeads, after.Capacity.KarvonLeads)
	}

	// Nothing left to do is a conflict, not an empty run.
	c.mustRequest(http.MethodPost, "/api/v1/integrations/instantly/cleanup/runs",
		`{"scope":"finished","min_idle_days":0,"include_replied":false}`, http.StatusConflict)

	// The cleaned-up address stays out of the next campaign by default.
	second := c.createCampaign("Austin gyms, round two", c.accountID, c.variantID)
	result := c.importLeads(second.ID, `{"primary_only":true}`)
	if got := result["skipped_contacted"]; got != float64(1) {
		t.Errorf("skipped_contacted = %v, want 1", got)
	}
	result = c.importLeads(second.ID, `{"primary_only":true,"exclude_contacted":false}`)
	if got := result["imported"]; got != float64(1) {
		t.Errorf("a follow-up import that allows contacted addresses imported %v, want 1", got)
	}
}

// waitForPrepared waits until a scheduled campaign has been readied at Instantly:
// its campaign created and every lead pushed, while it is still scheduled.
func (c *campaignHarness) waitForPrepared(id string) campaignPayload {
	c.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last campaignPayload
	for time.Now().Before(deadline) {
		rec := c.mustRequest(http.MethodGet, "/api/v1/campaigns/"+id, "", http.StatusOK)
		last = decodeBody[campaignPayload](c.t, rec)
		if last.InstantlyCampaignID != nil && last.LeadsTotal > 0 && last.LeadsPushed == last.LeadsTotal {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("campaign %s was never prepared at Instantly; last state: %+v", id, last)
	return last
}

func TestAScheduledLaunchIsPreparedNowAndActivatedAtItsTime(t *testing.T) {
	c := newCampaignHarness(t, 1)
	path := "/api/v1/campaigns/" + c.campaign.ID

	// In the past, measured by the campaign clock.
	c.mustRequest(http.MethodPost, path+"/launch", `{"scheduled_at":"2020-01-01T00:00:00Z"}`, http.StatusUnprocessableEntity)

	// Far enough ahead that the launch itself does not fire during the test.
	later := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rec := c.mustRequest(http.MethodPost, path+"/launch", fmt.Sprintf(`{"scheduled_at":%q}`, later), http.StatusAccepted)
	scheduled := decodeBody[struct {
		Status            string  `json:"status"`
		ScheduledLaunchAt *string `json:"scheduled_launch_at"`
	}](t, rec)
	if scheduled.Status != campaign.CampaignScheduled || scheduled.ScheduledLaunchAt == nil {
		t.Fatalf("got %+v, want a scheduled campaign with its time", scheduled)
	}

	// The Instantly campaign is created and filled straight away, but not started.
	prepared := c.waitForPrepared(c.campaign.ID)
	if prepared.Status != campaign.CampaignScheduled {
		t.Fatalf("a prepared campaign is %s, want still scheduled", prepared.Status)
	}
	if n := c.instantly.LeadsIn(*prepared.InstantlyCampaignID); n != prepared.LeadsTotal {
		t.Errorf("Instantly holds %d leads, want %d", n, prepared.LeadsTotal)
	}
	if n := c.instantly.Calls("ActivateCampaign"); n != 0 {
		t.Fatalf("a scheduled campaign was activated %d times before its time", n)
	}

	// Unschedule goes back to draft; a second unschedule has nothing to undo.
	rec = c.mustRequest(http.MethodPost, path+"/unschedule", "", http.StatusOK)
	if got := decodeBody[campaignPayload](t, rec).Status; got != campaign.CampaignDraft {
		t.Fatalf("unscheduled campaign is %s, want draft", got)
	}
	c.mustRequest(http.MethodPost, path+"/unschedule", "", http.StatusConflict)

	// Scheduled a moment ahead, it launches on its own, reusing the campaign
	// already prepared at Instantly.
	soon := time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339Nano)
	c.mustRequest(http.MethodPost, path+"/launch", fmt.Sprintf(`{"scheduled_at":%q}`, soon), http.StatusAccepted)
	launched := c.waitForCampaign(c.campaign.ID, campaign.CampaignActive, campaign.CampaignFailed)
	if launched.Status != campaign.CampaignActive {
		t.Fatalf("the scheduled launch ended %s (error %v)", launched.Status, launched.Error)
	}
	if n := c.instantly.Calls("CreateCampaign"); n != 1 {
		t.Errorf("Instantly campaigns created = %d, want 1", n)
	}
	if n := c.instantly.Calls("ActivateCampaign"); n != 1 {
		t.Errorf("Instantly campaign activated %d times, want 1", n)
	}
}

// Scheduling a launch frees Instantly contact slots before its leads go up: the
// leads of a campaign started in Instantly that has completed are deleted, while
// a campaign still running there keeps every lead.
func TestAScheduledLaunchClearsFinishedImportedLeadsFirst(t *testing.T) {
	c := newCampaignHarness(t, 1)
	steps := []instantly.Sequence{{Steps: []instantly.Step{{Type: "email"}}}}
	created := time.Now().Add(-72 * time.Hour).UTC()
	c.instantly.PutCampaign(instantly.Campaign{ID: "native-done", Name: "Old run", Status: instantly.CampaignStatusCompleted,
		Sequences: steps, TimestampCreated: created}, instantly.CampaignAnalytics{LeadsCount: 3})
	c.instantly.PutCampaign(instantly.Campaign{ID: "native-live", Name: "Still sending", Status: instantly.CampaignStatusActive,
		Sequences: steps, TimestampCreated: created}, instantly.CampaignAnalytics{LeadsCount: 2})
	for i := 0; i < 3; i++ {
		c.instantly.PutLeads(instantly.Lead{ID: fmt.Sprintf("done-%d", i), Email: fmt.Sprintf("done%d@old.test", i), Campaign: "native-done"})
	}
	for i := 0; i < 2; i++ {
		c.instantly.PutLeads(instantly.Lead{ID: fmt.Sprintf("live-%d", i), Email: fmt.Sprintf("live%d@old.test", i), Campaign: "native-live"})
	}
	c.waitForImported("native-done", func(p sourcedCampaignPayload) bool { return p.Status == campaign.CampaignCompleted })
	c.waitForImported("native-live", func(p sourcedCampaignPayload) bool { return p.Status == campaign.CampaignActive })

	later := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	c.mustRequest(http.MethodPost, "/api/v1/campaigns/"+c.campaign.ID+"/launch", fmt.Sprintf(`{"scheduled_at":%q}`, later), http.StatusAccepted)
	c.waitForPrepared(c.campaign.ID)

	if n := c.instantly.LeadsIn("native-done"); n != 0 {
		t.Errorf("the completed Instantly campaign still holds %d leads, want 0", n)
	}
	if n := c.instantly.LeadsIn("native-live"); n != 2 {
		t.Errorf("the running Instantly campaign holds %d leads, want its 2", n)
	}
}

func TestLaunchingAScheduledCampaignNowMakesTheScheduledJobStandDown(t *testing.T) {
	c := newCampaignHarness(t, 1)
	path := "/api/v1/campaigns/" + c.campaign.ID

	later := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	c.mustRequest(http.MethodPost, path+"/launch", fmt.Sprintf(`{"scheduled_at":%q}`, later), http.StatusAccepted)

	// No body: launch now instead.
	launched := c.launchCampaign(c.campaign.ID)
	if launched.Status != campaign.CampaignActive {
		t.Fatalf("the campaign is %s, want active (error %v)", launched.Status, launched.Error)
	}
	rec := c.mustRequest(http.MethodGet, path, "", http.StatusOK)
	if got := decodeBody[struct {
		ScheduledLaunchAt *string `json:"scheduled_launch_at"`
	}](t, rec).ScheduledLaunchAt; got != nil {
		t.Errorf("scheduled_launch_at is %s after launching now, want null", *got)
	}
	if n := c.instantly.Calls("CreateCampaign"); n != 1 {
		t.Errorf("Instantly campaigns created = %d, want 1", n)
	}
	// An active campaign cannot be scheduled.
	c.mustRequest(http.MethodPost, path+"/launch", fmt.Sprintf(`{"scheduled_at":%q}`, later), http.StatusConflict)
}
