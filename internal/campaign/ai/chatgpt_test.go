package ai_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/ai"
	"github.com/bory/karvon-be/internal/campaign/provider"
)

type staticToken string

func (s staticToken) AccessToken(context.Context) (string, error) { return string(s), nil }

// sse renders events the way the Responses API streams them.
func sse(events ...map[string]any) string {
	var b strings.Builder
	for _, ev := range events {
		raw, _ := json.Marshal(ev)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", ev["type"], raw)
	}
	return b.String()
}

func completedResponse(text string) map[string]any {
	return map[string]any{
		"id": "resp_1", "model": "gpt-plan-2026", "status": "completed",
		"output": []map[string]any{
			{"type": "message", "content": []map[string]any{{"type": "output_text", "text": text}}},
		},
		"usage": map[string]int{"input_tokens": 50, "output_tokens": 70},
	}
}

func TestChatGPTProviderStreamsAgainstThePlan(t *testing.T) {
	var gotAuth, gotAccept string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAccept = r.Header.Get("Authorization"), r.Header.Get("Accept")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse(
			map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_1"}},
			map[string]any{"type": "response.output_text.delta", "delta": `{"compo`},
			map[string]any{"type": "response.completed", "response": completedResponse(documented)},
		))
	}))
	t.Cleanup(srv.Close)

	p := ai.NewChatGPT(ai.ChatGPTConfig{Tokens: staticToken("plan-token"), BaseURL: srv.URL, Model: "gpt-plan"})
	if p.Name() != campaign.AIProviderChatGPT || p.Mode() != ai.ModeAPI || p.Model() != "gpt-plan" {
		t.Fatalf("identity = %s/%s/%q", p.Name(), p.Mode(), p.Model())
	}
	out, usage, err := p.Generate(context.Background(), ai.Brief{Company: "Karvon"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if gotAuth != "Bearer plan-token" || gotAccept != "text/event-stream" {
		t.Fatalf("headers = %q / %q", gotAuth, gotAccept)
	}
	if gotBody["store"] != false || gotBody["stream"] != true {
		t.Fatalf("plan usage needs store=false and stream=true, got %v / %v", gotBody["store"], gotBody["stream"])
	}
	if _, ok := gotBody["max_output_tokens"]; ok {
		t.Fatal("plan usage rejects max_output_tokens")
	}
	if usage.Model != "gpt-plan-2026" || usage.InputTokens != 50 || usage.OutputTokens != 70 {
		t.Fatalf("usage = %+v", usage)
	}
	if len(out.Components) != 3 || len(out.Variants) != 1 {
		t.Fatalf("out = %+v", out)
	}
}

func TestChatGPTProviderReportsTheUsedUpCap(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"before the stream": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"weekly cap reached"}}`)
		},
		"inside the stream": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, sse(map[string]any{"type": "response.failed", "response": map[string]any{
				"id": "resp_1", "status": "failed",
				"error": map[string]any{"code": "subscription_sharing_usage_limit_exceeded", "message": "weekly cap reached"},
			}}))
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			t.Cleanup(srv.Close)
			p := ai.NewChatGPT(ai.ChatGPTConfig{Tokens: staticToken("t"), BaseURL: srv.URL})
			_, _, err := p.Generate(context.Background(), ai.Brief{})
			if !errors.Is(err, ai.ErrPlanLimit) {
				t.Fatalf("err = %v, want ErrPlanLimit", err)
			}
			if errors.Is(err, provider.ErrRateLimited) {
				t.Fatal("a used-up weekly cap is not a retryable rate limit")
			}
		})
	}
}

func TestChatGPTProviderFailsAStreamThatNeverCompletes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, sse(map[string]any{"type": "response.output_text.delta", "delta": "{"}))
	}))
	t.Cleanup(srv.Close)
	p := ai.NewChatGPT(ai.ChatGPTConfig{Tokens: staticToken("t"), BaseURL: srv.URL})
	if _, _, err := p.Generate(context.Background(), ai.Brief{}); err == nil || !strings.Contains(err.Error(), "before the response completed") {
		t.Fatalf("err = %v", err)
	}
}

func TestChatGPTProviderMapsARevokedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"token revoked"}}`)
	}))
	t.Cleanup(srv.Close)
	p := ai.NewChatGPT(ai.ChatGPTConfig{Tokens: staticToken("t"), BaseURL: srv.URL})
	if _, _, err := p.Generate(context.Background(), ai.Brief{}); !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

/* ------------------------------------------------------------------ OAuth */

