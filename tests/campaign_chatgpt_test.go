package integration_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/bory/karvon-be/internal/config"
)

// fakeOpenAI plays auth.openai.com's token endpoint and the Responses API for a
// plan-usage token.
type fakeOpenAI struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	nonce      string // the nonce the next authorization code was issued for
	access     string // the access token the Responses API accepts
	refresh    string // the refresh token the token endpoint accepts
	issued     int    // tokens minted so far, for rotation
	refuseNext bool   // the next refresh is invalid_grant
	capReached bool   // the Responses API reports a used-up plan cap
	refreshes  int
}

func newFakeOpenAI(t *testing.T) *fakeOpenAI {
	f := &fakeOpenAI{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOpenAI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/token":
		_ = r.ParseForm()
		form := r.PostForm
		if form.Get("client_id") != "karvon-client" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		switch form.Get("grant_type") {
		case "authorization_code":
			if form.Get("code") != "good-code" || form.Get("code_verifier") == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			f.mint(w, true)
		case "refresh_token":
			f.refreshes++
			if f.refuseNext || form.Get("refresh_token") != f.refresh {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"refresh token revoked"}`)
				return
			}
			f.mint(w, false)
		}
	case "/v1/responses":
		if r.Header.Get("Authorization") != "Bearer "+f.access {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid token"}}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true || body["store"] != false {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"plan usage requires stream=true and store=false"}}`)
			return
		}
		if f.capReached {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"weekly cap reached"}}`)
			return
		}
		completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp_1", "model": "gpt-plan", "status": "completed",
			"output": []map[string]any{{"type": "message", "content": []map[string]any{
				{"type": "output_text", "text": pastedOutput},
			}}},
			"usage": map[string]int{"input_tokens": 40, "output_tokens": 90},
		}})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.completed\ndata: %s\n\n", completed)
	default:
		http.NotFound(w, r)
	}
}

// mint issues a fresh token pair; the caller holds f.mu.
func (f *fakeOpenAI) mint(w http.ResponseWriter, withIDToken bool) {
	f.issued++
	f.access = fmt.Sprintf("at-%d", f.issued)
	f.refresh = fmt.Sprintf("rt-%d", f.issued)
	reply := map[string]any{"access_token": f.access, "refresh_token": f.refresh, "expires_in": 3600,
		"scope": "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"}
	if withIDToken {
		claims, _ := json.Marshal(map[string]any{"iss": "https://auth.openai.com", "aud": "karvon-client",
			"sub": "user-42", "email": "operator@karvon.test", "nonce": f.nonce})
		reply["id_token"] = "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
	}
	_ = json.NewEncoder(w).Encode(reply)
}

func (f *fakeOpenAI) set(fn func(*fakeOpenAI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

type aiProviderPayload struct {
	Provider string `json:"provider"`
	Mode     string `json:"mode"`
	ChatGPT  *struct {
		Status    string  `json:"status"`
		Email     *string `json:"email"`
		LastError *string `json:"last_error"`
	} `json:"chatgpt"`
}

// callback is the browser returning from OpenAI: no API key, and the redirect is
// not followed.
func (h *harness) chatGPTCallback(query url.Values) *url.URL {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ai/chatgpt/callback?"+query.Encode(), nil)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		h.t.Fatalf("callback = %d, want 302: %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		h.t.Fatalf("callback location: %v", err)
	}
	return loc
}

func TestSignInWithChatGPTRunsGenerationsOnThePlan(t *testing.T) {
	openai := newFakeOpenAI(t)
	h := newHarness(t, nil, withConfig(func(cfg *config.Config) {
		cfg.ChatGPTClientID = "karvon-client"
		cfg.ChatGPTTokenURL = openai.srv.URL + "/token"
		cfg.ChatGPTReturnURL = "https://app.karvon.test/settings/ai"
		cfg.OpenAIBaseURL = openai.srv.URL + "/v1"
		cfg.ChatGPTModel = "gpt-plan"
	}))
	brief := `{"brief":{"campaign_goal":"book discovery calls","company":"Karvon","target_industry":"gyms","variant_count":1}}`

	provider := decodeBody[aiProviderPayload](t, h.mustRequest(http.MethodGet, "/api/v1/ai/provider", "", http.StatusOK))
	if provider.ChatGPT == nil || provider.ChatGPT.Status != "disconnected" || provider.Provider == "chatgpt_plan" {
		t.Fatalf("before signing in the fallback serves: %+v", provider)
	}

	// Connect: the dashboard gets the URL to send the browser to.
	start := decodeBody[struct {
		AuthorizeURL string `json:"authorize_url"`
	}](t, h.mustRequest(http.MethodPost, "/api/v1/ai/chatgpt/connect", "", http.StatusOK))
	authorize, err := url.Parse(start.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	q := authorize.Query()
	if q.Get("redirect_uri") != "https://karvon.test/api/v1/ai/chatgpt/callback" {
		t.Errorf("redirect_uri = %q, want one derived from the public base URL", q.Get("redirect_uri"))
	}
	state := q.Get("state")
	openai.set(func(f *fakeOpenAI) { f.nonce = q.Get("nonce") })

	// A forged state is turned away without touching OpenAI.
	loc := h.chatGPTCallback(url.Values{"state": {"forged"}, "code": {"good-code"}})
	if loc.Query().Get("chatgpt") != "error" || loc.Query().Get("reason") != "expired" {
		t.Fatalf("forged state landed on %s", loc)
	}

	loc = h.chatGPTCallback(url.Values{"state": {state}, "code": {"good-code"}})
	if loc.Host != "app.karvon.test" || loc.Path != "/settings/ai" || loc.Query().Get("chatgpt") != "connected" {
		t.Fatalf("sign-in landed on %s", loc)
	}
	// The state is single-use.
	if loc = h.chatGPTCallback(url.Values{"state": {state}, "code": {"good-code"}}); loc.Query().Get("reason") != "expired" {
		t.Fatalf("a replayed callback landed on %s", loc)
	}

	provider = decodeBody[aiProviderPayload](t, h.mustRequest(http.MethodGet, "/api/v1/ai/provider", "", http.StatusOK))
	if provider.Provider != "chatgpt_plan" || provider.Mode != "api" || provider.ChatGPT.Status != "connected" ||
		provider.ChatGPT.Email == nil || *provider.ChatGPT.Email != "operator@karvon.test" {
		t.Fatalf("after signing in: %+v", provider)
	}

	// A generation runs on the plan with the stored access token.
	generation := decodeBody[generationPayload](t, h.mustRequest(http.MethodPost, "/api/v1/ai/generations", brief, http.StatusCreated))
	if generation.Provider != "chatgpt_plan" || generation.Status != "parsed" || generation.ComponentCount != 3 {
		t.Fatalf("generation = %+v", generation)
	}

	// An hour on, the access token is refreshed and the rotated refresh token kept.
	h.now.Advance(2 * time.Hour)
	h.mustRequest(http.MethodPost, "/api/v1/ai/generations", brief, http.StatusCreated)
	h.now.Advance(2 * time.Hour)
	h.mustRequest(http.MethodPost, "/api/v1/ai/generations", brief, http.StatusCreated)
	openai.set(func(f *fakeOpenAI) {
		if f.refreshes != 2 || f.access != "at-3" {
			t.Errorf("refreshes = %d, access = %s; want two refreshes on rotated tokens", f.refreshes, f.access)
		}
	})

	// OpenAI drops the access token early: one refresh and the generation succeeds.
	openai.set(func(f *fakeOpenAI) { f.access = "revoked-early" })
	h.mustRequest(http.MethodPost, "/api/v1/ai/generations", brief, http.StatusCreated)
	openai.set(func(f *fakeOpenAI) {
		if f.refreshes != 3 {
			t.Errorf("refreshes = %d, want a third after the early revocation", f.refreshes)
		}
	})

	// A used-up cap is reported as such and is not billed anywhere else.
	openai.set(func(f *fakeOpenAI) { f.capReached = true })
	rec := h.mustRequest(http.MethodPost, "/api/v1/ai/generations", brief, http.StatusTooManyRequests)
	if code := errorCode(t, rec); code != "ai_plan_limit" {
		t.Fatalf("cap reached: code = %q", code)
	}
	if h.ai.Calls != 0 {
		t.Fatal("a used-up ChatGPT cap must not fall through to the API key")
	}
	// The way out while the cap is used up: the copy-and-paste flow on request.
	manual := decodeBody[generationPayload](t, h.mustRequest(http.MethodPost, "/api/v1/ai/generations",
		`{"manual":true,`+brief[1:], http.StatusCreated))
	if manual.Provider != "manual_chatgpt" || manual.Status != "awaiting_paste" || manual.Prompt == "" {
		t.Fatalf("manual generation = %+v", manual)
	}
	openai.set(func(f *fakeOpenAI) { f.capReached = false })

	// OpenAI revokes the sign-in: the connection asks for a new one and the
	// fallback serves again.
	h.now.Advance(2 * time.Hour)
	openai.set(func(f *fakeOpenAI) { f.refuseNext = true })
	rec = h.request(http.MethodPost, "/api/v1/ai/generations", brief)
	if code := errorCode(t, rec); code != "provider_auth" {
		t.Fatalf("revoked sign-in: %d %s", rec.Code, rec.Body.String())
	}
	provider = decodeBody[aiProviderPayload](t, h.mustRequest(http.MethodGet, "/api/v1/ai/provider", "", http.StatusOK))
	if provider.ChatGPT.Status != "needs_reconnect" || provider.Provider == "chatgpt_plan" || provider.ChatGPT.LastError == nil {
		t.Fatalf("after revocation: %+v", provider)
	}

	h.mustRequest(http.MethodDelete, "/api/v1/ai/chatgpt", "", http.StatusNoContent)
	provider = decodeBody[aiProviderPayload](t, h.mustRequest(http.MethodGet, "/api/v1/ai/provider", "", http.StatusOK))
	if provider.ChatGPT.Status != "disconnected" {
		t.Fatalf("after disconnecting: %+v", provider)
	}
}

func TestSignInWithChatGPTIsOffWithoutAClientID(t *testing.T) {
	h := newHarness(t, nil)
	provider := decodeBody[aiProviderPayload](t, h.mustRequest(http.MethodGet, "/api/v1/ai/provider", "", http.StatusOK))
	if provider.ChatGPT != nil {
		t.Fatalf("chatgpt = %+v, want null", provider.ChatGPT)
	}
	h.mustRequest(http.MethodPost, "/api/v1/ai/chatgpt/connect", "", http.StatusConflict)
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return env.Error.Code
}
