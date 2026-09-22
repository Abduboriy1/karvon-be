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

// interestedContact takes one lead all the way to "interested", which is where
// every newsletter decision starts.
func (c *campaignHarness) interestedContact(t *testing.T) contactPayload {
	t.Helper()
	target := c.leads(c.campaign.ID)[0]
	c.event(t, campaign.InstantlyEmailSent, target.Email, nil)
	c.event(t, campaign.InstantlyReplyReceived, target.Email, nil)
	c.event(t, campaign.InstantlyLeadInterested, target.Email, nil)
	return c.contactByEmail(target.Email)
}

// waitForSubscription polls until a contact's subscription settles.
func (c *campaignHarness) waitForSubscription(t *testing.T, contactID string, wanted ...string) subscriptionPayload {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last subscriptionPayload
	for time.Now().Before(deadline) {
		rec := c.mustRequest(http.MethodGet, "/api/v1/newsletter/subscriptions?per_page=100", "", http.StatusOK)
		for _, sub := range decodeBody[subscriptionListPayload](t, rec).Data {
			if sub.ContactID != contactID {
				continue
			}
			last = sub
			for _, want := range wanted {
				if sub.Status == want {
					return sub
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the subscription for %s never reached %v, last state: %+v", contactID, wanted, last)
	return last
}

func TestConsentMakesAContactEligibleAndPushesThemAsPending(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.configureMailchimp()
	c.campaign = c.launchCampaign(c.campaign.ID)
	audienceID := c.seedAudience(t)
	contact := c.interestedContact(t)

	// Consent, captured with its evidence, is the only door into the newsletter.
	c.mustRequest(http.MethodPost, "/api/v1/contacts/"+contact.ID+"/consents",
		`{"source":"explicit_reply","evidence":"yes, sign me up for the monthly note","captured_by":"tester"}`,
		http.StatusCreated)

	after := c.contactByEmail(contact.Email)
	if after.LifecycleStage != string(campaign.StageNewsletterEligible) {
		t.Fatalf("the contact is %s, want newsletter_eligible", after.LifecycleStage)
	}

	rec := c.mustRequest(http.MethodPost, "/api/v1/newsletter/push",
		fmt.Sprintf(`{"contact_ids":[%q],"audience_id":%q}`, contact.ID, audienceID), http.StatusOK)
	result := decodeBody[pushResultPayload](t, rec)
	if len(result.Queued) != 1 {
		t.Fatalf("the push queued %v and rejected %+v", result.Queued, result.Rejected)
	}

	sub := c.waitForSubscription(t, contact.ID, campaign.SubPending)
	if sub.Status != campaign.SubPending {
		t.Fatalf("the subscription is %s, want pending", sub.Status)
	}
	// Double opt-in by default: Mailchimp was asked for `pending`, not `subscribed`.
	var upserts int
	for _, req := range c.mailchimp.Requests() {
		if req.Method != "UpsertMember" {
			continue
		}
		upserts++
		payload := fmt.Sprint(req.Input)
		if strings.Contains(payload, "subscribed") && !strings.Contains(payload, "unsubscribed") {
			t.Fatalf("Mailchimp was asked to subscribe without single opt-in: %s", payload)
		}
	}
	if upserts == 0 {
		t.Fatal("Mailchimp was never called")
	}
	stage := c.contactByEmail(contact.Email).LifecycleStage
	if stage != string(campaign.StageMailchimpPending) {
		t.Fatalf("the contact is %s, want mailchimp_pending", stage)
	}
}

func TestSubscribedIsOnlyPushedWithConsentAndSingleOptIn(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.configureMailchimp()
	c.campaign = c.launchCampaign(c.campaign.ID)
	audienceID := c.seedAudience(t)
	contact := c.interestedContact(t)

	// The operator turns single opt-in on for this audience, deliberately.
	c.mustRequest(http.MethodPatch, "/api/v1/newsletter/audiences/"+audienceID,
		`{"allow_single_opt_in":true}`, http.StatusOK)
	c.mustRequest(http.MethodPost, "/api/v1/contacts/"+contact.ID+"/consents",
		`{"source":"form","evidence":"ticked the newsletter box on the demo form","captured_by":"tester"}`,
		http.StatusCreated)
	c.mustRequest(http.MethodPost, "/api/v1/newsletter/push",
		fmt.Sprintf(`{"contact_ids":[%q],"audience_id":%q}`, contact.ID, audienceID), http.StatusOK)

	sub := c.waitForSubscription(t, contact.ID, campaign.SubSubscribed, campaign.SubPending)
	if sub.Status != campaign.SubSubscribed {
		t.Fatalf("the subscription is %s, want subscribed", sub.Status)
	}
	stage := c.contactByEmail(contact.Email).LifecycleStage
	if stage != string(campaign.StageMailchimpSubscribed) {
		t.Fatalf("the contact is %s, want mailchimp_subscribed", stage)
	}
}

func TestRevokingConsentUnsubscribesTheContact(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.configureMailchimp()
	c.campaign = c.launchCampaign(c.campaign.ID)
	audienceID := c.seedAudience(t)
	contact := c.interestedContact(t)

	rec := c.mustRequest(http.MethodPost, "/api/v1/contacts/"+contact.ID+"/consents",
		`{"source":"verbal_confirmed","evidence":"said yes on the call","captured_by":"tester"}`, http.StatusCreated)
	var consent struct {
		ID string `json:"id"`
	}
	decodeInto(t, rec, &consent)
	c.mustRequest(http.MethodPost, "/api/v1/newsletter/push",
		fmt.Sprintf(`{"contact_ids":[%q],"audience_id":%q}`, contact.ID, audienceID), http.StatusOK)
	c.waitForSubscription(t, contact.ID, campaign.SubPending, campaign.SubSubscribed)

	c.mustRequest(http.MethodPost, "/api/v1/contacts/"+contact.ID+"/consents/"+consent.ID+"/revoke",
		`{"reason":"asked to be taken off the list"}`, http.StatusOK)

	sub := c.waitForSubscription(t, contact.ID, campaign.SubUnsubscribed)
	if sub.Status != campaign.SubUnsubscribed {
		t.Fatalf("the subscription is %s, want unsubscribed", sub.Status)
	}
	// Consent gone, so the contact drops out of the newsletter stages.
	stage := c.contactByEmail(contact.Email).LifecycleStage
	if stage == string(campaign.StageMailchimpSubscribed) || stage == string(campaign.StageNewsletterEligible) {
		t.Fatalf("the contact is still %s after revoking consent", stage)
	}
}

func TestAComplianceBlockedAddressIsNotRetriedForEver(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.configureMailchimp()
	c.campaign = c.launchCampaign(c.campaign.ID)
	audienceID := c.seedAudience(t)
	contact := c.interestedContact(t)

	// Mailchimp refuses a member who unsubscribed there before: only they can
	// undo that, so we record it and stop.
	c.mailchimp.ComplianceEmails[strings.ToLower(contact.Email)] = true

	c.mustRequest(http.MethodPost, "/api/v1/contacts/"+contact.ID+"/consents",
		`{"source":"explicit_reply","evidence":"asked for the newsletter","captured_by":"tester"}`, http.StatusCreated)
	c.mustRequest(http.MethodPost, "/api/v1/newsletter/push",
		fmt.Sprintf(`{"contact_ids":[%q],"audience_id":%q}`, contact.ID, audienceID), http.StatusOK)

	sub := c.waitForSubscription(t, contact.ID, campaign.SubComplianceBlocked)
	if sub.Status != campaign.SubComplianceBlocked {
		t.Fatalf("the subscription is %s, want compliance_blocked", sub.Status)
	}
	// One attempt, not a retry storm.
	var upserts int
	for _, req := range c.mailchimp.Requests() {
		if req.Method == "UpsertMember" {
			upserts++
		}
	}
	if upserts > 2 {
		t.Fatalf("Mailchimp was called %d times for a compliance-blocked address", upserts)
	}
	// And pushing again is refused up front.
	rec := c.mustRequest(http.MethodPost, "/api/v1/newsletter/push",
		fmt.Sprintf(`{"contact_ids":[%q],"audience_id":%q}`, contact.ID, audienceID), http.StatusOK)
	result := decodeBody[pushResultPayload](t, rec)
	if len(result.Queued) != 0 {
		t.Fatal("a compliance-blocked contact was queued again")
	}
}

func TestAFailedNewsletterPushCanBeRetried(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.configureMailchimp()
	c.campaign = c.launchCampaign(c.campaign.ID)
	audienceID := c.seedAudience(t)
	contact := c.interestedContact(t)

	// Mailchimp rejects the key, which is not something a retry can fix: the push
	// is marked failed straight away rather than burning attempts.
	c.mailchimp.Fail["UpsertMember"] = []error{provider.ErrAuth}
	c.mustRequest(http.MethodPost, "/api/v1/contacts/"+contact.ID+"/consents",
		`{"source":"explicit_reply","evidence":"asked for the newsletter","captured_by":"tester"}`, http.StatusCreated)
	c.mustRequest(http.MethodPost, "/api/v1/newsletter/push",
		fmt.Sprintf(`{"contact_ids":[%q],"audience_id":%q}`, contact.ID, audienceID), http.StatusOK)

	// It ends up failed rather than stuck half-applied.
	deadline := time.Now().Add(30 * time.Second)
	var sub subscriptionPayload
	for time.Now().Before(deadline) {
		rec := c.mustRequest(http.MethodGet, "/api/v1/newsletter/subscriptions?per_page=100", "", http.StatusOK)
		for _, s := range decodeBody[subscriptionListPayload](t, rec).Data {
			if s.ContactID == contact.ID {
				sub = s
			}
		}
		if sub.SyncStatus == campaign.SyncFailed {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if sub.SyncStatus != campaign.SyncFailed {
		t.Fatalf("the subscription is %s/%s, want a failed sync", sub.Status, sub.SyncStatus)
	}

	// The key is fixed, and retrying reuses the same subscription rather than
	// creating a second one.
	c.mustRequest(http.MethodPost, "/api/v1/newsletter/subscriptions/"+sub.ID+"/retry", "", http.StatusOK)
	settled := c.waitForSubscription(t, contact.ID, campaign.SubPending, campaign.SubSubscribed)
	if settled.ID != sub.ID {
		t.Fatalf("the retry created a second subscription %s", settled.ID)
	}
}
