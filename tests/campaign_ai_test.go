package integration_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

type generationPayload struct {
	ID             string  `json:"id"`
	Provider       string  `json:"provider"`
	Status         string  `json:"status"`
	Prompt         string  `json:"prompt"`
	PromptVersion  string  `json:"prompt_version"`
	ComponentCount int     `json:"component_count"`
	VariantCount   int     `json:"variant_count"`
	Error          *string `json:"error"`
	Parsed         *struct {
		Components []struct {
			Type string `json:"type"`
			Name string `json:"name"`
			Body string `json:"body"`
		} `json:"components"`
		Variants []struct {
			Name    string `json:"name"`
			Subject int    `json:"subject"`
		} `json:"variants"`
		Warnings []string `json:"warnings"`
	} `json:"parsed"`
}

const pastedOutput = `Here you go!

` + "```json" + `
{
  "components": [
    {"type": "subject", "name": "Evening classes", "body": "Quick question about {{company|your gym}}", "tags": ["short"]},
    {"type": "hook", "name": "Busy evenings", "body": "Hi {{first_name|there}}, your 6pm classes look packed.", "tags": []},
    {"type": "cta", "name": "Fifteen minutes", "body": "Worth 15 minutes next week?", "tags": []}
  ],
  "variants": [
    {"name": "Evening angle", "subject": 0, "hook": 1, "problem": null, "value_prop": null, "proof": null, "cta": 2, "closing": null, "ps": null}
  ]
}
` + "```"

func TestTheManualChatGPTFlowImportsPastedContentAsDrafts(t *testing.T) {
	h, _ := seededHarness(t)
	// The manual flow is the default: a ChatGPT subscription grants no API access.
	h.ai.ModeValue = "manual"

	rec := h.mustRequest(http.MethodGet, "/api/v1/ai/provider", "", http.StatusOK)
	var providerInfo struct {
		Provider string `json:"provider"`
		Mode     string `json:"mode"`
		Label    string `json:"label"`
	}
	decodeInto(t, rec, &providerInfo)
	if providerInfo.Mode != "manual" {
		t.Fatalf("the provider reports mode %q, want manual", providerInfo.Mode)
	}
	if providerInfo.Label != "ChatGPT" {
		t.Errorf("the provider is labelled %q, want ChatGPT", providerInfo.Label)
	}

	brief := `{"brief":{"campaign_goal":"book discovery calls","company":"Karvon","target_industry":"gyms",
		"target_job_title":"owner","pain_points":["empty off-peak classes"],"value_proposition":"fill off-peak hours",
		"desired_cta":"a 15 minute call","tone":"direct","language":"en","variant_count":1}}`
	rec = h.mustRequest(http.MethodPost, "/api/v1/ai/generations", brief, http.StatusCreated)
	generation := decodeBody[generationPayload](t, rec)

	if generation.Prompt == "" {
		t.Fatal("no prompt was built for the operator to paste")
	}
	if !strings.Contains(generation.Prompt, "gyms") {
		t.Error("the prompt does not carry the brief")
	}
	if generation.PromptVersion == "" {
		t.Error("the generation records no prompt version")
	}

	// The operator pastes ChatGPT's reply back, fences and prose included.
	body, err := jsonString(map[string]any{"raw_output": pastedOutput})
	if err != nil {
		t.Fatal(err)
	}
	rec = h.mustRequest(http.MethodPost, "/api/v1/ai/generations/"+generation.ID+"/parse", body, http.StatusOK)
	parsed := decodeBody[generationPayload](t, rec)
	if parsed.Status != "parsed" {
		t.Fatalf("the generation is %s, want parsed", parsed.Status)
	}
	if parsed.Parsed == nil || len(parsed.Parsed.Components) != 3 || len(parsed.Parsed.Variants) != 1 {
		t.Fatalf("the parsed output is %+v", parsed.Parsed)
	}

	// Importing creates content for review, never live content.
	rec = h.mustRequest(http.MethodPost, "/api/v1/ai/generations/"+generation.ID+"/import", `{}`, http.StatusOK)
	var imported struct {
		Components []componentPayload `json:"components"`
		Variants   []variantPayload   `json:"variants"`
	}
	decodeInto(t, rec, &imported)
	if len(imported.Components) != 3 || len(imported.Variants) != 1 {
		t.Fatalf("the import created %d components and %d variants", len(imported.Components), len(imported.Variants))
	}
	for _, comp := range imported.Components {
		if comp.Status != "ai_generated" {
			t.Errorf("component %s is %s, want ai_generated", comp.Name, comp.Status)
		}
	}
	if imported.Variants[0].Status != "ai_generated" {
		t.Errorf("the variant is %s, want ai_generated", imported.Variants[0].Status)
	}
	if imported.Variants[0].SubjectTemplate == "" || imported.Variants[0].BodyTemplate == "" {
		t.Error("the imported variant was not assembled")
	}

	// A second import is refused, so a double click cannot duplicate the library.
	h.mustRequest(http.MethodPost, "/api/v1/ai/generations/"+generation.ID+"/import", `{}`, http.StatusConflict)
}

