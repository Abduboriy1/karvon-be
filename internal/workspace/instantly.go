package workspace

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/campaign/provider/instantly"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/queue"
)

// Instantly connection states, mirroring the workspace_mailboxes.instantly_status
// check constraint. NULL means never connected.
const (
	InstantlyConnecting = "connecting"
	InstantlyConnected  = "connected"
	InstantlyFailed     = "failed"
	InstantlyExpired    = "expired"
)

// instantlyGrace is how long past a session's expiry it is still asked about, for a
// sign-in finished in its last seconds.
const instantlyGrace = time.Minute

// InstantlyConnector is the campaign module's Instantly connection: the client for
// the stored API key, and the accounts sync that mirrors a new account into
// sending_accounts.
type InstantlyConnector interface {
	Instantly(ctx context.Context) (instantly.Client, error)
	SyncSendingAccounts(ctx context.Context) error
}

// SetInstantly wires the campaign module in once it exists.
func (s *Service) SetInstantly(connector InstantlyConnector) { s.instantly = connector }

// KindInstantlyConnect is the River job kind that follows one OAuth session.
const KindInstantlyConnect = "workspace_instantly_connect"

// InstantlyConnectArgs follows one session until it succeeds, fails or expires. The
// session id is part of the job, so replacing a session leaves the old job nothing
// to write.
type InstantlyConnectArgs struct {
	MailboxID uuid.UUID `json:"mailbox_id"`
	SessionID string    `json:"session_id"`
}

// Kind implements river.JobArgs.
func (InstantlyConnectArgs) Kind() string { return KindInstantlyConnect }

// InsertOpts implements river.JobArgsWithInsertOpts. Polling is snoozing, which
// costs no attempts; attempts are spent only on errors.
func (InstantlyConnectArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: queue.QueueWorkspace, MaxAttempts: 10}
}

// InstantlyConnection is a started connection: the Google sign-in to open, and until
// when it works.
type InstantlyConnection struct {
	Mailbox   dbgen.WorkspaceMailbox
	AuthURL   string
	ExpiresAt time.Time
}

// ConnectInstantly starts connecting a created mailbox to Instantly through
// Instantly's Google OAuth flow. A person opens the returned URL and signs in as the
// mailbox; a worker follows the session and, once Instantly has the account, turns
// warmup on when asked and refreshes the sending accounts. Asking again while a
// session is still open returns that session.
func (s *Service) ConnectInstantly(ctx context.Context, id uuid.UUID, warmup bool) (InstantlyConnection, error) {
	mb, err := s.store.GetWorkspaceMailbox(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return InstantlyConnection{}, apperr.NotFound("mailbox")
	}
	if err != nil {
		return InstantlyConnection{}, apperr.Internal(err)
	}
	switch {
	case mb.Status != MailboxCreated:
		return InstantlyConnection{}, apperr.Conflict("%s has not been created in Workspace (%s)", mb.Email, mb.Status)
	case mb.InstantlyStatus != nil && *mb.InstantlyStatus == InstantlyConnected:
		return InstantlyConnection{}, apperr.Conflict("%s is already connected to Instantly", mb.Email)
	case mb.InstantlyStatus != nil && *mb.InstantlyStatus == InstantlyConnecting && mb.InstantlyAuthUrl != nil &&
		mb.InstantlySessionExpiresAt != nil && s.now().Add(time.Minute).Before(*mb.InstantlySessionExpiresAt):
		return InstantlyConnection{Mailbox: mb, AuthURL: *mb.InstantlyAuthUrl, ExpiresAt: *mb.InstantlySessionExpiresAt}, nil
	}
	if s.instantly == nil || s.queue == nil {
		return InstantlyConnection{}, apperr.Internal(errors.New("workspace: Instantly or the queue is not wired"))
	}
	client, err := s.instantly.Instantly(ctx)
	if err != nil {
		return InstantlyConnection{}, err
	}
	session, err := client.StartGoogleOAuth(ctx)
	if err != nil {
		return InstantlyConnection{}, instantlyError(err, "start the Instantly connection")
	}
	authURL := withLoginHint(session.AuthURL, mb.Email)

	var started dbgen.WorkspaceMailbox
	err = s.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		var err error
		started, err = dbgen.New(tx).StartInstantlyConnection(ctx, dbgen.StartInstantlyConnectionParams{
			ID: id, SessionID: &session.SessionID, AuthUrl: &authURL, ExpiresAt: &session.ExpiresAt, Warmup: warmup,
		})
		if err != nil {
			return err
		}
		_, err = s.queue.InsertTx(ctx, tx, InstantlyConnectArgs{MailboxID: id, SessionID: session.SessionID}, nil)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return InstantlyConnection{}, apperr.Conflict("%s changed while connecting; reload it", mb.Email)
	}
	if err != nil {
		return InstantlyConnection{}, apperr.Internal(err)
	}
	s.log.Info("instantly connection started", "email", mb.Email, "expires_at", session.ExpiresAt)
	return InstantlyConnection{Mailbox: started, AuthURL: authURL, ExpiresAt: session.ExpiresAt}, nil
}

