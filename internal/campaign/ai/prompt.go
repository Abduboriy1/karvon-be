package ai

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/bory/karvon-be/internal/campaign"
)

// schemaJSON is the strict JSON schema the model's reply must match. It is both
// pasted into the prompt for the manual flow and sent as the structured-output
// format to the API.
//
//go:embed schema.json
var schemaJSON string

// promptTemplate holds two named blocks, "system" and "user". It uses [[ ]] as
// delimiters so the {{first_name}} placeholders in the text reach the model.
//
//go:embed prompt.tmpl
var promptTemplate string

// Default counts, used when the brief asks for zero of something.
const (
	DefaultSubjectCount   = 5
	DefaultHookCount      = 4
	DefaultProblemCount   = 3
	DefaultValuePropCount = 3
	DefaultProofCount     = 2
	DefaultCTACount       = 4
	DefaultClosingCount   = 2
	DefaultPSCount        = 1
	DefaultVariantCount   = 4
)

// MaxCount caps any single requested count so a typo cannot ask for a thousand
// subject lines.
const MaxCount = 20

// Counts is how many of each component the prompt asks for.
type Counts struct {
	Subject   int
	Hook      int
	Problem   int
	ValueProp int
	Proof     int
	CTA       int
	Closing   int
	PS        int
	Variants  int
}

// Schema returns the embedded output schema, compacted.
func Schema() json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(schemaJSON)); err != nil {
		// The schema is a checked-in file; a test guards it.
		return json.RawMessage(schemaJSON)
	}
	return json.RawMessage(buf.Bytes())
}

// CountsFor resolves the brief's counts, filling defaults and clamping. BodyCount
// covers the middle of the email: problem, value_prop and proof.
func CountsFor(brief Brief) Counts {
	c := Counts{
		Subject:   pick(brief.SubjectCount, DefaultSubjectCount),
		Hook:      pick(brief.HookCount, DefaultHookCount),
		Problem:   pick(brief.BodyCount, DefaultProblemCount),
		ValueProp: pick(brief.BodyCount, DefaultValuePropCount),
		Proof:     pick(brief.BodyCount, DefaultProofCount),
		CTA:       pick(brief.CTACount, DefaultCTACount),
		Closing:   DefaultClosingCount,
		PS:        DefaultPSCount,
		Variants:  pick(brief.VariantCount, DefaultVariantCount),
	}
	return c
}

func pick(n, fallback int) int {
	if n <= 0 {
		return fallback
	}
	if n > MaxCount {
		return MaxCount
	}
	return n
}

var parsedTemplate = template.Must(template.New("prompt").Delims("[[", "]]").Parse(promptTemplate))

type promptData struct {
	Brief          Brief
	Counts         Counts
	ComponentTypes []string
	Schema         string
	PromptVersion  string
}

// buildPrompt renders the system and user blocks for a brief. Text is the two
// joined, which is what the operator pastes into ChatGPT.
func buildPrompt(brief Brief) (Prompt, error) {
	pretty, err := json.MarshalIndent(Schema(), "", "  ")
	if err != nil {
		return Prompt{}, fmt.Errorf("ai: format schema: %w", err)
	}
	data := promptData{
		Brief:          trimBrief(brief),
		Counts:         CountsFor(brief),
		ComponentTypes: campaign.ComponentTypes,
		Schema:         string(pretty),
		PromptVersion:  PromptVersion,
	}

	system, err := render("system", data)
	if err != nil {
		return Prompt{}, err
	}
	user, err := render("user", data)
	if err != nil {
		return Prompt{}, err
	}
	return Prompt{System: system, User: user, Text: system + "\n\n" + user}, nil
}

func render(name string, data promptData) (string, error) {
	var buf bytes.Buffer
	if err := parsedTemplate.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("ai: render %s prompt: %w", name, err)
	}
	return strings.TrimSpace(buf.String()), nil
}

// trimBrief strips whitespace and drops empty pain points so the template's
// "(not given)" fallbacks work.
func trimBrief(b Brief) Brief {
	b.CampaignGoal = strings.TrimSpace(b.CampaignGoal)
	b.Company = strings.TrimSpace(b.Company)
	b.Product = strings.TrimSpace(b.Product)
	b.TargetIndustry = strings.TrimSpace(b.TargetIndustry)
	b.TargetJobTitle = strings.TrimSpace(b.TargetJobTitle)
	b.TargetCompanySize = strings.TrimSpace(b.TargetCompanySize)
	b.ValueProposition = strings.TrimSpace(b.ValueProposition)
	b.DesiredCTA = strings.TrimSpace(b.DesiredCTA)
	b.Tone = strings.TrimSpace(b.Tone)
	b.Language = strings.TrimSpace(b.Language)
	b.AdditionalContext = strings.TrimSpace(b.AdditionalContext)
	points := make([]string, 0, len(b.PainPoints))
	for _, p := range b.PainPoints {
		if p = strings.TrimSpace(p); p != "" {
			points = append(points, p)
		}
	}
	b.PainPoints = points
	return b
}
