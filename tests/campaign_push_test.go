package integration_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider"
)

func TestAFailedLeadPushIsRetriedWithoutDuplicatingLeads(t *testing.T) {
	c := newCampaignHarness(t, 1)

	// Instantly drops the first two bulk calls, then recovers.
	c.instantly.Fail["AddLeads"] = []error{
		fmt.Errorf("instantly: unexpected status 503"),
		fmt.Errorf("instantly: unexpected status 502"),
	}
	launched := c.launchCampaign(c.campaign.ID)
	if launched.Status != campaign.CampaignActive {
		t.Fatalf("the campaign is %s, want active (error %v)", launched.Status, launched.Error)
	}
	c.campaign = launched

	leads := c.leads(c.campaign.ID)
	seen := map[string]int{}
	for _, lead := range leads {
		if lead.Status != campaign.LeadActive {
			t.Errorf("lead %s is %s, want active", lead.Email, lead.Status)
		}
		if lead.InstantlyLeadID == nil {
			t.Fatalf("lead %s has no Instantly id", lead.Email)
		}
		seen[*lead.InstantlyLeadID]++
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("the Instantly lead id %s is shared by %d local leads", id, count)
		}
	}
	// Instantly holds exactly one lead per address, whatever the retries did.
	counts := map[string]int{}
	for _, lead := range c.instantly.Leads {
		counts[strings.ToLower(lead.Email)]++
	}
	for email, count := range counts {
		if count > 1 {
			t.Errorf("Instantly holds %d leads for %s", count, email)
		}
	}
}

func TestARateLimitedPushReleasesItsClaimsAndSucceedsLater(t *testing.T) {
	c := newCampaignHarness(t, 1)

	c.instantly.Fail["AddLeads"] = []error{
		&provider.RetryAfterError{Err: provider.ErrRateLimited, After: time.Second},
	}
	launched := c.launchCampaign(c.campaign.ID)
	if launched.Status != campaign.CampaignActive {
		t.Fatalf("the campaign is %s, want active (error %v)", launched.Status, launched.Error)
	}

	for _, lead := range c.leads(launched.ID) {
		if lead.Status != campaign.LeadActive {
			t.Errorf("lead %s is %s, want active after the throttle cleared", lead.Email, lead.Status)
		}
	}
}

func TestAnAuthFailureStopsTheLaunchInsteadOfBurningRetries(t *testing.T) {
	c := newCampaignHarness(t, 1)

	c.instantly.Fail["CreateCampaign"] = []error{provider.ErrAuth}
	c.mustRequest(http.MethodPost, "/api/v1/campaigns/"+c.campaign.ID+"/launch", "", http.StatusAccepted)
	failed := c.waitForCampaign(c.campaign.ID, campaign.CampaignFailed)

	if failed.Error == nil || *failed.Error == "" {
		t.Fatal("the failed campaign carries no explanation")
	}
	var creates int
	for _, req := range c.instantly.Requests() {
		if req.Method == "CreateCampaign" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("Instantly was asked to create the campaign %d times after a rejected key", creates)
	}
	// Nothing was pushed, and the leads are still waiting.
	for _, lead := range c.leads(c.campaign.ID) {
		if lead.Status != campaign.LeadPending {
			t.Errorf("lead %s is %s, want pending", lead.Email, lead.Status)
		}
	}
}

func TestTheLaunchChecklistBlocksAnIncompleteCampaign(t *testing.T) {
	h, _ := seededHarness(t)
	h.configureInstantly()
	accountID := h.seedSendingAccount("sender@karvon.test")

	// A campaign with nothing attached.
	rec := h.mustRequest(http.MethodPost, "/api/v1/campaigns", `{"name":"Empty","steps":1}`, http.StatusCreated)
	camp := decodeBody[campaignPayload](t, rec)

	checklist := h.checklist(t, camp.ID)
	if checklist.Ready {
		t.Fatal("an empty campaign reported itself ready to launch")
	}
	failing := map[string]bool{}
	for _, item := range checklist.Items {
		if item.Blocking && !item.OK {
			failing[item.Key] = true
		}
		if !item.OK && item.Message == "" {
			t.Errorf("checklist item %s failed with no explanation", item.Key)
		}
	}
	for _, key := range []string{"sending_accounts", "variants_weights_100", "leads_pending"} {
		if !failing[key] {
			t.Errorf("the checklist did not flag %s", key)
		}
	}
	h.mustRequest(http.MethodPost, "/api/v1/campaigns/"+camp.ID+"/launch", "", http.StatusConflict)

	// A variant that has not been approved is refused too.
	draft := h.draftVariant(t, "Unreviewed")
	h.mustRequest(http.MethodPut, "/api/v1/campaigns/"+camp.ID+"/sending-accounts",
		fmt.Sprintf(`{"sending_account_ids":[%q]}`, accountID), http.StatusOK)
	h.mustRequest(http.MethodPut, "/api/v1/campaigns/"+camp.ID+"/variants",
		fmt.Sprintf(`{"items":[{"variant_id":%q,"step":1,"weight":100}]}`, draft), http.StatusUnprocessableEntity)
}

