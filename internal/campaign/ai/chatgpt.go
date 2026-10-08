package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider"
)

// planLimitCode is the error code OpenAI returns once the weekly cap the user set
// for this app, or their plan's own window, is used up.
const planLimitCode = "subscription_sharing_usage_limit_exceeded"

// maxEventBytes bounds one server-sent event; response.completed repeats the whole
// response, so it is the largest.
const maxEventBytes = 4 << 20

// ErrPlanLimit means the ChatGPT plan has no usage left for this app. OpenAI does
// not fall back to API billing and neither do we: the operator raises the cap in
// ChatGPT, waits for the window to reset, or uses the manual flow.
var ErrPlanLimit = errors.New("ai: chatgpt: the ChatGPT plan's usage limit for this app is reached")

// TokenSource hands out a current access token for the signed-in ChatGPT account,
// refreshing it when needed.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
}

// ChatGPTConfig configures the plan provider.
type ChatGPTConfig struct {
	Tokens     TokenSource
	BaseURL    string
	Model      string
	Timeout    time.Duration
	HTTPClient *http.Client
	Log        *slog.Logger
}

// ChatGPTPlanProvider runs the same Responses round trip as OpenAIAPIProvider, but
// with the operator's ChatGPT sign-in instead of an API key, so generations count
// against their ChatGPT plan. Plan usage requires store=false and stream=true and
// rejects max_output_tokens and temperature.
type ChatGPTPlanProvider struct {
	cfg  ChatGPTConfig
	http *http.Client
	log  *slog.Logger
}

// NewChatGPT builds the provider.
func NewChatGPT(cfg ChatGPTConfig) *ChatGPTPlanProvider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultOpenAIBaseURL
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	if cfg.Model == "" {
		cfg.Model = DefaultOpenAIModel
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultOpenAITimeout
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &ChatGPTPlanProvider{cfg: cfg, http: httpClient, log: cfg.Log}
}

// Name implements Provider.
func (*ChatGPTPlanProvider) Name() string { return campaign.AIProviderChatGPT }

// Mode implements Provider.
func (*ChatGPTPlanProvider) Mode() Mode { return ModeAPI }

// Model implements Provider.
func (p *ChatGPTPlanProvider) Model() string { return p.cfg.Model }

// BuildPrompt implements Provider.
func (*ChatGPTPlanProvider) BuildPrompt(brief Brief) (Prompt, error) { return buildPrompt(brief) }

// Parse implements Provider.
func (*ChatGPTPlanProvider) Parse(raw string) (Output, error) { return ParseOutput(raw) }

// Generate implements Provider.
func (p *ChatGPTPlanProvider) Generate(ctx context.Context, brief Brief) (Output, Usage, error) {
	if p.cfg.Tokens == nil {
		return Output{}, Usage{}, fmt.Errorf("ai: chatgpt: %w", provider.ErrNotConfigured)
	}
	prompt, err := buildPrompt(brief)
	if err != nil {
		return Output{}, Usage{}, err
	}
	token, err := p.cfg.Tokens.AccessToken(ctx)
	if err != nil {
		return Output{}, Usage{}, err
	}

	store := false
	resp, err := p.stream(ctx, token, responsesRequest{
		Model:        p.cfg.Model,
		Instructions: prompt.System,
		Input:        prompt.User,
		Text: responsesText{Format: responsesFormat{
			Type:   "json_schema",
			Name:   schemaName,
			Schema: Schema(),
			Strict: true,
		}},
		Store:  &store,
		Stream: true,
	})
	if err != nil {
		return Output{}, Usage{}, err
	}

	out, usage, err := readOutput("chatgpt", resp, p.cfg.Model)
	if err != nil {
		return Output{}, usage, err
	}
	p.log.Debug("generated cold-email content",
		"provider", campaign.AIProviderChatGPT, "model", usage.Model,
		"components", len(out.Components), "variants", len(out.Variants),
		"input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens)
	return out, usage, nil
}

// streamEvent is the part of a Responses stream event this provider reads. Only
// the terminal events matter: response.completed repeats the whole response, so
// the deltas before it are skipped rather than reassembled.
type streamEvent struct {
	Type     string            `json:"type"`
	Response responsesResponse `json:"response"`
	// A bare "error" event carries the error at the top level or under "error".
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Error   *openAIError `json:"error"`
}

// stream performs one streamed Responses request and returns the completed response.
func (p *ChatGPTPlanProvider) stream(ctx context.Context, token string, in responsesRequest) (responsesResponse, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return responsesResponse{}, fmt.Errorf("ai: chatgpt: encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		return responsesResponse{}, fmt.Errorf("ai: chatgpt: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := p.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return responsesResponse{}, fmt.Errorf("ai: chatgpt: request: %w", ctxErr)
		}
		return responsesResponse{}, fmt.Errorf("ai: chatgpt: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		var env errorEnvelope
		_ = json.Unmarshal(raw, &env)
		if env.Error.Code == planLimitCode {
			return responsesResponse{}, fmt.Errorf("%w: %s", ErrPlanLimit, strings.TrimSpace(env.Error.Message))
		}
		return responsesResponse{}, renameProvider(mapOpenAIError(resp.StatusCode, resp.Header.Get("Retry-After"), raw))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxEventBytes)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(rest, " "))
			continue
		}
		if line != "" || data.Len() == 0 {
			continue // event:, id:, comments, or a blank line with nothing pending
		}
		done, out, err := handleEvent(data.String())
		data.Reset()
		if err != nil || done {
			return out, err
		}
	}
	if data.Len() > 0 {
		if done, out, err := handleEvent(data.String()); err != nil || done {
			return out, err
		}
	}
	if err := scanner.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return responsesResponse{}, fmt.Errorf("ai: chatgpt: stream: %w", ctxErr)
		}
		return responsesResponse{}, fmt.Errorf("ai: chatgpt: stream: %w", err)
	}
	return responsesResponse{}, errors.New("ai: chatgpt: the stream ended before the response completed")
}

// handleEvent reads one event; done reports a terminal event.
func handleEvent(data string) (bool, responsesResponse, error) {
	if data == "[DONE]" {
		return false, responsesResponse{}, nil
	}
	var ev streamEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return false, responsesResponse{}, nil // a malformed delta is not worth failing over
	}
	switch ev.Type {
	case "response.completed":
		return true, ev.Response, nil
	case "response.failed":
		return true, responsesResponse{}, streamFailure(ev.Response.Error)
	case "response.incomplete":
		return true, responsesResponse{}, fmt.Errorf("ai: chatgpt: response %s ended incomplete", ev.Response.ID)
	case "error":
		e := ev.Error
		if e == nil {
			e = &openAIError{Code: ev.Code, Message: ev.Message}
		}
		return true, responsesResponse{}, streamFailure(e)
	}
	return false, responsesResponse{}, nil
}

func streamFailure(e *openAIError) error {
	if e == nil {
		return errors.New("ai: chatgpt: the response failed")
	}
	if e.Code == planLimitCode {
		return fmt.Errorf("%w: %s", ErrPlanLimit, strings.TrimSpace(e.Message))
	}
	return &provider.StatusError{Provider: "chatgpt", Code: http.StatusOK,
		Body: provider.Truncate(strings.TrimSpace(e.Code+": "+e.Message), 300)}
}

// renameProvider relabels the shared OpenAI error mapping for this provider.
func renameProvider(err error) error {
	var status *provider.StatusError
	if errors.As(err, &status) {
		status.Provider = "chatgpt"
	}
	return err
}

// compile-time proof that the provider satisfies the interface.
var _ Provider = (*ChatGPTPlanProvider)(nil)
