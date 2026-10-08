package service_test

import (
	"testing"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/campaign/service"
)

func TestInboxDirectionMirrorsReceivedAndSentButNotScheduled(t *testing.T) {
	cases := []struct {
		ueType int
		want   string
		ok     bool
	}{
		{instantly.EmailTypeReceived, campaign.InboxReceived, true},
		{instantly.EmailTypeSentFromCampaign, campaign.InboxSent, true},
		{instantly.EmailTypeSent, campaign.InboxSent, true},
		{instantly.EmailTypeScheduled, "", false},
		{0, "", false},
	}
	for _, c := range cases {
		got, ok := service.InboxDirection(c.ueType)
		if got != c.want || ok != c.ok {
			t.Errorf("InboxDirection(%d) = %q, %v; want %q, %v", c.ueType, got, ok, c.want, c.ok)
		}
	}
}

func TestInterestLabelOfPrefersTheStoredLabel(t *testing.T) {
	label := "meeting_booked"
	code := int32(instantly.InterestNotInterested)
	if got := service.InterestLabelOf(&label, &code); got != label {
		t.Errorf("got %q, want the stored label", got)
	}
	if got := service.InterestLabelOf(nil, &code); got != "not_interested" {
		t.Errorf("got %q, want the code's name", got)
	}
	if got := service.InterestLabelOf(nil, nil); got != "" {
		t.Errorf("got %q for a lead with no interest, want empty", got)
	}
}

// The export's columns are a contract with whoever opens the file; the outcome and
// Instantly's own status must both be there, next to each other.
func TestOutreachCSVHeaderCarriesOutcomeAndInterest(t *testing.T) {
	index := map[string]int{}
	for i, col := range service.OutreachCSVHeader {
		if _, dup := index[col]; dup {
			t.Fatalf("column %q appears twice", col)
		}
		index[col] = i
	}
	for _, col := range []string{"email", "campaign", "outcome", "interest_status", "sending_account", "last_reply_preview"} {
		if _, ok := index[col]; !ok {
			t.Errorf("the export has no %q column", col)
		}
	}
	if index["interest_status"] != index["outcome"]+1 {
		t.Errorf("interest_status should follow outcome")
	}
}
