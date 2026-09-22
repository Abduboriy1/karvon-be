package ai

import (
	"context"

	"github.com/bory/karvon-be/internal/campaign"
)

// ManualChatGPTProvider is the v1 flow: a ChatGPT subscription gives no API
// access, so the operator copies the prompt into ChatGPT and pastes the JSON
// back. It builds and parses; it never calls anything.
type ManualChatGPTProvider struct{}

// NewManual builds the manual provider.
func NewManual() *ManualChatGPTProvider { return &ManualChatGPTProvider{} }

// Name implements Provider.
func (*ManualChatGPTProvider) Name() string { return campaign.AIProviderManual }

// Mode implements Provider.
func (*ManualChatGPTProvider) Mode() Mode { return ModeManual }

// Model implements Provider; the operator's ChatGPT model is not known here.
func (*ManualChatGPTProvider) Model() string { return "" }

// BuildPrompt implements Provider.
func (*ManualChatGPTProvider) BuildPrompt(brief Brief) (Prompt, error) { return buildPrompt(brief) }

// Parse implements Provider.
func (*ManualChatGPTProvider) Parse(raw string) (Output, error) { return ParseOutput(raw) }

// Generate implements Provider by refusing: there is nothing to call.
func (*ManualChatGPTProvider) Generate(context.Context, Brief) (Output, Usage, error) {
	return Output{}, Usage{}, ErrUnsupported
}

// compile-time proof that the provider satisfies the interface.
var _ Provider = (*ManualChatGPTProvider)(nil)
