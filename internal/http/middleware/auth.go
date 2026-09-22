package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Authenticator compares the bearer token against the configured static API key.
type Authenticator struct {
	apiKey []byte
	// public paths never require a key.
	public map[string]struct{}
	// publicPrefixes exempt a whole subtree. Provider webhooks live under one:
	// neither Instantly nor Mailchimp can send our API key, so those routes carry
	// their own authentication (an unguessable path token plus, respectively, a
	// shared secret header and an HMAC signature) and are verified by the handler.
	publicPrefixes []string
}

// NewAuthenticator builds the API-key middleware. publicPaths are exact request paths
// served without authentication.
func NewAuthenticator(apiKey string, publicPaths ...string) *Authenticator {
	public := make(map[string]struct{}, len(publicPaths))
	for _, p := range publicPaths {
		public[p] = struct{}{}
	}
	return &Authenticator{apiKey: []byte(apiKey), public: public}
}

// PublicPrefix exempts every path under prefix from the API key.
func (a *Authenticator) PublicPrefix(prefixes ...string) *Authenticator {
	a.publicPrefixes = append(a.publicPrefixes, prefixes...)
	return a
}

// IsPublic reports whether a path is exempt from authentication.
func (a *Authenticator) IsPublic(path string) bool {
	if _, ok := a.public[path]; ok {
		return true
	}
	for _, prefix := range a.publicPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// Middleware rejects requests without a valid bearer token.
//
// unauthorized is supplied by the http package so the error envelope stays in one place.
func (a *Authenticator) Middleware(unauthorized func(http.ResponseWriter, *http.Request, string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a.IsPublic(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			token, ok := bearerToken(r)
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="karvon"`)
				unauthorized(w, r, "an Authorization: Bearer <api key> header is required")
				return
			}
			// Constant-time comparison keeps the key from leaking through timing.
			if subtle.ConstantTimeCompare([]byte(token), a.apiKey) != 1 {
				unauthorized(w, r, "the supplied API key is not valid")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the token from the Authorization header. EventSource cannot set
// headers, so the SSE route additionally accepts the key as a query parameter.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header != "" {
		scheme, token, found := strings.Cut(header, " ")
		if found && strings.EqualFold(scheme, "Bearer") && token != "" {
			return strings.TrimSpace(token), true
		}
		return "", false
	}
	if token := r.URL.Query().Get("api_key"); token != "" {
		return token, true
	}
	return "", false
}
