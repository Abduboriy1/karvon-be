// Package workspace is the mailboxes module: it sets up Google Workspace mail on
// domains whose DNS lives in the Cloudflare account, and creates the mailboxes the
// sending tool connects to.
//
// One Workspace account holds every domain as a secondary domain. For each domain the
// module adds it to Workspace, publishes the verification, MX, SPF and DMARC records
// through Cloudflare, waits for Google to verify it and creates up to
// MaxMailboxesPerDomain users. DKIM is the one step Google has no API for: the
// operator generates the key in the Admin console, hands it back, and the module
// publishes it.
//
// Every mailbox is a paid Workspace licence, so a setup is explicitly confirmed and
// capped, and a mailbox is never created twice: Google refuses a second user with the
// same address, and the attempt is counted before each call so that refusal can be
// told apart from someone else owning the address.
package workspace

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/queue"
	"github.com/bory/karvon-be/internal/registrar/cloudflare"
)

// The source row holding the service-account key.
const (
	RoleMailboxes       = "mailboxes"
	KindGoogleWorkspace = "google_workspace"
)

// MaxMailboxesPerDomain is the most mailboxes one domain gets. It is a hard rule, not
// a setting: the schema allows mailbox positions 0-4 only.
const MaxMailboxesPerDomain = 5

// MaxKeyLength bounds what a client may send as a service-account JSON key. Google's
// keys are about 2.4 KB.
const MaxKeyLength = 10_000

// Domain statuses, mirroring the workspace_domains.status check constraint.
const (
	DomainProvisioning = "provisioning"
	DomainDKIMRequired = "dkim_required"
	DomainActive       = "active"
	DomainFailed       = "failed"
)

// Mailbox statuses, mirroring the workspace_mailboxes.status check constraint.
const (
	MailboxPending = "pending"
	MailboxCreated = "created"
	MailboxFailed  = "failed"
)

// Error codes this module sets on a domain or a mailbox.
const (
	CodeNotConfigured        = "not_configured"
	CodeAuthFailed           = "auth_failed"
	CodeDNSAuthFailed        = "dns_auth_failed"
	CodeZoneNotFound         = "zone_not_found"
	CodeDNSConflict          = "dns_conflict"
	CodeDNSRejected          = "dns_rejected"
	CodeDomainRejected       = "domain_rejected"
	CodeVerificationTimedOut = "verification_timed_out"
	CodeAddressTaken         = "address_taken"
	CodeMailboxRejected      = "mailbox_rejected"
	CodeAborted              = "aborted"
)

// The records Google Workspace mail needs, as Google documents them for new setups.
const (
	MXHost              = "smtp.google.com"
	MXPriority          = 1
	SPFRecord           = "v=spf1 include:_spf.google.com ~all"
	SPFInclude          = "include:_spf.google.com"
	DMARCRecord         = "v=DMARC1; p=none"
	DefaultDKIMSelector = "google"
)

// KindSetup is the River job kind. It is persisted in the queue table, so renaming it
// is a migration rather than a refactor.
const KindSetup = "workspace_domain_setup"

// SetupArgs drives one domain from provisioning to dkim_required, or to failed.
//
// It is deliberately not a River unique job. Exactly one job exists per stretch of
// provisioning because only two things insert one, each in the transaction that
// moves the domain into provisioning: creating it, and a retry, which only a failed
// domain accepts. A job that fails its domain does so as its last write, so the job a
// retry inserts never overlaps with it, even while River has yet to mark the old one
// completed — the window in which a uniqueness check would drop the retry.
type SetupArgs struct {
	DomainID uuid.UUID `json:"domain_id"`
}

// Kind implements river.JobArgs.
func (SetupArgs) Kind() string { return KindSetup }

// InsertOpts implements river.JobArgsWithInsertOpts. Snoozes while Google verifies
// the domain do not count as attempts.
func (SetupArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: queue.QueueWorkspace, MaxAttempts: 10}
}

// localPartPattern is the mailbox part of an address Workspace accepts: letters,
// digits, dots, dashes and underscores, not starting or ending with a dot.
var localPartPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)

// selectorPattern is a DKIM selector: one DNS label.
var selectorPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeLocalPart lower-cases and checks the part of an address before the @.
func NormalizeLocalPart(raw string) (string, error) {
	local := strings.ToLower(strings.TrimSpace(raw))
	if !localPartPattern.MatchString(local) || strings.Contains(local, "..") {
		return "", errors.New("must be 1-64 letters, digits, dots, dashes or underscores, not starting or ending with a dot")
	}
	return local, nil
}

// NormalizeName trims a first or last name and checks Workspace will take it.
func NormalizeName(raw string) (string, error) {
	name := strings.Join(strings.Fields(raw), " ")
	if n := len([]rune(name)); n == 0 || n > 60 {
		return "", errors.New("must be between 1 and 60 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) || strings.ContainsRune("<>=", r) {
			return "", fmt.Errorf("must not contain %q", r)
		}
	}
	return name, nil
}

// NormalizeAdminEmail lower-cases and checks the admin address.
func NormalizeAdminEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || !strings.Contains(domain, ".") || strings.ContainsAny(email, " \t\r\n") ||
		strings.Contains(domain, "@") || len(email) > 254 {
		return "", errors.New("must be the email address of a Workspace super admin")
	}
	return email, nil
}

// NormalizeSelector lower-cases a DKIM selector, defaulting to Google's "google".
func NormalizeSelector(raw string) (string, error) {
	selector := strings.ToLower(strings.TrimSpace(raw))
	if selector == "" {
		return DefaultDKIMSelector, nil
	}
	if !selectorPattern.MatchString(selector) {
		return "", errors.New("must be one DNS label, e.g. google")
	}
	return selector, nil
}

// NormalizeDKIM checks the TXT value the Admin console shows for DKIM and returns it
// as one unquoted string, "v=DKIM1; k=rsa; p=...". Whitespace inside a tag (a key
// pasted with line breaks) is dropped.
func NormalizeDKIM(raw string) (string, error) {
	var tags []string
	for _, tag := range strings.Split(cloudflare.TXTValue(raw), ";") {
		tag = strings.Join(strings.Fields(tag), "")
		if tag != "" {
			tags = append(tags, tag)
		}
	}
	value := strings.Join(tags, "; ")
	switch {
	case len(value) > 4096:
		return "", errors.New("is longer than any DKIM record")
	case !strings.HasPrefix(value, "v=DKIM1"):
		return "", errors.New(`must be the TXT value from the Admin console, starting with "v=DKIM1"`)
	case !strings.Contains(value, "p="):
		return "", errors.New(`has no public key ("p=")`)
	}
	return value, nil
}
