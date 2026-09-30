package google_test

import (
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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bory/karvon-be/internal/workspace/google"
)

const admin = "admin@karvon.test"

// serviceAccountJSON builds a key file the way the Cloud console writes one.
func serviceAccountJSON(t *testing.T) ([]byte, *rsa.PublicKey) {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"client_email":   "karvon@project.iam.gserviceaccount.com",
		"client_id":      "123456789012345678901",
		"private_key_id": "kid-1",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw, &private.PublicKey
}

// fakeGoogle checks the assertion it is sent like Google does, then serves the API.
type fakeGoogle struct {
	t          *testing.T
	public     *rsa.PublicKey
	tokenURL   string
	tokens     atomic.Int32
	calls      atomic.Int32
	refuse     string // a token-endpoint error to answer with
	api        http.HandlerFunc
	lastClaims map[string]any
}

func (f *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		f.tokens.Add(1)
		if f.refuse != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"` + f.refuse + `","error_description":"Client is unauthorized to retrieve access tokens using this method"}`))
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			f.t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		parts := strings.Split(r.Form.Get("assertion"), ".")
		if len(parts) != 3 {
			f.t.Fatalf("assertion has %d parts", len(parts))
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		if err := rsa.VerifyPKCS1v15(f.public, crypto.SHA256, digest[:], sig); err != nil {
			f.t.Errorf("assertion signature: %v", err)
		}
		claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
		_ = json.Unmarshal(claimsJSON, &f.lastClaims)
		_, _ = w.Write([]byte(`{"access_token":"at-1","expires_in":3600,"token_type":"Bearer"}`))
		return
	}
	f.calls.Add(1)
	if r.Header.Get("Authorization") != "Bearer at-1" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.api(w, r)
}

func newClient(t *testing.T, api http.HandlerFunc) (*google.Client, *fakeGoogle) {
	t.Helper()
	raw, public := serviceAccountJSON(t)
	key, err := google.ParseKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeGoogle{t: t, public: public, api: api}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	fake.tokenURL = srv.URL + "/token"
	return google.New(google.Config{
		TokenURL:            fake.tokenURL,
		DirectoryURL:        srv.URL + "/directory",
		SiteVerificationURL: srv.URL + "/siteverification",
		Key:                 key,
		Subject:             admin,
	}), fake
}

func googleError(w http.ResponseWriter, status int, reason, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": status, "message": message, "errors": []map[string]string{{"reason": reason, "message": message}},
	}})
}

func TestParseKey(t *testing.T) {
	raw, _ := serviceAccountJSON(t)
	key, err := google.ParseKey(raw)
	if err != nil || key.ClientEmail != "karvon@project.iam.gserviceaccount.com" || key.ClientID != "123456789012345678901" {
		t.Fatalf("ParseKey = %+v, %v", key, err)
	}
	for name, bad := range map[string]string{
		"not json":     `nope`,
		"oauth client": `{"installed":{"client_id":"x"}}`,
		"no key":       `{"type":"service_account","client_email":"a@b.c","client_id":"1","private_key":""}`,
		"bad pem":      `{"type":"service_account","client_email":"a@b.c","client_id":"1","private_key":"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"}`,
	} {
		if _, err := google.ParseKey([]byte(bad)); err == nil {
			t.Errorf("%s: ParseKey accepted it", name)
		}
	}
}

func TestTheAssertionActsAsTheAdminAndTheTokenIsReused(t *testing.T) {
	client, fake := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"domains":[{"domainName":"karvon.test","verified":true,"isPrimary":true}]}`))
	})
	for range 3 {
		domains, err := client.ListDomains(context.Background())
		if err != nil || len(domains) != 1 || !domains[0].IsPrimary {
			t.Fatalf("ListDomains = %+v, %v", domains, err)
		}
	}
	if n := fake.tokens.Load(); n != 1 {
		t.Fatalf("asked for %d tokens, want 1", n)
	}
	if fake.lastClaims["sub"] != admin || fake.lastClaims["iss"] != "karvon@project.iam.gserviceaccount.com" ||
		fake.lastClaims["aud"] != fake.tokenURL || !strings.Contains(fake.lastClaims["scope"].(string), "admin.directory.user") {
		t.Fatalf("claims = %v", fake.lastClaims)
	}
}