// idToken builds an unsigned JWT; the client checks claims, not the signature.
func idToken(claims map[string]any) string {
	enc := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return enc(map[string]string{"alg": "none"}) + "." + enc(claims) + ".sig"
}

func TestChatGPTOAuthAuthorizeURLCarriesPKCEAndPlanScope(t *testing.T) {
	o := ai.NewChatGPTOAuth(ai.ChatGPTOAuthConfig{ClientID: "client-1", RedirectURL: "https://karvon.test/cb"})
	attempt, err := ai.NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(o.AuthorizeURL(attempt))
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "auth.openai.com" || u.Path != "/api/accounts/authorize" {
		t.Fatalf("authorize URL = %s", u)
	}
	q := u.Query()
	for key, want := range map[string]string{
		"client_id": "client-1", "redirect_uri": "https://karvon.test/cb", "response_type": "code",
		"code_challenge_method": "S256", "state": attempt.State, "nonce": attempt.Nonce,
		"resource": "https://api.openai.com/v1",
	} {
		if q.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, q.Get(key), want)
		}
	}
	if !strings.Contains(q.Get("scope"), "chatgpt.tokens.use.direct") || !strings.Contains(q.Get("scope"), "offline_access") {
		t.Errorf("scope = %q", q.Get("scope"))
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge") == attempt.Verifier {
		t.Error("the challenge must be derived from the verifier, never the verifier itself")
	}
}

func TestChatGPTOAuthExchangeChecksTheIDToken(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	claims := map[string]any{"iss": "https://auth.openai.com", "aud": []string{"client-1"}, "sub": "user-1",
		"email": "op@karvon.test", "nonce": "n-1", "exp": now.Add(time.Hour).Unix()}
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "refresh_token": "rt",
			"id_token": idToken(claims), "expires_in": 3600, "scope": "openid email offline_access chatgpt.tokens.use.direct"})
	}))
	t.Cleanup(srv.Close)
	o := ai.NewChatGPTOAuth(ai.ChatGPTOAuthConfig{ClientID: "client-1", ClientSecret: "s3cret", RedirectURL: "https://karvon.test/cb", TokenURL: srv.URL})

	tokens, id, err := o.Exchange(context.Background(), "code-1", ai.PKCE{Verifier: "v-1", Nonce: "n-1"}, now)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if gotForm.Get("grant_type") != "authorization_code" || gotForm.Get("code") != "code-1" ||
		gotForm.Get("code_verifier") != "v-1" || gotForm.Get("client_secret") != "s3cret" {
		t.Fatalf("form = %v", gotForm)
	}
	if id.Subject != "user-1" || id.Email != "op@karvon.test" || !tokens.HasPlanScope() || !tokens.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("tokens = %+v id = %+v", tokens, id)
	}

	if _, _, err := o.Exchange(context.Background(), "code-1", ai.PKCE{Verifier: "v-1", Nonce: "other"}, now); err == nil {
		t.Fatal("an ID token minted for another sign-in must be rejected")
	}
	claims["aud"] = "someone-else"
	if _, _, err := o.Exchange(context.Background(), "code-1", ai.PKCE{Verifier: "v-1", Nonce: "n-1"}, now); err == nil {
		t.Fatal("an ID token for another client must be rejected")
	}
}

func TestChatGPTOAuthRefresh(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	reply := `{"access_token":"at-2","refresh_token":"rt-2","expires_in":3600}`
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "rt-1" {
			t.Errorf("form = %v", r.PostForm)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	o := ai.NewChatGPTOAuth(ai.ChatGPTOAuthConfig{ClientID: "client-1", TokenURL: srv.URL})

	tokens, err := o.Refresh(context.Background(), "rt-1", now)
	if err != nil || tokens.AccessToken != "at-2" || tokens.RefreshToken != "rt-2" {
		t.Fatalf("rotated refresh = %+v, %v", tokens, err)
	}

	reply = `{"access_token":"at-3","expires_in":3600}`
	if tokens, err = o.Refresh(context.Background(), "rt-1", now); err != nil || tokens.RefreshToken != "rt-1" {
		t.Fatalf("a server that does not rotate keeps the old refresh token: %+v, %v", tokens, err)
	}

	status, reply = http.StatusBadRequest, `{"error":"invalid_grant","error_description":"refresh token expired"}`
	if _, err := o.Refresh(context.Background(), "rt-1", now); !errors.Is(err, ai.ErrOAuthGrant) {
		t.Fatalf("err = %v, want ErrOAuthGrant", err)
	}
}
