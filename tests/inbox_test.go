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

type inboxEmailPayload struct {
	ID             string  `json:"id"`
	ThreadID       *string `json:"thread_id"`
	LeadEmail      *string `json:"lead_email"`
	Subject        *string `json:"subject"`
	ContentPreview *string `json:"content_preview"`
	IsUnread       bool    `json:"is_unread"`
	CampaignName   *string `json:"campaign_name"`
	ContactID      *string `json:"contact_id"`
	LeadOutcome    *string `json:"lead_outcome"`
}

type inboxListPayload struct {
	Data        []inboxEmailPayload `json:"data"`
	UnreadTotal int64               `json:"unread_total"`
	Meta        struct {
		Total int64 `json:"total"`
	} `json:"meta"`
}

type inboxThreadPayload struct {
	ThreadID string `json:"thread_id"`
	Stale    bool   `json:"stale"`
	Emails   []struct {
		ID        string  `json:"id"`
		Direction string  `json:"direction"`
		BodyText  *string `json:"body_text"`
	} `json:"emails"`
}

type outreachListPayload struct {
	Data []struct {
		Email            string  `json:"email"`
		Outcome          string  `json:"outcome"`
		InterestStatus   *int    `json:"interest_status"`
		InterestLabel    *string `json:"interest_label"`
		LastReplyPreview *string `json:"last_reply_preview"`
		EmailsSent       int64   `json:"emails_sent"`
	} `json:"data"`
	Meta struct {
		Total int64 `json:"total"`
	} `json:"meta"`
	Outcomes map[string]int64 `json:"outcomes"`
}

