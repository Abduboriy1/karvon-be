package registrar

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
)

// unconfirmedGrace is how long a registration Cloudflare has no record of is given
// to appear before the call is treated as never having arrived.
const unconfirmedGrace = 2 * time.Minute

// maxRegisterAttempts bounds how often one domain is sent to Cloudflare. A second
// send is only ever made after Cloudflare has shown it has no record of the first.
const maxRegisterAttempts = 3

// PurchaseWorker registers the domains of one purchase, one at a time.
type PurchaseWorker struct {
	river.WorkerDefaults[PurchaseArgs]
	svc *Service
}

// NewPurchaseWorker builds the worker.
func NewPurchaseWorker(svc *Service) *PurchaseWorker {
	return &PurchaseWorker{svc: svc}
}

// Timeout covers ten registrations, each of which Cloudflare may hold for its full
// synchronous window, plus the checks around them.
func (w *PurchaseWorker) Timeout(*river.Job[PurchaseArgs]) time.Duration {
	return 10 * time.Minute
}

// Work advances the purchase as far as it can. Registrations Cloudflare is still
// working on are polled by snoozing, which costs no attempts.
func (w *PurchaseWorker) Work(ctx context.Context, job *river.Job[PurchaseArgs]) error {
	err := w.svc.advance(ctx, job.Args.PurchaseID)
	var snooze *rivertype.JobSnoozeError
	if err != nil && !errors.As(err, &snooze) && job.Attempt >= job.MaxAttempts {
		// The last attempt. Settle the purchase so it stops holding the one
		// in-flight slot, handing anything unconfirmed to a person.
		w.svc.abandon(context.WithoutCancel(ctx), job.Args.PurchaseID, err)
	}
	return err
}

// advance drives one purchase: reconcile registrations already sent, re-check the
// ones not yet sent, register those still at or under their confirmed price, and
// settle the purchase once nothing is left in flight.
func (s *Service) advance(ctx context.Context, id uuid.UUID) error {
	purchase, err := s.store.GetDomainPurchase(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("registrar: load purchase: %w", err)
	}
	if purchase.Status != PurchaseQueued && purchase.Status != PurchaseProcessing {
		return nil
	}
	if purchase, err = s.store.StartDomainPurchase(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("registrar: start purchase: %w", err)
	}

	items, err := s.store.ListDomainPurchaseItems(ctx, id)
	if err != nil {
		return fmt.Errorf("registrar: load purchase items: %w", err)
	}

	client, _, err := s.client(ctx, true)
	if err != nil {
		if countStatus(items, ItemRegistering) > 0 {
			// Something was sent and its outcome is unknown; that can only be
			// settled by asking Cloudflare, so wait for the connection to return.
			return fmt.Errorf("registrar: cannot reach Cloudflare to confirm registrations: %w", err)
		}
		s.failPending(ctx, items, ItemFailed, CodeNotConfigured, apperr.From(err).Message)
		return s.finish(ctx, id)
	}

	// 1. Registrations already sent: ask how they went.
	for _, item := range items {
		if item.Status != ItemRegistering {
			continue
		}
		if err := s.reconcile(ctx, client, item); err != nil {
			return err
		}
	}

	// 2. Registrations not yet sent: check them once more, then send them.
	if items, err = s.store.ListDomainPurchaseItems(ctx, id); err != nil {
		return fmt.Errorf("registrar: load purchase items: %w", err)
	}
	ready, err := s.recheck(ctx, client, purchase, items)
	if err != nil {
		return err
	}
	for _, item := range ready {
		if err := ctx.Err(); err != nil {
			return err
		}
		if stop := s.register(ctx, client, purchase, item); stop {
			if items, err = s.store.ListDomainPurchaseItems(ctx, id); err == nil {
				s.failPending(ctx, items, ItemFailed, CodeAuthFailed,
					"Cloudflare rejected the API token, so this domain was not registered")
			}
			break
		}
	}

	// 3. Settle, or come back for whatever Cloudflare is still working on.
	if items, err = s.store.ListDomainPurchaseItems(ctx, id); err != nil {
		return fmt.Errorf("registrar: load purchase items: %w", err)
	}
	if countStatus(items, ItemRegistering) > 0 || countStatus(items, ItemPending) > 0 {
		started := purchase.CreatedAt
		if purchase.StartedAt != nil {
			started = *purchase.StartedAt
		}
		if s.now().Sub(started) < s.cfg.MaxWait {
			return river.JobSnooze(s.cfg.PollInterval)
		}
		for _, item := range items {
			if item.Status == ItemRegistering {
				s.setItemFailed(ctx, item, ItemActionRequired, CodeTimedOut,
					"Cloudflare had not finished this registration when the purchase stopped waiting; check the Cloudflare dashboard")
			}
		}
		s.failPending(ctx, items, ItemFailed, CodeTimedOut, "the purchase stopped waiting before this domain was sent")
	}
	return s.finish(ctx, id)
}

