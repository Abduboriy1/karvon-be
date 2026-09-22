// Package verifier abstracts the third-party email verification vendors behind one
// interface, so swapping providers is a new package rather than a change to the
// verification pipeline.
package verifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Status is the vendor-neutral verdict. Every implementation maps its own vocabulary
// onto exactly these four values; the scoring layer knows nothing else.
type Status string

// The four verdicts.
const (
	// StatusDeliverable means the mailbox accepts mail.
	StatusDeliverable Status = "deliverable"
	// StatusRisky covers catch-all domains, role accounts and low-quality mailboxes
	// the vendor will not vouch for.
	StatusRisky Status = "risky"
	// StatusUnknown means the vendor could not decide. The Pass 1 score stands and
	// the address is retried later; it is never cached.
	StatusUnknown Status = "unknown"
	// StatusUndeliverable means the mailbox does not exist.
	StatusUndeliverable Status = "undeliverable"
)

// Result is one verification outcome.
type Result struct {
	Status Status
	// Reason is the vendor's own reason code, surfaced in the UI as-is.
	Reason string
	// Raw is the untouched vendor payload, stored so a disputed verdict can be
	// examined without calling out again.
	Raw json.RawMessage
	// CreditsUsed is how much the call cost. A cached vendor-side answer can be 0.
	CreditsUsed int
}

// Balance is the credit the account has left.
type Balance struct {
	Credits int64
}

// Verifier is one third-party provider.
type Verifier interface {
	// Verify checks a single address. Implementations must honour ctx.
	Verify(ctx context.Context, email string) (Result, error)
	// Balance reports remaining credit, and doubles as the connection test.
	Balance(ctx context.Context) (Balance, error)
	// Name is the human-readable provider name used in logs.
	Name() string
}

// Errors the pipeline reacts to differently. Anything else is a transient failure
// and is retried by the queue.
var (
	// ErrAuth means the key was rejected: retrying cannot help, so the run fails.
	ErrAuth = errors.New("verifier: authentication failed")
	// ErrInsufficientCredits means the account is out of credit; the run fails
	// rather than burning attempts.
	ErrInsufficientCredits = errors.New("verifier: insufficient credits")
	// ErrRateLimited means the vendor throttled us; the job is snoozed.
	ErrRateLimited = errors.New("verifier: rate limited")
	// ErrPending means the vendor accepted the address but has no answer yet. The
	// address is recorded as unknown and picked up by a later run.
	ErrPending = errors.New("verifier: result is not ready")
)

// RetryAfterError carries a vendor-supplied wait time alongside a sentinel error.
type RetryAfterError struct {
	Err   error
	After time.Duration
}

// Error implements error.
func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("%v (retry after %s)", e.Err, e.After)
}

// Unwrap lets errors.Is find the sentinel.
func (e *RetryAfterError) Unwrap() error { return e.Err }

// RetryAfter extracts a vendor-suggested delay, reporting false when there is none.
func RetryAfter(err error) (time.Duration, bool) {
	var retry *RetryAfterError
	if errors.As(err, &retry) && retry.After > 0 {
		return retry.After, true
	}
	return 0, false
}

// Options configure a provider client.
type Options struct {
	APIKey     string
	BaseURL    string
	Timeout    time.Duration
	HTTPClient *http.Client
}

// Client returns the HTTP client to use, defaulting to one bounded by Timeout.
func (o Options) Client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

// ParseRetryAfter reads the Retry-After header, which vendors send as seconds.
func ParseRetryAfter(header string, fallback time.Duration) time.Duration {
	if header == "" {
		return fallback
	}
	if seconds, err := time.ParseDuration(header + "s"); err == nil && seconds > 0 {
		return seconds
	}
	if at, err := http.ParseTime(header); err == nil {
		if wait := time.Until(at); wait > 0 {
			return wait
		}
	}
	return fallback
}