func TestVariantWeightsMustTotalOneHundred(t *testing.T) {
	c := newCampaignHarness(t, 2)
	variants := c.campaignVariants(t, c.campaign.ID)

	body := fmt.Sprintf(`{"items":[{"variant_id":%q,"step":1,"weight":70},{"variant_id":%q,"step":1,"weight":20}]}`,
		variants[0], variants[1])
	c.mustRequest(http.MethodPut, "/api/v1/campaigns/"+c.campaign.ID+"/variants", body, http.StatusUnprocessableEntity)

	body = fmt.Sprintf(`{"items":[{"variant_id":%q,"step":1,"weight":60},{"variant_id":%q,"step":1,"weight":40}]}`,
		variants[0], variants[1])
	c.mustRequest(http.MethodPut, "/api/v1/campaigns/"+c.campaign.ID+"/variants", body, http.StatusOK)
}

func TestASentVariantCannotBeDetachedOrRewritten(t *testing.T) {
	c := newCampaignHarness(t, 2)
	c.campaign = c.launchCampaign(c.campaign.ID)
	target := c.leads(c.campaign.ID)[0]
	assigned := c.leadDetail(c.campaign.ID, target.ID).Assignments[0]

	// Detaching the variant a lead has already been sent is refused; setting its
	// weight to zero is the supported way to retire it.
	variants := c.campaignVariants(t, c.campaign.ID)
	var other string
	for _, v := range variants {
		if v != assigned.VariantID {
			other = v
		}
	}
	c.mustRequest(http.MethodPut, "/api/v1/campaigns/"+c.campaign.ID+"/variants",
		fmt.Sprintf(`{"items":[{"variant_id":%q,"step":1,"weight":100}]}`, other), http.StatusConflict)

	c.mustRequest(http.MethodPut, "/api/v1/campaigns/"+c.campaign.ID+"/variants",
		fmt.Sprintf(`{"items":[{"variant_id":%q,"step":1,"weight":0},{"variant_id":%q,"step":1,"weight":100}]}`,
			assigned.VariantID, other), http.StatusOK)

	// The variant's own components are frozen once it has been sent.
	rec := c.mustRequest(http.MethodGet, "/api/v1/content/variants/"+assigned.VariantID, "", http.StatusOK)
	var detail struct {
		Components []struct {
			ID string `json:"id"`
		} `json:"components"`
	}
	decodeInto(t, rec, &detail)
	if len(detail.Components) == 0 {
		t.Fatal("the sent variant has no components")
	}
	c.mustRequest(http.MethodPatch, "/api/v1/content/components/"+detail.Components[0].ID,
		`{"body":"A completely different opening line"}`, http.StatusConflict)
}

func TestWeightedAssignmentFollowsTheConfiguredWeights(t *testing.T) {
	if testing.Short() {
		t.Skip("the distribution check needs a few thousand leads")
	}
	c := newCampaignHarness(t, 2)
	variants := c.campaignVariants(t, c.campaign.ID)
	c.mustRequest(http.MethodPut, "/api/v1/campaigns/"+c.campaign.ID+"/variants",
		fmt.Sprintf(`{"items":[{"variant_id":%q,"step":1,"weight":40},{"variant_id":%q,"step":1,"weight":60}]}`,
			variants[0], variants[1]), http.StatusOK)

	// Two thousand contacts is enough for the split to be meaningful.
	const total = 2000
	if _, err := c.app.Store().Pool().Exec(t.Context(), `
		INSERT INTO contacts (id, email, domain, source)
		SELECT gen_random_uuid(), 'weighted' || g || '@example.test', 'example.test', 'manual'
		FROM generate_series(1, $1) g`, total); err != nil {
		t.Fatalf("could not seed contacts: %v", err)
	}
	if _, err := c.app.Store().Pool().Exec(t.Context(), `
		INSERT INTO campaign_leads (id, campaign_id, contact_id)
		SELECT gen_random_uuid(), $1, id FROM contacts WHERE email LIKE 'weighted%@example.test'`, c.campaign.ID); err != nil {
		t.Fatalf("could not seed leads: %v", err)
	}

	c.campaign = c.launchCampaign(c.campaign.ID)

	counts := map[string]int{}
	rows, err := c.app.Store().Pool().Query(t.Context(), `
		SELECT va.variant_id::text, count(*) FROM variant_assignments va
		JOIN campaign_leads cl ON cl.id = va.campaign_lead_id
		WHERE cl.campaign_id = $1 GROUP BY 1`, c.campaign.ID)
	if err != nil {
		t.Fatalf("could not count assignments: %v", err)
	}
	defer rows.Close()
	assigned := 0
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		counts[id] = n
		assigned += n
	}
	if assigned < total {
		t.Fatalf("only %d of %d leads were assigned a variant", assigned, total)
	}
	share := float64(counts[variants[0]]) / float64(assigned) * 100
	if share < 36 || share > 44 {
		t.Fatalf("the 40%% variant took %.1f%% of %d assignments", share, assigned)
	}
}
