package ai_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/ai"
	"github.com/bory/karvon-be/internal/campaign/ai/fake"
	"github.com/bory/karvon-be/internal/campaign/provider"
)

// documented is a reply in exactly the shape the schema asks for.
const documented = `{
  "components": [
    {"type": "subject", "name": "Quick question", "body": "Quick question, {{first_name|there}}", "tags": ["curiosity"]},
    {"type": "hook", "name": "Saw the news", "body": "Saw {{company}} just opened a second warehouse.", "tags": []},
    {"type": "cta", "name": "15 minutes", "body": "Worth 15 minutes next week?", "tags": ["call"]}
  ],
  "variants": [
    {"name": "Curiosity", "subject": 0, "hook": 1, "problem": null, "value_prop": null, "proof": null, "cta": 2, "closing": null, "ps": null}
  ]
}`

func TestManualProviderBuildsAPromptContainingTheBrief(t *testing.T) {
	p := ai.NewManual()
	if p.Name() != campaign.AIProviderManual || p.Mode() != ai.ModeManual || p.Model() != "" {
		t.Fatalf("identity = %s/%s/%q", p.Name(), p.Mode(), p.Model())
	}

	prompt, err := p.BuildPrompt(ai.Brief{
		CampaignGoal:     "Book demos with logistics ops leads",
		Company:          "Karvon",
		Product:          "Route planning software",
		TargetIndustry:   "3PL logistics",
		TargetJobTitle:   "Head of Operations",
		PainPoints:       []string{"Late deliveries", " Manual dispatch "},
		ValueProposition: "Cut planning time by half",
		SubjectCount:     7,
		VariantCount:     2,
	})
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	for _, want := range []string{
		"Book demos with logistics ops leads", "Karvon", "Route planning software", "3PL logistics",
		"Head of Operations", "- Late deliveries", "- Manual dispatch", "Cut planning time by half",
		"- subject: 7", "- hook: 4", "- problem: 3", "- value_prop: 3", "- proof: 2", "- cta: 4",
		"- closing: 2", "- ps: 1", "- variants: 2",
		"{{first_name|there}}", "{{company}}", "{{title}}",
		"Reply with ONLY a JSON object matching this schema", `"additionalProperties": false`,
		ai.PromptVersion, "Language: English",
	} {
		if !strings.Contains(prompt.Text, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if !strings.Contains(prompt.System, "Reply with ONLY") || strings.Contains(prompt.System, "Karvon") {
		t.Fatal("the system block should hold the contract, not the brief")
	}
	if !strings.Contains(prompt.User, "Karvon") || strings.Contains(prompt.User, "Reply with ONLY a JSON object matching") {
		t.Fatal("the user block should hold the brief, not the schema")
	}
	if prompt.Text != prompt.System+"\n\n"+prompt.User {
		t.Fatal("Text must be System and User joined by a blank line")
	}
	if strings.Contains(prompt.Text, "[[") || strings.Contains(prompt.Text, "<no value>") {
		t.Fatal("template delimiters or missing values leaked into the prompt")
	}

	if _, _, err := p.Generate(context.Background(), ai.Brief{}); !errors.Is(err, ai.ErrUnsupported) {
		t.Fatalf("manual Generate = %v, want ErrUnsupported", err)
	}
}

func TestParseAcceptsTheDocumentedShape(t *testing.T) {
	out, err := ai.ParseOutput(documented)
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	if len(out.Components) != 3 || len(out.Variants) != 1 || len(out.Warnings) != 0 {
		t.Fatalf("out = %+v", out)
	}
	if out.Components[0].Type != "subject" || out.Components[0].Tags[0] != "curiosity" || out.Components[1].Tags != nil {
		t.Fatalf("components = %+v", out.Components)
	}
	v := out.Variants[0]
	if v.Name != "Curiosity" || v.SubjectIndex != 0 || v.HookIndex == nil || *v.HookIndex != 1 ||
		v.CTAIndex == nil || *v.CTAIndex != 2 || v.ProblemIndex != nil || v.PSIndex != nil {
		t.Fatalf("variant = %+v", v)
	}
}

func TestParseStripsMarkdownFences(t *testing.T) {
	raw := "Sure! Here is the JSON you asked for:\n\n```json\n" + documented + "\n```\n\nLet me know if you want changes."
	out, err := ai.ParseOutput(raw)
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	if len(out.Components) != 3 {
		t.Fatalf("components = %d", len(out.Components))
	}

	_, err = ai.ParseOutput("no json here")
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Code != apperr.CodeValidationFailed {
		t.Fatalf("prose-only err = %v, want validation", err)
	}
}

func TestParseRejectsAnUnknownComponentType(t *testing.T) {
	_, err := ai.ParseOutput(`{"components":[{"type":"headline","name":"x","body":"y","tags":[]}],"variants":[]}`)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Code != apperr.CodeValidationFailed {
		t.Fatalf("err = %v, want validation", err)
	}
	if len(appErr.Fields) != 1 || appErr.Fields[0].Field != "components[0].type" {
		t.Fatalf("fields = %+v", appErr.Fields)
	}

	_, err = ai.ParseOutput(`{"components":[{"type":"subject","name":"  ","body":"y","tags":[]}],"variants":[]}`)
	if !errors.As(err, &appErr) || len(appErr.Fields) != 1 || appErr.Fields[0].Field != "components[0].name" {
		t.Fatalf("blank name err = %v", err)
	}

	_, err = ai.ParseOutput(`{"components":[],"variants":[]}`)
	if !errors.As(err, &appErr) || appErr.Code != apperr.CodeValidationFailed {
		t.Fatalf("empty err = %v, want validation", err)
	}
}

func TestParseDropsAVariantWhoseSubjectIndexIsNotASubject(t *testing.T) {
	raw := `{"components":[
	  {"type":"subject","name":"S","body":"s","tags":[]},
	  {"type":"hook","name":"H","body":"h","tags":[]}],
	  "variants":[
	  {"name":"bad subject","subject":1,"hook":null,"problem":null,"value_prop":null,"proof":null,"cta":null,"closing":null,"ps":null},
	  {"name":"bad hook","subject":0,"hook":0,"problem":null,"value_prop":null,"proof":null,"cta":null,"closing":null,"ps":null},
	  {"name":"out of range","subject":0,"hook":9,"problem":null,"value_prop":null,"proof":null,"cta":null,"closing":null,"ps":null},
	  {"name":"","subject":0,"hook":1,"problem":null,"value_prop":null,"proof":null,"cta":null,"closing":null,"ps":null}]}`
	out, err := ai.ParseOutput(raw)
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	if len(out.Variants) != 1 || out.Variants[0].Name != "Variant 1" {
		t.Fatalf("variants = %+v", out.Variants)
	}
	if len(out.Warnings) != 3 {
		t.Fatalf("warnings = %v", out.Warnings)
	}
	if !strings.Contains(out.Warnings[0], "variant 0 dropped") || !strings.Contains(out.Warnings[0], "points at a hook") {
		t.Fatalf("warning[0] = %q", out.Warnings[0])
	}
	if !strings.Contains(out.Warnings[2], "out of range") {
		t.Fatalf("warning[2] = %q", out.Warnings[2])
	}
}

func TestParseSuffixesDuplicateNames(t *testing.T) {
	raw := `{"components":[
	  {"type":"subject","name":"Quick question","body":"a","tags":[]},
	  {"type":"subject","name":"quick question","body":"b","tags":[]},
	  {"type":"subject","name":"Quick question 2","body":"c","tags":[]},
	  {"type":"subject","name":"Quick Question","body":"d","tags":[]}],
	  "variants":[]}`
	out, err := ai.ParseOutput(raw)
	if err != nil {
		t.Fatalf("ParseOutput: %v", err)
	}
	names := []string{out.Components[0].Name, out.Components[1].Name, out.Components[2].Name, out.Components[3].Name}
	// Matching is case-insensitive, and a literal "Quick question 2" that arrives
	// after the renamed second component is itself a duplicate.
	want := []string{"Quick question", "quick question 2", "Quick question 2 2", "Quick Question 3"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %q, want %q", names, want)
		}
	}
	if len(out.Warnings) != 3 || !strings.Contains(out.Warnings[0], "duplicate name") {
		t.Fatalf("warnings = %v", out.Warnings)
	}
}

func TestOpenAIProviderSendsStructuredOutputFormat(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		reply, _ := json.Marshal(map[string]any{
			"id": "resp_1", "model": "gpt-test-2026",
			"output": []map[string]any{
				{"type": "reasoning", "content": []any{}},
				{"type": "message", "content": []map[string]any{{"type": "output_text", "text": documented}}},
			},
			"usage": map[string]int{"input_tokens": 321, "output_tokens": 123},
		})
		_, _ = w.Write(reply)
	}))
	t.Cleanup(srv.Close)

	p := ai.NewOpenAI(ai.OpenAIConfig{APIKey: "sk-test", BaseURL: srv.URL + "/", Model: "gpt-test"})
	if p.Name() != campaign.AIProviderOpenAI || p.Mode() != ai.ModeAPI || p.Model() != "gpt-test" {
		t.Fatalf("identity = %s/%s/%q", p.Name(), p.Mode(), p.Model())
	}

	out, usage, err := p.Generate(context.Background(), ai.Brief{Company: "Karvon"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if gotAuth != "Bearer sk-test" || gotPath != "/responses" {
		t.Fatalf("request = %s %q", gotPath, gotAuth)
	}
	if gotBody["model"] != "gpt-test" {
		t.Fatalf("model = %v", gotBody["model"])
	}
	instructions, _ := gotBody["instructions"].(string)
	input, _ := gotBody["input"].(string)
	if !strings.Contains(instructions, "Reply with ONLY") || !strings.Contains(input, "Karvon") {
		t.Fatal("instructions should carry the system block and input the brief")
	}
	text, _ := gotBody["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format["type"] != "json_schema" || format["strict"] != true || format["name"] != "cold_email_content" {
		t.Fatalf("text.format = %v", format)
	}
	if _, ok := format["schema"].(map[string]any); !ok {
		t.Fatalf("schema = %T", format["schema"])
	}
	if mot, _ := gotBody["max_output_tokens"].(float64); mot <= 0 {
		t.Fatal("max_output_tokens must be set")
	}
	if usage.Model != "gpt-test-2026" || usage.InputTokens != 321 || usage.OutputTokens != 123 {
		t.Fatalf("usage = %+v", usage)
	}
	if len(out.Components) != 3 || len(out.Variants) != 1 {
		t.Fatalf("out = %+v", out)
	}
}

func TestOpenAIProviderReadsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"resp_2","model":"gpt-test","output":[{"type":"message","content":[{"type":"refusal","refusal":"I can't help with that."}]}],"usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	t.Cleanup(srv.Close)
	p := ai.NewOpenAI(ai.OpenAIConfig{APIKey: "sk-test", BaseURL: srv.URL})

	_, usage, err := p.Generate(context.Background(), ai.Brief{})
	if !errors.Is(err, ai.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if !strings.Contains(err.Error(), "I can't help with that.") {
		t.Fatalf("err should carry the refusal text: %v", err)
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Fatalf("usage should still be reported: %+v", usage)
	}
}

func TestOpenAIProviderMapsAuthAndRateLimitErrors(t *testing.T) {
	status := http.StatusUnauthorized
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"nope","type":"invalid_request_error"}}`)
	}))
	t.Cleanup(srv.Close)
	p := ai.NewOpenAI(ai.OpenAIConfig{APIKey: "sk-test", BaseURL: srv.URL})

	_, _, err := p.Generate(context.Background(), ai.Brief{})
	if !errors.Is(err, provider.ErrAuth) || strings.Contains(err.Error(), "sk-test") {
		t.Fatalf("401 err = %v, want ErrAuth without the key", err)
	}

	status = http.StatusTooManyRequests
	_, _, err = p.Generate(context.Background(), ai.Brief{})
	if !errors.Is(err, provider.ErrRateLimited) {
		t.Fatalf("429 err = %v, want ErrRateLimited", err)
	}
	if after, ok := provider.RetryAfter(err); !ok || after.Seconds() != 12 {
		t.Fatalf("RetryAfter = %s, %v", after, ok)
	}

	unconfigured := ai.NewOpenAI(ai.OpenAIConfig{})
	if _, _, err := unconfigured.Generate(context.Background(), ai.Brief{}); !errors.Is(err, provider.ErrNotConfigured) {
		t.Fatalf("no key err = %v, want ErrNotConfigured", err)
	}
}

func TestSchemaIsValidJSONAndStrict(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(ai.Schema(), &schema); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	checkStrict(t, "$", schema)

	// The component type enum must match the domain's list exactly.
	props := schema["properties"].(map[string]any)
	items := props["components"].(map[string]any)["items"].(map[string]any)
	enum := items["properties"].(map[string]any)["type"].(map[string]any)["enum"].([]any)
	if len(enum) != len(campaign.ComponentTypes) {
		t.Fatalf("enum has %d entries, want %d", len(enum), len(campaign.ComponentTypes))
	}
	for i, ct := range campaign.ComponentTypes {
		if enum[i] != ct {
			t.Fatalf("enum[%d] = %v, want %s", i, enum[i], ct)
		}
	}
	variant := props["variants"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	hook := variant["hook"].(map[string]any)["type"].([]any)
	if len(hook) != 2 || hook[1] != "null" {
		t.Fatalf("optional index type = %v, want [integer null]", hook)
	}
}

// checkStrict walks every object schema and asserts what OpenAI strict mode needs:
// additionalProperties false and every property listed in required.
func checkStrict(t *testing.T, path string, node map[string]any) {
	t.Helper()
	if node["type"] != "object" {
		if items, ok := node["items"].(map[string]any); ok {
			checkStrict(t, path+"[]", items)
		}
		return
	}
	if node["additionalProperties"] != false {
		t.Errorf("%s: additionalProperties must be false", path)
	}
	props, _ := node["properties"].(map[string]any)
	required, _ := node["required"].([]any)
	if len(required) != len(props) {
		t.Errorf("%s: required lists %d of %d properties", path, len(required), len(props))
	}
	for name, sub := range props {
		found := false
		for _, r := range required {
			if r == name {
				found = true
			}
		}
		if !found {
			t.Errorf("%s.%s: not in required", path, name)
		}
		if child, ok := sub.(map[string]any); ok {
			checkStrict(t, path+"."+name, child)
		}
	}
}

func TestFakeProviderReturnsItsCannedOutput(t *testing.T) {
	want := ai.Output{Components: []ai.Component{{Type: "subject", Name: "S", Body: "s"}}}
	f := fake.New(want)
	if f.Name() != campaign.AIProviderOpenAI || f.Mode() != ai.ModeAPI || f.Model() == "" {
		t.Fatalf("identity = %s/%s/%q", f.Name(), f.Mode(), f.Model())
	}
	out, usage, err := f.Generate(context.Background(), ai.Brief{Company: "Karvon"})
	if err != nil || len(out.Components) != 1 || usage.Model != fake.FakeModel || f.Calls != 1 {
		t.Fatalf("Generate = %+v, %+v, %v; calls %d", out, usage, err, f.Calls)
	}
	if prompt, err := f.BuildPrompt(ai.Brief{Company: "Karvon"}); err != nil || !strings.Contains(prompt.Text, "Karvon") {
		t.Fatalf("BuildPrompt = %v", err)
	}
	if _, err := f.Parse(documented); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	f.ModeValue = ai.ModeManual
	if f.Name() != campaign.AIProviderManual || f.Model() != "" {
		t.Fatalf("manual identity = %s/%q", f.Name(), f.Model())
	}
	if _, _, err := f.Generate(context.Background(), ai.Brief{}); !errors.Is(err, ai.ErrUnsupported) {
		t.Fatalf("manual Generate = %v", err)
	}

	f.ModeValue = ai.ModeAPI
	f.Err = provider.ErrRateLimited
	if _, _, err := f.Generate(context.Background(), ai.Brief{}); !errors.Is(err, provider.ErrRateLimited) {
		t.Fatalf("Err = %v", err)
	}
	if f.Calls != 3 {
		t.Fatalf("calls = %d, want 3", f.Calls)
	}
}
