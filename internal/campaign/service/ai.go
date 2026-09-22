package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/ai"
	"github.com/bory/karvon-be/internal/campaign/render"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// AIProviderInfo describes the configured generator to the UI.
type AIProviderInfo struct {
	Provider string
	Mode     ai.Mode
	Model    string
	Label    string
}

// GenerationView is a generation with its parsed output decoded.
type GenerationView struct {
	Generation dbgen.AiGeneration
	Brief      ai.Brief
	Output     *ai.Output
}

// ImportSelection picks what to import from a parsed generation; nil means all.
type ImportSelection struct {
	ComponentIndexes []int
	VariantIndexes   []int
	CampaignID       *uuid.UUID
}

// ImportOutcome reports what an import created.
type ImportOutcome struct {
	Components []dbgen.EmailComponent
	Variants   []dbgen.EmailVariant
}

// AIProviderInfo reports the generator.
func (s *Service) AIProviderInfo() AIProviderInfo {
	return AIProviderInfo{Provider: s.ai.Name(), Mode: s.ai.Mode(), Model: s.ai.Model(), Label: "ChatGPT"}
}

// CreateGeneration builds the prompt for a brief. In API mode it also runs the
// generation and parses the answer.
func (s *Service) CreateGeneration(ctx context.Context, brief ai.Brief, campaignID *uuid.UUID) (GenerationView, error) {
	if strings.TrimSpace(brief.CampaignGoal) == "" && strings.TrimSpace(brief.ValueProposition) == "" {
		return GenerationView{}, apperr.Validation("brief is invalid", apperr.FieldError{Field: "campaign_goal", Message: "describe the goal or the value proposition"})
	}
	if campaignID != nil {
		if _, err := s.campaign(ctx, *campaignID); err != nil {
			return GenerationView{}, err
		}
	}
	prompt, err := s.ai.BuildPrompt(brief)
	if err != nil {
		return GenerationView{}, apperr.Internal(err)
	}
	briefRaw, _ := json.Marshal(brief)
	status := campaign.GenerationAwaitingPaste
	if s.ai.Mode() == ai.ModeAPI {
		status = campaign.GenerationPromptBuilt
	}
	row, err := s.store.CreateAIGeneration(ctx, dbgen.CreateAIGenerationParams{
		ID: ids.New(), Provider: s.ai.Name(), Model: campaign.Optional(s.ai.Model()), Status: status,
		CampaignID: campaign.NullUUID(campaignID), Brief: briefRaw, Prompt: prompt.Text, PromptVersion: ai.PromptVersion,
	})
	if err != nil {
		return GenerationView{}, apperr.Internal(err)
	}
	if s.ai.Mode() != ai.ModeAPI {
		return s.generationView(row)
	}
	output, usage, err := s.ai.Generate(ctx, brief)
	if err != nil {
		_ = s.store.SetAIGenerationFailed(ctx, dbgen.SetAIGenerationFailedParams{ID: row.ID, Error: campaign.Ptr(err.Error())})
		if errors.Is(err, ai.ErrRefused) {
			return GenerationView{}, apperr.Conflict("the model refused this brief: %s", err.Error())
		}
		return GenerationView{}, providerErr("the AI provider failed", err)
	}
	usageRaw, _ := json.Marshal(usage)
	parsedRaw, _ := json.Marshal(output)
	row, err = s.store.SetAIGenerationParsed(ctx, dbgen.SetAIGenerationParsedParams{
		ID: row.ID, RawOutput: campaign.Ptr(string(parsedRaw)), Parsed: parsedRaw, ComponentCount: campaign.Int32(len(output.Components)),
		VariantCount: campaign.Int32(len(output.Variants)), Usage: usageRaw, Model: campaign.Optional(usage.Model),
	})
	if err != nil {
		return GenerationView{}, apperr.Internal(err)
	}
	return s.generationView(row)
}

// GetGeneration loads one generation.
func (s *Service) GetGeneration(ctx context.Context, id uuid.UUID) (GenerationView, error) {
	row, err := s.store.GetAIGeneration(ctx, id)
	if err != nil {
		return GenerationView{}, notFound("generation", err)
	}
	return s.generationView(row)
}

