package ai

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bory/karvon-be/internal/campaign/provider"
)

// Sign in with ChatGPT defaults, from https://auth.openai.com/.well-known/openid-configuration.
const (
	DefaultChatGPTIssuer       = "https://auth.openai.com"
	DefaultChatGPTAuthorizeURL = "https://auth.openai.com/api/accounts/authorize"
	DefaultChatGPTTokenURL     = "https://auth.openai.com/api/accounts/oauth/token"
	// ChatGPTResource is the audience the plan-usage token is minted for.
	ChatGPTResource = "https://api.openai.com/v1"
	// ChatGPTScopes are the identity scopes, a refresh token, and permission to run
	// Responses requests against the user's ChatGPT plan.
	ChatGPTScopes = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
	// chatGPTPlanScope is the scope without which a token cannot spend plan usage.
	chatGPTPlanScope = "chatgpt.tokens.use.direct"
)

// ErrOAuthGrant means the authorization server rejected a code or refresh token:
// the user revoked access, the refresh token expired, or a code was replayed. The
// only way forward is to sign in again.
var ErrOAuthGrant = errors.New("ai: chatgpt: the sign-in is no longer valid")

// ChatGPTOAuthConfig configures the Sign in with ChatGPT client. ClientSecret is
// optional: OpenAI may issue a public client (PKCE only) or a confidential one.
type ChatGPTOAuthConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Issuer       string
	AuthorizeURL string
	TokenURL     string
	Timeout      time.Duration
	HTTPClient   *http.Client
}

// ChatGPTOAuth runs the authorization-code flow with PKCE against auth.openai.com.
type ChatGPTOAuth struct {
	cfg  ChatGPTOAuthConfig
	http *http.Client
}