// recheck runs the authoritative availability check for every unsent domain right
// before registering and drops the ones that are gone or dearer than confirmed.
func (s *Service) recheck(ctx context.Context, client *cloudflare.Client, purchase dbgen.DomainPurchase,
	items []dbgen.DomainPurchaseItem,
) ([]dbgen.DomainPurchaseItem, error) {
	var pending []dbgen.DomainPurchaseItem
	names := make([]string, 0, len(items))
	for _, item := range items {
		if item.Status == ItemPending {
			pending = append(pending, item)
			names = append(names, item.DomainName)
		}
	}
	if len(pending) == 0 {
		return nil, nil
	}

	offers, err := s.checkAll(ctx, client, names)
	if err != nil {
		if errors.Is(err, cloudflare.ErrAuth) {
			s.failPending(ctx, pending, ItemFailed, CodeAuthFailed,
				"Cloudflare rejected the API token, so this domain was not registered")
			return nil, nil
		}
		return nil, fmt.Errorf("registrar: check domains: %w", err)
	}

	ready := make([]dbgen.DomainPurchaseItem, 0, len(pending))
	for _, item := range pending {
		offer, found := offers[item.DomainName]
		code, message := refusal(item.DomainName, offer, found, item.QuotedCostCents)
		if code == "" && offer.Pricing.Currency != purchase.Currency {
			code, message = CodeCurrencyMismatch, fmt.Sprintf("%s is now priced in %s, not %s", item.DomainName,
				offer.Pricing.Currency, purchase.Currency)
		}
		if code != "" {
			if item.RegisterAttempts > 0 && owned(ctx, client, item.DomainName) {
				// An earlier send that Cloudflare had not recorded landed after
				// all; the domain is gone because it is ours.
				s.setItemSucceeded(ctx, item, nil)
				continue
			}
			s.setItemFailed(ctx, item, ItemFailed, code, message)
			continue
		}
		cost, renewal := offer.Pricing.RegistrationCostCents, offer.Pricing.RenewalCostCents
		if err := s.store.SetDomainItemPrice(ctx, dbgen.SetDomainItemPriceParams{
			ID: item.ID, CostCents: &cost, RenewalCostCents: &renewal,
		}); err != nil {
			return nil, fmt.Errorf("registrar: record price: %w", err)
		}
		ready = append(ready, item)
	}
	return ready, nil
}

