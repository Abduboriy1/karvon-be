package consent

import (
	"testing"

	"github.com/bory/karvon-be/internal/campaign"
)

func TestConsentGateRequiresAnActiveConsentForSubscribed(t *testing.T) {
	d := Evaluate(Input{
		Stage:            campaign.StagePermissionCaptured,
		HasActiveConsent: true,
		ConsentSource:    campaign.ConsentForm,
		AllowSingleOptIn: true,
	})
	if !d.Eligible || d.RequestedStatus != campaign.RequestSubscribed || d.Reason != "" {
		t.Fatalf("with consent: %+v", d)
	}
	if d.Explanation == "" {
		t.Fatal("expected an explanation")
	}

	d = Evaluate(Input{Stage: campaign.StagePermissionRequested, HasActiveConsent: false, AllowSingleOptIn: true})
	if d.Eligible || d.RequestedStatus != "" || d.Reason != ReasonNoConsent {
		t.Fatalf("without consent: %+v", d)
	}
}

func TestConsentGateDowngradesToPendingWhenSingleOptInIsOff(t *testing.T) {
	d := Evaluate(Input{
		Stage:            campaign.StagePermissionCaptured,
		HasActiveConsent: true,
		ConsentSource:    campaign.ConsentExplicitReply,
		AllowSingleOptIn: false,
	})
	if !d.Eligible {
		t.Fatalf("expected eligible: %+v", d)
	}
	if d.RequestedStatus != campaign.RequestPending {
		t.Fatalf("requested status = %q, want %q", d.RequestedStatus, campaign.RequestPending)
	}
	if d.Reason != "" {
		t.Fatalf("reason = %q, want empty", d.Reason)
	}
}

func TestConsentGateRejectsASuppressedContact(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want string
	}{
		{"do not contact", Input{Suppressed: true, SuppressionReason: campaign.SuppressDoNotContact}, ReasonSuppressed},
		{"bounced", Input{Suppressed: true, SuppressionReason: campaign.SuppressBounced}, ReasonSuppressed},
		{"invalid email", Input{Suppressed: true, SuppressionReason: campaign.SuppressInvalidEmail}, ReasonInvalidEmail},
		{"unsubscribed", Input{Suppressed: true, SuppressionReason: campaign.SuppressUnsubscribed}, ReasonUnsubscribed},
		{"suppressed without a reason", Input{Suppressed: true}, ReasonSuppressed},
		{"terminal stage without the flag", Input{Stage: campaign.StageNotInterested}, ReasonSuppressed},
		{"terminal invalid email stage", Input{Stage: campaign.StageInvalidEmail}, ReasonInvalidEmail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.HasActiveConsent = true
			in.AllowSingleOptIn = true
			d := Evaluate(in)
			if d.Eligible || d.RequestedStatus != "" {
				t.Fatalf("expected not eligible: %+v", d)
			}
			if d.Reason != tc.want {
				t.Fatalf("reason = %q, want %q", d.Reason, tc.want)
			}
			if d.Explanation == "" {
				t.Fatal("expected an explanation")
			}
		})
	}
}

func TestAnInterestedContactWithoutConsentIsNotEligible(t *testing.T) {
	for _, stage := range []campaign.Stage{campaign.StageReplied, campaign.StageInterested,
		campaign.StagePermissionRequested, campaign.StageEngaged} {
		d := Evaluate(Input{Stage: stage, HasActiveConsent: false, AllowSingleOptIn: true})
		if d.Eligible {
			t.Fatalf("stage %s without consent must not be eligible: %+v", stage, d)
		}
		if d.Reason != ReasonNoConsent {
			t.Fatalf("stage %s: reason = %q, want %q", stage, d.Reason, ReasonNoConsent)
		}
	}
}

func TestAnUnsubscribedContactIsNeverEligibleEvenWithConsent(t *testing.T) {
	inputs := []Input{
		{Suppressed: true, SuppressionReason: campaign.SuppressUnsubscribed, Stage: campaign.StageUnsubscribed,
			HasActiveConsent: true, ConsentSource: campaign.ConsentForm, AllowSingleOptIn: true},
		{Suppressed: true, SuppressionReason: campaign.SuppressUnsubscribed, Stage: campaign.StageInterested,
			HasActiveConsent: true, AllowSingleOptIn: true},
		{Stage: campaign.StageUnsubscribed, HasActiveConsent: true, AllowSingleOptIn: true},
	}
	for i, in := range inputs {
		d := Evaluate(in)
		if d.Eligible || d.RequestedStatus != "" {
			t.Fatalf("case %d: expected not eligible: %+v", i, d)
		}
		if d.Reason != ReasonUnsubscribed {
			t.Fatalf("case %d: reason = %q, want %q", i, d.Reason, ReasonUnsubscribed)
		}
	}
}
