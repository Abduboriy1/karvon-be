// Package service is the campaign module's application layer: the operations the
// HTTP handlers and the River jobs share, on top of the pure domain in package
// campaign and the provider clients.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/assign"
	"github.com/bory/karvon-be/internal/campaign/render"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// ErrNoVariants means a campaign step has no active variant to assign.
var ErrNoVariants = errors.New("campaign: no active variant for this step")

// VariantSlots loads a variant's components in slot order.
func VariantSlots(ctx context.Context, q *dbgen.Queries, variantID uuid.UUID) ([]render.Slot, error) {
	rows, err := q.ListVariantComponents(ctx, variantID)
	if err != nil {
		return nil, fmt.Errorf("campaign: list variant components: %w", err)
	}
	slots := make([]render.Slot, 0, len(rows))
	for _, r := range rows {
		slots = append(slots, render.Slot{Type: r.Slot, Body: r.Body, ComponentID: r.ID})
	}
	return slots, nil
}

// BusinessInfo is the little a business contributes to placeholders.
type BusinessInfo struct {
	Name  string
	City  string
	State string
}

// ContactVars builds the placeholder map for a contact.
func ContactVars(contact dbgen.Contact, business *BusinessInfo) map[string]string {
	in := render.ContactInput{
		FirstName: campaign.Deref(contact.FirstName), LastName: campaign.Deref(contact.LastName), Company: campaign.Deref(contact.Company),
		Title: campaign.Deref(contact.Title), Email: contact.Email, Domain: contact.Domain, Phone: campaign.Deref(contact.Phone),
		Website: campaign.Deref(contact.Website),
	}
	if business != nil {
		in.City, in.State = business.City, business.State
		if in.Company == "" {
			in.Company = business.Name
		}
	}
	return render.ContactVars(in)
}

// Rendered is a variant rendered for one contact.
type Rendered struct {
	Subject  string
	BodyText string
	BodyHTML string
}

// RenderVariant renders a variant's templates for one contact.
func RenderVariant(variant dbgen.EmailVariant, vars map[string]string) Rendered {
	subject := strings.TrimSpace(render.Render(variant.SubjectTemplate, vars))
	body := strings.TrimSpace(render.Render(variant.BodyTemplate, vars))
	return Rendered{Subject: subject, BodyText: body, BodyHTML: render.HTMLBody(body)}
}

// EnsureAssignment returns the lead's assignment for a step, creating it from the
// campaign's weights when none exists. An existing assignment is never changed —
// that is the whole point — even if the weights moved since.
func EnsureAssignment(ctx context.Context, q *dbgen.Queries, camp dbgen.Campaign, lead dbgen.CampaignLead,
	contact dbgen.Contact, business *BusinessInfo, step int,
) (dbgen.VariantAssignment, dbgen.EmailVariant, error) {
	step32 := campaign.Int32(step)
	if existing, err := q.GetVariantAssignment(ctx, dbgen.GetVariantAssignmentParams{CampaignLeadID: lead.ID, Step: step32}); err == nil {
		variant, err := q.GetEmailVariant(ctx, existing.VariantID)
		if err != nil {
			return dbgen.VariantAssignment{}, dbgen.EmailVariant{}, fmt.Errorf("campaign: load assigned variant: %w", err)
		}
		return existing, variant, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return dbgen.VariantAssignment{}, dbgen.EmailVariant{}, fmt.Errorf("campaign: load assignment: %w", err)
	}

	options, err := q.ListActiveCampaignVariantsForStep(ctx, dbgen.ListActiveCampaignVariantsForStepParams{CampaignID: camp.ID, Step: step32})
	if err != nil {
		return dbgen.VariantAssignment{}, dbgen.EmailVariant{}, fmt.Errorf("campaign: list variants: %w", err)
	}
	weighted := make([]assign.Weighted, 0, len(options))
	for _, o := range options {
		weighted = append(weighted, assign.Weighted{ID: o.VariantID, Weight: int(o.Weight)})
	}
	seed := assign.Seed(camp.ID, contact.ID, step, int(camp.WeightsVersion))
	chosen, ok := assign.Pick(seed, weighted)
	if !ok {
		return dbgen.VariantAssignment{}, dbgen.EmailVariant{}, ErrNoVariants
	}
	variant, err := q.GetEmailVariant(ctx, chosen)
	if err != nil {
		return dbgen.VariantAssignment{}, dbgen.EmailVariant{}, fmt.Errorf("campaign: load variant: %w", err)
	}
	slots, err := VariantSlots(ctx, q, variant.ID)
	if err != nil {
		return dbgen.VariantAssignment{}, dbgen.EmailVariant{}, err
	}
	rendered := RenderVariant(variant, ContactVars(contact, business))

	params := dbgen.CreateVariantAssignmentParams{
		ID: ids.New(), CampaignLeadID: lead.ID, Step: step32, VariantID: variant.ID,
		WeightsVersion: camp.WeightsVersion, SeedHash: assign.SeedHex(seed),
		RenderedSubject: rendered.Subject, RenderedBody: rendered.BodyHTML, ComponentIds: variant.ComponentIds,
	}
	for _, slot := range slots {
		id := slot.ComponentID
		switch slot.Type {
		case campaign.ComponentSubject:
			params.SubjectComponentID = uuid.NullUUID{UUID: id, Valid: true}
		case campaign.ComponentHook:
			params.HookComponentID = uuid.NullUUID{UUID: id, Valid: true}
		case campaign.ComponentCTA:
			params.CtaComponentID = uuid.NullUUID{UUID: id, Valid: true}
		}
	}
	created, err := q.CreateVariantAssignment(ctx, params)
	if err != nil {
		return dbgen.VariantAssignment{}, dbgen.EmailVariant{}, fmt.Errorf("campaign: create assignment: %w", err)
	}
	return created, variant, nil
}

// CustomVars is what a lead's assignments become at Instantly: one subject and one
// body variable per step, holding the rendered text.
func CustomVars(assignments []dbgen.VariantAssignment) map[string]any {
	vars := map[string]any{}
	for _, a := range assignments {
		vars[campaign.SubjectVar(int(a.Step))] = a.RenderedSubject
		vars[campaign.BodyVar(int(a.Step))] = a.RenderedBody
	}
	return vars
}

// EncodeCustomVars serialises the map for the campaign_leads.custom_vars column.
func EncodeCustomVars(vars map[string]any) []byte {
	raw, _ := json.Marshal(vars)
	return raw
}
