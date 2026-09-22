package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/campaign"
	"github.com/bory/karvon-be/internal/campaign/render"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// ComponentInput creates or updates a component.
type ComponentInput struct {
	Type     string
	Name     *string
	Body     *string
	Tags     []string
	Language *string
	Status   string
}

// ComponentFilter narrows the component list.
type ComponentFilter struct {
	Types    []string
	Statuses []string
	Tag      *string
	Q        *string
}

// ComponentUsage is where a component is used.
type ComponentUsage struct {
	Variants          []dbgen.EmailVariant
	Campaigns         []dbgen.Campaign
	LockedAssignments int64
}

// SlotRef assigns a component to a slot in a variant.
type SlotRef struct {
	Slot        string
	ComponentID uuid.UUID
}

// VariantInput creates or updates a variant.
type VariantInput struct {
	Name       *string
	Step       *int
	Components []SlotRef
	Notes      *string
	Status     string
}

// VariantFilter narrows the variant list.
type VariantFilter struct {
	Statuses []string
	Step     *int
	Q        *string
}

// VariantDetail is a variant with its components and the campaigns using it.
type VariantDetail struct {
	Variant    dbgen.EmailVariant
	Components []dbgen.ListVariantComponentsRow
	Campaigns  []dbgen.Campaign
}

// Preview is a rendered email.
type Preview struct {
	Subject      string
	BodyText     string
	BodyHTML     string
	Placeholders []string
}

/* ----------------------------------------------------------- components */

// CreateComponent stores a new building block.
func (s *Service) CreateComponent(ctx context.Context, in ComponentInput) (dbgen.EmailComponent, error) {
	if err := validateComponent(in, true); err != nil {
		return dbgen.EmailComponent{}, err
	}
	status := in.Status
	if status == "" {
		status = campaign.ContentDraft
	}
	if status != campaign.ContentDraft && status != campaign.ContentAIGenerated {
		return dbgen.EmailComponent{}, apperr.Validation("component is invalid", apperr.FieldError{Field: "status", Message: "a new component starts as draft"})
	}
	body := strings.TrimSpace(*in.Body)
	lang := "en"
	if in.Language != nil && *in.Language != "" {
		lang = *in.Language
	}
	row, err := s.store.CreateEmailComponent(ctx, dbgen.CreateEmailComponentParams{
		ID: ids.New(), Type: in.Type, Name: strings.TrimSpace(*in.Name), Body: body, Status: status,
		Tags: cleanTags(in.Tags), Language: lang, Placeholders: render.Placeholders(body),
	})
	if err != nil {
		return dbgen.EmailComponent{}, apperr.Internal(err)
	}
	return row, nil
}

// GetComponent returns one component.
func (s *Service) GetComponent(ctx context.Context, id uuid.UUID) (dbgen.EmailComponent, error) {
	row, err := s.store.GetEmailComponent(ctx, id)
	if err != nil {
		return dbgen.EmailComponent{}, notFound("component", err)
	}
	return row, nil
}

// ListComponents returns one page.
func (s *Service) ListComponents(ctx context.Context, f ComponentFilter, page, perPage int) (Page[dbgen.EmailComponent], error) {
	params := dbgen.ListEmailComponentsParams{Types: orEmpty(f.Types), Statuses: orEmpty(f.Statuses), Tag: f.Tag, Q: f.Q,
		Lim: campaign.Int32(perPage), Off: campaign.Int32((page - 1) * perPage)}
	rows, err := s.store.ListEmailComponents(ctx, params)
	if err != nil {
		return Page[dbgen.EmailComponent]{}, apperr.Internal(err)
	}
	total, err := s.store.CountEmailComponents(ctx, dbgen.CountEmailComponentsParams{Types: params.Types, Statuses: params.Statuses, Tag: f.Tag, Q: f.Q})
	if err != nil {
		return Page[dbgen.EmailComponent]{}, apperr.Internal(err)
	}
	return Page[dbgen.EmailComponent]{Rows: rows, Total: total}, nil
}