// ListGenerations returns one page.
func (s *Service) ListGenerations(ctx context.Context, campaignID *uuid.UUID, page, perPage int) (Page[GenerationView], error) {
	rows, err := s.store.ListAIGenerations(ctx, dbgen.ListAIGenerationsParams{CampaignID: campaign.NullUUID(campaignID),
		Lim: campaign.Int32(perPage), Off: campaign.Int32((page - 1) * perPage)})
	if err != nil {
		return Page[GenerationView]{}, apperr.Internal(err)
	}
	total, err := s.store.CountAIGenerations(ctx, campaign.NullUUID(campaignID))
	if err != nil {
		return Page[GenerationView]{}, apperr.Internal(err)
	}
	out := make([]GenerationView, 0, len(rows))
	for _, r := range rows {
		v, err := s.generationView(r)
		if err != nil {
			return Page[GenerationView]{}, err
		}
		out = append(out, v)
	}
	return Page[GenerationView]{Rows: out, Total: total}, nil
}

// ParseGeneration validates pasted model output and stores it.
func (s *Service) ParseGeneration(ctx context.Context, id uuid.UUID, raw string) (GenerationView, error) {
	row, err := s.store.GetAIGeneration(ctx, id)
	if err != nil {
		return GenerationView{}, notFound("generation", err)
	}
	if row.Status == campaign.GenerationImported {
		return GenerationView{}, apperr.Conflict("this generation was already imported")
	}
	if strings.TrimSpace(raw) == "" {
		return GenerationView{}, apperr.Validation("nothing to parse", apperr.FieldError{Field: "raw_output", Message: "paste the model's reply"})
	}
	output, err := s.ai.Parse(raw)
	if err != nil {
		var appErr *apperr.Error
		if errors.As(err, &appErr) {
			return GenerationView{}, err
		}
		return GenerationView{}, apperr.Validation("the pasted text is not valid output", apperr.FieldError{Field: "raw_output", Message: err.Error()})
	}
	parsedRaw, _ := json.Marshal(output)
	row, err = s.store.SetAIGenerationParsed(ctx, dbgen.SetAIGenerationParsedParams{
		ID: id, RawOutput: campaign.Ptr(raw), Parsed: parsedRaw, ComponentCount: campaign.Int32(len(output.Components)),
		VariantCount: campaign.Int32(len(output.Variants)),
	})
	if err != nil {
		return GenerationView{}, apperr.Internal(err)
	}
	return s.generationView(row)
}

