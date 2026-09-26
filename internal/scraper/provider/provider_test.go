package provider

import "testing"

func TestSearchQueryRendering(t *testing.T) {
	tests := []struct {
		name          string
		query         SearchQuery
		wantLocation  string
		wantFullPhase string
	}{
		{
			name:          "city and state",
			query:         SearchQuery{Term: "gyms", City: "Austin", State: "TX"},
			wantLocation:  "Austin, TX",
			wantFullPhase: "gyms in Austin, TX",
		},
		{
			name:          "city only",
			query:         SearchQuery{Term: "med spas", City: "Berlin"},
			wantLocation:  "Berlin",
			wantFullPhase: "med spas in Berlin",
		},
		{
			name:          "state only",
			query:         SearchQuery{Term: "gyms", State: "TX"},
			wantLocation:  "TX",
			wantFullPhase: "gyms in TX",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.query.LocationQuery(); got != tt.wantLocation {
				t.Errorf("LocationQuery = %q, want %q", got, tt.wantLocation)
			}
			if got := tt.query.String(); got != tt.wantFullPhase {
				t.Errorf("String = %q, want %q", got, tt.wantFullPhase)
			}
		})
	}
}

func TestPointerHelpersTreatZeroAsAbsent(t *testing.T) {
	if PtrFloat(0) != nil {
		t.Error("PtrFloat(0) should be nil so the column stays NULL")
	}
	if got := PtrFloat(4.5); got == nil || *got != 4.5 {
		t.Errorf("PtrFloat(4.5) = %v", got)
	}
	if PtrInt32(0) != nil {
		t.Error("PtrInt32(0) should be nil")
	}
	if got := PtrInt32(7); got == nil || *got != 7 {
		t.Errorf("PtrInt32(7) = %v", got)
	}
}

func TestStateCodeFoldsNamesAndCodes(t *testing.T) {
	cases := map[string]string{
		"California":           "CA",
		" california ":         "CA",
		"CA":                   "CA",
		"ca":                   "CA",
		"New  York":            "NY",
		"District of Columbia": "DC",
		"Ontario":              "Ontario",
		"":                     "",
	}
	for in, want := range cases {
		if got := StateCode(in); got != want {
			t.Errorf("StateCode(%q) = %q, want %q", in, got, want)
		}
	}
}