// UpdateComponent edits a component. The body is frozen once a variant using it
// has been sent: what was sent must stay reproducible.
func (s *Service) UpdateComponent(ctx context.Context, id uuid.UUID, in ComponentInput) (dbgen.EmailComponent, error) {
	current, err := s.GetComponent(ctx, id)
	if err != nil {
		return dbgen.EmailComponent{}, err
	}
	if err := validateComponent(in, false); err != nil {
		return dbgen.EmailComponent{}, err
	}
	params := dbgen.UpdateEmailComponentParams{ID: id, Language: in.Language}
	if in.Name != nil {
		params.Name = campaign.Ptr(strings.TrimSpace(*in.Name))
	}
	if in.Body != nil && strings.TrimSpace(*in.Body) != current.Body {
		locked, err := s.store.CountLockedAssignmentsForComponent(ctx, id)
		if err != nil {
			return dbgen.EmailComponent{}, apperr.Internal(err)
		}
		if locked > 0 {
			return dbgen.EmailComponent{}, apperr.Conflict("this component has been sent to %d leads; create a new component instead of editing the text", locked)
		}
		body := strings.TrimSpace(*in.Body)
		params.Body = &body
		params.Placeholders = render.Placeholders(body)
	}
	if in.Tags != nil {
		params.Tags = cleanTags(in.Tags)
	}
	row, err := s.store.UpdateEmailComponent(ctx, params)
	if err != nil {
		return dbgen.EmailComponent{}, notFound("component", err)
	}
	if params.Body != nil {
		if err := s.reassembleVariantsUsing(ctx, id); err != nil {
			return dbgen.EmailComponent{}, err
		}
	}
	return row, nil
}

// SetComponentStatus moves a component through the review flow.
func (s *Service) SetComponentStatus(ctx context.Context, id uuid.UUID, status string) (dbgen.EmailComponent, error) {
	current, err := s.GetComponent(ctx, id)
	if err != nil {
		return dbgen.EmailComponent{}, err
	}
	if !campaign.ContentTransitionAllowed(current.Status, status) {
		return dbgen.EmailComponent{}, apperr.Conflict("a %s component cannot become %s", current.Status, status)
	}
	row, err := s.store.SetEmailComponentStatus(ctx, dbgen.SetEmailComponentStatusParams{ID: id, Status: status})
	if err != nil {
		return dbgen.EmailComponent{}, apperr.Internal(err)
	}
	return row, nil
}

// GetComponentUsage lists where a component is used.
func (s *Service) GetComponentUsage(ctx context.Context, id uuid.UUID) (ComponentUsage, error) {
	if _, err := s.GetComponent(ctx, id); err != nil {
		return ComponentUsage{}, err
	}
	variants, err := s.store.ListVariantsUsingComponent(ctx, id)
	if err != nil {
		return ComponentUsage{}, apperr.Internal(err)
	}
	locked, err := s.store.CountLockedAssignmentsForComponent(ctx, id)
	if err != nil {
		return ComponentUsage{}, apperr.Internal(err)
	}
	seen := map[uuid.UUID]bool{}
	var camps []dbgen.Campaign
	for _, v := range variants {
		rows, err := s.store.ListCampaignsUsingVariant(ctx, v.ID)
		if err != nil {
			return ComponentUsage{}, apperr.Internal(err)
		}
		for _, c := range rows {
			if !seen[c.ID] {
				seen[c.ID] = true
				camps = append(camps, c)
			}
		}
	}
	return ComponentUsage{Variants: variants, Campaigns: camps, LockedAssignments: locked}, nil
}

func (s *Service) reassembleVariantsUsing(ctx context.Context, componentID uuid.UUID) error {
	variants, err := s.store.ListVariantsUsingComponent(ctx, componentID)
	if err != nil {
		return apperr.Internal(err)
	}
	for _, v := range variants {
		slots, err := VariantSlots(ctx, s.store.Queries, v.ID)
		if err != nil {
			return apperr.Internal(err)
		}
		subject, body := render.Assemble(slots)
		if _, err := s.store.UpdateEmailVariant(ctx, dbgen.UpdateEmailVariantParams{ID: v.ID, SubjectTemplate: &subject, BodyTemplate: &body}); err != nil {
			return apperr.Internal(err)
		}
	}
	return nil
}

/* -------------------------------------------------------------- variants */