// ImportGeneration turns parsed output into ai_generated components and variants.
// It runs once per generation.
func (s *Service) ImportGeneration(ctx context.Context, id uuid.UUID, sel ImportSelection) (ImportOutcome, error) {
	view, err := s.GetGeneration(ctx, id)
	if err != nil {
		return ImportOutcome{}, err
	}
	if view.Generation.Status == campaign.GenerationImported {
		return ImportOutcome{}, apperr.Conflict("this generation was already imported")
	}
	if view.Output == nil {
		return ImportOutcome{}, apperr.Conflict("parse the model output before importing")
	}
	if sel.CampaignID != nil {
		if _, err := s.campaign(ctx, *sel.CampaignID); err != nil {
			return ImportOutcome{}, err
		}
	}
	wantComponent := indexSet(sel.ComponentIndexes, len(view.Output.Components))
	wantVariant := indexSet(sel.VariantIndexes, len(view.Output.Variants))
	// Every component a selected variant needs is imported too.
	for i, v := range view.Output.Variants {
		if !wantVariant[i] {
			continue
		}
		for _, idx := range variantIndexes(v) {
			wantComponent[idx] = true
		}
	}

	var outcome ImportOutcome
	err = s.store.InTx(ctx, func(q *dbgen.Queries) error {
		created := map[int]dbgen.EmailComponent{}
		for i, c := range view.Output.Components {
			if !wantComponent[i] {
				continue
			}
			row, err := q.CreateEmailComponent(ctx, dbgen.CreateEmailComponentParams{
				ID: ids.New(), Type: c.Type, Name: c.Name, Body: c.Body, Status: campaign.ContentAIGenerated,
				Tags: cleanTags(append(c.Tags, "ai")), Language: orDefault(view.Brief.Language, "en"),
				AiGenerationID: uuid.NullUUID{UUID: id, Valid: true}, Placeholders: render.Placeholders(c.Body),
			})
			if err != nil {
				return err
			}
			created[i] = row
			outcome.Components = append(outcome.Components, row)
		}
		for i, v := range view.Output.Variants {
			if !wantVariant[i] {
				continue
			}
			var slots []render.Slot
			var refs []SlotRef
			add := func(slot string, idx *int) {
				if idx == nil {
					return
				}
				c, ok := created[*idx]
				if !ok {
					return
				}
				slots = append(slots, render.Slot{Type: slot, Body: c.Body, ComponentID: c.ID})
				refs = append(refs, SlotRef{Slot: slot, ComponentID: c.ID})
			}
			add(campaign.ComponentSubject, &v.SubjectIndex)
			add(campaign.ComponentHook, v.HookIndex)
			add(campaign.ComponentProblem, v.ProblemIndex)
			add(campaign.ComponentValueProp, v.ValuePropIndex)
			add(campaign.ComponentProof, v.ProofIndex)
			add(campaign.ComponentCTA, v.CTAIndex)
			add(campaign.ComponentClosing, v.ClosingIndex)
			add(campaign.ComponentPS, v.PSIndex)
			if len(slots) == 0 {
				continue
			}
			subject, body := render.Assemble(slots)
			name := v.Name
			if strings.TrimSpace(name) == "" {
				name = fmt.Sprintf("AI variant %d", i+1)
			}
			row, err := q.CreateEmailVariant(ctx, dbgen.CreateEmailVariantParams{
				ID: ids.New(), Name: name, Step: 1, Status: campaign.ContentAIGenerated, SubjectTemplate: subject,
				BodyTemplate: body, ComponentIds: componentIDs(slots), AiGenerationID: uuid.NullUUID{UUID: id, Valid: true},
			})
			if err != nil {
				return err
			}
			for pos, ref := range refs {
				if err := q.AddVariantComponent(ctx, dbgen.AddVariantComponentParams{VariantID: row.ID, ComponentID: ref.ComponentID, Position: campaign.Int32(pos), Slot: ref.Slot}); err != nil {
					return err
				}
			}
			outcome.Variants = append(outcome.Variants, row)
		}
		compIDs := make([]uuid.UUID, 0, len(outcome.Components))
		for _, c := range outcome.Components {
			compIDs = append(compIDs, c.ID)
		}
		varIDs := make([]uuid.UUID, 0, len(outcome.Variants))
		for _, v := range outcome.Variants {
			varIDs = append(varIDs, v.ID)
		}
		_, err := q.SetAIGenerationImported(ctx, dbgen.SetAIGenerationImportedParams{ID: id, ComponentIds: compIDs, VariantIds: varIDs})
		return err
	})
	if err != nil {
		return ImportOutcome{}, apperr.Internal(err)
	}
	if outcome.Components == nil {
		outcome.Components = []dbgen.EmailComponent{}
	}
	if outcome.Variants == nil {
		outcome.Variants = []dbgen.EmailVariant{}
	}
	return outcome, nil
}

func (s *Service) generationView(row dbgen.AiGeneration) (GenerationView, error) {
	view := GenerationView{Generation: row}
	if len(row.Brief) > 0 {
		_ = json.Unmarshal(row.Brief, &view.Brief)
	}
	if len(row.Parsed) > 0 {
		var out ai.Output
		if err := json.Unmarshal(row.Parsed, &out); err == nil {
			view.Output = &out
		}
	}
	return view, nil
}

func indexSet(indexes []int, n int) map[int]bool {
	set := map[int]bool{}
	if indexes == nil {
		for i := 0; i < n; i++ {
			set[i] = true
		}
		return set
	}
	for _, i := range indexes {
		if i >= 0 && i < n {
			set[i] = true
		}
	}
	return set
}

func variantIndexes(v ai.VariantRef) []int {
	out := []int{v.SubjectIndex}
	for _, p := range []*int{v.HookIndex, v.ProblemIndex, v.ValuePropIndex, v.ProofIndex, v.CTAIndex, v.ClosingIndex, v.PSIndex} {
		if p != nil {
			out = append(out, *p)
		}
	}
	return out
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