// NewChatGPTOAuth builds the client, filling in OpenAI's endpoints.
func NewChatGPTOAuth(cfg ChatGPTOAuthConfig) *ChatGPTOAuth {
	if cfg.Issuer == "" {
		cfg.Issuer = DefaultChatGPTIssuer
	}
	if cfg.AuthorizeURL == "" {
		cfg.AuthorizeURL = DefaultChatGPTAuthorizeURL
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = DefaultChatGPTTokenURL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &ChatGPTOAuth{cfg: cfg, http: httpClient}
}

// PKCE is one sign-in attempt's secrets: state ties the callback to the attempt,
// the verifier proves the token request comes from whoever started it, and the
// nonce ties the ID token to it.
type PKCE struct {
	State    string
	Verifier string
	Nonce    string
}

// NewPKCE draws fresh random values for one sign-in attempt.
func NewPKCE() (PKCE, error) {
	var out PKCE
	for _, dst := range []*string{&out.State, &out.Verifier, &out.Nonce} {
		v, err := randomToken(32)
		if err != nil {
			return PKCE{}, err
		}
		*dst = v
	}
	return out, nil
}

// AuthorizeURL is where the operator's browser is sent to sign in.
func (o *ChatGPTOAuth) AuthorizeURL(p PKCE) string {
	challenge := sha256.Sum256([]byte(p.Verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {o.cfg.ClientID},
		"redirect_uri":          {o.cfg.RedirectURL},
		"scope":                 {ChatGPTScopes},
		"resource":              {ChatGPTResource},
		"state":                 {p.State},
		"nonce":                 {p.Nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(o.cfg.AuthorizeURL, "?") {
		sep = "&"
	}
	return o.cfg.AuthorizeURL + sep + q.Encode()
}

// TokenSet is what the token endpoint returns.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	Scope        string
	ExpiresAt    time.Time
}

// HasPlanScope reports whether the token may spend ChatGPT plan usage. A user can
// sign in but decline sharing, or be on a plan that has none to share. An absent
// scope means the requested scope was granted in full (RFC 6749 §5.1).
func (t TokenSet) HasPlanScope() bool {
	if strings.TrimSpace(t.Scope) == "" {
		return true
	}
	for _, s := range strings.Fields(t.Scope) {
		if s == chatGPTPlanScope {
			return true
		}
	}
	return false
}

// Identity is the verified part of the ID token we keep.
type Identity struct {
	Subject string
	Email   string
}

// Exchange trades an authorization code for tokens and checks the ID token
// against the attempt's nonce.
func (o *ChatGPTOAuth) Exchange(ctx context.Context, code string, p PKCE, now time.Time) (TokenSet, Identity, error) {
	tokens, err := o.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {p.Verifier},
		"redirect_uri":  {o.cfg.RedirectURL},
		"resource":      {ChatGPTResource},
	}, now)
	if err != nil {
		return TokenSet{}, Identity{}, err
	}
	if tokens.RefreshToken == "" {
		return TokenSet{}, Identity{}, errors.New("ai: chatgpt: the sign-in returned no refresh token")
	}
	id, err := o.verifyIDToken(tokens.IDToken, p.Nonce, now)
	if err != nil {
		return TokenSet{}, Identity{}, err
	}
	return tokens, id, nil
}

// Refresh trades a refresh token for a new access token. OpenAI rotates refresh
// tokens, so the returned RefreshToken replaces the old one and the old one must
// not be used again.
func (o *ChatGPTOAuth) Refresh(ctx context.Context, refreshToken string, now time.Time) (TokenSet, error) {
	tokens, err := o.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"resource":      {ChatGPTResource},
	}, now)
	if err != nil {
		return TokenSet{}, err
	}
	if tokens.RefreshToken == "" {
		// A server that does not rotate keeps the old one valid.
		tokens.RefreshToken = refreshToken
	}
	return tokens, nil
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
	Scope            string `json:"scope"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (o *ChatGPTOAuth) token(ctx context.Context, form url.Values, now time.Time) (TokenSet, error) {
	form.Set("client_id", o.cfg.ClientID)
	if o.cfg.ClientSecret != "" {
		form.Set("client_secret", o.cfg.ClientSecret)
	}

	ctx, cancel := context.WithTimeout(ctx, o.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenSet{}, fmt.Errorf("ai: chatgpt: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return TokenSet{}, fmt.Errorf("ai: chatgpt: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return TokenSet{}, fmt.Errorf("ai: chatgpt: read token response: %w", err)
	}

	var body tokenResponse
	_ = json.Unmarshal(raw, &body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 || body.Error != "" {
		detail := strings.TrimSpace(body.Error + ": " + body.ErrorDescription)
		if body.Error == "" {
			detail = provider.Truncate(strings.TrimSpace(string(raw)), 300)
		}
		// RFC 6749 §5.2: invalid_grant covers an expired, revoked or replayed grant.
		if body.Error == "invalid_grant" {
			return TokenSet{}, fmt.Errorf("%w: %s", ErrOAuthGrant, detail)
		}
		if resp.StatusCode == http.StatusUnauthorized || body.Error == "invalid_client" {
			return TokenSet{}, fmt.Errorf("ai: chatgpt: %w: %s", provider.ErrAuth, detail)
		}
		return TokenSet{}, &provider.StatusError{Provider: "chatgpt", Code: resp.StatusCode, Body: detail}
	}
	if body.AccessToken == "" {
		return TokenSet{}, errors.New("ai: chatgpt: the token response carried no access token")
	}
	expiresIn := time.Duration(body.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = time.Hour
	}
	return TokenSet{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		IDToken:      body.IDToken,
		Scope:        body.Scope,
		ExpiresAt:    now.Add(expiresIn),
	}, nil
}

// verifyIDToken checks the claims that bind the ID token to this client and this
// sign-in. The signature is not checked: the token came straight from the token
// endpoint over TLS, which OpenID Connect Core §3.1.3.7 accepts in its place, and
// nothing here grants access on the strength of it; it only names the account.
func (o *ChatGPTOAuth) verifyIDToken(token, nonce string, now time.Time) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, errors.New("ai: chatgpt: the sign-in returned no valid ID token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Identity{}, fmt.Errorf("ai: chatgpt: decode ID token: %w", err)
	}
	var claims struct {
		Issuer   string          `json:"iss"`
		Subject  string          `json:"sub"`
		Audience json.RawMessage `json:"aud"`
		Expiry   int64           `json:"exp"`
		Nonce    string          `json:"nonce"`
		Email    string          `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Identity{}, fmt.Errorf("ai: chatgpt: decode ID token claims: %w", err)
	}
	if strings.TrimSuffix(claims.Issuer, "/") != strings.TrimSuffix(o.cfg.Issuer, "/") {
		return Identity{}, fmt.Errorf("ai: chatgpt: ID token issuer %q is not %q", claims.Issuer, o.cfg.Issuer)
	}
	if !audienceContains(claims.Audience, o.cfg.ClientID) {
		return Identity{}, errors.New("ai: chatgpt: the ID token was not issued to this client")
	}
	if claims.Nonce != nonce {
		return Identity{}, errors.New("ai: chatgpt: the ID token belongs to a different sign-in")
	}
	if claims.Expiry != 0 && now.After(time.Unix(claims.Expiry, 0).Add(time.Minute)) {
		return Identity{}, errors.New("ai: chatgpt: the ID token has expired")
	}
	if claims.Subject == "" {
		return Identity{}, errors.New("ai: chatgpt: the ID token names no account")
	}
	return Identity{Subject: claims.Subject, Email: claims.Email}, nil
}

// audienceContains accepts aud as a single string or a list, as JWT allows.
func audienceContains(raw json.RawMessage, clientID string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == clientID
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, a := range many {
			if a == clientID {
				return true
			}
		}
	}
	return false
}

func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("ai: chatgpt: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
