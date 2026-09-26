package integration_test

import (
	"encoding/csv"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
)

type exclusionPayload struct {
	ID           string  `json:"id"`
	Kind         string  `json:"kind"`
	Value        string  `json:"value"`
	DisplayValue string  `json:"display_value"`
	MatchMode    string  `json:"match_mode"`
	Source       string  `json:"source"`
	RemovedAt    *string `json:"removed_at"`
	Affected     struct {
		Businesses int64 `json:"businesses"`
		Emails     int64 `json:"emails"`
		Contacts   int64 `json:"contacts"`
		LiveLeads  int64 `json:"live_leads"`
	} `json:"affected"`
}

type exclusionRefPayload struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type excludedBusinessListPayload struct {
	Data []struct {
		ID        string               `json:"id"`
		Domain    string               `json:"domain"`
		Exclusion *exclusionRefPayload `json:"exclusion"`
	} `json:"data"`
	Meta struct {
		Total int64 `json:"total"`
	} `json:"meta"`
}

func (h *harness) exclude(body string) exclusionPayload {
	h.t.Helper()
	rec := h.mustRequest(http.MethodPost, "/api/v1/exclusions", body, http.StatusCreated)
	return decodeBody[exclusionPayload](h.t, rec)
}

// waitForLeadStatus polls until a campaign lead reaches the wanted status.
func (h *harness) waitForLeadStatus(campaignID, email, want string) campaignLeadPayload {
	h.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last campaignLeadPayload
	for time.Now().Before(deadline) {
		last = h.leadByEmail(campaignID, email)
		if last.Status == want {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("the lead for %s never became %s; it is %s", email, want, last.Status)
	return last
}

func TestADomainExclusionReachesEveryRecordAndStopsPushedLeads(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.campaign = c.launchCampaign(c.campaign.ID)
	target := c.leadByEmail(c.campaign.ID, "hi@austinbarbell.com")
	if target.Status != campaign.LeadActive || target.InstantlyLeadID == nil {
		t.Fatalf("the lead was not pushed before the exclusion: %+v", target)
	}

	// Any spelling of the domain lands on the same rule.
	rule := c.exclude(`{"kind":"domain","value":"https://WWW.AustinBarbell.com/contact","reason":"big brand"}`)
	if rule.Value != "austinbarbell.com" {
		t.Fatalf("the rule was stored as %q, want austinbarbell.com", rule.Value)
	}
	if rule.Affected.Businesses != 1 || rule.Affected.Contacts != 1 {
		t.Errorf("the rule covers %+v, want one business and one contact", rule.Affected)
	}
	rec := c.request(http.MethodPost, "/api/v1/exclusions", `{"kind":"domain","value":"austinbarbell.com."}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("a duplicate rule returned %d, want 409", rec.Code)
	}

	// The pushed lead leaves the campaign and Instantly, so no follow-up is sent.
	c.waitForLeadStatus(c.campaign.ID, "hi@austinbarbell.com", campaign.LeadExcluded)
	deadline := time.Now().Add(20 * time.Second)
	for !c.instantlyDeleted() {
		if time.Now().After(deadline) {
			t.Fatal("the excluded lead was never removed from Instantly")
		}
		time.Sleep(100 * time.Millisecond)
	}
	other := c.leadByEmail(c.campaign.ID, "info@ironworksgym.com")
	if other.Status != campaign.LeadActive {
		t.Errorf("an unrelated lead became %s", other.Status)
	}

	// The data is still there, marked rather than deleted.
	rec = c.mustRequest(http.MethodGet, "/api/v1/businesses?excluded=true", "", http.StatusOK)
	excluded := decodeBody[excludedBusinessListPayload](t, rec)
	if excluded.Meta.Total != 1 || excluded.Data[0].Exclusion == nil || excluded.Data[0].Exclusion.ID != rule.ID {
		t.Fatalf("the excluded business list is %+v", excluded)
	}
	rec = c.mustRequest(http.MethodGet, "/api/v1/businesses?excluded=false", "", http.StatusOK)
	for _, row := range decodeBody[excludedBusinessListPayload](t, rec).Data {
		if row.Domain == "austinbarbell.com" {
			t.Error("excluded=false still lists the excluded business")
		}
	}
	contact := c.contactByEmail("hi@austinbarbell.com")
	if contact.SuppressedAt != nil {
		t.Error("an exclusion must not suppress the contact")
	}

	// An export is a contact list, so it never carries the excluded business.
	rec = c.mustRequest(http.MethodPost, "/api/v1/businesses/export", `{}`, http.StatusOK)
	if _, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll(); err != nil {
		t.Fatalf("export is not valid CSV: %v", err)
	}
	if strings.Contains(rec.Body.String(), "austinbarbell.com") {
		t.Error("the export still carries the excluded business")
	}

	// Removing the rule applies at once; the pushed lead stays out.
	c.mustRequest(http.MethodDelete, "/api/v1/exclusions/"+rule.ID+"?note=mistake", "", http.StatusNoContent)
	rec = c.mustRequest(http.MethodGet, "/api/v1/businesses?excluded=true", "", http.StatusOK)
	if decodeBody[excludedBusinessListPayload](t, rec).Meta.Total != 0 {
		t.Error("the business is still excluded after the rule was removed")
	}
	time.Sleep(500 * time.Millisecond)
	if lead := c.leadByEmail(c.campaign.ID, "hi@austinbarbell.com"); lead.Status != campaign.LeadExcluded {
		t.Errorf("a pushed lead came back as %s; it must stay excluded", lead.Status)
	}
	rec = c.request(http.MethodDelete, "/api/v1/exclusions/"+rule.ID, "")
	if rec.Code != http.StatusConflict {
		t.Errorf("removing twice returned %d, want 409", rec.Code)
	}
	rec = c.mustRequest(http.MethodGet, "/api/v1/exclusions?status=removed", "", http.StatusOK)
	if !strings.Contains(rec.Body.String(), rule.ID) {
		t.Error("the removed rule is not kept for audit")
	}
}

func (c *campaignHarness) instantlyDeleted() bool {
	for _, req := range c.instantly.Requests() {
		if req.Method == "DeleteLead" {
			return true
		}
	}
	return false
}

func TestACompanyExclusionHoldsBackUnpushedLeadsAndRemovalRestoresThem(t *testing.T) {
	c := newCampaignHarness(t, 1)
	lead := c.leadByEmail(c.campaign.ID, "info@ironworksgym.com")
	if lead.Status != campaign.LeadPending {
		t.Fatalf("the lead is %s before launch, want pending", lead.Status)
	}

	// Punctuation, case and a legal suffix do not matter.
	rule := c.exclude(`{"kind":"company","value":"The Iron-Works Gym, LLC"}`)
	if rule.Value != "iron works gym" {
		t.Fatalf("the company key is %q, want \"iron works gym\"", rule.Value)
	}
	c.waitForLeadStatus(c.campaign.ID, "info@ironworksgym.com", campaign.LeadExcluded)

	// A launch pushes everything except the excluded lead.
	c.campaign = c.launchCampaign(c.campaign.ID)
	for _, req := range c.instantly.Requests() {
		in, ok := req.Input.(instantly.AddLeadsInput)
		if !ok {
			continue
		}
		for _, l := range in.Leads {
			if strings.HasSuffix(strings.ToLower(l.Email), "@ironworksgym.com") {
				t.Fatal("an excluded address was pushed to Instantly")
			}
		}
	}
	if estimate := c.estimateImport(c.campaign.ID, ""); estimate["skipped_excluded"].(float64) != 1 {
		t.Errorf("the import estimate reports %v excluded, want 1", estimate["skipped_excluded"])
	}

	c.mustRequest(http.MethodDelete, "/api/v1/exclusions/"+rule.ID, "", http.StatusNoContent)
	c.waitForLeadStatus(c.campaign.ID, "info@ironworksgym.com", campaign.LeadPending)
}

func TestExcludedAddressesAreNeverVerified(t *testing.T) {
	h := verificationHarness(t)
	h.runVerification(runAllSelf)
	barbell := h.verificationFor("info@austinbarbell.com")

	h.exclude(`{"kind":"email_domain","value":"@austinbarbell.com"}`)

	rec := h.request(http.MethodPost, "/api/v1/verification/emails/"+barbell.ID+"/self", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("verifying an excluded address returned %d, want 409: %s", rec.Code, rec.Body.String())
	}
	rec = h.request(http.MethodPost, "/api/v1/verification/emails/"+barbell.ID+"/third-party", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("a paid check of an excluded address returned %d, want 409", rec.Code)
	}

	run := h.runVerification(runAllSelf)
	if run.Total != 2 {
		t.Errorf("a bulk run picked up %d addresses, want the 2 that are not excluded", run.Total)
	}

	rec = h.mustRequest(http.MethodGet, "/api/v1/verification/emails?excluded=true", "", http.StatusOK)
	list := decodeBody[verificationListPayload](t, rec)
	if len(list.Data) != 1 || list.Data[0].Email != "info@austinbarbell.com" {
		t.Fatalf("the excluded address list is %+v", list.Data)
	}
}

func TestPreviewMeasuresARuleWithoutSavingIt(t *testing.T) {
	h, _ := seededHarness(t)

	rec := h.mustRequest(http.MethodPost, "/api/v1/exclusions/preview",
		`{"kind":"domain","value":"ironworksgym.com"}`, http.StatusOK)
	preview := decodeBody[struct {
		Value    string `json:"value"`
		Affected struct {
			Businesses int64 `json:"businesses"`
			Emails     int64 `json:"emails"`
		} `json:"affected"`
		SampleEmails []string `json:"sample_emails"`
		Warning      *string  `json:"warning"`
	}](t, rec)
	if preview.Affected.Businesses != 1 || preview.Affected.Emails != 1 || len(preview.SampleEmails) != 1 {
		t.Errorf("the preview is %+v", preview)
	}
	rec = h.mustRequest(http.MethodGet, "/api/v1/exclusions", "", http.StatusOK)
	if strings.Contains(rec.Body.String(), "ironworksgym") {
		t.Error("the preview saved the rule")
	}

	rec = h.mustRequest(http.MethodPost, "/api/v1/exclusions/preview",
		`{"kind":"email_domain","value":"gmail.com"}`, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"warning":"gmail.com is a shared mailbox provider`) {
		t.Errorf("a free-mail rule carried no warning: %s", rec.Body.String())
	}
	rec = h.request(http.MethodPost, "/api/v1/exclusions/preview", `{"kind":"domain","value":"co.uk"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a public suffix returned %d, want 422", rec.Code)
	}

	rec = h.mustRequest(http.MethodPost, "/api/v1/exclusions/check", `{"email":"Someone@IronWorksGym.com"}`, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"excluded":false`) {
		t.Errorf("an unexcluded address checked as %s", rec.Body.String())
	}
	h.exclude(`{"kind":"domain","value":"ironworksgym.com"}`)
	rec = h.mustRequest(http.MethodPost, "/api/v1/exclusions/check", `{"email":"Someone@mail.IronWorksGym.com"}`, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"excluded":true`) {
		t.Errorf("a subdomain address of an excluded domain checked as %s", rec.Body.String())
	}
}
