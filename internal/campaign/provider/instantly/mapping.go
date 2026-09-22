package instantly

import "github.com/bory/karvon-be/internal/campaign"

// unknownLabel is what every label function returns for a code it does not know,
// so a new Instantly status shows up as such rather than as a wrong word.
const unknownLabel = "unknown"

// LeadStatus maps an Instantly lead status code onto our lead status vocabulary.
// Unknown codes are read as active, which is the safest assumption for a lead
// Instantly is still holding.
func LeadStatus(code int) string {
	switch code {
	case LeadStatusActive:
		return campaign.LeadActive
	case LeadStatusPaused:
		return campaign.LeadPaused
	case LeadStatusCompleted:
		return campaign.LeadCompleted
	case LeadStatusBounced:
		return campaign.LeadBounced
	case LeadStatusUnsubscribed:
		return campaign.LeadUnsubscribed
	case LeadStatusSkipped:
		return campaign.LeadSkipped
	default:
		return campaign.LeadActive
	}
}

// InterestLabel names an lt_interest_status code.
func InterestLabel(code int) string {
	switch code {
	case InterestOutOfOffice:
		return "out_of_office"
	case InterestInterested:
		return "interested"
	case InterestMeetingBooked:
		return "meeting_booked"
	case InterestMeetingCompleted:
		return "meeting_completed"
	case InterestWon:
		return "won"
	case InterestNotInterested:
		return "not_interested"
	case InterestWrongPerson:
		return "wrong_person"
	case InterestLost:
		return "lost"
	case InterestNoShow:
		return "no_show"
	default:
		return unknownLabel
	}
}

// InterestPositive reports whether an interest code is a positive outcome:
// interested, meeting booked, meeting completed or won.
func InterestPositive(code int) bool {
	return code >= InterestInterested && code <= InterestWon
}

// CampaignStatusLabel names a campaign status code.
func CampaignStatusLabel(code int) string {
	switch code {
	case CampaignStatusDraft:
		return "draft"
	case CampaignStatusActive:
		return "active"
	case CampaignStatusPaused:
		return "paused"
	case CampaignStatusCompleted:
		return "completed"
	case CampaignStatusRunningSubsequences:
		return "running_subsequences"
	case CampaignStatusAccountsUnhealthy:
		return "accounts_unhealthy"
	case CampaignStatusBounceProtect:
		return "bounce_protect"
	case CampaignStatusAccountSuspended:
		return "account_suspended"
	default:
		return unknownLabel
	}
}

// AccountStatusLabel names a sending account status code.
func AccountStatusLabel(code int) string {
	switch code {
	case AccountStatusActive:
		return "active"
	case AccountStatusPaused:
		return "paused"
	case AccountStatusMaintenance:
		return "maintenance"
	case AccountStatusConnectionError:
		return "connection_error"
	case AccountStatusSoftBounceError:
		return "soft_bounce_error"
	case AccountStatusSendingError:
		return "sending_error"
	default:
		return unknownLabel
	}
}

// WarmupStatusLabel names a sending account warmup_status code.
func WarmupStatusLabel(code int) string {
	switch code {
	case 0:
		return "paused"
	case 1:
		return "active"
	case -1:
		return "banned"
	case -2:
		return "spam_folder_unknown"
	case -3:
		return "permanent_suspension"
	default:
		return unknownLabel
	}
}

// ProviderCodeLabel names a sending account provider_code.
func ProviderCodeLabel(code int) string {
	switch code {
	case 1:
		return "IMAP/SMTP"
	case 2:
		return "Google"
	case 3:
		return "Microsoft"
	case 4:
		return "AWS"
	case 8:
		return "AirMail"
	case 11:
		return "Airmail Instant"
	default:
		return unknownLabel
	}
}