// register sends one registration. The row says "registering" before the call, so
// if this process dies mid-call the next attempt asks Cloudflare instead of sending
// it again. It reports true when the token was rejected and nothing more should be
// sent.
func (s *Service) register(ctx context.Context, client *cloudflare.Client, purchase dbgen.DomainPurchase,
	item dbgen.DomainPurchaseItem,
) bool {
	marked, err := s.store.MarkDomainItemRegistering(ctx, item.ID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.log.Warn("could not mark a domain as registering", "domain", item.DomainName, "error", err)
		}
		// Either another worker has it or the database is unwell; nothing was sent.
		return false
	}

	workflow, err := client.Register(ctx, cloudflare.RegisterInput{
		DomainName: marked.DomainName,
		AutoRenew:  purchase.AutoRenew,
	})
	if err != nil {
		switch {
		case errors.Is(err, cloudflare.ErrAuth):
			s.setItemFailed(ctx, marked, ItemFailed, CodeAuthFailed,
				"Cloudflare rejected the API token, so this domain was not registered")
			return true
		case cloudflare.Definite(err):
			message := "Cloudflare refused the registration"
			var apiErr *cloudflare.APIError
			if errors.As(err, &apiErr) && apiErr.Detail() != "" {
				message = apiErr.Detail()
			}
			s.setItemFailed(ctx, marked, ItemFailed, CodeRejected, message)
		default:
			// The request may or may not have reached Cloudflare. The row stays
			// "registering" and the next pass asks Cloudflare which it was.
			s.log.Warn("domain registration outcome unknown, will confirm with Cloudflare",
				"purchase_id", purchase.ID, "domain", marked.DomainName, "error", err)
		}
		return false
	}

	s.log.Info("domain registration answered", "purchase_id", purchase.ID,
		"domain", marked.DomainName, "state", workflow.State)
	s.applyWorkflow(ctx, client, marked, workflow)
	return false
}

// reconcile settles a registration that was sent, by asking Cloudflare about it.
func (s *Service) reconcile(ctx context.Context, client *cloudflare.Client, item dbgen.DomainPurchaseItem) error {
	workflow, err := client.RegistrationStatus(ctx, item.DomainName)
	if err == nil {
		s.applyWorkflow(ctx, client, item, workflow)
		return nil
	}
	if !errors.Is(err, cloudflare.ErrNotFound) {
		return fmt.Errorf("registrar: registration status for %s: %w", item.DomainName, err)
	}

	// No workflow on record. If the account owns the domain, it was ours.
	if _, err := client.GetRegistration(ctx, item.DomainName); err == nil {
		s.setItemSucceeded(ctx, item, nil)
		return nil
	} else if !errors.Is(err, cloudflare.ErrNotFound) {
		return fmt.Errorf("registrar: registration for %s: %w", item.DomainName, err)
	}

	attempted := item.UpdatedAt
	if item.AttemptedAt != nil {
		attempted = *item.AttemptedAt
	}
	if s.now().Sub(attempted) < unconfirmedGrace {
		// Too soon to conclude anything; look again on the next pass.
		return nil
	}
	if int(item.RegisterAttempts) >= maxRegisterAttempts {
		s.setItemFailed(ctx, item, ItemActionRequired, CodeUnconfirmed,
			"Cloudflare never confirmed this registration; check the Cloudflare dashboard before trying again")
		return nil
	}
	// Cloudflare has no trace of the send, so it never arrived. Sending again is
	// safe: a domain can be registered once, and the re-check before it notices if
	// the first send landed after all.
	if err := s.store.ResetDomainItemPending(ctx, item.ID); err != nil {
		return fmt.Errorf("registrar: reset %s: %w", item.DomainName, err)
	}
	s.log.Warn("domain registration never reached Cloudflare, sending again",
		"domain", item.DomainName, "attempts", item.RegisterAttempts)
	return nil
}

// applyWorkflow records what Cloudflare said about a registration. A workflow still
// running leaves the row "registering" for the next pass.
func (s *Service) applyWorkflow(ctx context.Context, client *cloudflare.Client, item dbgen.DomainPurchaseItem,
	workflow cloudflare.Workflow,
) {
	switch workflow.State {
	case cloudflare.StateSucceeded:
		reg := workflow.Context.Registration
		if reg == nil {
			if fetched, err := client.GetRegistration(ctx, item.DomainName); err == nil {
				reg = &fetched
			}
		}
		s.setItemSucceeded(ctx, item, reg)
	case cloudflare.StateFailed:
		code, message := CodeFailed, "Cloudflare could not register this domain"
		if workflow.Error != nil {
			if workflow.Error.Code != "" {
				code = workflow.Error.Code.String()
			}
			if workflow.Error.Message != "" {
				message = workflow.Error.Message
			}
		}
		s.setItemFailed(ctx, item, ItemFailed, code, message)
	case cloudflare.StateActionRequired, cloudflare.StateBlocked:
		message := "Cloudflare needs you to finish this registration in the dashboard"
		if workflow.Error != nil && workflow.Error.Message != "" {
			message = workflow.Error.Message
		}
		s.setItemFailed(ctx, item, ItemActionRequired, workflow.State, message)
	}
}