// withLoginHint pre-fills the mailbox on Google's sign-in page. Only a Google URL is
// touched; anything else is returned as Instantly sent it.
func withLoginHint(raw, email string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host != "accounts.google.com" {
		return raw
	}
	query := u.Query()
	query.Set("login_hint", email)
	u.RawQuery = query.Encode()
	return u.String()
}

// instantlyError turns an Instantly failure into the error the client sees.
func instantlyError(err error, action string) error {
	switch {
	case errors.Is(err, provider.ErrAuth):
		return apperr.ProviderAuth("Instantly rejected the API key; it needs the accounts:create, accounts:read and " +
			"accounts:update scopes").WithCause(err)
	case errors.Is(err, provider.ErrPaymentRequired):
		return apperr.ProviderError("the Instantly workspace has no active plan").WithCause(err)
	case errors.Is(err, provider.ErrRateLimited):
		return apperr.ProviderError("Instantly is rate limiting; try again in a minute").WithCause(err)
	}
	return apperr.ProviderError("could not %s", action).WithCause(err)
}

// InstantlyConnectWorker follows one OAuth session.
type InstantlyConnectWorker struct {
	river.WorkerDefaults[InstantlyConnectArgs]
	svc *Service
}

// NewInstantlyConnectWorker builds the worker.
func NewInstantlyConnectWorker(svc *Service) *InstantlyConnectWorker {
	return &InstantlyConnectWorker{svc: svc}
}

// Work asks Instantly how the session is going and records the end of it.
func (w *InstantlyConnectWorker) Work(ctx context.Context, job *river.Job[InstantlyConnectArgs]) error {
	err := w.svc.followInstantly(ctx, job.Args)
	if err != nil && job.Attempt >= job.MaxAttempts {
		w.svc.endInstantly(context.WithoutCancel(ctx), job.Args, InstantlyFailed,
			fmt.Sprintf("Karvon stopped following the connection after repeated errors: %v; connect again", err))
	}
	return err
}

