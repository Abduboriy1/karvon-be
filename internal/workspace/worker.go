package workspace

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/registrar/cloudflare"
	"github.com/bory/karvon-be/internal/workspace/google"
)

// SetupWorker sets up one domain.
type SetupWorker struct {
	river.WorkerDefaults[SetupArgs]
	svc *Service
}

// NewSetupWorker builds the worker.
func NewSetupWorker(svc *Service) *SetupWorker {
	return &SetupWorker{svc: svc}
}

// Timeout covers one pass: a handful of DNS writes and up to five user creations.
func (w *SetupWorker) Timeout(*river.Job[SetupArgs]) time.Duration {
	return 5 * time.Minute
}

// Work advances the setup as far as it can. Waiting for Google to see the
// verification record is done by snoozing, which costs no attempts.
func (w *SetupWorker) Work(ctx context.Context, job *river.Job[SetupArgs]) error {
	err := w.svc.advance(ctx, job.Args.DomainID)
	var snooze *rivertype.JobSnoozeError
	if err != nil && !errors.As(err, &snooze) && job.Attempt >= job.MaxAttempts {
		// The last attempt. Hand the domain to a person; a retry resumes it.
		w.svc.fail(context.WithoutCancel(ctx), job.Args.DomainID, CodeAborted,
			fmt.Sprintf("the setup stopped after repeated errors: %v; retry it", err))
	}
	return err
}

