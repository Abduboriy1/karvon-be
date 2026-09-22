package verify

import "testing"

// The brief fixes both the individual weights and the fact that a local run can
// never reach "Verified". If either changes, this test is the place it is noticed.
func TestPass1WeightsSumToMax(t *testing.T) {
	total := 0
	for _, points := range Pass1Weights() {
		total += points
	}
	if total != Pass1MaxScore {
		t.Fatalf("Pass 1 weights sum to %d, want %d", total, Pass1MaxScore)
	}
	if Pass1MaxScore >= MinScoreGreen {
		t.Fatalf("Pass1MaxScore %d reaches the green band (%d); only a third party may",
			Pass1MaxScore, MinScoreGreen)
	}
	if TagFor(Pass1MaxScore) != TagLightGreen {
		t.Fatalf("a perfect local score tags as %q, want %q", TagFor(Pass1MaxScore), TagLightGreen)
	}
}

// Every check the pipeline can run must be in the catalogue, and every weighted key
// must be a check the pipeline knows about.
func TestCheckCatalogCoversEveryWeight(t *testing.T) {
	catalogued := make(map[string]struct{}, len(checkCatalog))
	for _, entry := range checkCatalog {
		if entry.Label == "" {
			t.Errorf("check %q has no label", entry.Key)
		}
		if _, dup := catalogued[entry.Key]; dup {
			t.Errorf("check %q appears twice in the catalogue", entry.Key)
		}
		catalogued[entry.Key] = struct{}{}
	}
	for key := range Pass1Weights() {
		if _, ok := catalogued[key]; !ok {
			t.Errorf("weighted check %q is not in the catalogue", key)
		}
	}
}

func TestTagForBoundaries(t *testing.T) {
	tests := []struct {
		score int
		want  Tag
	}{
		{-10, TagRed},
		{0, TagRed},
		{29, TagRed},
		{30, TagOrange},
		{49, TagOrange},
		{50, TagYellow},
		{69, TagYellow},
		{70, TagLightGreen},
		{89, TagLightGreen},
		{90, TagGreen},
		{100, TagGreen},
	}
	for _, tt := range tests {
		if got := TagFor(tt.score); got != tt.want {
			t.Errorf("TagFor(%d) = %q, want %q", tt.score, got, tt.want)
		}
	}
}

func TestTagLabelsAreDistinctAndNonEmpty(t *testing.T) {
	seen := make(map[string]Tag, len(Tags))
	for _, tag := range Tags {
		label := tag.Label()
		if label == "" {
			t.Errorf("tag %q has no label", tag)
		}
		if other, dup := seen[label]; dup {
			t.Errorf("tags %q and %q share the label %q", tag, other, label)
		}
		seen[label] = tag
	}
}

func TestScoreForMapsProviderVerdicts(t *testing.T) {
	tests := []struct {
		status     Pass2Status
		want       int
		conclusive bool
	}{
		{Pass2Deliverable, ScoreDeliverable, true},
		{Pass2Risky, ScoreRisky, true},
		{Pass2Undeliverable, ScoreUndeliverable, true},
		{Pass2Unknown, 0, false},
		{Pass2Error, 0, false},
	}
	for _, tt := range tests {
		score, ok := ScoreFor(tt.status)
		if ok != tt.conclusive {
			t.Errorf("ScoreFor(%q) conclusive = %v, want %v", tt.status, ok, tt.conclusive)
		}
		if ok && score != tt.want {
			t.Errorf("ScoreFor(%q) = %d, want %d", tt.status, score, tt.want)
		}
		if tt.status.Conclusive() != tt.conclusive {
			t.Errorf("%q.Conclusive() = %v, want %v", tt.status, tt.status.Conclusive(), tt.conclusive)
		}
	}
}

// final_score is the Pass 2 score when Pass 2 reached a verdict, and the Pass 1
// score otherwise. This is the one rule the whole tag column depends on.
func TestFinalizePrefersTheThirdPartyVerdict(t *testing.T) {
	deliverable, undeliverable, risky := ScoreDeliverable, ScoreUndeliverable, ScoreRisky

	tests := []struct {
		name      string
		pass1     int
		pass2     *int
		wantScore int
		wantTag   Tag
	}{
		{"local only", 85, nil, 85, TagLightGreen},
		{"provider confirms", 85, &deliverable, 100, TagGreen},
		{"provider rejects a strong local score", 85, &undeliverable, 0, TagRed},
		{"provider is unsure", 60, &risky, 70, TagLightGreen},
		{"hard local failure", 0, nil, 0, TagRed},
		{"typo cap", TypoScoreCap, nil, 40, TagOrange},
		{"role penalty cap", RoleSoftScoreCap, nil, 60, TagYellow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score, tag := Finalize(tt.pass1, tt.pass2)
			if score != tt.wantScore || tag != tt.wantTag {
				t.Fatalf("Finalize(%d, %v) = (%d, %q), want (%d, %q)",
					tt.pass1, tt.pass2, score, tag, tt.wantScore, tt.wantTag)
			}
		})
	}
}

// The gate and the caps have to stay consistent: a capped address must never be
// eligible for a paid verification.
func TestCapsStayBelowTheThirdPartyGate(t *testing.T) {
	if TypoScoreCap >= MinScoreYellow {
		t.Fatalf("a typo'd address (cap %d) would qualify for the paid pass (gate %d)",
			TypoScoreCap, MinScoreYellow)
	}
	if TagFor(TypoScoreCap) != TagOrange {
		t.Fatalf("a typo'd address tags as %q, want %q", TagFor(TypoScoreCap), TagOrange)
	}
}
