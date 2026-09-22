package verify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

func TestCostCents(t *testing.T) {
	tests := []struct {
		credits int64
		per1k   int
		want    int64
	}{
		{credits: 1000, per1k: 500, want: 500},
		{credits: 2500, per1k: 500, want: 1250},
		{credits: 0, per1k: 500, want: 0},
		// A handful of addresses at a per-1000 rate rounds down to nothing, which is
		// why the confirmation dialog shows the count as well as the cost.
		{credits: 1, per1k: 500, want: 0},
		{credits: 100, per1k: 1000, want: 100},
	}
	for _, tt := range tests {
		if got := CostCents(tt.credits, tt.per1k); got != tt.want {
			t.Errorf("CostCents(%d, %d) = %d, want %d", tt.credits, tt.per1k, got, tt.want)
		}
	}
}

// The gate is the rule that keeps money from being spent where it would tell us
// nothing: below the floor the address is already ruled out, at or above the ceiling
// the free providers already agree. Every branch is worth pinning down.
func TestCheckGate(t *testing.T) {
	// No settings store, so the service falls back to the shipped defaults: a band
	// of 50 (inclusive) to FreeMaxScore+1 (exclusive), which is every free score the
	// floor lets through. TestCheckGateCeiling covers a ceiling an operator lowered.
	service := &Service{}
	ctx := context.Background()

	now := time.Now().UTC()
	longAgo := now.AddDate(-3, 0, 0)

	tests := []struct {
		name    string
		row     dbgen.EmailVerification
		wantErr bool
		wantSub string
	}{
		{
			name:    "never scored by the free providers",
			row:     dbgen.EmailVerification{},
			wantErr: true,
			wantSub: "free checks",
		},
		{
			name:    "below the floor",
			row:     dbgen.EmailVerification{FreeScoredAt: &now, FreeScore: 40},
			wantErr: true,
			wantSub: "below",
		},
		{
			name: "a high free score is still paid for: it describes the domain, not the mailbox",
			row:  dbgen.EmailVerification{FreeScoredAt: &now, FreeScore: 75},
		},
		{
			name: "the best the free stage can award still qualifies",
			row:  dbgen.EmailVerification{FreeScoredAt: &now, FreeScore: FreeMaxScore},
		},
		{
			name: "already sent to a third party",
			row: dbgen.EmailVerification{
				FreeScoredAt: &now, FreeScore: 60, ThirdPartySentAt: &now,
			},
			wantErr: true,
			wantSub: "never sent to one twice",
		},
		{
			name: "a send never expires, however old it is",
			row: dbgen.EmailVerification{
				FreeScoredAt: &now, FreeScore: 60,
				Pass2VerifiedAt: &longAgo, ThirdPartySentAt: &longAgo,
			},
			wantErr: true,
			wantSub: "never sent to one twice",
		},
		{
			name: "sent but never answered, so the lock outranks the band it sits outside",
			row: dbgen.EmailVerification{
				FreeScoredAt: &now, FreeScore: 10, ThirdPartySentAt: &now,
			},
			wantErr: true,
			wantSub: "never sent to one twice",
		},
		{
			name: "exactly at the floor",
			row:  dbgen.EmailVerification{FreeScoredAt: &now, FreeScore: 50},
		},
		{
			name: "inside the band",
			row:  dbgen.EmailVerification{FreeScoredAt: &now, FreeScore: 74},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := service.checkGate(ctx, tt.row)
			if tt.wantErr {
				if err == nil {
					t.Fatal("the gate let this address through")
				}
				if tt.wantSub != "" && !strings.Contains(err.Error(), tt.wantSub) {
					t.Fatalf("error %q does not mention %q", err, tt.wantSub)
				}
				var appErr *apperr.Error
				if !errors.As(err, &appErr) {
					t.Fatalf("error %v is not an application error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("the gate refused a qualifying address: %v", err)
			}
		})
	}
}

// An operator who lowers the ceiling to save credits must have it honoured: at or
// above it the free providers are taken to agree and no paid check is bought.
func TestCheckGateCeiling(t *testing.T) {
	settings := DefaultSettings()
	settings.PaidThreshold = 75
	service := &Service{
		settings: &SettingsStore{defaults: settings, cached: settings, cachedAt: time.Now()},
	}
	now := time.Now().UTC()

	for _, score := range []int32{75, FreeMaxScore} {
		row := dbgen.EmailVerification{FreeScoredAt: &now, FreeScore: score}
		err := service.checkGate(context.Background(), row)
		if err == nil {
			t.Fatalf("the gate let a free score of %d through", score)
		}
		if !strings.Contains(err.Error(), "threshold") {
			t.Errorf("error %q does not mention %q", err, "threshold")
		}
	}
}

// Switching the paid stage off must stop a single-address paid action too, not just
// bulk runs.
func TestCheckGateWithPaidDisabled(t *testing.T) {
	settings := DefaultSettings()
	settings.PaidEnabled = false
	service := &Service{
		settings: &SettingsStore{defaults: settings, cached: settings, cachedAt: time.Now()},
	}

	now := time.Now().UTC()
	err := service.checkGate(context.Background(),
		dbgen.EmailVerification{FreeScoredAt: &now, FreeScore: 60})
	if err == nil {
		t.Fatal("the gate let an address through while paid verification is switched off")
	}
	if !strings.Contains(err.Error(), "switched off") {
		t.Fatalf("error %q does not explain that paid verification is off", err)
	}
}

func TestValidateFilter(t *testing.T) {
	badScore := 500
	negativeDays := -1

	tests := []struct {
		name    string
		filter  RunFilter
		wantErr bool
	}{
		{name: "everything", filter: RunFilter{Scope: ScopeAll}},
		{
			name:   "a selection of addresses",
			filter: RunFilter{Scope: ScopeSelection, IDs: []uuid.UUID{uuid.New()}},
		},
		{
			name:   "a selection of businesses",
			filter: RunFilter{Scope: ScopeSelection, BusinessIDs: []uuid.UUID{uuid.New()}},
		},
		{name: "an empty selection", filter: RunFilter{Scope: ScopeSelection}, wantErr: true},
		{name: "an unknown scope", filter: RunFilter{Scope: "everything"}, wantErr: true},
		{
			name:    "an unknown tag",
			filter:  RunFilter{Scope: ScopeAll, Tags: []string{"chartreuse"}},
			wantErr: true,
		},
		{name: "known tags", filter: RunFilter{Scope: ScopeAll, Tags: []string{"red", "green"}}},
		{
			name:    "an impossible score",
			filter:  RunFilter{Scope: ScopeAll, MinScore: &badScore},
			wantErr: true,
		},
		{
			name:    "negative staleness",
			filter:  RunFilter{Scope: ScopeAll, StaleAfterDays: &negativeDays},
			wantErr: true,
		},
		{
			name:    "too many ids",
			filter:  RunFilter{Scope: ScopeSelection, IDs: make([]uuid.UUID, MaxSelectionIDs+1)},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFilter(tt.filter)
			if tt.wantErr != (err != nil) {
				t.Fatalf("validateFilter(%+v) error = %v, want error: %v", tt.filter, err, tt.wantErr)
			}
		})
	}
}

