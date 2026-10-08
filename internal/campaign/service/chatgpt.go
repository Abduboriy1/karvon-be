package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/ai"
	"github.com/bory/karvon-be/internal/campaign/provider"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// ChatGPT connection statuses as the API reports them. The first two are stored;
// disconnected is the absence of a row.
const (
	ChatGPTConnected      = "connected"
	ChatGPTNeedsReconnect = "needs_reconnect"
	ChatGPTDisconnected   = "disconnected"
)

const (
	// chatGPTStateTTL is how long a sign-in attempt may take.
	chatGPTStateTTL = 10 * time.Minute
	// chatGPTRefreshSkew refreshes an access token this long before it expires, so
	// a token never runs out in the middle of a generation.
	chatGPTRefreshSkew = 3 * time.Minute
)

// errChatGPTNotConnected means a plan generation was attempted with no usable sign-in.
var errChatGPTNotConnected = errors.New("chatgpt: no account is connected")

// ChatGPTConnection is the sign-in state shown in the UI. It never carries a token.
type ChatGPTConnection struct {
	Status      string
	Email       *string
	ConnectedAt *time.Time
	LastError   *string
}

// ChatGPTSignInError is a failed callback. Reason is a short code the dashboard
// can show a message for; the cause is logged, never put in the redirect.
type ChatGPTSignInError struct {
	Reason string
	Err    error
}

func (e *ChatGPTSignInError) Error() string {
	return "chatgpt sign-in: " + e.Reason + ": " + e.Err.Error()
}
func (e *ChatGPTSignInError) Unwrap() error { return e.Err }

// Reasons a sign-in can fail.
const (
	SignInDenied        = "access_denied"
	SignInExpired       = "expired"
	SignInNoPlanAccess  = "no_plan_access"
	SignInFailed        = "failed"
	SignInNotConfigured = "not_configured"
)

// chatGPT is the Sign in with ChatGPT half of the generator; nil when no client id
// is configured.
type chatGPT struct {
	oauth    *ai.ChatGPTOAuth
	provider *ai.ChatGPTPlanProvider
}

// EnableChatGPT turns on Sign in with ChatGPT. Once an account is connected,
// generations run on its plan; until then s.ai keeps serving them.
func (s *Service) EnableChatGPT(oauth *ai.ChatGPTOAuth, cfg ai.ChatGPTConfig) {
	cfg.Tokens = chatGPTTokens{s: s}
	s.chatgpt = &chatGPT{oauth: oauth, provider: ai.NewChatGPT(cfg)}
}

// ChatGPTEnabled reports whether a client id is configured.
func (s *Service) ChatGPTEnabled() bool { return s.chatgpt != nil }

// generator picks the provider for this request: the ChatGPT plan when an account
// is connected, otherwise the configured fallback.
func (s *Service) generator(ctx context.Context) ai.Provider {
	if s.chatgpt == nil {
		return s.ai
	}
	conn, err := s.store.GetChatGPTConnection(ctx)
	if err != nil || conn.Status != ChatGPTConnected {
		return s.ai
	}
	return s.chatgpt.provider
}

// ChatGPTStatus reports the connection, or nil when the feature is off.
func (s *Service) ChatGPTStatus(ctx context.Context) (*ChatGPTConnection, error) {
	if s.chatgpt == nil {
		return nil, nil
	}
	conn, err := s.store.GetChatGPTConnection(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return &ChatGPTConnection{Status: ChatGPTDisconnected}, nil
	}
	if err != nil {
		return nil, apperr.Internal(err)
	}
	connectedAt := conn.ConnectedAt
	return &ChatGPTConnection{Status: conn.Status, Email: conn.Email, ConnectedAt: &connectedAt, LastError: conn.LastError}, nil
}