func TestARefusedTokenIsAnAuthError(t *testing.T) {
	client, fake := newClient(t, func(http.ResponseWriter, *http.Request) {})
	fake.refuse = "unauthorized_client"
	_, err := client.ListDomains(context.Background())
	var apiErr *google.APIError
	if !errors.Is(err, google.ErrAuth) || !errors.As(err, &apiErr) || !apiErr.Token {
		t.Fatalf("err = %v, want a token ErrAuth", err)
	}
	if fake.calls.Load() != 0 {
		t.Fatal("the API was called without a token")
	}
}

func TestCreatingAUserIsSentOnce(t *testing.T) {
	client, fake := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/directory/users" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["primaryEmail"] != "jane@shop.test" || body["changePasswordAtNextLogin"] != false {
			t.Errorf("body = %v", body)
		}
		w.WriteHeader(http.StatusBadGateway)
	})
	_, err := client.InsertUser(context.Background(), google.NewUser{
		Email: "jane@shop.test", GivenName: "Jane", FamilyName: "Doe", Password: "pw-123456789",
	})
	if err == nil || google.Definite(err) {
		t.Fatalf("err = %v, want an indefinite failure", err)
	}
	if n := fake.calls.Load(); n != 1 {
		t.Fatalf("InsertUser was sent %d times, want 1", n)
	}
}

func TestErrorsMapToSentinels(t *testing.T) {
	cases := []struct {
		status   int
		reason   string
		sentinel error
		definite bool
	}{
		{http.StatusConflict, "duplicate", google.ErrExists, true},
		{http.StatusNotFound, "notFound", google.ErrNotFound, true},
		{http.StatusForbidden, "forbidden", google.ErrAuth, true},
		{http.StatusForbidden, "userRateLimitExceeded", google.ErrRateLimited, false},
		{http.StatusTooManyRequests, "rateLimitExceeded", google.ErrRateLimited, false},
	}
	for _, tc := range cases {
		client, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
			googleError(w, tc.status, tc.reason, "nope")
		})
		_, err := client.InsertDomain(context.Background(), "shop.test")
		if !errors.Is(err, tc.sentinel) || google.Definite(err) != tc.definite {
			t.Errorf("%d %s: err = %v (definite %t)", tc.status, tc.reason, err, google.Definite(err))
		}
	}
}

func TestReadsRetryAThrottle(t *testing.T) {
	var n atomic.Int32
	client, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			googleError(w, http.StatusForbidden, "rateLimitExceeded", "slow down")
			return
		}
		_, _ = w.Write([]byte(`{"domainName":"shop.test","verified":false}`))
	})
	domain, err := client.GetDomain(context.Background(), "shop.test")
	if err != nil || domain.DomainName != "shop.test" || n.Load() != 2 {
		t.Fatalf("GetDomain = %+v, %v after %d calls", domain, err, n.Load())
	}
}

func TestSiteVerification(t *testing.T) {
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Site struct {
				Type       string `json:"type"`
				Identifier string `json:"identifier"`
			} `json:"site"`
			Method string `json:"verificationMethod"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Site.Type != "INET_DOMAIN" || body.Site.Identifier != "shop.test" {
			t.Errorf("site = %+v", body.Site)
		}
		switch r.URL.Path {
		case "/siteverification/token":
			_, _ = w.Write([]byte(`{"method":"DNS_TXT","token":"google-site-verification=abc"}`))
		case "/siteverification/webResource":
			if r.URL.Query().Get("verificationMethod") != "DNS_TXT" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
			googleError(w, http.StatusBadRequest, "badRequest", "The necessary verification token could not be found on your site.")
		}
	})
	token, err := client.VerificationToken(context.Background(), "shop.test")
	if err != nil || token != "google-site-verification=abc" {
		t.Fatalf("VerificationToken = %q, %v", token, err)
	}
	if err := client.Verify(context.Background(), "shop.test"); !google.Definite(err) {
		t.Fatalf("Verify err = %v, want a definite refusal", err)
	}
}
