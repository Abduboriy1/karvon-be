package integration_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/webhook"
)

func TestADuplicateProviderEventIsProcessedOnce(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.campaign = c.launchCampaign(c.campaign.ID)
	target := c.leads(c.campaign.ID)[0]

	at := c.now.Advance(time.Minute)
	payload := instantlyEvent(campaign.InstantlyEmailSent, *c.campaign.InstantlyCampaignID, target.Email, at, nil)

	first := c.sendInstantlyWebhook(c.token, c.secret, payload)
	if first.Duplicate {
		t.Fatal("the first delivery was reported as a duplicate")
	}
	c.waitForEvent(t, target.Email, campaign.InstantlyEmailSent)

	// The very same payload again: stored once, applied once.
	second := c.sendInstantlyWebhook(c.token, c.secret, payload)
	if !second.Duplicate {
		t.Fatal("a redelivered webhook was not recognised as a duplicate")
	}

	contact := c.contactByEmail(target.Email)
	var sent int
	for _, ev := range c.timeline(contact.ID) {
		if ev.Type == campaign.EventSent {
			sent++
		}
	}
	if sent != 1 {
		t.Fatalf("the timeline has %d sent events, want 1", sent)
	}
	detail := c.leadDetail(c.campaign.ID, target.ID)
	if len(detail.Sends) != 1 {
		t.Fatalf("the lead has %d sends, want 1", len(detail.Sends))
	}
}

func TestAWebhookWithAWrongSecretIsRejected(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.campaign = c.launchCampaign(c.campaign.ID)
	target := c.leads(c.campaign.ID)[0]

	payload := instantlyEvent(campaign.InstantlyEmailSent, *c.campaign.InstantlyCampaignID, target.Email, c.now.Now(), nil)
	c.postWebhook("/api/v1/webhooks/instantly/"+c.token, "not-the-secret", payload, http.StatusUnauthorized)
	c.postWebhook("/api/v1/webhooks/instantly/"+c.token, "", payload, http.StatusUnauthorized)
	// An unknown token is a 404, so it cannot be told apart from a wrong path.
	c.postWebhook("/api/v1/webhooks/instantly/deadbeef", c.secret, payload, http.StatusNotFound)

	contact := c.contactByEmail(target.Email)
	for _, ev := range c.timeline(contact.ID) {
		if ev.Type == campaign.EventSent {
			t.Fatal("a rejected webhook still reached the timeline")
		}
	}
}

func TestEventsStayAttributedToTheLockedAssignment(t *testing.T) {
	c := newCampaignHarness(t, 2)
	c.campaign = c.launchCampaign(c.campaign.ID)
	target := c.leads(c.campaign.ID)[0]

	before := c.leadDetail(c.campaign.ID, target.ID)
	if len(before.Assignments) != 1 {
		t.Fatalf("the lead has %d assignments, want 1", len(before.Assignments))
	}
	assigned := before.Assignments[0]

	// Re-weight the campaign entirely onto the other variant, which is exactly
	// the moment attribution could silently drift.
	variants := c.campaignVariants(t, c.campaign.ID)
	if len(variants) != 2 {
		t.Fatalf("the campaign has %d variants, want 2", len(variants))
	}
	var items []string
	for _, v := range variants {
		weight := 0
		if v != assigned.VariantID {
			weight = 100
		}
		items = append(items, fmt.Sprintf(`{"variant_id":%q,"step":1,"weight":%d}`, v, weight))
	}
	c.mustRequest(http.MethodPut, "/api/v1/campaigns/"+c.campaign.ID+"/variants",
		`{"items":[`+strings.Join(items, ",")+`]}`, http.StatusOK)

	c.event(t, campaign.InstantlyEmailSent, target.Email, nil)
	c.event(t, campaign.InstantlyReplyReceived, target.Email, nil)

	after := c.leadDetail(c.campaign.ID, target.ID)
	if len(after.Assignments) != 1 {
		t.Fatalf("the lead now has %d assignments, want 1", len(after.Assignments))
	}
	got := after.Assignments[0]
	if got.VariantID != assigned.VariantID {
		t.Fatalf("the assignment moved from %s to %s after re-weighting", assigned.VariantID, got.VariantID)
	}
	if got.RenderedSubject != assigned.RenderedSubject || got.RenderedBody != assigned.RenderedBody {
		t.Fatal("the rendered snapshot changed after re-weighting")
	}
	if got.WeightsVersion != assigned.WeightsVersion {
		t.Fatalf("the assignment's weights version moved from %d to %d", assigned.WeightsVersion, got.WeightsVersion)
	}
	if len(after.Sends) == 0 {
		t.Fatal("no send was recorded")
	}
	send := after.Sends[0]
	if send.VariantID == nil || *send.VariantID != assigned.VariantID {
		t.Fatalf("the send points at %v, want the assigned variant %s", send.VariantID, assigned.VariantID)
	}
	if send.AssignmentID == nil || *send.AssignmentID != assigned.ID {
		t.Fatalf("the send points at assignment %v, want %s", send.AssignmentID, assigned.ID)
	}
	if send.RepliedAt == nil {
		t.Fatal("the reply was not recorded on the send")
	}
}

