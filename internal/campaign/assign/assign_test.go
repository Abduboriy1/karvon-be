package assign

import (
	"strconv"
	"testing"

	"github.com/google/uuid"
)

func fixedVariants() []Weighted {
	return []Weighted{
		{ID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Weight: 40},
		{ID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Weight: 30},
		{ID: uuid.MustParse("00000000-0000-0000-0000-000000000003"), Weight: 30},
	}
}

func TestWeightedPickIsDeterministicForTheSameSeed(t *testing.T) {
	campaignID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	contactID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	seed := Seed(campaignID, contactID, 1, 3)
	if seed != Seed(campaignID, contactID, 1, 3) {
		t.Fatal("seed is not stable")
	}
	if seed == Seed(campaignID, contactID, 2, 3) || seed == Seed(campaignID, contactID, 1, 4) {
		t.Fatal("seed should change with the step and the weights version")
	}
	if len(SeedHex(seed)) != 64 {
		t.Fatalf("hex seed length = %d", len(SeedHex(seed)))
	}

	first, ok := Pick(seed, fixedVariants())
	if !ok {
		t.Fatal("expected a pick")
	}
	for i := 0; i < 100; i++ {
		got, ok := Pick(seed, fixedVariants())
		if !ok || got != first {
			t.Fatalf("pick %d = %s (ok=%v), want %s", i, got, ok, first)
		}
	}
}

func TestWeightedPickIsIndependentOfInputOrder(t *testing.T) {
	for i := 0; i < 500; i++ {
		seed := Seed(uuid.New(), uuid.New(), 1, 1)
		v := fixedVariants()
		a, _ := Pick(seed, v)
		b, _ := Pick(seed, []Weighted{v[2], v[0], v[1]})
		c, _ := Pick(seed, []Weighted{v[1], v[2], v[0]})
		if a != b || a != c {
			t.Fatalf("seed %s: picks differ by order: %s %s %s", SeedHex(seed), a, b, c)
		}
	}
}

func TestWeightedPickDistributionMatchesWeightsWithinTwoPercentOverTenThousandContacts(t *testing.T) {
	const n = 10000
	campaignID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	variants := fixedVariants()
	counts := map[uuid.UUID]int{}
	for i := 0; i < n; i++ {
		contactID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("contact-"+strconv.Itoa(i)))
		id, ok := Pick(Seed(campaignID, contactID, 1, 1), variants)
		if !ok {
			t.Fatal("expected a pick")
		}
		counts[id]++
	}
	for _, v := range variants {
		got := float64(counts[v.ID]) / n * 100
		if diff := got - float64(v.Weight); diff > 2 || diff < -2 {
			t.Fatalf("variant %s: got %.2f%%, want %d%% (±2)", v.ID, got, v.Weight)
		}
	}
}

func TestAZeroWeightVariantIsNeverPicked(t *testing.T) {
	zero := uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	negative := uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	variants := append(fixedVariants(), Weighted{ID: zero, Weight: 0}, Weighted{ID: negative, Weight: -5})
	for i := 0; i < 2000; i++ {
		id, ok := Pick(Seed(uuid.New(), uuid.New(), 1, 1), variants)
		if !ok {
			t.Fatal("expected a pick")
		}
		if id == zero || id == negative {
			t.Fatalf("picked a non-positive weight variant %s", id)
		}
	}
	if _, ok := Pick(Seed(uuid.New(), uuid.New(), 1, 1), []Weighted{{ID: zero, Weight: 0}}); ok {
		t.Fatal("no positive weight should yield no pick")
	}
	if _, ok := Pick(Seed(uuid.New(), uuid.New(), 1, 1), nil); ok {
		t.Fatal("no variants should yield no pick")
	}
}

func TestValidateWeightsRequiresATotalOfOneHundred(t *testing.T) {
	cases := []struct {
		name    string
		weights []int
		wantErr bool
	}{
		{"single 100", []int{100}, false},
		{"40/30/30", []int{40, 30, 30}, false},
		{"with a zero", []int{100, 0}, false},
		{"sums to 90", []int{50, 40}, true},
		{"sums to 110", []int{60, 50}, true},
		{"negative", []int{-10, 110}, true},
		{"above 100", []int{101}, true},
		{"all zero", []int{0, 0}, true},
		{"empty", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWeights(tc.weights)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateWeights(%v) = %v, wantErr=%v", tc.weights, err, tc.wantErr)
			}
		})
	}
}
