// Package google is the Google Workspace client the workspace module needs: the Admin
// SDK Directory API for domains and users, and the Site Verification API that proves
// a domain belongs to the account. It authenticates as a service account with
// domain-wide delegation, acting as one super admin, and keeps every Google-specific
// field name and error shape inside this package.
//
// Creating a user adds a paid licence. It is never repeated by this client: a lost
// answer is settled by the caller, who can rely on Google refusing a second user with
// the same address.
package google

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Production endpoints.
const (
	DefaultTokenURL            = "https://oauth2.googleapis.com/token" //nolint:gosec // G101: an endpoint, not a credential
	DefaultDirectoryURL        = "https://admin.googleapis.com/admin/directory/v1"
	DefaultSiteVerificationURL = "https://www.googleapis.com/siteVerification/v1"
)

// Scopes are the OAuth scopes the service account must be granted in the Admin
// console (Security → Access and data control → API controls → Domain-wide
// delegation).
var Scopes = []string{
	"https://www.googleapis.com/auth/admin.directory.domain",
	"https://www.googleapis.com/auth/admin.directory.user",
	"https://www.googleapis.com/auth/siteverification",
}

// maxBodyBytes bounds how much of a response is read.
const maxBodyBytes = 1 << 20

// readRetries is how many times an idempotent call is repeated after a transient
// failure.
const readRetries = 2

// tokenLifetime is how long a requested access token lives; Google allows an hour.
const tokenLifetime = time.Hour

// Errors the service reacts to differently. Anything else is a transient failure.
var (
	// ErrAuth means the key, the delegation or the admin was refused.
	ErrAuth = errors.New("google: authentication failed")
	// ErrNotFound means Google has no such resource.
	ErrNotFound = errors.New("google: not found")
	// ErrExists means the resource already exists.
	ErrExists = errors.New("google: already exists")
	// ErrRateLimited means Google throttled the request.
	ErrRateLimited = errors.New("google: rate limited")
)

// APIError is a non-2xx response. It unwraps to one of the sentinels when the status
// maps to one.
type APIError struct {
	Status  int
	Reason  string
	Message string
	// Token is true when the token endpoint refused, not an API.
	Token    bool
	sentinel error
}

// Error implements error.
func (e *APIError) Error() string {
	where := "api"
	if e.Token {
		where = "token"
	}
	if e.Message == "" {
		return fmt.Sprintf("google %s: unexpected status %d", where, e.Status)
	}
	if e.Reason != "" {
		return fmt.Sprintf("google %s: status %d: %s (%s)", where, e.Status, e.Message, e.Reason)
	}
	return fmt.Sprintf("google %s: status %d: %s", where, e.Status, e.Message)
}

// Unwrap lets errors.Is find the sentinel.
func (e *APIError) Unwrap() error { return e.sentinel }

// Detail is Google's message, for showing to a person.
func (e *APIError) Detail() string { return e.Message }

// Definite reports whether the response proves the request was refused. A 4xx other
// than 429 and a rate-limit 403 means Google looked at it and said no; anything else
// leaves open whether it was acted on.
func Definite(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status >= 400 && apiErr.Status < 500 && !errors.Is(err, ErrRateLimited)
}

// Key is the part of a service-account JSON key the client uses.
type Key struct {
	ClientEmail  string
	ClientID     string
	PrivateKeyID string
	privateKey   *rsa.PrivateKey
}

