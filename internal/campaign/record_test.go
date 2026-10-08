package campaign_test

import (
	"testing"

	"github.com/bory/karvon-be/internal/campaign"
)

// Instantly's negative interest codes (not interested is -1) must survive being
// stored; Int32 is for counts and floors them at zero, which reads as out-of-office.
func TestSignedInt32KeepsNegativeInterestCodes(t *testing.T) {
	for _, code := range []int{-4, -3, -2, -1, 0, 1, 4} {
		if got := campaign.SignedInt32(code); int(got) != code {
			t.Errorf("SignedInt32(%d) = %d", code, got)
		}
	}
	if got := campaign.SignedInt32(1 << 40); got != 1<<31-1 {
		t.Errorf("SignedInt32 did not clamp a large value: %d", got)
	}
	if got := campaign.SignedInt32(-1 << 40); got != -1<<31 {
		t.Errorf("SignedInt32 did not clamp a small value: %d", got)
	}
	if got := campaign.Int32(-1); got != 0 {
		t.Errorf("Int32(-1) = %d, want the count floor 0", got)
	}
}
