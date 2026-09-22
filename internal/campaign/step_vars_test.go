package campaign_test

import (
	"strconv"
	"testing"

	"github.com/bory/karvon-be/internal/campaign"
)

// SubjectVar and BodyVar name the Instantly custom variables that carry a step's
// rendered copy, and the same name has to come back out of the provider. They are
// built with a hand-rolled two-digit itoa that does arithmetic on a rune: for any
// step outside 0-99 it emits punctuation instead of digits, so SubjectVar(100) is
// "k_subject_:0" and SubjectVar(-1) is "k_subject_/".
//
// MaxSteps is 5 today, which is the only reason this has not bitten yet. Raising it,
// or calling either function with a step that came from a provider payload rather
// than from our own loop, silently produces a variable name that matches nothing —
// the lead is pushed with copy Instantly will never substitute.
func TestStepVarsUseDecimalDigitsForEveryStep(t *testing.T) {
	for _, step := range []int{0, 1, 5, 9, 10, 42, 99, 100, 255} {
		wantSubject := "k_subject_" + strconv.Itoa(step)
		wantBody := "k_body_" + strconv.Itoa(step)
		if got := campaign.SubjectVar(step); got != wantSubject {
			t.Errorf("SubjectVar(%d) = %q, want %q", step, got, wantSubject)
		}
		if got := campaign.BodyVar(step); got != wantBody {
			t.Errorf("BodyVar(%d) = %q, want %q", step, got, wantBody)
		}
	}
}
