// Package campaign is the outreach domain: campaigns, contacts and their lifecycle,
// reusable email content and its variants, the per-lead variant assignment, the
// normalised event log, consent and suppression, and the mapping onto the two
// delivery providers. Instantly sends the cold email; Mailchimp sends the newsletter;
// this package owns everything in between.
package campaign

// Source kinds and roles this module adds to the sources table.
const (
	RoleOutreach   = "outreach"
	RoleNewsletter = "newsletter"
	KindInstantly  = "instantly"
	KindMailchimp  = "mailchimp"
)

// Deterministic source ids seeded by the migration.
const (
	InstantlySourceID = "0192f000-0000-7000-8000-000000000004"
	MailchimpSourceID = "0192f000-0000-7000-8000-000000000005"
)

// Campaign statuses.
const (
	CampaignDraft     = "draft"
	CampaignReady     = "ready"
	CampaignLaunching = "launching"
	CampaignActive    = "active"
	CampaignPaused    = "paused"
	CampaignCompleted = "completed"
	CampaignFailed    = "failed"
	CampaignArchived  = "archived"
)

// CampaignStatuses lists every campaign status the schema accepts.
var CampaignStatuses = []string{CampaignDraft, CampaignReady, CampaignLaunching, CampaignActive,
	CampaignPaused, CampaignCompleted, CampaignFailed, CampaignArchived}

// Campaign lead statuses: the delivery state of one contact inside one campaign.
const (
	LeadPending      = "pending"
	LeadPushing      = "pushing"
	LeadActive       = "active"
	LeadPaused       = "paused"
	LeadCompleted    = "completed"
	LeadReplied      = "replied"
	LeadBounced      = "bounced"
	LeadUnsubscribed = "unsubscribed"
	LeadSkipped      = "skipped"
	LeadSuppressed   = "suppressed"
	LeadFailed       = "failed"
)

// LeadStatuses lists every campaign lead status the schema accepts.
var LeadStatuses = []string{LeadPending, LeadPushing, LeadActive, LeadPaused, LeadCompleted, LeadReplied,
	LeadBounced, LeadUnsubscribed, LeadSkipped, LeadSuppressed, LeadFailed}

// Contact sources.
const (
	ContactSourceBusinessImport = "business_import"
	ContactSourceCSV            = "csv"
	ContactSourceManual         = "manual"
)

// ContactSources lists every contact source the schema accepts.
var ContactSources = []string{ContactSourceBusinessImport, ContactSourceCSV, ContactSourceManual}

// Suppression reasons. The first three are permanent.
const (
	SuppressUnsubscribed  = "unsubscribed"
	SuppressBounced       = "bounced"
	SuppressInvalidEmail  = "invalid_email"
	SuppressDoNotContact  = "do_not_contact"
	SuppressNotInterested = "not_interested"
	SuppressWrongPerson   = "wrong_person"
)

// SuppressionReasons lists every reason the schema accepts.
var SuppressionReasons = []string{SuppressUnsubscribed, SuppressBounced, SuppressDoNotContact,
	SuppressInvalidEmail, SuppressNotInterested, SuppressWrongPerson}

// PermanentSuppression reports whether a reason can never be lifted.
func PermanentSuppression(reason string) bool {
	switch reason {
	case SuppressUnsubscribed, SuppressBounced, SuppressInvalidEmail:
		return true
	default:
		return false
	}
}

// Suppression sources.
const (
	SuppressionSourceInstantly = "instantly_webhook"
	SuppressionSourceMailchimp = "mailchimp_webhook"
	SuppressionSourceReconcile = "reconcile"
	SuppressionSourceManual    = "manual"
	SuppressionSourceImport    = "import"
)

// SuppressionSources lists every suppression source the schema accepts.
var SuppressionSources = []string{SuppressionSourceInstantly, SuppressionSourceMailchimp,
	SuppressionSourceReconcile, SuppressionSourceManual, SuppressionSourceImport}

// Consent sources: how permission was captured.
const (
	ConsentExplicitReply   = "explicit_reply"
	ConsentForm            = "form"
	ConsentVerbalConfirmed = "verbal_confirmed"
)

// ConsentSources lists every consent source the schema accepts.
var ConsentSources = []string{ConsentExplicitReply, ConsentForm, ConsentVerbalConfirmed}

// Component types, in the order they are assembled into an email body. The subject
// is its own line; the rest are joined with blank lines.
const (
	ComponentSubject   = "subject"
	ComponentHook      = "hook"
	ComponentProblem   = "problem"
	ComponentValueProp = "value_prop"
	ComponentProof     = "proof"
	ComponentCTA       = "cta"
	ComponentClosing   = "closing"
	ComponentPS        = "ps"
)

// ComponentTypes lists every component type in assembly order.
var ComponentTypes = []string{ComponentSubject, ComponentHook, ComponentProblem, ComponentValueProp,
	ComponentProof, ComponentCTA, ComponentClosing, ComponentPS}

// ValidComponentType reports whether a value is one of the component types.
func ValidComponentType(v string) bool {
	for _, t := range ComponentTypes {
		if t == v {
			return true
		}
	}
	return false
}

// Content statuses, shared by components and variants.
const (
	ContentDraft       = "draft"
	ContentAIGenerated = "ai_generated"
	ContentReviewed    = "reviewed"
	ContentApproved    = "approved"
	ContentActive      = "active"
	ContentArchived    = "archived"
)

