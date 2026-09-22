// Package fake is a canned ai.Provider for tests: it returns a fixed Output (or
// error) and counts calls. Prompts are built and pasted output parsed for real,
// so a test exercises the same code an operator would.
package fake

import (
	"context"
	"sync"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/ai"
)

// FakeModel is what Model reports in API mode.
const FakeModel = "fake-model"

// Provider is the fake. Set the fields before use; Calls is read after.
type Provider struct {
	// Output is what Generate returns.
	Output ai.Output
	// Err, when set, is returned by Generate instead of Output.
	Err error
	// ModeValue is ModeAPI unless a test wants the manual flow.
	ModeValue ai.Mode
	// Calls counts Generate invocations.
	Calls int
	// Briefs records what Generate was asked for, in order.
	Briefs []ai.Brief

	mu sync.Mutex
}

// New builds an API-mode fake that generates output.
func New(output ai.Output) *Provider {
	return &Provider{Output: output, ModeValue: ai.ModeAPI}
}

// Name implements ai.Provider with a schema-valid provider name for the mode.
func (p *Provider) Name() string {
	if p.Mode() == ai.ModeManual {
		return campaign.AIProviderManual
	}
	return campaign.AIProviderOpenAI
}

// Mode implements ai.Provider.
func (p *Provider) Mode() ai.Mode {
	if p.ModeValue == "" {
		return ai.ModeAPI
	}
	return p.ModeValue
}

// Model implements ai.Provider.
func (p *Provider) Model() string {
	if p.Mode() == ai.ModeManual {
		return ""
	}
	return FakeModel
}

// BuildPrompt implements ai.Provider with the real template.
func (*Provider) BuildPrompt(brief ai.Brief) (ai.Prompt, error) {
	return ai.NewManual().BuildPrompt(brief)
}

// Parse implements ai.Provider with the real parser.
func (*Provider) Parse(raw string) (ai.Output, error) { return ai.ParseOutput(raw) }

// Generate implements ai.Provider.
func (p *Provider) Generate(_ context.Context, brief ai.Brief) (ai.Output, ai.Usage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls++
	p.Briefs = append(p.Briefs, brief)
	if p.Mode() == ai.ModeManual {
		return ai.Output{}, ai.Usage{}, ai.ErrUnsupported
	}
	if p.Err != nil {
		return ai.Output{}, ai.Usage{}, p.Err
	}
	usage := ai.Usage{
		Model:        FakeModel,
		InputTokens:  1000,
		OutputTokens: 100 * len(p.Output.Components),
	}
	return p.Output, usage, nil
}

// compile-time proof that the fake satisfies the interface.
var _ ai.Provider = (*Provider)(nil)