// A reply lands in the inbox as soon as its webhook arrives, opening the thread
// brings in our side of it, and the lead's answer shows up in the outcomes table
// and its export.
func TestARepliedLeadReachesTheInboxAndTheOutcomesTable(t *testing.T) {
	c := newCampaignHarness(t, 1)
	c.campaign = c.launchCampaign(c.campaign.ID)
	target := c.leads(c.campaign.ID)[0]
	instantlyID := *c.campaign.InstantlyCampaignID

	c.event(t, campaign.InstantlyEmailSent, target.Email, nil)

	sentAt := c.now.Advance(time.Minute)
	repliedAt := c.now.Advance(time.Hour)
	unread := 1
	thread := "thread-" + target.Email
	c.instantly.AddEmails(
		instantly.Email{
			ID: "sent-1", ThreadID: thread, CampaignID: instantlyID, LeadEmail: target.Email,
			EAccount: "sender@karvon.test", Subject: "Quick question", Body: instantly.EmailBody{Text: "Hi there"},
			UEType: instantly.EmailTypeSentFromCampaign, TimestampEmail: sentAt, TimestampCreated: sentAt,
		},
		instantly.Email{
			ID: "reply-1", ThreadID: thread, CampaignID: instantlyID, LeadEmail: target.Email,
			EAccount: "sender@karvon.test", FromAddress: target.Email, Subject: "Re: Quick question",
			Body:     instantly.EmailBody{Text: "Thanks, but we are all set.", HTML: "<p>Thanks, but we are all set.</p>"},
			IsUnread: &unread, UEType: instantly.EmailTypeReceived, TimestampEmail: repliedAt, TimestampCreated: repliedAt,
		},
	)
	c.event(t, campaign.InstantlyReplyReceived, target.Email, nil)

	// The reply webhook queued an inbox sync; the email appears without waiting
	// for the periodic pass.
	var inbox inboxListPayload
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		inbox = decodeBody[inboxListPayload](t, c.mustRequest(http.MethodGet, "/api/v1/inbox", "", http.StatusOK))
		if len(inbox.Data) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(inbox.Data) != 1 {
		t.Fatalf("the inbox has %d emails, want the one reply (sent emails are not listed)", len(inbox.Data))
	}
	got := inbox.Data[0]
	if got.ID != "reply-1" || got.ContactID == nil || got.CampaignName == nil || *got.CampaignName != "Austin gyms" {
		t.Fatalf("the reply was not linked to its contact and campaign: %+v", got)
	}
	if got.ContentPreview == nil || !strings.Contains(*got.ContentPreview, "all set") {
		t.Fatalf("the reply has no preview: %+v", got)
	}
	if !got.IsUnread || inbox.UnreadTotal != 1 {
		t.Fatalf("the reply should be unread (unread total %d)", inbox.UnreadTotal)
	}
	if got.LeadOutcome == nil || *got.LeadOutcome != campaign.OutcomeReplied {
		t.Fatalf("the lead's outcome is %v, want replied", got.LeadOutcome)
	}

	// Opening the thread fetches the sent side, which the sync does not mirror.
	threadView := decodeBody[inboxThreadPayload](t, c.mustRequest(http.MethodGet, "/api/v1/inbox/threads/"+thread, "", http.StatusOK))
	if threadView.Stale || len(threadView.Emails) != 2 {
		t.Fatalf("the thread has %d emails (stale %v), want both sides", len(threadView.Emails), threadView.Stale)
	}
	if threadView.Emails[0].Direction != campaign.InboxSent || threadView.Emails[1].Direction != campaign.InboxReceived {
		t.Fatalf("the thread is not oldest first: %+v", threadView.Emails)
	}
	if b := threadView.Emails[1].BodyText; b == nil || *b != "Thanks, but we are all set." {
		t.Fatalf("the reply's body was not kept: %v", b)
	}

	c.mustRequest(http.MethodPost, "/api/v1/inbox/threads/"+thread+"/read", "", http.StatusNoContent)
	inbox = decodeBody[inboxListPayload](t, c.mustRequest(http.MethodGet, "/api/v1/inbox", "", http.StatusOK))
	if inbox.UnreadTotal != 0 || inbox.Data[0].IsUnread {
		t.Fatalf("the thread is still unread after marking it read")
	}

	// Marked not interested in the Unibox: a negative interest code, which must
	// be stored as such and fold into the bad outcome.
	c.event(t, campaign.InstantlyLeadNotInterested, target.Email, nil)

	outreach := decodeBody[outreachListPayload](t, c.mustRequest(http.MethodGet,
		"/api/v1/outreach/leads?outcome=bad", "", http.StatusOK))
	if len(outreach.Data) != 1 || outreach.Data[0].Email != target.Email {
		t.Fatalf("the bad outcome lists %+v, want the lead", outreach.Data)
	}
	row := outreach.Data[0]
	if row.InterestStatus == nil || *row.InterestStatus != instantly.InterestNotInterested {
		t.Fatalf("the interest code is %v, want %d", row.InterestStatus, instantly.InterestNotInterested)
	}
	if row.InterestLabel == nil || *row.InterestLabel != "not_interested" {
		t.Fatalf("the interest label is %v", row.InterestLabel)
	}
	if row.LastReplyPreview == nil || !strings.Contains(*row.LastReplyPreview, "all set") || row.EmailsSent != 1 {
		t.Fatalf("the row is missing its reply or send: %+v", row)
	}
	if outreach.Outcomes[campaign.OutcomeBad] != 1 {
		t.Fatalf("the outcome counts are %v", outreach.Outcomes)
	}

	rec := c.mustRequest(http.MethodGet, "/api/v1/outreach/leads/export.csv", "", http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("the export is %q, want CSV", ct)
	}
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("the export is not valid CSV: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("the export has %d lines, want a header and the one contacted lead", len(records))
	}
	header := map[string]int{}
	for i, col := range records[0] {
		header[col] = i
	}
	line := records[1]
	if line[header["email"]] != target.Email || line[header["outcome"]] != campaign.OutcomeBad ||
		line[header["interest_status"]] != "not_interested" || line[header["campaign"]] != "Austin gyms" {
		t.Fatalf("the exported row is %v", line)
	}
}
