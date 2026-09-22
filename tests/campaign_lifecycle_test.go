package integration_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/campaign"
)

// campaignHarness is a seeded scrape plus a configured Instantly, one sending
// account, approved content and a campaign with its leads imported.
type campaignHarness struct {
	*harness
	campaign  campaignPayload
	variantID string
	accountID string
	token     string
	secret    string
}

func newCampaignHarness(t *testing.T, variants int) *campaignHarness {
	t.Helper()

	h, _ := seededHarness(t)
	h.configureInstantly()
	accountID := h.seedSendingAccount("sender@karvon.test")

	variantIDs := make([]string, 0, variants)
	for i := 0; i < variants; i++ {
		name := fmt.Sprintf("Variant %c", 'A'+i)
		variantIDs = append(variantIDs, h.seedContent(name, fmt.Sprintf("Quick question %d", i+1)))
	}
	camp := h.createCampaign("Austin gyms", accountID, variantIDs...)
	h.importLeads(camp.ID, "")

	token, secret := h.registerInstantlyWebhook()
	return &campaignHarness{harness: h, campaign: camp, variantID: variantIDs[0], accountID: accountID, token: token, secret: secret}
}

// event posts one Instantly webhook for a lead and waits for it to be applied.
func (c *campaignHarness) event(t *testing.T, eventType, leadEmail string, extra map[string]any) {
	t.Helper()
	at := c.now.Advance(time.Minute)
	instantlyID := ""
	if c.campaign.InstantlyCampaignID != nil {
		instantlyID = *c.campaign.InstantlyCampaignID
	}
	ack := c.sendInstantlyWebhook(c.token, c.secret, instantlyEvent(eventType, instantlyID, leadEmail, at, extra))
	if !ack.accepted() {
		t.Fatalf("the %s webhook was not accepted", eventType)
	}
	c.waitForEvent(t, leadEmail, eventType)
}

