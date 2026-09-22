package integration_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/campaign"
)

type metricsPayload struct {
	Sends             int64   `json:"sends"`
	UniqueContacts    int64   `json:"unique_contacts"`
	Opened            int64   `json:"opened"`
	Clicked           int64   `json:"clicked"`
	Replied           int64   `json:"replied"`
	PositiveReplies   int64   `json:"positive_replies"`
	Bounced           int64   `json:"bounced"`
	Unsubscribed      int64   `json:"unsubscribed"`
	OpenRate          float64 `json:"open_rate"`
	ClickRate         float64 `json:"click_rate"`
	ReplyRate         float64 `json:"reply_rate"`
	PositiveReplyRate float64 `json:"positive_reply_rate"`
	BounceRate        float64 `json:"bounce_rate"`
}

func TestAnalyticsAnswerTheQuestionsTheDashboardAsks(t *testing.T) {
	c := newCampaignHarness(t, 2)
	c.configureMailchimp()
	c.campaign = c.launchCampaign(c.campaign.ID)
	leads := c.leads(c.campaign.ID)
	if len(leads) < 2 {
		t.Fatalf("this test needs at least two leads, got %d", len(leads))
	}

	// One lead runs the whole story; another bounces.
	winner, loser := leads[0], leads[1]
	for _, eventType := range []string{
		campaign.InstantlyEmailSent, campaign.InstantlyEmailOpened,
		campaign.InstantlyEmailLinkClicked, campaign.InstantlyReplyReceived, campaign.InstantlyLeadInterested,
	} {
		c.event(t, eventType, winner.Email, nil)
	}
	c.event(t, campaign.InstantlyEmailSent, loser.Email, nil)
	c.event(t, campaign.InstantlyEmailBounced, loser.Email, nil)

	rec := c.mustRequest(http.MethodGet, "/api/v1/campaign-analytics/campaigns/"+c.campaign.ID, "", http.StatusOK)
	var analytics struct {
		Local    metricsPayload `json:"local"`
		Mismatch []any          `json:"mismatch"`
		Funnel   []struct {
			Stage       string `json:"stage"`
			Current     int64  `json:"current"`
			EverReached int64  `json:"ever_reached"`
		} `json:"funnel"`
	}
	decodeInto(t, rec, &analytics)

	m := analytics.Local
	if m.Sends != 2 {
		t.Errorf("sends = %d, want 2", m.Sends)
	}
	if m.Opened != 1 || m.Clicked != 1 || m.Replied != 1 {
		t.Errorf("opened/clicked/replied = %d/%d/%d, want 1/1/1", m.Opened, m.Clicked, m.Replied)
	}
	if m.PositiveReplies != 1 {
		t.Errorf("positive replies = %d, want 1 (the interested lead)", m.PositiveReplies)
	}
	if m.Bounced != 1 {
		t.Errorf("bounced = %d, want 1", m.Bounced)
	}
	if m.ReplyRate <= 0 || m.BounceRate <= 0 || m.PositiveReplyRate <= 0 {
		t.Errorf("the rates were not derived: %+v", m)
	}

	// "Ever reached" survives a later terminal stage: the bounced lead still
	// counts as contacted.
	reached := map[string]int64{}
	for _, step := range analytics.Funnel {
		reached[step.Stage] = step.EverReached
	}
	if reached[string(campaign.StageContacted)] < 2 {
		t.Errorf("only %d contacts were ever contacted, want 2", reached[string(campaign.StageContacted)])
	}
	if reached[string(campaign.StageInterested)] < 1 {
		t.Error("the interested lead is missing from the funnel")
	}

	// Per-variant: the winner's variant carries the reply.
	rec = c.mustRequest(http.MethodGet, "/api/v1/campaign-analytics/campaigns/"+c.campaign.ID+"/variants", "", http.StatusOK)
	var variants []struct {
		VariantID       string `json:"variant_id"`
		VariantName     string `json:"variant_name"`
		Assigned        int64  `json:"assigned"`
		Sends           int64  `json:"sends"`
		Replied         int64  `json:"replied"`
		PositiveReplies int64  `json:"positive_replies"`
	}
	decodeInto(t, rec, &variants)
	if len(variants) == 0 {
		t.Fatal("no per-variant analytics were returned")
	}
	var totalSends, totalPositive int64
	for _, v := range variants {
		totalSends += v.Sends
		totalPositive += v.PositiveReplies
		if v.Assigned == 0 {
			t.Errorf("variant %s reports no assignments", v.VariantName)
		}
	}
	if totalSends != 2 || totalPositive != 1 {
		t.Errorf("per-variant totals are %d sends / %d positive, want 2 / 1", totalSends, totalPositive)
	}

	// Per-component: the subject that earned the positive reply is visible.
	rec = c.mustRequest(http.MethodGet, "/api/v1/campaign-analytics/components?type=subject", "", http.StatusOK)
	var components []struct {
		Name            string `json:"name"`
		Type            string `json:"type"`
		Sends           int64  `json:"sends"`
		PositiveReplies int64  `json:"positive_replies"`
	}
	decodeInto(t, rec, &components)
	if len(components) == 0 {
		t.Fatal("no per-component analytics were returned")
	}
	var componentPositive int64
	for _, comp := range components {
		if comp.Type != campaign.ComponentSubject {
			t.Errorf("the filter returned a %s component", comp.Type)
		}
		componentPositive += comp.PositiveReplies
	}
	if componentPositive != 1 {
		t.Errorf("the subject components report %d positive replies, want 1", componentPositive)
	}

	// Per-account: the mailbox that sent everything is reported with its bounce rate.
	rec = c.mustRequest(http.MethodGet, "/api/v1/campaign-analytics/sending-accounts", "", http.StatusOK)
	var accounts []struct {
		Email string         `json:"email"`
		Local metricsPayload `json:"local"`
	}
	decodeInto(t, rec, &accounts)
	if len(accounts) == 0 {
		t.Fatal("no per-account analytics were returned")
	}
	if accounts[0].Local.Sends != 2 || accounts[0].Local.Bounced != 1 {
		t.Errorf("the account reports %d sends / %d bounces, want 2 / 1", accounts[0].Local.Sends, accounts[0].Local.Bounced)
	}

	// The overview ties it together.
	rec = c.mustRequest(http.MethodGet, "/api/v1/campaign-analytics/overview", "", http.StatusOK)
	var overview struct {
		Local      metricsPayload   `json:"local"`
		Interested int64            `json:"interested"`
		Campaigns  map[string]int64 `json:"campaigns"`
	}
	decodeInto(t, rec, &overview)
	if overview.Local.Sends != 2 {
		t.Errorf("the overview reports %d sends, want 2", overview.Local.Sends)
	}
	if overview.Interested < 1 {
		t.Error("the overview lost the interested lead")
	}
	if overview.Campaigns[campaign.CampaignActive] < 1 {
		t.Error("the overview does not count the active campaign")
	}
}