// CreateVariant assembles a new variant from components.
func (s *Service) CreateVariant(ctx context.Context, in VariantInput) (VariantDetail, error) {
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		return VariantDetail{}, apperr.Validation("variant is invalid", apperr.FieldError{Field: "name", Message: "is required"})
	}
	step := 1
	if in.Step != nil {
		step = *in.Step
	}
	if step < 1 || step > campaign.MaxSteps {
		return VariantDetail{}, apperr.Validation("variant is invalid", apperr.FieldError{Field: "step", Message: fmt.Sprintf("must be between 1 and %d", campaign.MaxSteps)})
	}
	slots, components, err := s.resolveSlots(ctx, in.Components)
	if err != nil {
		return VariantDetail{}, err
	}
	status := in.Status
	if status == "" {
		status = campaign.ContentDraft
	}
	subject, body := render.Assemble(slots)
	var detail VariantDetail
	err = s.store.InTx(ctx, func(q *dbgen.Queries) error {
		row, err := q.CreateEmailVariant(ctx, dbgen.CreateEmailVariantParams{
			ID: ids.New(), Name: strings.TrimSpace(*in.Name), Step: campaign.Int32(step), Status: status,
			SubjectTemplate: subject, BodyTemplate: body, ComponentIds: componentIDs(slots), Notes: in.Notes,
		})
		if err != nil {
			return err
		}
		for i, ref := range components {
			if err := q.AddVariantComponent(ctx, dbgen.AddVariantComponentParams{VariantID: row.ID, ComponentID: ref.ComponentID, Position: campaign.Int32(i), Slot: ref.Slot}); err != nil {
				return err
			}
		}
		detail.Variant = row
		return nil
	})
	if err != nil {
		return VariantDetail{}, apperr.Internal(err)
	}
	return s.GetVariant(ctx, detail.Variant.ID)
}

// GetVariant loads a variant with its components.
func (s *Service) GetVariant(ctx context.Context, id uuid.UUID) (VariantDetail, error) {
	row, err := s.store.GetEmailVariant(ctx, id)
	if err != nil {
		return VariantDetail{}, notFound("variant", err)
	}
	comps, err := s.store.ListVariantComponents(ctx, id)
	if err != nil {
		return VariantDetail{}, apperr.Internal(err)
	}
	camps, err := s.store.ListCampaignsUsingVariant(ctx, id)
	if err != nil {
		return VariantDetail{}, apperr.Internal(err)
	}
	return VariantDetail{Variant: row, Components: comps, Campaigns: camps}, nil
}

// ListVariants returns one page.
func (s *Service) ListVariants(ctx context.Context, f VariantFilter, page, perPage int) (Page[dbgen.EmailVariant], error) {
	var step *int32
	if f.Step != nil {
		step = campaign.Ptr(campaign.Int32(*f.Step))
	}
	rows, err := s.store.ListEmailVariants(ctx, dbgen.ListEmailVariantsParams{Statuses: orEmpty(f.Statuses), Step: step, Q: f.Q,
		Lim: campaign.Int32(perPage), Off: campaign.Int32((page - 1) * perPage)})
	if err != nil {
		return Page[dbgen.EmailVariant]{}, apperr.Internal(err)
	}
	total, err := s.store.CountEmailVariants(ctx, dbgen.CountEmailVariantsParams{Statuses: orEmpty(f.Statuses), Step: step, Q: f.Q})
	if err != nil {
		return Page[dbgen.EmailVariant]{}, apperr.Internal(err)
	}
	return Page[dbgen.EmailVariant]{Rows: rows, Total: total}, nil
}

