// Package consent is the newsletter eligibility gate: the one place that decides
// whether a contact may be pushed to Mailchimp and with which requested status.
//
// The rule it enforces is that interest is not consent. A contact who replied or
// was marked interested is never eligible until an explicit consent record exists,
// and a suppressed contact is never eligible at all.
package consent

import (
	"github.com/bory/karvon-be/internal/campaign"
)

// Machine reasons a contact is not eligible.
const (
	ReasonSuppressed   = "suppressed"
	ReasonNoConsent    = "no_consent"
	ReasonInvalidEmail = "invalid_email"
	ReasonUnsubscribed = "unsubscribed"
	ReasonExcluded     = "excluded"
)

// Input is everything the gate looks at.
type Input struct {
	Suppressed        bool
	SuppressionReason string
	Stage             campaign.Stage
	HasActiveConsent  bool
	ConsentSource     string
	AllowSingleOptIn  bool // the audience setting
	// Excluded is set when a global exclusion covers the contact.
	Excluded bool
}

// Decision is the outcome of the gate.
type Decision struct {
	Eligible        bool   // may be pushed to Mailchimp at all
	RequestedStatus string // campaign.RequestPending or campaign.RequestSubscribed
	Reason          string // machine reason: "", "suppressed", "excluded", "no_consent", "invalid_email", "unsubscribed"
	Explanation     string // human sentence
}

// Evaluate applies the gate.
//
// A suppressed contact is not eligible: the reason is "unsubscribed" or
// "invalid_email" when that is the suppression reason, "suppressed" otherwise. A
// contact in any terminal stage is treated as suppressed too. Without an active
// consent record the contact is not eligible ("no_consent"), whatever the stage. A
// globally excluded contact is never eligible ("excluded"), consent or not.
// Otherwise the contact is eligible; the requested status is subscribed only when
// the audience allows single opt-in, pending otherwise.
func Evaluate(in Input) Decision {
	if in.Suppressed || in.Stage.Terminal() {
		return suppressedDecision(in)
	}
	if in.Excluded {
		return Decision{
			Reason:      ReasonExcluded,
			Explanation: "The contact is on the global exclusion list, so nothing may be sent to them.",
		}
	}
	if !in.HasActiveConsent {
		return Decision{
			Reason: ReasonNoConsent,
			Explanation: "The contact has no active consent record; interest or a reply is not " +
				"permission to add them to the newsletter.",
		}
	}
	if in.AllowSingleOptIn {
		return Decision{
			Eligible:        true,
			RequestedStatus: campaign.RequestSubscribed,
			Explanation: "The contact has active consent" + sourceClause(in.ConsentSource) +
				" and the audience allows single opt-in, so they can be subscribed directly.",
		}
	}
	return Decision{
		Eligible:        true,
		RequestedStatus: campaign.RequestPending,
		Explanation: "The contact has active consent" + sourceClause(in.ConsentSource) +
			" but the audience requires double opt-in, so Mailchimp will send a confirmation email first.",
	}
}

func suppressedDecision(in Input) Decision {
	switch {
	case in.SuppressionReason == campaign.SuppressUnsubscribed || in.Stage == campaign.StageUnsubscribed:
		return Decision{
			Reason:      ReasonUnsubscribed,
			Explanation: "The contact unsubscribed and can never be added to the newsletter.",
		}
	case in.SuppressionReason == campaign.SuppressInvalidEmail || in.Stage == campaign.StageInvalidEmail:
		return Decision{
			Reason:      ReasonInvalidEmail,
			Explanation: "The contact's email address is invalid, so they cannot be added to the newsletter.",
		}
	default:
		reason := in.SuppressionReason
		if reason == "" && in.Stage.Terminal() {
			reason = string(in.Stage)
		}
		explanation := "The contact is suppressed and cannot be added to the newsletter."
		if reason != "" {
			explanation = "The contact is suppressed (" + reason + ") and cannot be added to the newsletter."
		}
		return Decision{Reason: ReasonSuppressed, Explanation: explanation}
	}
}

func sourceClause(source string) string {
	if source == "" {
		return ""
	}
	return " (" + source + ")"
}