// StartChatGPTSignIn records a sign-in attempt and returns the URL to send the
// operator's browser to.
func (s *Service) StartChatGPTSignIn(ctx context.Context) (string, error) {
	if s.chatgpt == nil {
		return "", apperr.Conflict("Sign in with ChatGPT is not configured: set KARVON_CHATGPT_CLIENT_ID")
	}
	now := s.now()
	if err := s.store.PruneChatGPTOAuthStates(ctx, now); err != nil {
		return "", apperr.Internal(err)
	}
	attempt, err := ai.NewPKCE()
	if err != nil {
		return "", apperr.Internal(err)
	}
	verifierEnc, err := s.cipher.EncryptString(attempt.Verifier)
	if err != nil {
		return "", apperr.Internal(err)
	}
	if err := s.store.CreateChatGPTOAuthState(ctx, dbgen.CreateChatGPTOAuthStateParams{
		StateHash: hashState(attempt.State), CodeVerifierEnc: verifierEnc, Nonce: attempt.Nonce,
		ExpiresAt: now.Add(chatGPTStateTTL),
	}); err != nil {
		return "", apperr.Internal(err)
	}
	return s.chatgpt.oauth.AuthorizeURL(attempt), nil
}

// FinishChatGPTSignIn handles the browser's return from OpenAI. oauthErr is the
// callback's error parameter, set when the user declined. Every failure is a
// *ChatGPTSignInError.
func (s *Service) FinishChatGPTSignIn(ctx context.Context, state, code, oauthErr string) error {
	if s.chatgpt == nil {
		return &ChatGPTSignInError{Reason: SignInNotConfigured, Err: errors.New("no client id is configured")}
	}
	if state == "" {
		return &ChatGPTSignInError{Reason: SignInExpired, Err: errors.New("the callback carried no state")}
	}
	// The state is consumed whatever happens next: one attempt, one callback.
	row, err := s.store.TakeChatGPTOAuthState(ctx, hashState(state))
	if errors.Is(err, pgx.ErrNoRows) {
		return &ChatGPTSignInError{Reason: SignInExpired, Err: errors.New("unknown or already used state")}
	}
	if err != nil {
		return &ChatGPTSignInError{Reason: SignInFailed, Err: err}
	}
	now := s.now()
	if !now.Before(row.ExpiresAt) {
		return &ChatGPTSignInError{Reason: SignInExpired, Err: errors.New("the sign-in attempt expired")}
	}
	if oauthErr != "" {
		return &ChatGPTSignInError{Reason: SignInDenied, Err: errors.New(oauthErr)}
	}
	if code == "" {
		return &ChatGPTSignInError{Reason: SignInFailed, Err: errors.New("the callback carried no code")}
	}
	verifier, err := s.cipher.DecryptString(row.CodeVerifierEnc)
	if err != nil {
		return &ChatGPTSignInError{Reason: SignInFailed, Err: err}
	}

	tokens, identity, err := s.chatgpt.oauth.Exchange(ctx, code, ai.PKCE{State: state, Verifier: verifier, Nonce: row.Nonce}, now)
	if err != nil {
		return &ChatGPTSignInError{Reason: SignInFailed, Err: err}
	}
	if !tokens.HasPlanScope() {
		return &ChatGPTSignInError{Reason: SignInNoPlanAccess,
			Err: fmt.Errorf("granted scope %q lacks plan usage", tokens.Scope)}
	}
	accessEnc, refreshEnc, err := s.encryptTokens(tokens)
	if err != nil {
		return &ChatGPTSignInError{Reason: SignInFailed, Err: err}
	}
	if _, err := s.store.UpsertChatGPTConnection(ctx, dbgen.UpsertChatGPTConnectionParams{
		Subject: identity.Subject, Email: campaign.Optional(identity.Email), Scope: tokens.Scope,
		AccessTokenEnc: accessEnc, RefreshTokenEnc: refreshEnc, AccessExpiresAt: tokens.ExpiresAt,
	}); err != nil {
		return &ChatGPTSignInError{Reason: SignInFailed, Err: err}
	}
	s.log.Info("chatgpt account connected", "subject", identity.Subject)
	return nil
}

