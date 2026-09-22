// Package ai generates cold-email components and variants from a campaign brief.
//
// Two providers share one interface. ManualChatGPTProvider builds a prompt the
// operator pastes into ChatGPT and parses the JSON they paste back; OpenAIAPIProvider
// does the same round trip against the OpenAI API when a key is configured. The rest
// of the module only sees Provider, so a third provider is a new file, not a rewrite.
package ai

import (
	"context"
	"errors"
)

// Mode is how a provider produces output.
type Mode string

// The two modes.
const (
	// ModeManual means the operator carries the prompt to ChatGPT and back.
	ModeManual Mode = "manual"
	// ModeAPI means Generate calls the provider directly.
	ModeAPI Mode = "api"
)

// PromptVersion is stamped on every generation so a later prompt change can be
// told apart in the analytics.
const PromptVersion = "2026-09-v1"

// ErrUnsupported is returned by Generate on a manual provider.
var ErrUnsupported = errors.New("ai: this provider cannot generate directly; use the manual flow")

// ErrRefused means the model declined the request.
var ErrRefused = errors.New("ai: the model refused the request")

// Brief is everything the generator is told about the campaign.
type Brief struct {
	CampaignGoal      string   `json:"campaign_goal"`
	Company           string   `json:"company"`
	Product           string   `json:"product"`
	TargetIndustry    string   `json:"target_industry"`
	TargetJobTitle    string   `json:"target_job_title"`
	TargetCompanySize string   `json:"target_company_size"`
	PainPoints        []string `json:"pain_points"`
	ValueProposition  string   `json:"value_proposition"`
	DesiredCTA        string   `json:"desired_cta"`
	Tone              string   `json:"tone"`
	Language          string   `json:"language"`
	AdditionalContext string   `json:"additional_context"`
	// Counts asks for a number of each component type; zero means the default.
	SubjectCount int `json:"subject_count"`
	HookCount    int `json:"hook_count"`
	BodyCount    int `json:"body_count"`
	CTACount     int `json:"cta_count"`
	VariantCount int `json:"variant_count"`
}

// Prompt is what the operator copies (or what the API is sent).
type Prompt struct {
	System string `json:"system"`
	User   string `json:"user"`
	// Text is the single block to paste into ChatGPT.
	Text string `json:"text"`
}

// Component is one generated building block.
type Component struct {
	Type string   `json:"type"`
	Name string   `json:"name"`
	Body string   `json:"body"`
	Tags []string `json:"tags,omitempty"`
}

// VariantRef points at components by their index in the components list.
type VariantRef struct {
	Name           string `json:"name"`
	SubjectIndex   int    `json:"subject"`
	HookIndex      *int   `json:"hook,omitempty"`
	ProblemIndex   *int   `json:"problem,omitempty"`
	ValuePropIndex *int   `json:"value_prop,omitempty"`
	ProofIndex     *int   `json:"proof,omitempty"`
	CTAIndex       *int   `json:"cta,omitempty"`
	ClosingIndex   *int   `json:"closing,omitempty"`
	PSIndex        *int   `json:"ps,omitempty"`
}

// Output is the parsed, validated generation.
type Output struct {
	Components []Component  `json:"components"`
	Variants   []VariantRef `json:"variants"`
	// Warnings are non-fatal problems found while parsing (a duplicate name, a
	// variant dropped for a bad index).
	Warnings []string `json:"warnings,omitempty"`
}

// Usage is what an API provider reports about a call.
type Usage struct {
	Model        string `json:"model"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

// Provider is one way of turning a brief into content.
type Provider interface {
	// Name is the stored provider identifier (campaign.AIProviderManual / AIProviderOpenAI).
	Name() string
	// Mode tells the UI whether to show the copy/paste flow or a Generate button.
	Mode() Mode
	// Model is the model id used by an API provider, or "" for manual.
	Model() string
	// BuildPrompt renders the prompt for a brief.
	BuildPrompt(brief Brief) (Prompt, error)
	// Parse validates raw model output against the schema.
	Parse(raw string) (Output, error)
	// Generate runs the whole round trip. Manual providers return ErrUnsupported.
	Generate(ctx context.Context, brief Brief) (Output, Usage, error)
}