func (s *Service) followInstantly(ctx context.Context, args InstantlyConnectArgs) error {
	mb, err := s.store.GetWorkspaceMailbox(ctx, args.MailboxID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("workspace: load mailbox: %w", err)
	}
	if mb.InstantlyStatus == nil || *mb.InstantlyStatus != InstantlyConnecting ||
		mb.InstantlySessionID == nil || *mb.InstantlySessionID != args.SessionID {
		return nil // replaced or already settled
	}
	if s.instantly == nil {
		return errors.New("workspace: Instantly is not wired")
	}
	client, err := s.instantly.Instantly(ctx)
	if err != nil {
		s.endInstantly(ctx, args, InstantlyFailed, apperr.From(err).Message)
		return nil
	}

	status, err := client.OAuthSessionStatus(ctx, args.SessionID)
	switch {
	case errors.Is(err, provider.ErrNotFound):
		s.endInstantly(ctx, args, InstantlyExpired, "")
		return nil
	case errors.Is(err, provider.ErrRateLimited):
		return river.JobSnooze(s.cfg.InstantlyPollInterval * 5)
	case errors.Is(err, provider.ErrAuth):
		s.endInstantly(ctx, args, InstantlyFailed, apperr.From(instantlyError(err, "")).Message)
		return nil
	case err != nil:
		return fmt.Errorf("workspace: instantly session %s: %w", args.SessionID, err)
	}

	switch status.Status {
	case instantly.OAuthSuccess:
		return s.instantlyConnected(ctx, client, mb, args, status)
	case instantly.OAuthError:
		message := status.ErrorDescription
		if message == "" {
			message = "Instantly could not connect the mailbox (" + status.Error + ")"
		}
		s.endInstantly(ctx, args, InstantlyFailed, message)
		return nil
	case instantly.OAuthExpired:
		s.endInstantly(ctx, args, InstantlyExpired, "")
		return nil
	}
	// Still pending.
	if mb.InstantlySessionExpiresAt != nil && s.now().After(mb.InstantlySessionExpiresAt.Add(instantlyGrace)) {
		s.endInstantly(ctx, args, InstantlyExpired, "")
		return nil
	}
	return river.JobSnooze(s.cfg.InstantlyPollInterval)
}

// instantlyConnected records the account, turns warmup on when it was asked for, and
// refreshes the sending accounts so it can be put on a campaign.
func (s *Service) instantlyConnected(ctx context.Context, client instantly.Client, mb dbgen.WorkspaceMailbox,
	args InstantlyConnectArgs, status instantly.OAuthStatus,
) error {
	if !strings.EqualFold(strings.TrimSpace(status.Email), mb.Email) {
		s.endInstantly(ctx, args, InstantlyFailed, fmt.Sprintf(
			"Instantly connected %s, not this mailbox; sign in as %s and connect again", status.Email, mb.Email))
		return nil
	}
	accountID := status.AccountID
	if _, err := s.store.ConnectInstantly(ctx, dbgen.ConnectInstantlyParams{
		ID: mb.ID, SessionID: &args.SessionID, AccountID: &accountID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("workspace: record instantly connection: %w", err)
	}
	s.log.Info("mailbox connected to instantly", "email", mb.Email)

	if mb.InstantlyWarmup {
		params := dbgen.SetInstantlyWarmupResultParams{ID: mb.ID}
		if _, err := client.EnableWarmup(ctx, []string{mb.Email}); err != nil {
			message := "connected, but Instantly would not turn warmup on: " + apperr.From(instantlyError(err, "enable warmup")).Message
			params.Error = &message
			s.log.Warn("instantly warmup not enabled", "email", mb.Email, "error", err)
		} else {
			now := s.now()
			params.EnabledAt = &now
		}
		if err := s.store.SetInstantlyWarmupResult(ctx, params); err != nil {
			s.log.Warn("could not record the warmup result", "email", mb.Email, "error", err)
		}
	}
	if err := s.instantly.SyncSendingAccounts(ctx); err != nil {
		s.log.Warn("could not queue the sending accounts sync", "error", err)
	}
	return nil
}

func (s *Service) endInstantly(ctx context.Context, args InstantlyConnectArgs, status, message string) {
	params := dbgen.EndInstantlyConnectionParams{ID: args.MailboxID, SessionID: &args.SessionID, Status: &status}
	if message != "" {
		params.Error = &message
	}
	if err := s.store.EndInstantlyConnection(ctx, params); err != nil {
		s.log.Warn("could not record the end of an instantly connection", "mailbox_id", args.MailboxID, "error", err)
		return
	}
	s.log.Info("instantly connection ended", "mailbox_id", args.MailboxID, "status", status, "message", message)
}
