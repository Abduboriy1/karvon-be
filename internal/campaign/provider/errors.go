// Package provider holds what the Instantly and Mailchimp clients share: the
// sentinel errors the jobs react to, and the retry-after carrier.
//
// It imports nothing from the verification module on purpose; the two provider
// families evolve independently.
package provider

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Errors the jobs react to differently. Anything else is a transient failure and is
// retried by the queue.
var (
	// ErrAuth means the key was rejected: no retry can help.
	ErrAuth = errors.New("provider: authentication failed")
	// ErrPaymentRequired means the provider workspace has no active plan.
	ErrPaymentRequired = errors.New("provider: payment required")
	// ErrRateLimited means the provider throttled us; the job is snoozed.
	ErrRateLimited = errors.New("provider: rate limited")
	// ErrNotFound means the provider no longer has the resource.
	ErrNotFound = errors.New("provider: not found")
	// ErrInvalid means the provider rejected the request body.
	ErrInvalid = errors.New("provider: invalid request")
	// ErrComplianceState is Mailchimp refusing to (re)subscribe a member who
	// unsubscribed, bounced or was flagged. Only the member can undo that.
	ErrComplianceState = errors.New("provider: member is in a compliance state")
	// ErrMemberExists is Mailchimp refusing a POST for an address it already has.
	ErrMemberExists = errors.New("provider: member already exists")
	// ErrNotConfigured means no key is stored for the provider.
	ErrNotConfigured = errors.New("provider: not configured")
	// ErrCircuitOpen is returned while the breaker is open.
	ErrCircuitOpen = errors.New("provider: circuit open, provider is unavailable")
)

// RetryAfterError carries a provider-supplied wait time alongside a sentinel.
type RetryAfterError struct {
	Err   error
	After time.Duration
}

// Error implements error.
func (e *RetryAfterError) Error() string { return fmt.Sprintf("%v (retry after %s)", e.Err, e.After) }

// Unwrap lets errors.Is find the sentinel.
func (e *RetryAfterError) Unwrap() error { return e.Err }

// RetryAfter extracts a provider-suggested delay, reporting false when there is none.
func RetryAfter(err error) (time.Duration, bool) {
	var retry *RetryAfterError
	if errors.As(err, &retry) && retry.After > 0 {
		return retry.After, true
	}
	return 0, false
}

// StatusError is a non-2xx response that mapped to no sentinel.
type StatusError struct {
	Provider string
	Code     int
	Body     string
}

// Error implements error.
func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s: unexpected status %d", e.Provider, e.Code)
	}
	return fmt.Sprintf("%s: unexpected status %d: %s", e.Provider, e.Code, e.Body)
}

// Retryable reports whether repeating a request could plausibly help.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrAuth), errors.Is(err, ErrPaymentRequired), errors.Is(err, ErrNotFound),
		errors.Is(err, ErrInvalid), errors.Is(err, ErrComplianceState), errors.Is(err, ErrMemberExists),
		errors.Is(err, ErrNotConfigured):
		return false
	case errors.Is(err, ErrRateLimited):
		return true
	}
	var status *StatusError
	if errors.As(err, &status) {
		return status.Code == http.StatusTooManyRequests || status.Code >= 500
	}
	return true
}

// ParseRetryAfter reads a Retry-After header, which providers send as seconds or as
// an HTTP date.
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

// Truncate shortens a body for an error message.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