// ParseKey reads a service-account JSON key as downloaded from the Cloud console.
func ParseKey(raw []byte) (Key, error) {
	var file struct {
		Type         string `json:"type"`
		ClientEmail  string `json:"client_email"`
		ClientID     string `json:"client_id"`
		PrivateKeyID string `json:"private_key_id"`
		PrivateKey   string `json:"private_key"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return Key{}, errors.New("is not valid JSON")
	}
	if file.Type != "service_account" {
		return Key{}, errors.New(`must be a service-account key ("type": "service_account")`)
	}
	if file.ClientEmail == "" || file.ClientID == "" {
		return Key{}, errors.New("has no client_email or client_id")
	}
	block, _ := pem.Decode([]byte(file.PrivateKey))
	if block == nil {
		return Key{}, errors.New("has no PEM private_key")
	}
	var private *rsa.PrivateKey
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return Key{}, errors.New("private_key is not an RSA key")
		}
		private = rsaKey
	} else if rsaKey, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		private = rsaKey
	} else {
		return Key{}, errors.New("private_key cannot be read")
	}
	return Key{
		ClientEmail:  file.ClientEmail,
		ClientID:     file.ClientID,
		PrivateKeyID: file.PrivateKeyID,
		privateKey:   private,
	}, nil
}

// Domain is a domain in the Workspace account.
type Domain struct {
	DomainName string `json:"domainName"`
	Verified   bool   `json:"verified"`
	IsPrimary  bool   `json:"isPrimary"`
}

// User is a Workspace user.
type User struct {
	ID           string     `json:"id"`
	PrimaryEmail string     `json:"primaryEmail"`
	CreationTime *time.Time `json:"creationTime"`
	Suspended    bool       `json:"suspended"`
}

// NewUser is the body of a user creation.
type NewUser struct {
	Email      string
	GivenName  string
	FamilyName string
	Password   string
}

// Config configures the client.
type Config struct {
	TokenURL            string
	DirectoryURL        string
	SiteVerificationURL string
	Key                 Key
	// Subject is the super admin the service account acts as.
	Subject    string
	Timeout    time.Duration
	HTTPClient *http.Client
	// Now is the clock the token assertion is stamped with; nil means time.Now.
	Now func() time.Time
}

// Client talks to the Directory and Site Verification APIs. It is safe for concurrent
// use and keeps its access token until shortly before it expires.
type Client struct {
	cfg  Config
	http *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

// New builds a client.
func New(cfg Config) *Client {
	if cfg.TokenURL == "" {
		cfg.TokenURL = DefaultTokenURL
	}
	if cfg.DirectoryURL == "" {
		cfg.DirectoryURL = DefaultDirectoryURL
	}
	if cfg.SiteVerificationURL == "" {
		cfg.SiteVerificationURL = DefaultSiteVerificationURL
	}
	cfg.DirectoryURL = strings.TrimRight(cfg.DirectoryURL, "/")
	cfg.SiteVerificationURL = strings.TrimRight(cfg.SiteVerificationURL, "/")
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		httpClient = &http.Client{Timeout: timeout}
	}
	return &Client{cfg: cfg, http: httpClient}
}

// ListDomains returns every domain in the account. It is the connection test: it
// proves the key, the delegation, the domain scope and the admin in one read.
func (c *Client) ListDomains(ctx context.Context) ([]Domain, error) {
	var out struct {
		Domains []Domain `json:"domains"`
	}
	err := c.do(ctx, http.MethodGet, c.cfg.DirectoryURL+"/customer/my_customer/domains", nil, true, &out)
	return out.Domains, err
}

// GetDomain returns one domain of the account, or ErrNotFound.
func (c *Client) GetDomain(ctx context.Context, name string) (Domain, error) {
	var out Domain
	err := c.do(ctx, http.MethodGet, c.cfg.DirectoryURL+"/customer/my_customer/domains/"+url.PathEscape(name), nil, true, &out)
	return out, err
}

// InsertDomain adds a secondary domain, unverified. ErrExists means it is already
// there.
func (c *Client) InsertDomain(ctx context.Context, name string) (Domain, error) {
	var out Domain
	err := c.do(ctx, http.MethodPost, c.cfg.DirectoryURL+"/customer/my_customer/domains",
		map[string]string{"domainName": name}, false, &out)
	return out, err
}

type site struct {
	Type       string `json:"type"`
	Identifier string `json:"identifier"`
}

// VerificationToken returns the TXT record value that proves ownership of a domain.
// Google answers with the same value for the same admin and domain.
func (c *Client) VerificationToken(ctx context.Context, domain string) (string, error) {
	body := map[string]any{
		"site":               site{Type: "INET_DOMAIN", Identifier: domain},
		"verificationMethod": "DNS_TXT",
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := c.do(ctx, http.MethodPost, c.cfg.SiteVerificationURL+"/token", body, true, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("google: site verification returned no token")
	}
	return out.Token, nil
}

// Verify asks Google to look up the verification record. A Definite error usually
// means the record is not visible to Google yet.
func (c *Client) Verify(ctx context.Context, domain string) error {
	body := map[string]any{"site": site{Type: "INET_DOMAIN", Identifier: domain}}
	return c.do(ctx, http.MethodPost, c.cfg.SiteVerificationURL+"/webResource?verificationMethod=DNS_TXT", body, false, nil)
}

// InsertUser creates a user, which takes a licence. It is sent exactly once;
// ErrExists means the address is taken.
func (c *Client) InsertUser(ctx context.Context, in NewUser) (User, error) {
	body := map[string]any{
		"primaryEmail":              in.Email,
		"name":                      map[string]string{"givenName": in.GivenName, "familyName": in.FamilyName},
		"password":                  in.Password,
		"changePasswordAtNextLogin": false,
	}
	var out User
	err := c.do(ctx, http.MethodPost, c.cfg.DirectoryURL+"/users", body, false, &out)
	return out, err
}

// GetUser returns one user by address, or ErrNotFound.
func (c *Client) GetUser(ctx context.Context, email string) (User, error) {
	var out User
	err := c.do(ctx, http.MethodGet, c.cfg.DirectoryURL+"/users/"+url.PathEscape(email), nil, true, &out)
	return out, err
}

// do sends one API request. An idempotent request is repeated after a transient
// failure; any other is sent once.
func (c *Client) do(ctx context.Context, method, target string, body any, idempotent bool, out any) error {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("google: encode request: %w", err)
		}
		payload = encoded
	}
	attempts := 1
	if idempotent {
		attempts += readRetries
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		err := c.attempt(ctx, method, target, payload, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !transient(err) || ctx.Err() != nil {
			break
		}
	}
	return lastErr
}

func (c *Client) attempt(ctx context.Context, method, target string, payload []byte, out any) error {
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("google: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	raw, status, err := c.send(req)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		if status == http.StatusUnauthorized {
			// A token that stopped working is dropped so the next call asks again.
			c.mu.Lock()
			c.accessToken = ""
			c.mu.Unlock()
		}
		return apiError(status, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("google: decode response: %w", err)
		}
	}
	return nil
}

func (c *Client) send(req *http.Request) ([]byte, int, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("google: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, 0, fmt.Errorf("google: read response: %w", err)
	}
	if len(raw) > maxBodyBytes {
		return nil, 0, errors.New("google: response body exceeds the size cap")
	}
	return raw, resp.StatusCode, nil
}

// token returns a delegated access token, asking for a new one when the cached one is
// missing or about to expire.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.cfg.Now()
	if c.accessToken != "" && now.Add(time.Minute).Before(c.expiresAt) {
		return c.accessToken, nil
	}
	assertion, err := c.assertion(now)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("google: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	raw, status, err := c.send(req)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		var body struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(raw, &body)
		apiErr := &APIError{Status: status, Reason: body.Error, Message: body.Description, Token: true}
		switch {
		case status == http.StatusTooManyRequests:
			apiErr.sentinel = ErrRateLimited
		case status >= 400 && status < 500:
			apiErr.sentinel = ErrAuth
		}
		return "", apiErr
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.AccessToken == "" {
		return "", errors.New("google: token response has no access_token")
	}
	c.accessToken = body.AccessToken
	c.expiresAt = now.Add(time.Duration(body.ExpiresIn) * time.Second)
	return c.accessToken, nil
}

// assertion is the signed JWT exchanged for an access token: the service account
// asking to act as the admin, for the module's scopes.
func (c *Client) assertion(now time.Time) (string, error) {
	if c.cfg.Key.privateKey == nil {
		return "", &APIError{Status: http.StatusUnauthorized, Message: "no service-account key is loaded", Token: true, sentinel: ErrAuth}
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if c.cfg.Key.PrivateKeyID != "" {
		header["kid"] = c.cfg.Key.PrivateKeyID
	}
	claims := map[string]any{
		"iss":   c.cfg.Key.ClientEmail,
		"sub":   c.cfg.Subject,
		"scope": strings.Join(Scopes, " "),
		"aud":   c.cfg.TokenURL,
		"iat":   now.Unix(),
		"exp":   now.Add(tokenLifetime).Unix(),
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(headerJSON) + "." + enc.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, c.cfg.Key.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("google: sign assertion: %w", err)
	}
	return signingInput + "." + enc.EncodeToString(signature), nil
}

// rateLimitReasons are the 403 reasons that mean "slow down" rather than "no".
var rateLimitReasons = map[string]bool{
	"rateLimitExceeded":     true,
	"userRateLimitExceeded": true,
	"quotaExceeded":         true,
}

// apiError reads Google's error envelope.
func apiError(status int, raw []byte) error {
	var body struct {
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	apiErr := &APIError{Status: status, Message: body.Error.Message}
	if len(body.Error.Errors) > 0 {
		apiErr.Reason = body.Error.Errors[0].Reason
	}
	switch {
	case status == http.StatusTooManyRequests, status == http.StatusForbidden && rateLimitReasons[apiErr.Reason]:
		apiErr.sentinel = ErrRateLimited
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		apiErr.sentinel = ErrAuth
	case status == http.StatusNotFound:
		apiErr.sentinel = ErrNotFound
	case status == http.StatusConflict:
		apiErr.sentinel = ErrExists
	}
	return apiErr
}

// transient reports whether repeating a read could help.
func transient(err error) bool {
	if errors.Is(err, ErrRateLimited) {
		return true
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= 500
	}
	return !errors.Is(err, context.Canceled)
}
