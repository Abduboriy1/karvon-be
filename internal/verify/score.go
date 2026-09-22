package verify

// Pass 1 soft-signal weights. They are declared once here and read from nowhere
// else, so the breakdown the UI renders and the score stored on the row can never
// drift apart. TestPass1WeightsSumToMax guards the total.
const (
	PointsMX              = 30
	PointsARecord         = 10
	PointsDomainAge       = 10
	PointsSPF             = 5
	PointsDMARC           = 5
	PointsHumanLocal      = 10
	PointsNotFreeProvider = 5
	PointsNoTypo          = 5
	PointsStructure       = 5
)

// Pass1MaxScore is the most a local run can award. It is deliberately below 90: an
// address that only passed local checks can never be tagged "Verified", because only
// a third party can confirm that a mailbox actually accepts mail.
const Pass1MaxScore = 85

// Score caps applied after the points are summed.
const (
	// TypoScoreCap keeps a misspelled domain below the third-party gate, so a typo
	// is never paid for; the suggestion is offered instead.
	TypoScoreCap = 40
	// RoleSoftScoreCap is the ceiling for a shared business mailbox when
	// KARVON_VERIFY_ROLE_SOFT_MODE is "penalty" rather than "hard".
	RoleSoftScoreCap = 60
)

// Pass 2 scores. The adapter maps every vendor verdict onto exactly one of these.
const (
	ScoreDeliverable   = 100
	ScoreRisky         = 70
	ScoreUndeliverable = 0
)

// Tag boundaries, inclusive lower bounds.
const (
	MinScoreGreen      = 90
	MinScoreLightGreen = 70
	MinScoreYellow     = 50
	MinScoreOrange     = 30
)

// pass1Weights is the canonical weight per check key. A check missing from this map
// contributes nothing, which is how the hard fails are represented.
var pass1Weights = map[string]int{
	CheckStructure:    PointsStructure,
	CheckTypo:         PointsNoTypo,
	CheckHumanLocal:   PointsHumanLocal,
	CheckFreeProvider: PointsNotFreeProvider,
	CheckMX:           PointsMX,
	CheckARecord:      PointsARecord,
	CheckSPF:          PointsSPF,
	CheckDMARC:        PointsDMARC,
	CheckDomainAge:    PointsDomainAge,
}

// Pass1Weights returns a copy of the weight table, for tests and for the API's
// self-description of the scoring model.
func Pass1Weights() map[string]int {
	out := make(map[string]int, len(pass1Weights))
	for key, points := range pass1Weights {
		out[key] = points
	}
	return out
}

// TagFor maps a final score onto its colour band.
func TagFor(score int) Tag {
	switch {
	case score >= MinScoreGreen:
		return TagGreen
	case score >= MinScoreLightGreen:
		return TagLightGreen
	case score >= MinScoreYellow:
		return TagYellow
	case score >= MinScoreOrange:
		return TagOrange
	default:
		return TagRed
	}
}

// ScoreFor maps a conclusive provider verdict onto our scale. The second return is
// false for verdicts that leave the Pass 1 score in place.
func ScoreFor(status Pass2Status) (int, bool) {
	switch status {
	case Pass2Deliverable:
		return ScoreDeliverable, true
	case Pass2Risky:
		return ScoreRisky, true
	case Pass2Undeliverable:
		return ScoreUndeliverable, true
	default:
		return 0, false
	}
}

// Finalize resolves the score the UI shows: the third-party result when one was
// reached, otherwise the local one. pass2 is nil when Pass 2 has not produced a
// conclusive verdict for this address.
func Finalize(pass1Score int, pass2Score *int) (int, Tag) {
	score := pass1Score
	if pass2Score != nil {
		score = *pass2Score
	}
	score = clampScore(score)
	return score, TagFor(score)
}

func clampScore(score int) int {
	switch {
	case score < 0:
		return 0
	case score > 100:
		return 100
	default:
		return score
	}
}
