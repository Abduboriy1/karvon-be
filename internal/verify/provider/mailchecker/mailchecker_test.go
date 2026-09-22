package mailchecker

import (
	"context"
	"strings"
	"testing"

	"github.com/bory/karvon-be/internal/verify/provider"
)

func TestCheck(t *testing.T) {
	tests := []struct {
		name       string
		email      string
		wantScore  int
		wantReason string
	}{
		{
			name:       "an ordinary address on a real domain",
			email:      "jane.doe@example.com",
			wantScore:  ScoreClean,
			wantReason: "not a known disposable provider",
		},
		{
			name:      "a business domain",
			email:     "hello@karvon.local.test",
			wantScore: ScoreClean,
		},
		{
			name:       "a throwaway provider",
			email:      "someone@yopmail.com",
			wantScore:  ScoreRejected,
			wantReason: "disposable",
		},
		{
			name:       "another throwaway provider",
			email:      "someone@mailinator.com",
			wantScore:  ScoreRejected,
			wantReason: "disposable",
		},
		{
			name:       "no at sign at all",
			email:      "not-an-address",
			wantScore:  ScoreRejected,
			wantReason: "does not parse",
		},
		{
			name:       "an empty local part",
			email:      "@example.com",
			wantScore:  ScoreRejected,
			wantReason: "does not parse",
		},
		{
			name:       "an empty string",
			email:      "",
			wantScore:  ScoreRejected,
			wantReason: "does not parse",
		},
	}

	p := New()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := p.Check(context.Background(), tc.email)

			if result.Provider != provider.KeyMailChecker {
				t.Errorf("provider = %q, want %q", result.Provider, provider.KeyMailChecker)
			}
			// MailChecker never touches the network, so it always reaches a verdict.
			if result.Status != provider.StatusScored {
				t.Fatalf("status = %q, want %q", result.Status, provider.StatusScored)
			}
			if result.Score != tc.wantScore {
				t.Errorf("score = %d, want %d (reason: %q)", result.Score, tc.wantScore, result.Reason)
			}
			if tc.wantReason != "" && !strings.Contains(result.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", result.Reason, tc.wantReason)
			}
		})
	}
}

// A disposable domain and a syntax failure both score 0, but they mean very
// different things to an operator looking at the row, so they must not collapse into
// one message.
func TestCheckDistinguishesDisposableFromInvalid(t *testing.T) {
	p := New()

	disposable := p.Check(context.Background(), "someone@yopmail.com")
	invalid := p.Check(context.Background(), "not-an-address")

	if disposable.Reason == invalid.Reason {
		t.Fatalf("a disposable domain and a syntax failure share the reason %q", disposable.Reason)
	}
	if got, _ := disposable.Metadata["disposable"].(bool); !got {
		t.Error("the disposable verdict is not recorded in the metadata")
	}
	if got, ok := invalid.Metadata["valid_syntax"].(bool); !ok || got {
		t.Error("the syntax verdict is not recorded in the metadata")
	}
}

// A disposable domain and an unparseable address are facts, not opinions, so they
// must zero the combined score rather than be averaged against providers that never
// checked for either.
func TestCheckDisqualifies(t *testing.T) {
	p := New()

	for _, email := range []string{"someone@yopmail.com", "not-an-address"} {
		if !p.Check(context.Background(), email).Disqualifying {
			t.Errorf("Check(%q) did not disqualify the address", email)
		}
	}
	if p.Check(context.Background(), "jane.doe@example.com").Disqualifying {
		t.Error("an ordinary address was disqualified")
	}
}

// The score is binary by design: MailChecker exposes two booleans and nothing else,
// so a normalized score between the two would be invented rather than measured.
func TestCheckIsBinary(t *testing.T) {
	p := New()
	for _, email := range []string{"a@example.com", "a@yopmail.com", "nope", "a@b@c"} {
		score := p.Check(context.Background(), email).Score
		if score != ScoreClean && score != ScoreRejected {
			t.Errorf("Check(%q) scored %d; MailChecker can only justify %d or %d",
				email, score, ScoreRejected, ScoreClean)
		}
	}
}