// waitForEvent blocks until the event queue has applied something for a lead.
func (c *campaignHarness) waitForEvent(t *testing.T, leadEmail, eventType string) {
	t.Helper()
	contact := c.contactByEmail(leadEmail)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range c.timeline(contact.ID) {
			if matchesInstantlyEvent(ev.Type, eventType) {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the %s event for %s was never applied", eventType, leadEmail)
}

func matchesInstantlyEvent(local, instantlyType string) bool {
	switch instantlyType {
	case campaign.InstantlyEmailSent:
		return local == campaign.EventSent
	case campaign.InstantlyEmailOpened:
		return local == campaign.EventOpened
	case campaign.InstantlyEmailLinkClicked, campaign.InstantlyLinkClicked:
		return local == campaign.EventClicked
	case campaign.InstantlyReplyReceived:
		return local == campaign.EventReplied
	case campaign.InstantlyLeadInterested:
		return local == campaign.EventInterested
	case campaign.InstantlyLeadNotInterested:
		return local == campaign.EventNotInterested
	case campaign.InstantlyEmailBounced:
		return local == campaign.EventBounced
	case campaign.InstantlyLeadUnsubscribed:
		return local == campaign.EventUnsubscribed
	default:
		return local == campaign.EventNote
	}
}

func TestALaunchedCampaignPushesEveryLeadWithItsOwnRenderedEmail(t *testing.T) {
	c := newCampaignHarness(t, 1)
	launched := c.launchCampaign(c.campaign.ID)

	if launched.Status != campaign.CampaignActive {
		t.Fatalf("the campaign is %s, want active (error: %v)", launched.Status, launched.Error)
	}
	if launched.InstantlyCampaignID == nil {
		t.Fatal("the campaign has no Instantly id")
	}
	c.campaign = launched

	leads := c.leads(c.campaign.ID)
	if len(leads) == 0 {
		t.Fatal("no leads were imported")
	}
	for _, lead := range leads {
		if lead.Status != campaign.LeadActive {
			t.Errorf("lead %s is %s, want active", lead.Email, lead.Status)
		}
		if lead.InstantlyLeadID == nil || *lead.InstantlyLeadID == "" {
			t.Errorf("lead %s has no Instantly id", lead.Email)
		}
		detail := c.leadDetail(c.campaign.ID, lead.ID)
		if len(detail.Assignments) != 1 {
			t.Fatalf("lead %s has %d assignments, want 1", lead.Email, len(detail.Assignments))
		}
		a := detail.Assignments[0]
		if a.LockedAt == nil {
			t.Errorf("the assignment for %s was not locked at push time", lead.Email)
		}
		if a.RenderedSubject == "" || a.RenderedBody == "" {
			t.Errorf("the assignment for %s has no rendered snapshot", lead.Email)
		}
		if strings.Contains(a.RenderedBody, "{{") {
			t.Errorf("the body for %s still contains a placeholder: %s", lead.Email, a.RenderedBody)
		}
	}

	// Instantly received the rendered copy as custom variables, one per step.
	var seen int
	for _, req := range c.instantly.Requests() {
		if req.Method != "AddLeads" {
			continue
		}
		seen++
	}
	if seen == 0 {
		t.Fatal("no leads were pushed to Instantly")
	}
}

func TestASuppressedContactIsNeverPushedToInstantlyAgain(t *testing.T) {
	c := newCampaignHarness(t, 1)
	leads := c.leads(c.campaign.ID)
	if len(leads) < 2 {
		t.Fatalf("this test needs at least two leads, got %d", len(leads))
	}
	target := leads[0]

	// Suppressed before the campaign ever launches.
	contact := c.contactByEmail(target.Email)
	c.mustRequest(http.MethodPost, "/api/v1/contacts/"+contact.ID+"/suppress",
		`{"reason":"do_not_contact","note":"asked us not to contact them"}`, http.StatusOK)

	launched := c.launchCampaign(c.campaign.ID)
	if launched.Status != campaign.CampaignActive {
		t.Fatalf("the campaign is %s, want active", launched.Status)
	}

	for _, req := range c.instantly.Requests() {
		if req.Method != "AddLeads" {
			continue
		}
		if strings.Contains(fmt.Sprint(req.Input), target.Email) {
			t.Fatalf("the suppressed contact %s was pushed to Instantly", target.Email)
		}
	}
	after := c.leadByEmail(c.campaign.ID, target.Email)
	if after.Status != campaign.LeadSuppressed {
		t.Errorf("the suppressed contact's lead is %s, want suppressed", after.Status)
	}
	if after.InstantlyLeadID != nil && *after.InstantlyLeadID != "" {
		t.Error("the suppressed contact reached Instantly")
	}
	// Everyone else still went out.
	for _, lead := range c.leads(c.campaign.ID) {
		if lead.Email == target.Email {
			continue
		}
		if lead.Status != campaign.LeadActive {
			t.Errorf("lead %s is %s, want active", lead.Email, lead.Status)
		}
	}
}

func TestAnUnsubscribedContactCannotBecomeActiveAgain(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.campaign = c.launchCampaign(c.campaign.ID)
	target := c.leads(c.campaign.ID)[0]

	c.event(t, campaign.InstantlyEmailSent, target.Email, nil)
	c.event(t, campaign.InstantlyLeadUnsubscribed, target.Email, nil)

	contact := c.contactByEmail(target.Email)
	if contact.LifecycleStage != string(campaign.StageUnsubscribed) {
		t.Fatalf("the contact is %s, want unsubscribed", contact.LifecycleStage)
	}
	detail := c.contactDetail(contact.ID)
	if len(detail.Suppressions) == 0 {
		t.Fatal("no suppression was recorded for the unsubscribe")
	}
	suppressionID, _ := detail.Suppressions[0]["id"].(string)

	// The unsubscribe is permanent: it cannot be lifted, and the lead is stopped.
	c.mustRequest(http.MethodPost,
		"/api/v1/contacts/"+contact.ID+"/suppressions/"+suppressionID+"/lift",
		`{"note":"they changed their mind"}`, http.StatusConflict)

	lead := c.leadByEmail(c.campaign.ID, target.Email)
	if lead.Status != campaign.LeadUnsubscribed && lead.Status != campaign.LeadSuppressed {
		t.Errorf("the lead is %s, want unsubscribed or suppressed", lead.Status)
	}

	// And a second campaign refuses to take them.
	second := c.createCampaign("Second attempt", c.accountID, c.variantID)
	c.importLeads(second.ID, "")
	for _, lead := range c.leads(second.ID) {
		if strings.EqualFold(lead.Email, target.Email) {
			t.Fatal("the unsubscribed contact was imported into a new campaign")
		}
	}
}

func TestAnInterestedLeadDoesNotBecomeAMailchimpSubscriber(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.configureMailchimp()
	audienceID := c.seedAudience(t)
	c.campaign = c.launchCampaign(c.campaign.ID)
	target := c.leads(c.campaign.ID)[0]

	c.event(t, campaign.InstantlyEmailSent, target.Email, nil)
	c.event(t, campaign.InstantlyReplyReceived, target.Email, map[string]any{
		"reply_text_snippet": "This sounds interesting, tell me more",
	})
	c.event(t, campaign.InstantlyLeadInterested, target.Email, nil)

	contact := c.contactByEmail(target.Email)
	if contact.LifecycleStage != string(campaign.StageInterested) {
		t.Fatalf("the contact is %s, want interested", contact.LifecycleStage)
	}
	detail := c.contactDetail(contact.ID)
	if len(detail.Subscriptions) != 0 {
		t.Fatal("an interested lead was given a newsletter subscription")
	}
	for _, req := range c.mailchimp.Requests() {
		if req.Method == "UpsertMember" {
			t.Fatal("an interested lead was pushed to Mailchimp")
		}
	}

	// Pushing them explicitly is refused too: interest is not consent.
	rec := c.mustRequest(http.MethodPost, "/api/v1/newsletter/push",
		fmt.Sprintf(`{"contact_ids":[%q],"audience_id":%q}`, contact.ID, audienceID), http.StatusOK)
	result := decodeBody[pushResultPayload](t, rec)
	if len(result.Queued) != 0 {
		t.Fatal("a contact with no consent was queued for Mailchimp")
	}
	if len(result.Rejected) != 1 || result.Rejected[0].Reason != "no_consent" {
		t.Fatalf("the push was rejected as %+v, want no_consent", result.Rejected)
	}
}

func TestTheTimelineIsChronologicalAndLogical(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.configureMailchimp()
	c.campaign = c.launchCampaign(c.campaign.ID)
	target := c.leads(c.campaign.ID)[0]

	for _, eventType := range []string{
		campaign.InstantlyEmailSent,
		campaign.InstantlyEmailOpened,
		campaign.InstantlyEmailLinkClicked,
		campaign.InstantlyReplyReceived,
		campaign.InstantlyLeadInterested,
	} {
		c.event(t, eventType, target.Email, nil)
	}

	contact := c.contactByEmail(target.Email)
	c.mustRequest(http.MethodPost, "/api/v1/contacts/"+contact.ID+"/consents",
		`{"source":"explicit_reply","evidence":"yes, please add me to the newsletter","captured_by":"tester"}`,
		http.StatusCreated)

	events := c.timeline(contact.ID)
	var order []string
	var last time.Time
	for _, ev := range events {
		at, err := time.Parse(time.RFC3339Nano, ev.OccurredAt)
		if err != nil {
			t.Fatalf("event %s has an unparseable timestamp %q", ev.Type, ev.OccurredAt)
		}
		if at.Before(last) {
			t.Fatalf("the timeline is out of order: %s at %s follows %s", ev.Type, ev.OccurredAt, last)
		}
		last = at
		order = append(order, ev.Type)
	}

	// The story reads in the order it happened.
	want := []string{
		campaign.EventImported, campaign.EventPushed, campaign.EventSent, campaign.EventOpened,
		campaign.EventClicked, campaign.EventReplied, campaign.EventInterested, campaign.EventConsentCaptured,
	}
	if !isSubsequence(want, order) {
		t.Fatalf("the timeline %v does not contain %v in order", order, want)
	}
	// Reaching a newsletter stage requires the consent event to come first.
	consentAt, eligibleAt := -1, -1
	for i, typ := range order {
		if typ == campaign.EventConsentCaptured && consentAt < 0 {
			consentAt = i
		}
		if typ == campaign.EventNewsletterEligible && eligibleAt < 0 {
			eligibleAt = i
		}
	}
	if eligibleAt >= 0 && eligibleAt < consentAt {
		t.Fatal("the contact became newsletter-eligible before consent was captured")
	}
}

// isSubsequence reports whether want appears in order inside got.
func isSubsequence(want, got []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// count reads one integer out of an import result or estimate.
func count(t *testing.T, result map[string]any, field string) int {
	t.Helper()
	value, ok := result[field].(float64)
	if !ok {
		t.Fatalf("the result has no %s: %v", field, result)
	}
	return int(value)
}

// The estimate is the import's own arithmetic run without the writes. An operator
// confirms a number before turning scraped addresses into contacts, so the number
// they confirm has to be the one the import then produces — a bare match count
// would leave "would be imported" at zero and the button behind it dead.
func TestTheImportEstimateMatchesWhatTheImportDoes(t *testing.T) {
	h, _ := seededHarness(t)
	h.configureInstantly()
	accountID := h.seedSendingAccount("sender@karvon.test")
	variantID := h.seedContent("Variant A", "Quick question")
	camp := h.createCampaign("Austin gyms", accountID, variantID)

	estimate := h.estimateImport(camp.ID, "")
	if count(t, estimate, "matched") == 0 {
		t.Fatal("the seeded scrape matched nothing, so this test proves nothing")
	}
	if count(t, estimate, "imported") == 0 {
		t.Fatalf("the estimate matched %d addresses but would import none: %v",
			count(t, estimate, "matched"), estimate)
	}

	imported := h.importLeads(camp.ID, "")
	for _, field := range []string{"matched", "imported", "skipped_suppressed", "skipped_existing", "skipped_invalid"} {
		if got, want := count(t, imported, field), count(t, estimate, field); got != want {
			t.Errorf("the import reported %s=%d, the estimate promised %d", field, got, want)
		}
	}
	if got, want := len(h.leads(camp.ID)), count(t, imported, "imported"); got != want {
		t.Errorf("the campaign holds %d leads, the import reported %d", got, want)
	}

	// Run again and the same addresses are all already here.
	second := h.estimateImport(camp.ID, "")
	if got := count(t, second, "imported"); got != 0 {
		t.Errorf("a repeat estimate would import %d, want 0", got)
	}
	if got, want := count(t, second, "skipped_existing"), count(t, imported, "imported"); got != want {
		t.Errorf("a repeat estimate skips %d as existing, want %d", got, want)
	}
}

// A suppressed contact is counted as suppressed, not offered as importable.
func TestTheImportEstimateCountsASuppressedContactAsSkipped(t *testing.T) {
	c := newCampaignHarness(t, 1)
	target := c.leads(c.campaign.ID)[0]
	contact := c.contactByEmail(target.Email)
	c.mustRequest(http.MethodPost, "/api/v1/contacts/"+contact.ID+"/suppress",
		`{"reason":"do_not_contact","note":"asked us not to contact them"}`, http.StatusOK)

	second := c.createCampaign("Second attempt", c.accountID, c.variantID)
	estimate := c.estimateImport(second.ID, "")
	if got := count(t, estimate, "skipped_suppressed"); got != 1 {
		t.Errorf("the estimate skipped %d suppressed addresses, want 1", got)
	}

	imported := c.importLeads(second.ID, "")
	if got, want := count(t, imported, "skipped_suppressed"), count(t, estimate, "skipped_suppressed"); got != want {
		t.Errorf("the import skipped %d suppressed, the estimate promised %d", got, want)
	}
	if got, want := count(t, imported, "imported"), count(t, estimate, "imported"); got != want {
		t.Errorf("the import brought in %d, the estimate promised %d", got, want)
	}
}