// ContentStatuses lists every content status the schema accepts.
var ContentStatuses = []string{ContentDraft, ContentAIGenerated, ContentReviewed, ContentApproved,
	ContentActive, ContentArchived}

// ContentTransitionAllowed encodes the review flow: draft → reviewed → approved →
// active → archived, with AI output entering at ai_generated. Archiving is always
// allowed; a reviewer can also send anything back to draft.
func ContentTransitionAllowed(from, to string) bool {
	if from == to {
		return true
	}
	switch to {
	case ContentArchived, ContentDraft:
		return true
	case ContentReviewed:
		return from == ContentDraft || from == ContentAIGenerated || from == ContentApproved
	case ContentApproved:
		return from == ContentReviewed
	case ContentActive:
		return from == ContentApproved
	default:
		return false
	}
}

// ContentUsable reports whether a component or variant may go into a live campaign.
func ContentUsable(status string) bool {
	return status == ContentApproved || status == ContentActive
}

// Campaign variant statuses.
const (
	CampaignVariantActive = "active"
	CampaignVariantPaused = "paused"
)

// Reply classifications on a send.
const (
	ReplyPositive    = "positive"
	ReplyNegative    = "negative"
	ReplyNeutral     = "neutral"
	ReplyAutoReply   = "auto_reply"
	ReplyOutOfOffice = "out_of_office"
	ReplyUnknown     = "unknown"
)

// ReplyClassifications lists every classification the schema accepts.
var ReplyClassifications = []string{ReplyPositive, ReplyNegative, ReplyNeutral, ReplyAutoReply,
	ReplyOutOfOffice, ReplyUnknown}

// Send sources.
const (
	SendSourceWebhook   = "webhook"
	SendSourceReconcile = "reconcile"
)

// Provider names.
const (
	ProviderInstantly = "instantly"
	ProviderMailchimp = "mailchimp"
)

// Provider event sources.
const (
	ProviderEventWebhook = "webhook"
	ProviderEventReplay  = "replay"
	ProviderEventTest    = "test"
)

// Newsletter subscription statuses (what Mailchimp says).
const (
	SubLocalPending      = "local_pending"
	SubPending           = "pending"
	SubSubscribed        = "subscribed"
	SubUnsubscribed      = "unsubscribed"
	SubCleaned           = "cleaned"
	SubTransactional     = "transactional"
	SubArchived          = "archived"
	SubComplianceBlocked = "compliance_blocked"
	SubError             = "error"
)

// SubscriptionStatuses lists every subscription status the schema accepts.
var SubscriptionStatuses = []string{SubLocalPending, SubPending, SubSubscribed, SubUnsubscribed, SubCleaned,
	SubTransactional, SubArchived, SubComplianceBlocked, SubError}

// Requested subscription statuses (what we ask Mailchimp for).
const (
	RequestPending      = "pending"
	RequestSubscribed   = "subscribed"
	RequestUnsubscribed = "unsubscribed"
)

// Sync statuses on a subscription.
const (
	SyncQueued  = "queued"
	SyncSyncing = "syncing"
	SyncSynced  = "synced"
	SyncFailed  = "failed"
)

// SyncStatuses lists every sync status the schema accepts.
var SyncStatuses = []string{SyncQueued, SyncSyncing, SyncSynced, SyncFailed}

// AI providers and generation statuses.
const (
	AIProviderManual = "manual_chatgpt"
	AIProviderOpenAI = "openai_api"

	GenerationPromptBuilt   = "prompt_built"
	GenerationAwaitingPaste = "awaiting_paste"
	GenerationParsed        = "parsed"
	GenerationImported      = "imported"
	GenerationFailed        = "failed"
)

// GenerationStatuses lists every generation status the schema accepts.
var GenerationStatuses = []string{GenerationPromptBuilt, GenerationAwaitingPaste, GenerationParsed,
	GenerationImported, GenerationFailed}

// Sync run kinds.
const (
	SyncKindInstantlyCampaign     = "instantly_campaign"
	SyncKindInstantlyCampaignsAll = "instantly_campaigns_all"
	SyncKindInstantlyLeadsFull    = "instantly_leads_full"
	SyncKindInstantlyAccounts     = "instantly_accounts"
	SyncKindInstantlyReplay       = "instantly_webhook_replay"
	SyncKindMailchimpMembers      = "mailchimp_members"
	SyncKindMailchimpAudiences    = "mailchimp_audiences"
)

// SyncKinds lists every sync run kind the schema accepts.
var SyncKinds = []string{SyncKindInstantlyCampaign, SyncKindInstantlyCampaignsAll, SyncKindInstantlyLeadsFull,
	SyncKindInstantlyAccounts, SyncKindInstantlyReplay, SyncKindMailchimpMembers, SyncKindMailchimpAudiences}

// Analytics snapshot sources.
const (
	SnapshotInstantly = "instantly"
	SnapshotLocal     = "local"
)

// MaxSteps is the most follow-up steps a campaign can have.
const MaxSteps = 5

// SubjectVar is the custom variable holding the rendered subject for a step. The
// Instantly step variant is literally "{{k_subject_1}}" / "{{k_body_1}}", and the
// push fills these per lead, which is what makes local rendering work.
func SubjectVar(step int) string { return "k_subject_" + itoa(step) }

// BodyVar is the custom variable that holds the rendered body for a step.
func BodyVar(step int) string { return "k_body_" + itoa(step) }

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}