// DisconnectChatGPT forgets the stored tokens. OpenAI keeps the app listed under
// the user's ChatGPT settings until they remove it there.
func (s *Service) DisconnectChatGPT(ctx context.Context) error {
	if s.chatgpt == nil {
		return apperr.Conflict("Sign in with ChatGPT is not configured")
	}
	if err := s.store.DeleteChatGPTConnection(ctx); err != nil {
		return apperr.Internal(err)
	}
	return nil
}

// chatGPTTokens adapts the service to ai.TokenSource.
type chatGPTTokens struct{ s *Service }

// AccessToken implements ai.TokenSource.
func (t chatGPTTokens) AccessToken(ctx context.Context) (string, error) {
	return t.s.chatGPTAccessToken(ctx)
}

// chatGPTAccessToken returns a current access token, refreshing it under a row
// lock: the refresh token rotates, so two refreshes racing would leave one of them
// holding a dead token.
func (s *Service) chatGPTAccessToken(ctx context.Context) (string, error) {
	conn, err := s.store.GetChatGPTConnection(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errChatGPTNotConnected
	}
	if err != nil {
		return "", err
	}
	if conn.Status != ChatGPTConnected {
		return "", errChatGPTNotConnected
	}
	if s.now().Add(chatGPTRefreshSkew).Before(conn.AccessExpiresAt) {
		return s.cipher.DecryptString(conn.AccessTokenEnc)
	}

	var access string
	var grantErr error
	err = s.store.InTx(ctx, func(q *dbgen.Queries) error {
		locked, err := q.LockChatGPTConnection(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return errChatGPTNotConnected
		}
		if err != nil {
			return err
		}
		if locked.Status != ChatGPTConnected {
			return errChatGPTNotConnected
		}
		now := s.now()
		// Another request may have refreshed while this one waited for the lock.
		if now.Add(chatGPTRefreshSkew).Before(locked.AccessExpiresAt) {
			access, err = s.cipher.DecryptString(locked.AccessTokenEnc)
			return err
		}
		refresh, err := s.cipher.DecryptString(locked.RefreshTokenEnc)
		if err != nil {
			return err
		}
		tokens, err := s.chatgpt.oauth.Refresh(ctx, refresh, now)
		if errors.Is(err, ai.ErrOAuthGrant) {
			grantErr = err
			return nil
		}
		if err != nil {
			return err
		}
		accessEnc, refreshEnc, err := s.encryptTokens(tokens)
		if err != nil {
			return err
		}
		access = tokens.AccessToken
		return q.SetChatGPTTokens(ctx, dbgen.SetChatGPTTokensParams{
			AccessTokenEnc: accessEnc, RefreshTokenEnc: refreshEnc, AccessExpiresAt: tokens.ExpiresAt, Scope: tokens.Scope,
		})
	})
	if err != nil {
		return "", err
	}
	if grantErr != nil {
		s.markChatGPTNeedsReconnect(ctx, grantErr)
		return "", errChatGPTNotConnected
	}
	return access, nil
}

// markChatGPTNeedsReconnect records that OpenAI no longer accepts the sign-in.
func (s *Service) markChatGPTNeedsReconnect(ctx context.Context, cause error) {
	s.log.Warn("chatgpt sign-in rejected; the operator has to connect again", "error", cause)
	if err := s.store.SetChatGPTNeedsReconnect(ctx, campaign.Ptr(provider.Truncate(cause.Error(), 500))); err != nil {
		s.log.Error("could not record the chatgpt sign-in state", "error", err)
	}
}

func (s *Service) encryptTokens(tokens ai.TokenSet) ([]byte, []byte, error) {
	accessEnc, err := s.cipher.EncryptString(tokens.AccessToken)
	if err != nil {
		return nil, nil, err
	}
	refreshEnc, err := s.cipher.EncryptString(tokens.RefreshToken)
	if err != nil {
		return nil, nil, err
	}
	return accessEnc, refreshEnc, nil
}

func hashState(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}
