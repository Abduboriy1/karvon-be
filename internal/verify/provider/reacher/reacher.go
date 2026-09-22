// Package reacher talks to a self-hosted Reacher (check-if-email-exists) backend.
//
// Reacher is the only verification provider that opens an SMTP conversation with the
// recipient's mail server, which makes it the most informative free signal we have
// and also the slowest and the most fragile: it depends on a separate container, on
// outbound port 25, and on mail servers that greylist, throttle and lie. Every
// design choice in this file follows from that — a bounded number of in-flight
// requests, a per-request deadline, a small number of retries, and a circuit breaker
// so that a Reacher that is simply down costs one connection attempt per cooldown
// rather than one per address.
//
// Nothing here can fail a verification. Every failure mode becomes a Result whose
// status says the provider contributed nothing, and the scoring layer redistributes
// its weight.
//
// Licensing: Reacher is dual-licensed AGPL-3.0 / commercial. This package speaks to
// it over its documented HTTP API from a separate process; no Reacher code is linked
// into this binary. See "Multi-stage verification plan (karvon-be).md" §7.
package reacher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bory/karvon-be/internal/verify/provider"
)

// DefaultBaseURL is where the Compose stack publishes the backend.
const DefaultBaseURL = "http://localhost:8081"

// SecretHeader is the shared-secret header Reacher's `header_secret` config expects.
const SecretHeader = "x-reacher-secret" //nolint:gosec // G101: a header name, not a credential

// maxBodyBytes bounds how much of a response is read, so a misbehaving backend
// cannot exhaust memory. Reacher's payload is a couple of kilobytes.
const maxBodyBytes = 1 << 20

// Normalized scores. Reacher reports a four-value verdict plus SMTP detail; these
// are the points each combination is worth on our 0-100 scale. A plain "risky" is
// 70, the same value the paid pass gives a risky verdict, so the two scales agree.
const (
	// ScoreSafe means the mail server accepted the recipient.
	ScoreSafe = 100
	// ScoreRisky is a mailbox the backend will not vouch for.
	ScoreRisky = 70
	// ScoreCatchAll is a domain that accepts everything, so the mailbox itself is
	// unproven. Worth less than a plain risky verdict, which at least concerns
	// this address.
	ScoreCatchAll = 60
	// ScoreFullInbox is a real mailbox that cannot currently receive mail.
	ScoreFullInbox = 50
	// ScoreInvalid means the address does not exist.
	ScoreInvalid = 0
)

// Config configures the client.
type Config struct {
	// BaseURL is the root of the Reacher backend, without a trailing slash.
	BaseURL string
	// Secret is sent as x-reacher-secret. Empty when the backend has no
	// header_secret configured.
	Secret string
	// Timeout bounds one verification, including retries of a single attempt.
	Timeout time.Duration
	// Retries is how many times a failed attempt is repeated. 0 means one attempt.
	Retries int
	// Concurrency caps in-flight requests, so we never outrun the backend's own
	// throttle. Zero or less means 4.
	Concurrency int
	// BreakerThreshold is how many consecutive failures open the circuit. Zero or
	// less disables the breaker.
	BreakerThreshold int
	// BreakerCooldown is how long the circuit stays open.
	BreakerCooldown time.Duration
	// HelloName and FromEmail override the backend's SMTP identity per request.
	// Empty leaves the backend's own configuration in charge, which is the norm.
	HelloName string
	FromEmail string

	HTTPClient *http.Client
	Log        *slog.Logger
	// Now is swappable so the breaker can be tested without sleeping.
	Now func() time.Time
}

// Client is a Reacher backend client. It is safe for concurrent use.
type Client struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger
	now  func() time.Time

	// sem bounds in-flight requests.
	sem chan struct{}

	mu       sync.Mutex
	failures int
	openTill time.Time
}

// ErrCircuitOpen is returned while the breaker is open. It is not a verification
// failure: the address is simply scored without Reacher.
var ErrCircuitOpen = errors.New("reacher: backend is unavailable, circuit is open")

// New builds a client. It never fails: a misconfigured client reports every address
// as unavailable rather than preventing the service from starting.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.BreakerCooldown <= 0 {
		cfg.BreakerCooldown = time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		// No client-level timeout: each call carries its own context deadline, so
		// the deadline covers the retries as a whole rather than each attempt.
		httpClient = &http.Client{}
	}

	return &Client{
		cfg:  cfg,
		http: httpClient,
		log:  cfg.Log,
		now:  cfg.Now,
		sem:  make(chan struct{}, cfg.Concurrency),
	}
}

// Key implements provider.Provider.
func (*Client) Key() provider.Key { return provider.KeyReacher }

/* ----------------------------------------------------------------- the wire */

type checkRequest struct {
	ToEmail   string `json:"to_email"`
	HelloName string `json:"hello_name,omitempty"`
	FromEmail string `json:"from_email,omitempty"`
}

