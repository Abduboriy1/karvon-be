package campaign

// Stage is a contact's position in the cold → subscriber funnel.
type Stage string

// The funnel stages, then the terminal ones.
const (
	StageCold                = Stage("cold")
	StageQueuedForInstantly  = Stage("queued_for_instantly")
	StageContacted           = Stage("contacted")
	StageEngaged             = Stage("engaged")
	StageReplied             = Stage("replied")
	StageInterested          = Stage("interested")
	StagePermissionRequested = Stage("permission_requested")
	StagePermissionCaptured  = Stage("permission_captured")
	StageNewsletterEligible  = Stage("newsletter_eligible")
	StageMailchimpPending    = Stage("mailchimp_pending")
	StageMailchimpSubscribed = Stage("mailchimp_subscribed")

	StageNotInterested = Stage("not_interested")
	StageWrongPerson   = Stage("wrong_person")
	StageDoNotContact  = Stage("do_not_contact")
	StageInvalidEmail  = Stage("invalid_email")
	StageBounced       = Stage("bounced")
	StageUnsubscribed  = Stage("unsubscribed")
)

// Stages lists every stage in funnel order, terminals last.
var Stages = []Stage{
	StageCold, StageQueuedForInstantly, StageContacted, StageEngaged, StageReplied, StageInterested,
	StagePermissionRequested, StagePermissionCaptured, StageNewsletterEligible, StageMailchimpPending,
	StageMailchimpSubscribed,
	StageNotInterested, StageWrongPerson, StageDoNotContact, StageInvalidEmail, StageBounced, StageUnsubscribed,
}

// FunnelStages are the non-terminal stages in order.
var FunnelStages = Stages[:11]

var stageRank = map[Stage]int{
	StageCold: 0, StageQueuedForInstantly: 1, StageContacted: 2, StageEngaged: 3, StageReplied: 4,
	StageInterested: 5, StagePermissionRequested: 6, StagePermissionCaptured: 7, StageNewsletterEligible: 8,
	StageMailchimpPending: 9, StageMailchimpSubscribed: 10,
	StageNotInterested: 100, StageWrongPerson: 100, StageDoNotContact: 100, StageInvalidEmail: 100,
	StageBounced: 100, StageUnsubscribed: 200,
}

// Rank orders stages: funnel stages ascend, terminals outrank them all, and
// unsubscribed outranks every other terminal. Unknown stages rank -1.
func (s Stage) Rank() int {
	if r, ok := stageRank[s]; ok {
		return r
	}
	return -1
}

// Valid reports whether the value is a known stage.
func (s Stage) Valid() bool { return s.Rank() >= 0 }

// Terminal reports whether the stage is a suppression state.
func (s Stage) Terminal() bool { return s.Rank() >= 100 }

// Newsletter reports whether the stage requires an active consent record.
func (s Stage) Newsletter() bool {
	return s == StageNewsletterEligible || s == StageMailchimpPending || s == StageMailchimpSubscribed
}

// Label is the human-readable name shown in the UI.
func (s Stage) Label() string {
	switch s {
	case StageCold:
		return "Cold lead"
	case StageQueuedForInstantly:
		return "Queued for Instantly"
	case StageContacted:
		return "Contacted"
	case StageEngaged:
		return "Opened / clicked"
	case StageReplied:
		return "Replied"
	case StageInterested:
		return "Interested"
	case StagePermissionRequested:
		return "Permission requested"
	case StagePermissionCaptured:
		return "Permission captured"
	case StageNewsletterEligible:
		return "Newsletter eligible"
	case StageMailchimpPending:
		return "Mailchimp pending"
	case StageMailchimpSubscribed:
		return "Mailchimp subscriber"
	case StageNotInterested:
		return "Not interested"
	case StageWrongPerson:
		return "Wrong person"
	case StageDoNotContact:
		return "Do not contact"
	case StageInvalidEmail:
		return "Invalid email"
	case StageBounced:
		return "Bounced"
	case StageUnsubscribed:
		return "Unsubscribed"
	default:
		return string(s)
	}
}

// StageForSuppression is the terminal stage a suppression reason maps to.
func StageForSuppression(reason string) Stage {
	switch reason {
	case SuppressUnsubscribed:
		return StageUnsubscribed
	case SuppressBounced:
		return StageBounced
	case SuppressInvalidEmail:
		return StageInvalidEmail
	case SuppressDoNotContact:
		return StageDoNotContact
	case SuppressNotInterested:
		return StageNotInterested
	case SuppressWrongPerson:
		return StageWrongPerson
	default:
		return StageDoNotContact
	}
}

// AdvanceStage decides whether a contact moves from cur to next and returns the stage
// it ends up in.
//
// The rules:
//  1. unsubscribed never changes;
//  2. a terminal stage beats any funnel stage, and among terminals only unsubscribed
//     can replace another;
//  3. funnel stages only move forward, so a late "opened" after "replied" does not
//     regress the contact.
//
// It knows nothing about consent: the consent gate is applied by the caller and by
// the database trigger, so "interested" can never slip into a newsletter stage here.
func AdvanceStage(cur, next Stage) (Stage, bool) {
	if !next.Valid() || cur == next {
		return cur, false
	}
	if cur == StageUnsubscribed {
		return cur, false
	}
	if cur.Terminal() {
		if next == StageUnsubscribed {
			return next, true
		}
		return cur, false
	}
	if next.Terminal() {
		return next, true
	}
	if next.Rank() > cur.Rank() {
		return next, true
	}
	return cur, false
}
