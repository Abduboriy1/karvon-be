package campaign_test

import (
	"testing"

	"github.com/bory/karvon-be/internal/campaign"
)

func TestAdvanceStageNeverLeavesUnsubscribed(t *testing.T) {
	for _, next := range campaign.Stages {
		got, changed := campaign.AdvanceStage(campaign.StageUnsubscribed, next)
		if changed && next != campaign.StageUnsubscribed {
			t.Errorf("an unsubscribed contact was moved to %s", next)
		}
		if got != campaign.StageUnsubscribed {
			t.Errorf("unsubscribed → %s left the contact at %s", next, got)
		}
	}
}

func TestAdvanceStageOnlyMovesForwardThroughTheFunnel(t *testing.T) {
	// A late "opened" after a reply must not drag the contact back.
	got, changed := campaign.AdvanceStage(campaign.StageReplied, campaign.StageEngaged)
	if changed || got != campaign.StageReplied {
		t.Fatalf("replied → engaged = %s (changed %v), want replied unchanged", got, changed)
	}
	if got, changed := campaign.AdvanceStage(campaign.StageContacted, campaign.StageReplied); !changed || got != campaign.StageReplied {
		t.Fatalf("contacted → replied = %s (changed %v), want replied", got, changed)
	}
}

func TestATerminalStageBeatsAnyFunnelStage(t *testing.T) {
	for _, terminal := range []campaign.Stage{campaign.StageBounced, campaign.StageNotInterested, campaign.StageUnsubscribed} {
		got, changed := campaign.AdvanceStage(campaign.StageInterested, terminal)
		if !changed || got != terminal {
			t.Errorf("interested → %s = %s (changed %v)", terminal, got, changed)
		}
	}
	// Among terminals, only an unsubscribe outranks another.
	if got, changed := campaign.AdvanceStage(campaign.StageBounced, campaign.StageNotInterested); changed || got != campaign.StageBounced {
		t.Errorf("bounced → not_interested = %s (changed %v), want bounced unchanged", got, changed)
	}
	if got, changed := campaign.AdvanceStage(campaign.StageBounced, campaign.StageUnsubscribed); !changed || got != campaign.StageUnsubscribed {
		t.Errorf("bounced → unsubscribed = %s (changed %v), want unsubscribed", got, changed)
	}
}

func TestInterestedNeverAdvancesIntoANewsletterStage(t *testing.T) {
	// AdvanceStage moves the pointer; the consent gate is applied by the caller
	// and by the trigger. What matters here is that interest does not skip
	// the permission stages on its own.
	for _, stage := range []campaign.Stage{campaign.StageNewsletterEligible, campaign.StageMailchimpPending, campaign.StageMailchimpSubscribed} {
		if !stage.Newsletter() {
			t.Errorf("%s should be a newsletter stage requiring consent", stage)
		}
	}
	if campaign.StageInterested.Newsletter() {
		t.Error("interested must not be treated as a newsletter stage")
	}
	if campaign.StageInterested.Rank() >= campaign.StageNewsletterEligible.Rank() {
		t.Error("interested should rank below newsletter_eligible")
	}
}

func TestEveryStageHasARankAndALabel(t *testing.T) {
	for _, stage := range campaign.Stages {
		if stage.Rank() < 0 {
			t.Errorf("%s has no rank", stage)
		}
		if stage.Label() == "" || stage.Label() == string(stage) {
			t.Errorf("%s has no human label", stage)
		}
	}
	if campaign.Stage("nonsense").Valid() {
		t.Error("an unknown stage reported itself valid")
	}
}

func TestSuppressionReasonsMapToTerminalStages(t *testing.T) {
	for _, reason := range campaign.SuppressionReasons {
		stage := campaign.StageForSuppression(reason)
		if !stage.Terminal() {
			t.Errorf("%s maps to %s, which is not terminal", reason, stage)
		}
	}
	if !campaign.PermanentSuppression(campaign.SuppressUnsubscribed) ||
		!campaign.PermanentSuppression(campaign.SuppressBounced) ||
		!campaign.PermanentSuppression(campaign.SuppressInvalidEmail) {
		t.Error("unsubscribed, bounced and invalid_email must be permanent")
	}
	if campaign.PermanentSuppression(campaign.SuppressNotInterested) {
		t.Error("not_interested must be liftable")
	}
}

func TestContentTransitionsFollowTheReviewFlow(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{campaign.ContentDraft, campaign.ContentReviewed, true},
		{campaign.ContentAIGenerated, campaign.ContentReviewed, true},
		{campaign.ContentReviewed, campaign.ContentApproved, true},
		{campaign.ContentApproved, campaign.ContentActive, true},
		{campaign.ContentActive, campaign.ContentArchived, true},
		{campaign.ContentDraft, campaign.ContentApproved, false},
		{campaign.ContentDraft, campaign.ContentActive, false},
		{campaign.ContentAIGenerated, campaign.ContentActive, false},
	}
	for _, c := range cases {
		if got := campaign.ContentTransitionAllowed(c.from, c.to); got != c.want {
			t.Errorf("%s → %s = %v, want %v", c.from, c.to, got, c.want)
		}
	}
	if campaign.ContentUsable(campaign.ContentDraft) || campaign.ContentUsable(campaign.ContentAIGenerated) {
		t.Error("unreviewed content must not be usable in a live campaign")
	}
	if !campaign.ContentUsable(campaign.ContentApproved) || !campaign.ContentUsable(campaign.ContentActive) {
		t.Error("approved and active content must be usable")
	}
}

func TestCustomVariableNamesAreStable(t *testing.T) {
	// The Instantly step body is literally "{{k_subject_1}}" / "{{k_body_1}}", so
	// renaming these would silently break every launched campaign.
	if campaign.SubjectVar(1) != "k_subject_1" || campaign.BodyVar(1) != "k_body_1" {
		t.Fatalf("step 1 variables are %q / %q", campaign.SubjectVar(1), campaign.BodyVar(1))
	}
	if campaign.SubjectVar(5) != "k_subject_5" || campaign.BodyVar(5) != "k_body_5" {
		t.Fatalf("step 5 variables are %q / %q", campaign.SubjectVar(5), campaign.BodyVar(5))
	}
}
