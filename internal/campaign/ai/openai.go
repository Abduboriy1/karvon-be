package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/provider"
)

// OpenAI defaults.
const (
	// DefaultOpenAIBaseURL is the API root; the Responses endpoint is appended.
	DefaultOpenAIBaseURL = "https://api.openai.com/v1"
	// DefaultOpenAIModel is used when the config names none.
	DefaultOpenAIModel = "gpt-4o-mini"
	// DefaultOpenAITimeout bounds one generation; a full set of components is
	// several thousand output tokens.
	DefaultOpenAITimeout = 2 * time.Minute
	// DefaultMaxOutputTokens is what a generation is allowed to spend.
	DefaultMaxOutputTokens = 8192
	// schemaName is the structured-output format name the API requires.
	schemaName = "cold_email_content"
	// maxBodyBytes bounds how much of a response is read.
	maxBodyBytes = 1 << 20
	// defaultRetryAfter is used when a 429 carries no Retry-After header.
	defaultRetryAfter = 30 * time.Second
)

// OpenAIConfig configures the API provider. The key is never logged.
type OpenAIConfig struct {
	APIKey     string
	BaseURL    string
	Model      string
	Timeout    time.Duration
	HTTPClient *http.Client
	Log        *slog.Logger
}

// OpenAIAPIProvider runs the round trip against the OpenAI Responses API with a
// strict JSON-schema output format, so the reply is the schema or a refusal.
type OpenAIAPIProvider struct {
	cfg  OpenAIConfig
	http *http.Client
	log  *slog.Logger
}

// NewOpenAI builds the provider. It never fails; a missing key surfaces as
// provider.ErrNotConfigured from Generate.
func NewOpenAI(cfg OpenAIConfig) *OpenAIAPIProvider {
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
	return &OpenAIAPIProvider{cfg: cfg, http: httpClient, log: cfg.Log}
}

// Name implements Provider.
func (*OpenAIAPIProvider) Name() string { return campaign.AIProviderOpenAI }

// Mode implements Provider.
func (*OpenAIAPIProvider) Mode() Mode { return ModeAPI }

// Model implements Provider.
func (p *OpenAIAPIProvider) Model() string { return p.cfg.Model }

// BuildPrompt implements Provider.
func (*OpenAIAPIProvider) BuildPrompt(brief Brief) (Prompt, error) { return buildPrompt(brief) }

// Parse implements Provider.
func (*OpenAIAPIProvider) Parse(raw string) (Output, error) { return ParseOutput(raw) }

/* --------------------------------------------------------------------- wire */

type responsesRequest struct {
	Model           string        `json:"model"`
	Instructions    string        `json:"instructions"`
	Input           string        `json:"input"`
	Text            responsesText `json:"text"`
	MaxOutputTokens int           `json:"max_output_tokens,omitempty"`
	// Store and Stream are only sent by the ChatGPT plan provider, which must
	// set store=false and stream=true.
	Store  *bool `json:"store,omitempty"`
	Stream bool  `json:"stream,omitempty"`
}

type responsesText struct {
	Format responsesFormat `json:"format"`
}

type responsesFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

type responsesResponse struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Status string `json:"status"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *openAIError `json:"error"`
}

type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// errorEnvelope is the body of a non-2xx answer.
type errorEnvelope struct {
	Error openAIError `json:"error"`
}

// Generate implements Provider. Usage is reported even when the reply failed to
// parse, so spent tokens are still recorded.
func (p *OpenAIAPIProvider) Generate(ctx context.Context, brief Brief) (Output, Usage, error) {
	if p.cfg.APIKey == "" {
		return Output{}, Usage{}, fmt.Errorf("ai: openai: %w", provider.ErrNotConfigured)
	}
	prompt, err := buildPrompt(brief)
	if err != nil {
		return Output{}, Usage{}, err
	}

	resp, err := p.call(ctx, responsesRequest{
		Model:        p.cfg.Model,
		Instructions: prompt.System,
		Input:        prompt.User,
		Text: responsesText{Format: responsesFormat{
			Type:   "json_schema",
			Name:   schemaName,
			Schema: Schema(),
			Strict: true,
		}},
		MaxOutputTokens: DefaultMaxOutputTokens,
	})
	if err != nil {
		return Output{}, Usage{}, err
	}

	out, usage, err := readOutput("openai", resp, p.cfg.Model)
	if err != nil {
		return Output{}, usage, err
	}
	p.log.Debug("generated cold-email content",
		"provider", campaign.AIProviderOpenAI, "model", usage.Model,
		"components", len(out.Components), "variants", len(out.Variants),
		"input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens)
	return out, usage, nil
}

// readOutput turns a finished Responses object into parsed output. Usage is
// returned even when the reply fails to parse, so spent tokens are still recorded.
func readOutput(name string, resp responsesResponse, fallbackModel string) (Output, Usage, error) {
	usage := Usage{
		Model:        resp.Model,
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
	}
	if usage.Model == "" {
		usage.Model = fallbackModel
	}

	var text strings.Builder
	for _, item := range resp.Output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			switch part.Type {
			case "refusal":
				return Output{}, usage, fmt.Errorf("%w: %s", ErrRefused, strings.TrimSpace(part.Refusal))
			case "output_text":
				text.WriteString(part.Text)
			}
		}
	}
	if text.Len() == 0 {
		return Output{}, usage, fmt.Errorf("ai: %s: response %s (%s) carried no text", name, resp.ID, resp.Status)
	}

	out, err := ParseOutput(text.String())
	if err != nil {
		return Output{}, usage, err
	}
	return out, usage, nil
}

// call performs one Responses request.
func (p *OpenAIAPIProvider) call(ctx context.Context, in responsesRequest) (responsesResponse, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return responsesResponse{}, fmt.Errorf("ai: openai: encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		return responsesResponse{}, fmt.Errorf("ai: openai: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return responsesResponse{}, fmt.Errorf("ai: openai: request: %w", ctxErr)
		}
		return responsesResponse{}, fmt.Errorf("ai: openai: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return responsesResponse{}, mapOpenAIError(resp.StatusCode, resp.Header.Get("Retry-After"), raw)
	}
	if readErr != nil {
		return responsesResponse{}, fmt.Errorf("ai: openai: read response: %w", readErr)
	}

	var out responsesResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return responsesResponse{}, fmt.Errorf("ai: openai: decode response: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return responsesResponse{}, &provider.StatusError{Provider: "openai", Code: resp.StatusCode,
			Body: provider.Truncate(out.Error.Message, 300)}
	}
	return out, nil
}

// mapOpenAIError turns a non-2xx answer into the shared sentinels.
func mapOpenAIError(status int, retryAfter string, raw []byte) error {
	var env errorEnvelope
	_ = json.Unmarshal(raw, &env)
	detail := strings.TrimSpace(env.Error.Message)
	if detail == "" {
		detail = provider.Truncate(strings.TrimSpace(string(raw)), 300)
	}
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("ai: openai: %w: %s", provider.ErrAuth, detail)
	case http.StatusTooManyRequests:
		return &provider.RetryAfterError{
			Err:   fmt.Errorf("ai: openai: %w: %s", provider.ErrRateLimited, detail),
			After: provider.ParseRetryAfter(retryAfter, defaultRetryAfter),
		}
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return fmt.Errorf("ai: openai: %w: %s", provider.ErrInvalid, detail)
	default:
		return &provider.StatusError{Provider: "openai", Code: status, Body: provider.Truncate(string(raw), 300)}
	}
}

// compile-time proof that the provider satisfies the interface.
var _ Provider = (*OpenAIAPIProvider)(nil)