func TestValidatePass(t *testing.T) {
	if err := validatePass(PassSelf); err != nil {
		t.Errorf("self is not accepted: %v", err)
	}
	if err := validatePass(PassThirdParty); err != nil {
		t.Errorf("third_party is not accepted: %v", err)
	}
	if err := validatePass("free"); err == nil {
		t.Error("an unknown pass was accepted")
	}
}

// The limiter spaces calls out; a vendor that throttles on short-term rate sees an
// even stream rather than a burst.
func TestRateLimiterSpacesCalls(t *testing.T) {
	limiter := NewRateLimiter(200) // 5ms apart

	start := time.Now()
	for range 4 {
		if err := limiter.Wait(context.Background()); err != nil {
			t.Fatalf("Wait returned %v", err)
		}
	}
	elapsed := time.Since(start)

	// Three gaps of 5ms after the first immediate call.
	if elapsed < 10*time.Millisecond {
		t.Fatalf("four calls took %s, want them spaced out", elapsed)
	}
}

func TestRateLimiterHonoursCancellation(t *testing.T) {
	limiter := NewRateLimiter(1) // one per second

	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatalf("the first call should not wait: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := limiter.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait returned %v, want the cancellation", err)
	}
}

func TestDecodeChecksToleratesAnEmptyDocument(t *testing.T) {
	checks, err := DecodeChecks(nil)
	if err != nil {
		t.Fatalf("DecodeChecks(nil) returned %v", err)
	}
	if checks == nil {
		t.Fatal("DecodeChecks returned a nil slice, which would serialise as null")
	}

	checks, err = DecodeChecks([]byte(`[{"key":"mx","label":"MX","status":"pass","points":30,"max":30}]`))
	if err != nil {
		t.Fatalf("DecodeChecks returned %v", err)
	}
	if len(checks) != 1 || checks[0].Points != 30 {
		t.Fatalf("checks = %+v", checks)
	}

	if _, err := DecodeChecks([]byte(`not json`)); err == nil {
		t.Error("a corrupt breakdown was accepted")
	}
}