func TestProviderIdsResolveToLocalObjects(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.campaign = c.launchCampaign(c.campaign.ID)
	target := c.leads(c.campaign.ID)[0]

	// An event for a campaign we do not know is kept but not applied.
	at := c.now.Advance(time.Minute)
	ack := c.sendInstantlyWebhook(c.token, c.secret,
		instantlyEvent(campaign.InstantlyEmailSent, "ic_not_ours", target.Email, at, nil))
	if !ack.accepted() {
		t.Fatal("an unmatched event was not accepted")
	}
	// And one for an address we do not know.
	ack = c.sendInstantlyWebhook(c.token, c.secret,
		instantlyEvent(campaign.InstantlyEmailSent, *c.campaign.InstantlyCampaignID, "stranger@example.test", c.now.Advance(time.Minute), nil))
	if !ack.accepted() {
		t.Fatal("an event for an unknown contact was not accepted")
	}

	// The real one resolves all the way down to the lead.
	c.event(t, campaign.InstantlyEmailSent, target.Email, nil)
	detail := c.leadDetail(c.campaign.ID, target.ID)
	if len(detail.Sends) != 1 {
		t.Fatalf("the lead has %d sends, want 1", len(detail.Sends))
	}
	if detail.Sends[0].Source != campaign.SendSourceWebhook {
		t.Errorf("the send came from %s, want webhook", detail.Sends[0].Source)
	}

	// The unmatched deliveries are visible, with a reason, and never applied.
	rec := c.mustRequest(http.MethodGet, "/api/v1/integrations/events?provider=instantly&per_page=50", "", http.StatusOK)
	var events struct {
		Data []struct {
			EventType   string  `json:"event_type"`
			ProcessedAt *string `json:"processed_at"`
			Error       *string `json:"error"`
		} `json:"data"`
	}
	decodeInto(t, rec, &events)
	var unmatched int
	for _, ev := range events.Data {
		if ev.Error != nil && strings.Contains(*ev.Error, "unmatched") {
			unmatched++
			if ev.ProcessedAt == nil {
				t.Error("an unmatched event was left unprocessed and will be retried for ever")
			}
		}
	}
	if unmatched != 2 {
		t.Fatalf("%d events were recorded as unmatched, want 2", unmatched)
	}
}

func TestAHardBounceSuppressesTheContactEverywhere(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.campaign = c.launchCampaign(c.campaign.ID)
	target := c.leads(c.campaign.ID)[0]

	c.event(t, campaign.InstantlyEmailSent, target.Email, nil)
	c.event(t, campaign.InstantlyEmailBounced, target.Email, nil)

	contact := c.contactByEmail(target.Email)
	if contact.LifecycleStage != string(campaign.StageBounced) {
		t.Fatalf("the contact is %s, want bounced", contact.LifecycleStage)
	}
	if contact.SuppressedAt == nil {
		t.Fatal("the bounce did not suppress the contact")
	}
	lead := c.leadByEmail(c.campaign.ID, target.Email)
	if lead.Status != campaign.LeadBounced && lead.Status != campaign.LeadSuppressed {
		t.Errorf("the lead is %s, want bounced or suppressed", lead.Status)
	}

	// Instantly is told to drop the lead, so it cannot be mailed again.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, req := range c.instantly.Requests() {
			if req.Method == "DeleteLead" {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the bounced lead was never removed from Instantly")
}

func TestTheMailchimpWebhookRejectsATamperedBody(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.configureMailchimp()

	audienceID := c.seedAudience(t)
	c.mustRequest(http.MethodPost, "/api/v1/newsletter/audiences/"+audienceID+"/webhook", "", http.StatusOK)

	token, secret := c.mailchimpWebhookCredentials(t, audienceID)
	form := "type=unsubscribe&fired_at=2026-03-02+10%3A00%3A00&data%5Bemail%5D=someone%40example.test&data%5Blist_id%5D=list-1"

	// Mailchimp validates the URL with a GET before any secret exists.
	req := newRequest(http.MethodGet, "/api/v1/webhooks/mailchimp/"+token, "")
	rec := newRecorder()
	c.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the Mailchimp URL validation answered %d, want 200", rec.Code)
	}

	signature := webhook.SignMailchimp([]byte(form), secret, c.now.Now())
	post := func(body, sig string, want int) {
		t.Helper()
		req := newRequest(http.MethodPost, "/api/v1/webhooks/mailchimp/"+token, body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if sig != "" {
			req.Header.Set(webhook.MailchimpSignatureHeader, sig)
		}
		rec := newRecorder()
		c.handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("POST with signature %q = %d, want %d: %s", sig, rec.Code, want, rec.Body.String())
		}
	}
	post(form, "", http.StatusUnauthorized)
	post(form+"&data%5Bextra%5D=1", signature, http.StatusUnauthorized)
	post(form, webhook.SignMailchimp([]byte(form), "the-wrong-secret", c.now.Now()), http.StatusUnauthorized)
	post(form, webhook.SignMailchimp([]byte(form), secret, c.now.Now().Add(-time.Hour)), http.StatusUnauthorized)
	post(form, signature, http.StatusOK)
}