func TestReconcileBackfillsASendTheWebhookNeverDelivered(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.campaign = c.launchCampaign(c.campaign.ID)
	target := c.leads(c.campaign.ID)[0]

	// Instantly knows about a sent email we were never told about.
	at := c.now.Advance(time.Hour)
	c.instantly.Emails = append(c.instantly.Emails, instantlySentEmail(
		*c.campaign.InstantlyCampaignID, target.Email, "sender@karvon.test", at))

	c.mustRequest(http.MethodPost, "/api/v1/campaigns/"+c.campaign.ID+"/sync", "", http.StatusAccepted)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		detail := c.leadDetail(c.campaign.ID, target.ID)
		if len(detail.Sends) > 0 {
			if detail.Sends[0].Source != campaign.SendSourceReconcile {
				t.Fatalf("the backfilled send came from %s, want reconcile", detail.Sends[0].Source)
			}
			contact := c.contactByEmail(target.Email)
			if contact.LifecycleStage == string(campaign.StageCold) || contact.LifecycleStage == string(campaign.StageQueuedForInstantly) {
				t.Fatalf("the contact is still %s after a reconciled send", contact.LifecycleStage)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the reconcile never backfilled the send")
}

func TestArchivingACampaignPausesItAndStopsItsWork(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.campaign = c.launchCampaign(c.campaign.ID)

	c.mustRequest(http.MethodDelete, "/api/v1/campaigns/"+c.campaign.ID, "", http.StatusOK)

	rec := c.mustRequest(http.MethodGet, "/api/v1/campaigns/"+c.campaign.ID, "", http.StatusOK)
	archived := decodeBody[campaignPayload](t, rec)
	if archived.Status != campaign.CampaignArchived {
		t.Fatalf("the campaign is %s, want archived", archived.Status)
	}
	var paused bool
	for _, req := range c.instantly.Requests() {
		if req.Method == "PauseCampaign" {
			paused = true
		}
	}
	if !paused {
		t.Error("archiving did not pause the campaign at Instantly")
	}
	// An archived campaign is hidden from the default list.
	rec = c.mustRequest(http.MethodGet, "/api/v1/campaigns", "", http.StatusOK)
	var list struct {
		Data []campaignPayload `json:"data"`
	}
	decodeInto(t, rec, &list)
	for _, item := range list.Data {
		if item.ID == c.campaign.ID {
			t.Error("the archived campaign is still in the default list")
		}
	}
	rec = c.mustRequest(http.MethodGet, "/api/v1/campaigns?include_archived=true", "", http.StatusOK)
	decodeInto(t, rec, &list)
	var found bool
	for _, item := range list.Data {
		if item.ID == c.campaign.ID {
			found = true
		}
	}
	if !found {
		t.Error("include_archived=true did not return the archived campaign")
	}
	_ = fmt.Sprint()
}
