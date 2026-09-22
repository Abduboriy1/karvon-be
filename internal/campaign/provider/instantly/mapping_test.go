package instantly_test

import (
	"testing"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
)

func TestLeadStatusMappingCoversEveryInstantlyCode(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{instantly.LeadStatusActive, campaign.LeadActive},
		{instantly.LeadStatusPaused, campaign.LeadPaused},
		{instantly.LeadStatusCompleted, campaign.LeadCompleted},
		{instantly.LeadStatusBounced, campaign.LeadBounced},
		{instantly.LeadStatusUnsubscribed, campaign.LeadUnsubscribed},
		{instantly.LeadStatusSkipped, campaign.LeadSkipped},
		{0, campaign.LeadActive},
		{99, campaign.LeadActive},
	}
	for _, tc := range cases {
		if got := instantly.LeadStatus(tc.code); got != tc.want {
			t.Errorf("LeadStatus(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

func TestInterestLabelsAndPolarity(t *testing.T) {
	labels := map[int]string{
		instantly.InterestOutOfOffice:      "out_of_office",
		instantly.InterestInterested:       "interested",
		instantly.InterestMeetingBooked:    "meeting_booked",
		instantly.InterestMeetingCompleted: "meeting_completed",
		instantly.InterestWon:              "won",
		instantly.InterestNotInterested:    "not_interested",
		instantly.InterestWrongPerson:      "wrong_person",
		instantly.InterestLost:             "lost",
		instantly.InterestNoShow:           "no_show",
		42:                                 "unknown",
	}
	for code, want := range labels {
		if got := instantly.InterestLabel(code); got != want {
			t.Errorf("InterestLabel(%d) = %q, want %q", code, got, want)
		}
	}
	for code := -5; code <= 5; code++ {
		want := code >= 1 && code <= 4
		if got := instantly.InterestPositive(code); got != want {
			t.Errorf("InterestPositive(%d) = %v, want %v", code, got, want)
		}
	}
}

func TestStatusLabelsNameEveryDocumentedCode(t *testing.T) {
	campaigns := map[int]string{
		instantly.CampaignStatusDraft:               "draft",
		instantly.CampaignStatusActive:              "active",
		instantly.CampaignStatusPaused:              "paused",
		instantly.CampaignStatusCompleted:           "completed",
		instantly.CampaignStatusRunningSubsequences: "running_subsequences",
		instantly.CampaignStatusAccountsUnhealthy:   "accounts_unhealthy",
		instantly.CampaignStatusBounceProtect:       "bounce_protect",
		instantly.CampaignStatusAccountSuspended:    "account_suspended",
		7:                                           "unknown",
	}
	for code, want := range campaigns {
		if got := instantly.CampaignStatusLabel(code); got != want {
			t.Errorf("CampaignStatusLabel(%d) = %q, want %q", code, got, want)
		}
	}

	accounts := map[int]string{
		instantly.AccountStatusActive:          "active",
		instantly.AccountStatusPaused:          "paused",
		instantly.AccountStatusMaintenance:     "maintenance",
		instantly.AccountStatusConnectionError: "connection_error",
		instantly.AccountStatusSoftBounceError: "soft_bounce_error",
		instantly.AccountStatusSendingError:    "sending_error",
		0:                                      "unknown",
	}
	for code, want := range accounts {
		if got := instantly.AccountStatusLabel(code); got != want {
			t.Errorf("AccountStatusLabel(%d) = %q, want %q", code, got, want)
		}
	}

	warmup := map[int]string{0: "paused", 1: "active", -1: "banned", -2: "spam_folder_unknown", -3: "permanent_suspension", 9: "unknown"}
	for code, want := range warmup {
		if got := instantly.WarmupStatusLabel(code); got != want {
			t.Errorf("WarmupStatusLabel(%d) = %q, want %q", code, got, want)
		}
	}

	providers := map[int]string{1: "IMAP/SMTP", 2: "Google", 3: "Microsoft", 4: "AWS", 8: "AirMail", 11: "Airmail Instant", 5: "unknown"}
	for code, want := range providers {
		if got := instantly.ProviderCodeLabel(code); got != want {
			t.Errorf("ProviderCodeLabel(%d) = %q, want %q", code, got, want)
		}
	}
}