// advance drives one domain: add it to Workspace, publish its records, have Google
// verify it, create its mailboxes, then wait for the operator's DKIM key. Every step
// is recorded when done, so a later pass or a retry skips it.
func (s *Service) advance(ctx context.Context, id uuid.UUID) error {
	row, err := s.store.GetWorkspaceDomain(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("workspace: load domain: %w", err)
	}
	if row.Status != DomainProvisioning {
		return nil
	}

	client, _, err := s.client(ctx, true)
	if err != nil {
		s.fail(ctx, id, CodeNotConfigured, apperr.From(err).Message)
		return nil
	}
	dns, err := s.dns.DNSClient(ctx)
	if err != nil {
		s.fail(ctx, id, CodeNotConfigured, apperr.From(err).Message)
		return nil
	}

	// 1. The domain in Workspace.
	domain, err := s.ensureDomain(ctx, client, row)
	if err != nil {
		return s.stepFailed(ctx, row, err, CodeDomainRejected)
	}

	// 2. The records: mail, and the verification token while Google needs it.
	if row.DnsPublishedAt == nil {
		if !domain.Verified && row.VerificationToken == nil {
			token, err := client.VerificationToken(ctx, row.DomainName)
			if err != nil {
				return s.stepFailed(ctx, row, err, CodeDomainRejected)
			}
			if err := s.store.SetWorkspaceVerificationToken(ctx, dbgen.SetWorkspaceVerificationTokenParams{
				ID: id, VerificationToken: &token,
			}); err != nil {
				return fmt.Errorf("workspace: store verification token: %w", err)
			}
			row.VerificationToken = &token
		}
		if err := publishRecords(ctx, dns, row.DomainName, row.VerificationToken); err != nil {
			return s.stepFailed(ctx, row, err, CodeDNSRejected)
		}
		if err := s.store.MarkWorkspaceDNSPublished(ctx, id); err != nil {
			return fmt.Errorf("workspace: record DNS: %w", err)
		}
		published := s.now()
		row.DnsPublishedAt = &published
		s.log.Info("workspace DNS published", "domain", row.DomainName)
	}

	// 3. Verification.
	if row.VerifiedAt == nil {
		if !domain.Verified {
			if err := client.Verify(ctx, row.DomainName); err != nil {
				if google.Definite(err) && !errors.Is(err, google.ErrAuth) {
					// Google cannot see the record yet; DNS caches take a while.
					return s.awaitVerification(ctx, row, err)
				}
				return s.stepFailed(ctx, row, err, CodeDomainRejected)
			}
			// Site verification can land a moment before Workspace shows it.
			if domain, err = client.GetDomain(ctx, row.DomainName); err != nil {
				return s.stepFailed(ctx, row, err, CodeDomainRejected)
			}
			if !domain.Verified {
				return s.awaitVerification(ctx, row, nil)
			}
		}
		if err := s.store.MarkWorkspaceDomainVerified(ctx, id); err != nil {
			return fmt.Errorf("workspace: record verification: %w", err)
		}
		s.log.Info("workspace domain verified", "domain", row.DomainName)
	}

	// 4. Mailboxes.
	mailboxes, err := s.store.ListWorkspaceMailboxes(ctx, id)
	if err != nil {
		return fmt.Errorf("workspace: load mailboxes: %w", err)
	}
	for _, mb := range mailboxes {
		if mb.Status != MailboxPending {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.createMailbox(ctx, client, mb); err != nil {
			return s.stepFailed(ctx, row, err, CodeMailboxRejected)
		}
	}

	// 5. Everything Google has an API for is done.
	done, err := s.store.FinishWorkspaceProvisioning(ctx, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("workspace: finish: %w", err)
	}
	s.log.Info("workspace setup provisioned", "domain", row.DomainName, "status", done.Status)
	return nil
}

// ensureDomain adds the domain to Workspace unless it is there already.
func (s *Service) ensureDomain(ctx context.Context, client *google.Client, row dbgen.WorkspaceDomain) (google.Domain, error) {
	domain, err := client.GetDomain(ctx, row.DomainName)
	if errors.Is(err, google.ErrNotFound) {
		domain, err = client.InsertDomain(ctx, row.DomainName)
		if errors.Is(err, google.ErrExists) {
			domain, err = client.GetDomain(ctx, row.DomainName)
			if errors.Is(err, google.ErrNotFound) {
				return google.Domain{}, &dnsConflict{fmt.Sprintf("Google says %s is already in use, but not in this "+
					"Workspace account; it may belong to another Google Workspace or Cloud Identity account", row.DomainName)}
			}
		}
	}
	if err != nil {
		return google.Domain{}, err
	}
	if row.AddedAt == nil {
		if err := s.store.MarkWorkspaceDomainAdded(ctx, row.ID); err != nil {
			return google.Domain{}, fmt.Errorf("workspace: record domain: %w", err)
		}
		s.log.Info("workspace domain added", "domain", row.DomainName, "verified", domain.Verified)
	}
	return domain, nil
}

// awaitVerification comes back later, or gives up once the records have been out
// longer than VerifyMaxWait.
func (s *Service) awaitVerification(ctx context.Context, row dbgen.WorkspaceDomain, cause error) error {
	since := s.now()
	if row.DnsPublishedAt != nil {
		since = *row.DnsPublishedAt
	}
	if s.now().Sub(since) < s.cfg.VerifyMaxWait {
		return river.JobSnooze(s.cfg.PollInterval)
	}
	message := fmt.Sprintf("Google could not verify %s within %s of the records being published; "+
		"check the google-site-verification TXT record in Cloudflare and retry", row.DomainName, s.cfg.VerifyMaxWait)
	var apiErr *google.APIError
	if errors.As(cause, &apiErr) && apiErr.Detail() != "" {
		message += " (" + apiErr.Detail() + ")"
	}
	s.fail(ctx, row.ID, CodeVerificationTimedOut, message)
	return nil
}

// createMailbox creates one user. The attempt is counted before the call; a "this
// address exists" answer on the first attempt means someone else has it, and on a
// later one that an earlier call of ours landed without its answer arriving.
func (s *Service) createMailbox(ctx context.Context, client *google.Client, mb dbgen.WorkspaceMailbox) error {
	marked, err := s.store.CountWorkspaceMailboxAttempt(ctx, mb.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // no longer pending
	}
	if err != nil {
		return fmt.Errorf("workspace: count attempt for %s: %w", mb.Email, err)
	}
	password, err := s.cipher.DecryptString(mb.PasswordEnc)
	if err != nil {
		return fmt.Errorf("workspace: decrypt password for %s: %w", mb.Email, err)
	}

	user, err := client.InsertUser(ctx, google.NewUser{
		Email: mb.Email, GivenName: mb.GivenName, FamilyName: mb.FamilyName, Password: password,
	})
	switch {
	case err == nil:
	case errors.Is(err, google.ErrExists) && marked.CreateAttempts <= 1:
		s.failMailbox(ctx, mb, CodeAddressTaken,
			"this address already exists in Google Workspace and was not created by Karvon, so it was left alone")
		return nil
	case errors.Is(err, google.ErrExists):
		if user, err = client.GetUser(ctx, mb.Email); err != nil {
			return err
		}
	case errors.Is(err, google.ErrAuth), errors.Is(err, google.ErrRateLimited):
		// Refused before Google looked at it: this attempt does not count.
		if err := s.store.UncountWorkspaceMailboxAttempt(ctx, mb.ID); err != nil {
			s.log.Warn("could not uncount a mailbox attempt", "email", mb.Email, "error", err)
		}
		return err
	case google.Definite(err):
		message := "Google refused to create this mailbox"
		var apiErr *google.APIError
		if errors.As(err, &apiErr) && apiErr.Detail() != "" {
			message = apiErr.Detail()
		}
		s.failMailbox(ctx, mb, CodeMailboxRejected, message)
		return nil
	default:
		// The request may or may not have reached Google; the next pass sends it
		// again and Google's uniqueness settles which.
		return err
	}

	userID := user.ID
	if err := s.store.SucceedWorkspaceMailbox(ctx, dbgen.SucceedWorkspaceMailboxParams{ID: mb.ID, GoogleUserID: &userID}); err != nil {
		// Logged loudly: the licence exists but the record says otherwise until the
		// next pass finds it.
		s.log.Error("could not record a created mailbox", "email", mb.Email, "error", err)
		return fmt.Errorf("workspace: record mailbox: %w", err)
	}
	s.log.Info("workspace mailbox created", "email", mb.Email)
	return nil
}

// stepFailed settles an error from a step. A refusal that a person has to act on
// fails the domain; anything that may pass on its own is returned for River to retry.
func (s *Service) stepFailed(ctx context.Context, row dbgen.WorkspaceDomain, err error, refusedCode string) error {
	var conflict *dnsConflict
	var googleErr *google.APIError
	var cloudflareErr *cloudflare.APIError
	switch {
	case errors.As(err, &conflict):
		s.fail(ctx, row.ID, CodeDNSConflict, conflict.message)
	case errors.Is(err, google.ErrAuth):
		s.fail(ctx, row.ID, CodeAuthFailed, authMessage(err))
	case errors.Is(err, cloudflare.ErrAuth):
		s.fail(ctx, row.ID, CodeDNSAuthFailed, dnsAuthMessage)
	case errors.Is(err, cloudflare.ErrNotFound):
		s.fail(ctx, row.ID, CodeZoneNotFound, fmt.Sprintf("%s has no DNS zone in the Cloudflare account", row.DomainName))
	case errors.As(err, &googleErr) && google.Definite(err):
		s.fail(ctx, row.ID, refusedCode, "Google refused: "+detailOr(googleErr.Detail(), err))
	case errors.As(err, &cloudflareErr) && cloudflare.Definite(err):
		s.fail(ctx, row.ID, CodeDNSRejected, "Cloudflare refused: "+detailOr(cloudflareErr.Detail(), err))
	default:
		s.log.Warn("workspace setup step failed, will retry", "domain", row.DomainName, "error", err)
		return fmt.Errorf("workspace: %s: %w", row.DomainName, err)
	}
	return nil
}

func detailOr(detail string, err error) string {
	if detail != "" {
		return detail
	}
	return err.Error()
}

// fail hands a provisioning domain to a person.
func (s *Service) fail(ctx context.Context, id uuid.UUID, code, message string) {
	if err := s.store.FailWorkspaceDomain(ctx, dbgen.FailWorkspaceDomainParams{
		ID: id, ErrorCode: &code, ErrorMessage: &message,
	}); err != nil {
		s.log.Error("could not record a failed workspace setup", "domain_id", id, "error", err)
		return
	}
	s.log.Warn("workspace setup failed", "domain_id", id, "code", code, "message", message)
}

func (s *Service) failMailbox(ctx context.Context, mb dbgen.WorkspaceMailbox, code, message string) {
	if err := s.store.FailWorkspaceMailbox(ctx, dbgen.FailWorkspaceMailboxParams{
		ID: mb.ID, ErrorCode: &code, ErrorMessage: &message,
	}); err != nil {
		s.log.Warn("could not record a mailbox outcome", "email", mb.Email, "error", err)
	}
}
