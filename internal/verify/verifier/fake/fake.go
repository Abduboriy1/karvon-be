// Package fake is a deterministic third-party verifier for tests. The suite never
// makes a real API call, and never spends a credit.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"

	"github.com/bory/karvon-be/internal/verify/verifier"
)

// Verifier answers from a script, then from a per-address rule, then from a stable
// hash of the address so an unscripted test still gets a repeatable mix.
type Verifier struct {
	mu sync.Mutex

	// ByEmail pins the verdict for specific addresses.
	ByEmail map[string]verifier.Status
	// ErrByEmail makes specific addresses fail.
	ErrByEmail map[string]error
	// Err fails every call, for outage tests.
	Err error
	// FailTimes makes the next N calls fail with Err before succeeding, which is
	// how retry and backoff behaviour is exercised.
	FailTimes int
	// Credits is what Balance reports.
	Credits int64
	// BalanceErr makes Balance fail.
	BalanceErr error

	calls   int
	byEmail map[string]int
}

// New builds a fake with a credit balance.
func New(credits int64) *Verifier {
	return &Verifier{
		ByEmail:    map[string]verifier.Status{},
		ErrByEmail: map[string]error{},
		Credits:    credits,
		byEmail:    map[string]int{},
	}
}

// Name implements verifier.Verifier.
func (v *Verifier) Name() string { return "fake" }

// Verify implements verifier.Verifier.
func (v *Verifier) Verify(_ context.Context, email string) (verifier.Result, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	email = strings.ToLower(strings.TrimSpace(email))
	v.calls++
	v.byEmail[email]++

	if err, ok := v.ErrByEmail[email]; ok {
		return verifier.Result{}, err
	}
	if v.FailTimes > 0 {
		v.FailTimes--
		if v.Err != nil {
			return verifier.Result{}, v.Err
		}
		return verifier.Result{}, fmt.Errorf("fake: transient failure")
	}
	if v.Err != nil {
		return verifier.Result{}, v.Err
	}

	status, ok := v.ByEmail[email]
	if !ok {
		status = statusForEmail(email)
	}

	credits := 1
	if status == verifier.StatusUnknown {
		credits = 0
	}
	if v.Credits > 0 {
		v.Credits -= int64(credits)
	}

	raw, _ := json.Marshal(map[string]any{"state": string(status), "email": email})
	return verifier.Result{
		Status:      status,
		Reason:      "fake_" + string(status),
		Raw:         raw,
		CreditsUsed: credits,
	}, nil
}

// Balance implements verifier.Verifier.
func (v *Verifier) Balance(context.Context) (verifier.Balance, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.BalanceErr != nil {
		return verifier.Balance{}, v.BalanceErr
	}
	return verifier.Balance{Credits: v.Credits}, nil
}

// Calls is how many times Verify ran, which is how a test proves the 90-day cache
// stopped a second charge.
func (v *Verifier) Calls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls
}

// CallsFor is Calls for one address.
func (v *Verifier) CallsFor(email string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.byEmail[strings.ToLower(strings.TrimSpace(email))]
}

// statusForEmail gives an unscripted address a stable verdict: roughly seven in ten
// deliverable, the rest split between risky and undeliverable.
func statusForEmail(email string) verifier.Status {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(email))
	switch hash.Sum32() % 10 {
	case 7:
		return verifier.StatusRisky
	case 8:
		return verifier.StatusUndeliverable
	case 9:
		return verifier.StatusUnknown
	default:
		return verifier.StatusDeliverable
	}
}

var _ verifier.Verifier = (*Verifier)(nil)