// CheckResponse is Reacher's CheckEmailOutput, narrowed to the fields we normalize.
//
// misc, mx and smtp are a union of the detail object and an error object in
// Reacher's schema, so every field is optional and a missing section is read as "we
// learned nothing here" rather than as a decode failure.
type CheckResponse struct {
	Input       string `json:"input"`
	IsReachable string `json:"is_reachable"`
	Syntax      struct {
		Domain        string `json:"domain"`
		Username      string `json:"username"`
		IsValidSyntax bool   `json:"is_valid_syntax"`
	} `json:"syntax"`
	Mx struct {
		AcceptsMail bool     `json:"accepts_mail"`
		Records     []string `json:"records"`
	} `json:"mx"`
	SMTP struct {
		CanConnectSMTP bool `json:"can_connect_smtp"`
		HasFullInbox   bool `json:"has_full_inbox"`
		IsCatchAll     bool `json:"is_catch_all"`
		IsDeliverable  bool `json:"is_deliverable"`
		IsDisabled     bool `json:"is_disabled"`
	} `json:"smtp"`
	Misc struct {
		IsDisposable  bool `json:"is_disposable"`
		IsRoleAccount bool `json:"is_role_account"`
	} `json:"misc"`
}

// The four values of Reacher's is_reachable enum.
const (
	ReachableSafe    = "safe"
	ReachableRisky   = "risky"
	ReachableInvalid = "invalid"
	ReachableUnknown = "unknown"
)

/* --------------------------------------------------------------------- check */

// Check implements provider.Provider. It never returns an error: every failure is a
// Result that contributes nothing to the weighted score.
func (c *Client) Check(ctx context.Context, email string) provider.Result {
	started := c.now()

	if open, until := c.circuitOpen(); open {
		return withDuration(provider.Unavailable(provider.KeyReacher, ErrCircuitOpen,
			fmt.Sprintf("the Reacher backend is unavailable; retrying after %s",
				until.Format(time.TimeOnly))), c.now().Sub(started))
	}

	resp, err := c.check(ctx, email)
	if err != nil {
		c.recordFailure(err)
		return withDuration(c.failureResult(err), c.now().Sub(started))
	}
	c.recordSuccess()
	return withDuration(Normalize(resp), c.now().Sub(started))
}

// failureResult separates "the backend is not usable" from "the backend answered
// badly", because only the first is worth opening the circuit over in the operator's
// eyes. Both contribute nothing to the score.
func (c *Client) failureResult(err error) provider.Result {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return provider.Unavailable(provider.KeyReacher, err,
			"the Reacher backend did not answer within the timeout")
	}
	var status *statusError
	if errors.As(err, &status) && status.code == http.StatusTooManyRequests {
		return provider.Unavailable(provider.KeyReacher, err,
			"the Reacher backend is throttling; this address was not checked")
	}
	return provider.Failed(provider.KeyReacher, err)
}

// check performs the request with retries and concurrency control.
func (c *Client) check(ctx context.Context, email string) (CheckResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	// Hold a slot for the whole call. Waiting for one is part of the timeout: an
	// address that cannot get a slot in time is better skipped than queued behind
	// a backlog.
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return CheckResponse{}, fmt.Errorf("reacher: waiting for a request slot: %w", ctx.Err())
	}

	var lastErr error
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return CheckResponse{}, err
			}
		}
		resp, err := c.attempt(ctx, email)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryable(err) {
			break
		}
		c.log.Debug("retrying a Reacher verification",
			"email", email, "attempt", attempt+1, "error", err)
	}
	return CheckResponse{}, lastErr
}

