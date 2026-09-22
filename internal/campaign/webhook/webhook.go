// Package webhook parses and verifies the inbound webhooks from the two delivery
// providers. Instantly posts JSON guarded only by a shared secret header; Mailchimp
// posts a form guarded by a timestamped HMAC signature. Both parsers are pure and
// produce a normalised event plus a stable dedupe key, leaving persistence and
// side effects to the caller.
package webhook

import (
	"crypto/subtle"
	"errors"
)

// SecretHeader carries the shared secret Instantly is configured to send.
const SecretHeader = "X-Karvon-Webhook-Secret" //nolint:gosec

// Sentinel errors shared by both parsers and verifiers.
var (
	ErrMissingEventType = errors.New("webhook: missing event type")
	ErrMissingSignature = errors.New("webhook: missing or malformed signature")
	ErrStaleSignature   = errors.New("webhook: signature timestamp outside tolerance")
	ErrBadSignature     = errors.New("webhook: signature mismatch")
)

// VerifySecret compares a presented secret with the configured one in constant
// time. It is false when no secret is configured, so an unset secret never opens
// the endpoint.
func VerifySecret(got, want string) bool {
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