func (s *Service) setItemSucceeded(ctx context.Context, item dbgen.DomainPurchaseItem, reg *cloudflare.Registration) {
	params := dbgen.SucceedDomainItemParams{ID: item.ID}
	if reg != nil {
		params.RegisteredAt = reg.CreatedAt
		params.ExpiresAt = reg.ExpiresAt
	}
	if err := s.store.SucceedDomainItem(ctx, params); err != nil {
		// Logged loudly: the domain is bought but the record says otherwise until
		// the next pass reconciles it.
		s.log.Error("could not record a registered domain", "domain", item.DomainName, "error", err)
		return
	}
	s.log.Info("domain registered", "purchase_id", item.PurchaseID, "domain", item.DomainName)
}

func (s *Service) setItemFailed(ctx context.Context, item dbgen.DomainPurchaseItem, status, code, message string) {
	if err := s.store.FailDomainItem(ctx, dbgen.FailDomainItemParams{
		ID: item.ID, Status: status, ErrorCode: &code, ErrorMessage: &message,
	}); err != nil {
		s.log.Warn("could not record a domain outcome", "domain", item.DomainName, "error", err)
	}
}

// failPending fails every domain that was never sent.
func (s *Service) failPending(ctx context.Context, items []dbgen.DomainPurchaseItem, status, code, message string) {
	for _, item := range items {
		if item.Status == ItemPending {
			s.setItemFailed(ctx, item, status, code, message)
		}
	}
}

// finish settles the purchase from its items.
func (s *Service) finish(ctx context.Context, id uuid.UUID) error {
	items, err := s.store.ListDomainPurchaseItems(ctx, id)
	if err != nil {
		return fmt.Errorf("registrar: load purchase items: %w", err)
	}
	succeeded := countStatus(items, ItemSucceeded)
	status := PurchasePartial
	var message *string
	switch {
	case succeeded == len(items):
		status = PurchaseSucceeded
	case succeeded == 0:
		status = PurchaseFailed
		text := "no domain was registered"
		message = &text
	default:
		text := fmt.Sprintf("%d of %d domains were not registered", len(items)-succeeded, len(items))
		message = &text
	}
	if _, err := s.store.FinishDomainPurchase(ctx, dbgen.FinishDomainPurchaseParams{
		ID: id, Status: status, Error: message,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("registrar: finish purchase: %w", err)
	}
	s.log.Info("domain purchase settled", "purchase_id", id, "status", status,
		"registered", succeeded, "domains", len(items))
	return nil
}

// abandon settles a purchase whose worker has run out of attempts. Unsent domains
// fail; sent-but-unconfirmed ones go to a person, because only Cloudflare knows
// whether they were bought.
func (s *Service) abandon(ctx context.Context, id uuid.UUID, cause error) {
	items, err := s.store.ListDomainPurchaseItems(ctx, id)
	if err != nil {
		s.log.Error("could not abandon a domain purchase", "purchase_id", id, "error", err)
		return
	}
	reason := fmt.Sprintf("the purchase stopped after repeated errors: %v", cause)
	for _, item := range items {
		switch item.Status {
		case ItemPending:
			s.setItemFailed(ctx, item, ItemFailed, CodeAborted, reason)
		case ItemRegistering:
			s.setItemFailed(ctx, item, ItemActionRequired, CodeUnconfirmed,
				"Cloudflare's answer for this registration was never received; check the Cloudflare dashboard before trying again")
		}
	}
	if err := s.finish(ctx, id); err != nil {
		s.log.Error("could not settle an abandoned domain purchase", "purchase_id", id, "error", err)
	}
}

func owned(ctx context.Context, client *cloudflare.Client, domain string) bool {
	_, err := client.GetRegistration(ctx, domain)
	return err == nil
}

func countStatus(items []dbgen.DomainPurchaseItem, status string) int {
	n := 0
	for _, item := range items {
		if item.Status == status {
			n++
		}
	}
	return n
}