// attempt performs exactly one request.
func (c *Client) attempt(ctx context.Context, email string) (CheckResponse, error) {
	payload, err := json.Marshal(checkRequest{
		ToEmail:   email,
		HelloName: c.cfg.HelloName,
		FromEmail: c.cfg.FromEmail,
	})
	if err != nil {
		return CheckResponse{}, fmt.Errorf("reacher: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.BaseURL+"/v1/check_email", bytes.NewReader(payload))
	if err != nil {
		return CheckResponse{}, fmt.Errorf("reacher: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.cfg.Secret != "" {
		req.Header.Set(SecretHeader, c.cfg.Secret)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Surface the context cause rather than the wrapped transport error, so
		// the caller can tell a timeout from a refused connection.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CheckResponse{}, fmt.Errorf("reacher: request: %w", ctxErr)
		}
		return CheckResponse{}, fmt.Errorf("reacher: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode != http.StatusOK {
		return CheckResponse{}, &statusError{code: resp.StatusCode, body: truncate(string(body), 300)}
	}
	if readErr != nil {
		return CheckResponse{}, fmt.Errorf("reacher: read response: %w", readErr)
	}

	var out CheckResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return CheckResponse{}, fmt.Errorf("reacher: decode response: %w", err)
	}
	return out, nil
}

/* -------------------------------------------------------------------- health */

// Health implements provider.HealthChecker against Reacher's unauthenticated
// GET /version, so the settings page can show whether the backend is reachable.
func (c *Client) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/version", nil)
	if err != nil {
		return fmt.Errorf("reacher: build health request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reacher: health request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))

	if resp.StatusCode != http.StatusOK {
		return &statusError{code: resp.StatusCode}
	}
	return nil
}

/* ------------------------------------------------------------------ breaker */

// circuitOpen reports whether the breaker is currently short-circuiting calls.
func (c *Client) circuitOpen() (bool, time.Time) {
	if c.cfg.BreakerThreshold <= 0 {
		return false, time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.openTill.IsZero() || !c.now().Before(c.openTill) {
		return false, time.Time{}
	}
	return true, c.openTill
}

func (c *Client) recordFailure(err error) {
	if c.cfg.BreakerThreshold <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures++
	if c.failures < c.cfg.BreakerThreshold {
		return
	}
	c.openTill = c.now().Add(c.cfg.BreakerCooldown)
	c.failures = 0
	c.log.Warn("the Reacher backend keeps failing; pausing calls",
		"cooldown", c.cfg.BreakerCooldown, "error", err)
}

func (c *Client) recordSuccess() {
	if c.cfg.BreakerThreshold <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures = 0
	c.openTill = time.Time{}
}

/* -------------------------------------------------------------- normalization */

// Normalize maps one Reacher verdict onto our 0-100 scale.
//
// The verdict alone is too coarse: "risky" covers a catch-all domain, a role
// mailbox and a full inbox, which are worth very different amounts to someone about
// to send an email. The SMTP detail is what separates them.
func Normalize(resp CheckResponse) provider.Result {
	meta := map[string]any{
		"is_reachable":     resp.IsReachable,
		"can_connect_smtp": resp.SMTP.CanConnectSMTP,
		"accepts_mail":     resp.Mx.AcceptsMail,
	}
	if resp.SMTP.IsCatchAll {
		meta["is_catch_all"] = true
	}
	if resp.Misc.IsRoleAccount {
		meta["is_role_account"] = true
	}
	if resp.Misc.IsDisposable {
		meta["is_disposable"] = true
	}

	scored := func(score int, reason string) provider.Result {
		return provider.Scored(provider.KeyReacher, score, reason).WithMetadata(meta)
	}

	// A disposable domain is disqualifying whatever the SMTP conversation said:
	// the mailbox may well accept mail today and be gone tomorrow. A rejected
	// recipient is not treated the same way, because "invalid" is the verdict most
	// prone to false negatives from greylisting and blocked ports.
	if resp.Misc.IsDisposable {
		return scored(ScoreInvalid, "the domain is a disposable mail provider").Disqualify()
	}

	switch strings.ToLower(strings.TrimSpace(resp.IsReachable)) {
	case ReachableSafe:
		return scored(ScoreSafe, "the mail server accepted the recipient")

	case ReachableInvalid:
		switch {
		case !resp.Syntax.IsValidSyntax:
			return scored(ScoreInvalid, "the address is not valid syntax")
		case !resp.Mx.AcceptsMail:
			return scored(ScoreInvalid, "the domain does not accept mail")
		case resp.SMTP.IsDisabled:
			return scored(ScoreInvalid, "the mailbox has been disabled")
		default:
			return scored(ScoreInvalid, "the mail server rejected the recipient")
		}

	case ReachableRisky:
		switch {
		case resp.SMTP.HasFullInbox:
			return scored(ScoreFullInbox, "the mailbox exists but its inbox is full")
		case resp.SMTP.IsCatchAll:
			return scored(ScoreCatchAll, "the domain accepts every address, so this mailbox is unproven")
		case resp.Misc.IsRoleAccount:
			return scored(ScoreRisky, "a shared role mailbox the server will not vouch for")
		default:
			return scored(ScoreRisky, "the mail server would not confirm the recipient")
		}

	default:
		// "unknown" is Reacher declining to answer: greylisting, a blocked port
		// 25, or a provider that never gives a straight answer. It must not drag
		// the score down, so it contributes nothing and its weight moves to the
		// providers that did answer.
		return provider.Inconclusive(provider.KeyReacher,
			"the mail server gave no usable answer").WithMetadata(meta)
	}
}

/* ------------------------------------------------------------------- helpers */

// statusError is a non-200 response.
type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("reacher: unexpected status %d", e.code)
	}
	return fmt.Sprintf("reacher: unexpected status %d: %s", e.code, e.body)
}

// retryable reports whether repeating the request could plausibly help. A 4xx other
// than 429 is our mistake and will fail identically every time.
func retryable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var status *statusError
	if errors.As(err, &status) {
		return status.code == http.StatusTooManyRequests || status.code >= 500
	}
	// A transport or decode failure: worth one more go.
	return true
}

// backoff is the delay before attempt n (1-based), doubling from 250ms.
func backoff(attempt int) time.Duration {
	delay := 250 * time.Millisecond << (attempt - 1)
	if delay > 5*time.Second {
		delay = 5 * time.Second
	}
	return delay
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("reacher: backoff interrupted: %w", ctx.Err())
	}
}

func withDuration(r provider.Result, d time.Duration) provider.Result {
	r.Duration = d
	return r
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// compile-time proof that the client satisfies both interfaces.
var (
	_ provider.Provider      = (*Client)(nil)
	_ provider.HealthChecker = (*Client)(nil)
)