func TestAIGeneratedContentCannotGoStraightIntoACampaign(t *testing.T) {
	h, _ := seededHarness(t)
	h.configureInstantly()
	accountID := h.seedSendingAccount("sender@karvon.test")
	h.ai.ModeValue = "manual"

	rec := h.mustRequest(http.MethodPost, "/api/v1/ai/generations",
		`{"brief":{"campaign_goal":"book calls","value_proposition":"fill off-peak hours","variant_count":1}}`,
		http.StatusCreated)
	generation := decodeBody[generationPayload](t, rec)
	body, err := jsonString(map[string]any{"raw_output": pastedOutput})
	if err != nil {
		t.Fatal(err)
	}
	h.mustRequest(http.MethodPost, "/api/v1/ai/generations/"+generation.ID+"/parse", body, http.StatusOK)
	rec = h.mustRequest(http.MethodPost, "/api/v1/ai/generations/"+generation.ID+"/import", `{}`, http.StatusOK)
	var imported struct {
		Variants []variantPayload `json:"variants"`
	}
	decodeInto(t, rec, &imported)
	variantID := imported.Variants[0].ID

	rec = h.mustRequest(http.MethodPost, "/api/v1/campaigns", `{"name":"AI drafted","steps":1}`, http.StatusCreated)
	camp := decodeBody[campaignPayload](t, rec)
	h.mustRequest(http.MethodPut, "/api/v1/campaigns/"+camp.ID+"/sending-accounts",
		fmt.Sprintf(`{"sending_account_ids":[%q]}`, accountID), http.StatusOK)

	// Unreviewed AI output cannot be attached to a campaign.
	h.mustRequest(http.MethodPut, "/api/v1/campaigns/"+camp.ID+"/variants",
		fmt.Sprintf(`{"items":[{"variant_id":%q,"step":1,"weight":100}]}`, variantID), http.StatusUnprocessableEntity)

	// A human reviews and approves it, and then it is allowed.
	rec = h.mustRequest(http.MethodGet, "/api/v1/content/variants/"+variantID, "", http.StatusOK)
	var detail struct {
		Components []componentPayload `json:"components"`
	}
	decodeInto(t, rec, &detail)
	for _, comp := range detail.Components {
		for _, status := range []string{"reviewed", "approved"} {
			h.mustRequest(http.MethodPost, "/api/v1/content/components/"+comp.ID+"/status",
				fmt.Sprintf(`{"status":%q}`, status), http.StatusOK)
		}
	}
	for _, status := range []string{"reviewed", "approved"} {
		h.mustRequest(http.MethodPost, "/api/v1/content/variants/"+variantID+"/status",
			fmt.Sprintf(`{"status":%q}`, status), http.StatusOK)
	}
	h.mustRequest(http.MethodPut, "/api/v1/campaigns/"+camp.ID+"/variants",
		fmt.Sprintf(`{"items":[{"variant_id":%q,"step":1,"weight":100}]}`, variantID), http.StatusOK)
}

func TestPastingSomethingThatIsNotTheContractIsRejected(t *testing.T) {
	h, _ := seededHarness(t)
	h.ai.ModeValue = "manual"

	rec := h.mustRequest(http.MethodPost, "/api/v1/ai/generations",
		`{"brief":{"campaign_goal":"book calls","value_proposition":"fill off-peak hours"}}`, http.StatusCreated)
	generation := decodeBody[generationPayload](t, rec)

	for _, raw := range []string{
		"I'm sorry, I can't help with that.",
		`{"components":[{"type":"haiku","name":"n","body":"b"}],"variants":[]}`,
	} {
		body, err := jsonString(map[string]any{"raw_output": raw})
		if err != nil {
			t.Fatal(err)
		}
		h.mustRequest(http.MethodPost, "/api/v1/ai/generations/"+generation.ID+"/parse", body, http.StatusUnprocessableEntity)
	}
	h.mustRequest(http.MethodPost, "/api/v1/ai/generations/"+generation.ID+"/import", `{}`, http.StatusConflict)
}
