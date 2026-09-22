package campaign

// Contact event types: one per thing that can appear on a timeline.
const (
	EventImported                 = "imported"
	EventQueued                   = "queued"
	EventPushed                   = "pushed"
	EventPushFailed               = "push_failed"
	EventSent                     = "sent"
	EventOpened                   = "opened"
	EventClicked                  = "clicked"
	EventReplied                  = "replied"
	EventAutoReplied              = "auto_replied"
	EventInterested               = "interested"
	EventNotInterested            = "not_interested"
	EventWrongPerson              = "wrong_person"
	EventMeetingBooked            = "meeting_booked"
	EventBounced                  = "bounced"
	EventUnsubscribed             = "unsubscribed"
	EventSkipped                  = "skipped"
	EventSuppressed               = "suppressed"
	EventSuppressionLifted        = "suppression_lifted"
	EventPermissionRequested      = "permission_requested"
	EventConsentCaptured          = "consent_captured"
	EventConsentRevoked           = "consent_revoked"
	EventNewsletterEligible       = "newsletter_eligible"
	EventNewsletterPushed         = "newsletter_pushed"
	EventNewsletterPending        = "newsletter_pending"
	EventNewsletterSubscribed     = "newsletter_subscribed"
	EventNewsletterUnsubscribed   = "newsletter_unsubscribed"
	EventNewsletterCleaned        = "newsletter_cleaned"
	EventNewsletterProfileUpdated = "newsletter_profile_updated"
	EventStageChanged             = "stage_changed"
	EventNote                     = "note"
)

// EventTypes lists every contact event type the schema accepts.
var EventTypes = []string{EventImported, EventQueued, EventPushed, EventPushFailed, EventSent, EventOpened,
	EventClicked, EventReplied, EventAutoReplied, EventInterested, EventNotInterested, EventWrongPerson,
	EventMeetingBooked, EventBounced, EventUnsubscribed, EventSkipped, EventSuppressed, EventSuppressionLifted,
	EventPermissionRequested, EventConsentCaptured, EventConsentRevoked, EventNewsletterEligible,
	EventNewsletterPushed, EventNewsletterPending, EventNewsletterSubscribed, EventNewsletterUnsubscribed,
	EventNewsletterCleaned, EventNewsletterProfileUpdated, EventStageChanged, EventNote}

// Event sources.
const (
	EventSourceInstantly = "instantly_webhook"
	EventSourceMailchimp = "mailchimp_webhook"
	EventSourceReconcile = "reconcile"
	EventSourceManual    = "manual"
	EventSourceSystem    = "system"
	EventSourceImport    = "import"
)

// EventSources lists every event source the schema accepts.
var EventSources = []string{EventSourceInstantly, EventSourceMailchimp, EventSourceReconcile,
	EventSourceManual, EventSourceSystem, EventSourceImport}

// Instantly webhook event types, exactly as Instantly names them. The guide and the
// create-webhook enum disagree on two names, so both spellings are accepted.
const (
	InstantlyEmailSent            = "email_sent"
	InstantlyEmailOpened          = "email_opened"
	InstantlyEmailLinkClicked     = "email_link_clicked"
	InstantlyLinkClicked          = "link_clicked"
	InstantlyReplyReceived        = "reply_received"
	InstantlyAutoReplyReceived    = "auto_reply_received"
	InstantlyEmailBounced         = "email_bounced"
	InstantlyLeadUnsubscribed     = "lead_unsubscribed"
	InstantlyCampaignCompleted    = "campaign_completed"
	InstantlyAccountError         = "account_error"
	InstantlyLeadNeutral          = "lead_neutral"
	InstantlyLeadInterested       = "lead_interested"
	InstantlyLeadNotInterested    = "lead_not_interested"
	InstantlyLeadMeetingBooked    = "lead_meeting_booked"
	InstantlyLeadMeetingCompleted = "lead_meeting_completed"
	InstantlyLeadClosed           = "lead_closed"
	InstantlyLeadOutOfOffice      = "lead_out_of_office"
	InstantlyLeadWrongPerson      = "lead_wrong_person"
	InstantlyLeadNoShow           = "lead_no_show"
)

// Mailchimp webhook event types.
const (
	MailchimpSubscribe   = "subscribe"
	MailchimpUnsubscribe = "unsubscribe"
	MailchimpProfile     = "profile"
	MailchimpCleaned     = "cleaned"
	MailchimpUpEmail     = "upemail"
	MailchimpCampaign    = "campaign"
)