// UpdateVariant edits a variant. Its components are frozen once it has been sent.
func (s *Service) UpdateVariant(ctx context.Context, id uuid.UUID, in VariantInput) (VariantDetail, error) {
	current, err := s.store.GetEmailVariant(ctx, id)
	if err != nil {
		return VariantDetail{}, notFound("variant", err)
	}
	params := dbgen.UpdateEmailVariantParams{ID: id, Notes: in.Notes}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			return VariantDetail{}, apperr.Validation("variant is invalid", apperr.FieldError{Field: "name", Message: "is required"})
		}
		params.Name = &name
	}
	if in.Step != nil {
		if *in.Step < 1 || *in.Step > campaign.MaxSteps {
			return VariantDetail{}, apperr.Validation("variant is invalid", apperr.FieldError{Field: "step", Message: fmt.Sprintf("must be between 1 and %d", campaign.MaxSteps)})
		}
		params.Step = campaign.Ptr(campaign.Int32(*in.Step))
	}
	var components []SlotRef
	if in.Components != nil {
		locked, err := s.store.CountLockedAssignmentsForVariant(ctx, id)
		if err != nil {
			return VariantDetail{}, apperr.Internal(err)
		}
		if locked > 0 {
			return VariantDetail{}, apperr.Conflict("this variant has been sent to %d leads; its components can no longer change", locked)
		}
		slots, refs, err := s.resolveSlots(ctx, in.Components)
		if err != nil {
			return VariantDetail{}, err
		}
		subject, body := render.Assemble(slots)
		params.SubjectTemplate, params.BodyTemplate, params.ComponentIds = &subject, &body, componentIDs(slots)
		components = refs
	}
	err = s.store.InTx(ctx, func(q *dbgen.Queries) error {
		if _, err := q.UpdateEmailVariant(ctx, params); err != nil {
			return err
		}
		if components != nil {
			if err := q.ReplaceVariantComponents(ctx, id); err != nil {
				return err
			}
			for i, ref := range components {
				if err := q.AddVariantComponent(ctx, dbgen.AddVariantComponentParams{VariantID: id, ComponentID: ref.ComponentID, Position: campaign.Int32(i), Slot: ref.Slot}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return VariantDetail{}, apperr.Internal(err)
	}
	_ = current
	return s.GetVariant(ctx, id)
}

// SetVariantStatus moves a variant through the review flow. Approval requires every
// component to be approved (or active) already.
func (s *Service) SetVariantStatus(ctx context.Context, id uuid.UUID, status string) (VariantDetail, error) {
	detail, err := s.GetVariant(ctx, id)
	if err != nil {
		return VariantDetail{}, err
	}
	if !campaign.ContentTransitionAllowed(detail.Variant.Status, status) {
		return VariantDetail{}, apperr.Conflict("a %s variant cannot become %s", detail.Variant.Status, status)
	}
	if status == campaign.ContentApproved || status == campaign.ContentActive {
		if len(detail.Components) == 0 {
			return VariantDetail{}, apperr.Conflict("an empty variant cannot be approved")
		}
		hasSubject := false
		for _, c := range detail.Components {
			if c.Slot == campaign.ComponentSubject {
				hasSubject = true
			}
			if !campaign.ContentUsable(c.Status) {
				return VariantDetail{}, apperr.Conflict("component %q is %s; approve every component first", c.Name, c.Status)
			}
		}
		if !hasSubject {
			return VariantDetail{}, apperr.Conflict("a variant needs a subject before it can be approved")
		}
	}
	if _, err := s.store.SetEmailVariantStatus(ctx, dbgen.SetEmailVariantStatusParams{ID: id, Status: status}); err != nil {
		return VariantDetail{}, apperr.Internal(err)
	}
	return s.GetVariant(ctx, id)
}

// PreviewVariant renders a variant for a contact, or with sample values.
func (s *Service) PreviewVariant(ctx context.Context, id uuid.UUID, contactID *uuid.UUID) (Preview, error) {
	detail, err := s.GetVariant(ctx, id)
	if err != nil {
		return Preview{}, err
	}
	vars, err := s.previewVars(ctx, contactID)
	if err != nil {
		return Preview{}, err
	}
	r := RenderVariant(detail.Variant, vars)
	return Preview{Subject: r.Subject, BodyText: r.BodyText, BodyHTML: r.BodyHTML,
		Placeholders: render.Placeholders(detail.Variant.SubjectTemplate + "\n" + detail.Variant.BodyTemplate)}, nil
}

// PreviewAssembly renders an ad-hoc set of components.
func (s *Service) PreviewAssembly(ctx context.Context, refs []SlotRef, contactID *uuid.UUID) (Preview, error) {
	slots, _, err := s.resolveSlots(ctx, refs)
	if err != nil {
		return Preview{}, err
	}
	vars, err := s.previewVars(ctx, contactID)
	if err != nil {
		return Preview{}, err
	}
	subject, body := render.Assemble(slots)
	r := RenderVariant(dbgen.EmailVariant{SubjectTemplate: subject, BodyTemplate: body}, vars)
	return Preview{Subject: r.Subject, BodyText: r.BodyText, BodyHTML: r.BodyHTML, Placeholders: render.Placeholders(subject + "\n" + body)}, nil
}

func (s *Service) previewVars(ctx context.Context, contactID *uuid.UUID) (map[string]string, error) {
	if contactID == nil {
		return render.ContactVars(render.ContactInput{FirstName: "Alex", LastName: "Rivera", Company: "Acme Fitness",
			Title: "Owner", Email: "alex@acme.example", Domain: "acme.example", City: "Austin", State: "TX"}), nil
	}
	contact, err := s.contact(ctx, *contactID)
	if err != nil {
		return nil, err
	}
	return ContactVars(contact, s.businessInfo(ctx, contact.BusinessID)), nil
}

func (s *Service) resolveSlots(ctx context.Context, refs []SlotRef) ([]render.Slot, []SlotRef, error) {
	if len(refs) == 0 {
		return nil, nil, apperr.Validation("variant is invalid", apperr.FieldError{Field: "components", Message: "at least one component is required"})
	}
	seen := map[string]bool{}
	ids := make([]uuid.UUID, 0, len(refs))
	for i, r := range refs {
		if !campaign.ValidComponentType(r.Slot) {
			return nil, nil, apperr.Validation("variant is invalid", apperr.FieldError{Field: fmt.Sprintf("components[%d].slot", i), Message: "is not a component type"})
		}
		if seen[r.Slot] {
			return nil, nil, apperr.Validation("variant is invalid", apperr.FieldError{Field: fmt.Sprintf("components[%d].slot", i), Message: "each slot can be used once"})
		}
		seen[r.Slot] = true
		ids = append(ids, r.ComponentID)
	}
	rows, err := s.store.ListEmailComponentsByIDs(ctx, ids)
	if err != nil {
		return nil, nil, apperr.Internal(err)
	}
	byID := map[uuid.UUID]dbgen.EmailComponent{}
	for _, c := range rows {
		byID[c.ID] = c
	}
	slots := make([]render.Slot, 0, len(refs))
	for i, r := range refs {
		c, ok := byID[r.ComponentID]
		if !ok {
			return nil, nil, apperr.Validation("variant is invalid", apperr.FieldError{Field: fmt.Sprintf("components[%d].component_id", i), Message: "does not exist"})
		}
		if c.Type != r.Slot {
			return nil, nil, apperr.Validation("variant is invalid", apperr.FieldError{Field: fmt.Sprintf("components[%d].component_id", i), Message: fmt.Sprintf("is a %s component, not a %s", c.Type, r.Slot)})
		}
		if c.Status == campaign.ContentArchived {
			return nil, nil, apperr.Validation("variant is invalid", apperr.FieldError{Field: fmt.Sprintf("components[%d].component_id", i), Message: "is archived"})
		}
		slots = append(slots, render.Slot{Type: c.Type, Body: c.Body, ComponentID: c.ID})
	}
	return slots, refs, nil
}

func componentIDs(slots []render.Slot) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(slots))
	for _, s := range slots {
		out = append(out, s.ComponentID)
	}
	return out
}

func validateComponent(in ComponentInput, create bool) error {
	var fields []apperr.FieldError
	if create && !campaign.ValidComponentType(in.Type) {
		fields = append(fields, apperr.FieldError{Field: "type", Message: "must be one of: " + strings.Join(campaign.ComponentTypes, ", ")})
	}
	if create && (in.Name == nil || strings.TrimSpace(*in.Name) == "") {
		fields = append(fields, apperr.FieldError{Field: "name", Message: "is required"})
	}
	if in.Name != nil && len(strings.TrimSpace(*in.Name)) > 120 {
		fields = append(fields, apperr.FieldError{Field: "name", Message: "must be at most 120 characters"})
	}
	if create && (in.Body == nil || strings.TrimSpace(*in.Body) == "") {
		fields = append(fields, apperr.FieldError{Field: "body", Message: "is required"})
	}
	if in.Body != nil && len(strings.TrimSpace(*in.Body)) > 5000 {
		fields = append(fields, apperr.FieldError{Field: "body", Message: "must be at most 5000 characters"})
	}
	if len(fields) > 0 {
		return apperr.Validation("component is invalid", fields...)
	}
	return nil
}

func cleanTags(tags []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

var _ = pgx.ErrNoRows

// businessInfo loads the placeholder-relevant fields of a contact's business.
func (s *Service) businessInfo(ctx context.Context, id uuid.NullUUID) *BusinessInfo {
	if !id.Valid {
		return nil
	}
	b, err := s.store.GetBusiness(ctx, id.UUID)
	if err != nil {
		return nil
	}
	return &BusinessInfo{Name: b.Name, City: campaign.Deref(b.City), State: campaign.Deref(b.State)}
}
