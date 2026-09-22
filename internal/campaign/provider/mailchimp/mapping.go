package mailchimp

import (
	"strings"

	"github.com/bory/karvon-be/internal/campaign"
)

// StatusLabel is the operator-facing wording for a Mailchimp member status. An
// unknown value is returned as-is so nothing is hidden.
func StatusLabel(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case StatusSubscribed:
		return "Subscribed"
	case StatusUnsubscribed:
		return "Unsubscribed"
	case StatusCleaned:
		return "Cleaned (bounced)"
	case StatusPending:
		return "Pending confirmation"
	case StatusTransactional:
		return "Transactional only"
	case StatusArchived:
		return "Archived"
	case "":
		return "Unknown"
	default:
		return status
	}
}

// LocalStatus maps a Mailchimp member status onto the subscription status the
// schema stores. Anything Mailchimp adds later lands in campaign.SubError so the
// sync notices rather than silently mis-filing it.
func LocalStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case StatusSubscribed:
		return campaign.SubSubscribed
	case StatusUnsubscribed:
		return campaign.SubUnsubscribed
	case StatusCleaned:
		return campaign.SubCleaned
	case StatusPending:
		return campaign.SubPending
	case StatusTransactional:
		return campaign.SubTransactional
	case StatusArchived:
		return campaign.SubArchived
	default:
		return campaign.SubError
	}
}
