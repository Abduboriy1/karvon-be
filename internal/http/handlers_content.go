package httpapi

import (
	"net/http"

	campaignsvc "github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/http/gen"
)

/* ------------------------------------------------------------- components */

// ListEmailComponents implements GET /email-components.
func (s *Server) ListEmailComponents(w http.ResponseWriter, r *http.Request, params gen.ListEmailComponentsParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	filter := campaignsvc.ComponentFilter{Tag: params.Tag, Q: params.Q}
	if params.Type != nil {
		for _, value := range *params.Type {
			filter.Types = append(filter.Types, string(value))
		}
	}
	if params.Status != nil {
		for _, value := range *params.Status {
			filter.Statuses = append(filter.Statuses, string(value))
		}
	}

	result, err := s.campaigns.ListComponents(r.Context(), filter, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.EmailComponent, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPIEmailComponent(row))
	}
	writeJSON(w, r, http.StatusOK, gen.EmailComponentList{
		Data: data,
		Meta: pageMeta(page, perPage, result.Total),
	})
}

// CreateEmailComponent implements POST /email-components.
func (s *Server) CreateEmailComponent(w http.ResponseWriter, r *http.Request) {
	var body gen.ComponentCreate
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	in := campaignsvc.ComponentInput{
		Type:     string(body.Type),
		Name:     &body.Name,
		Body:     &body.Body,
		Language: body.Language,
	}
	if body.Tags != nil {
		in.Tags = *body.Tags
	}
	if body.Status != nil {
		in.Status = string(*body.Status)
	}

	row, err := s.campaigns.CreateComponent(r.Context(), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toAPIEmailComponent(row))
}

// GetEmailComponent implements GET /email-components/{id}.
func (s *Server) GetEmailComponent(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	row, err := s.campaigns.GetComponent(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIEmailComponent(row))
}

// UpdateEmailComponent implements PATCH /email-components/{id}.
func (s *Server) UpdateEmailComponent(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var body gen.ComponentUpdate
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	in := campaignsvc.ComponentInput{
		Name:     body.Name,
		Body:     body.Body,
		Language: body.Language,
	}
	if body.Tags != nil {
		in.Tags = *body.Tags
	}

	row, err := s.campaigns.UpdateComponent(r.Context(), id, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIEmailComponent(row))
}

// SetEmailComponentStatus implements PUT /email-components/{id}/status.
func (s *Server) SetEmailComponentStatus(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var body gen.ContentStatusRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	row, err := s.campaigns.SetComponentStatus(r.Context(), id, string(body.Status))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIEmailComponent(row))
}

// GetEmailComponentUsage implements GET /email-components/{id}/usage.
func (s *Server) GetEmailComponentUsage(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	usage, err := s.campaigns.GetComponentUsage(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIComponentUsage(usage))
}

// PreviewEmailComponents implements POST /email-components/preview. It renders a
// set of slots that has not been saved as a variant yet.
func (s *Server) PreviewEmailComponents(w http.ResponseWriter, r *http.Request) {
	var body gen.ComponentPreviewRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	preview, err := s.campaigns.PreviewAssembly(r.Context(), toSlotRefs(&body.Components), body.ContactId)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIPreview(preview))
}

/* --------------------------------------------------------------- variants */

// ListEmailVariants implements GET /email-variants.
func (s *Server) ListEmailVariants(w http.ResponseWriter, r *http.Request, params gen.ListEmailVariantsParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	filter := campaignsvc.VariantFilter{Step: params.Step, Q: params.Q}
	if params.Status != nil {
		for _, value := range *params.Status {
			filter.Statuses = append(filter.Statuses, string(value))
		}
	}

	result, err := s.campaigns.ListVariants(r.Context(), filter, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.EmailVariant, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPIEmailVariant(row))
	}
	writeJSON(w, r, http.StatusOK, gen.EmailVariantList{
		Data: data,
		Meta: pageMeta(page, perPage, result.Total),
	})
}

// CreateEmailVariant implements POST /email-variants.
func (s *Server) CreateEmailVariant(w http.ResponseWriter, r *http.Request) {
	var body gen.VariantCreate
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	in := campaignsvc.VariantInput{
		Name:       &body.Name,
		Step:       body.Step,
		Components: toSlotRefs(&body.Components),
		Notes:      body.Notes,
	}
	detail, err := s.campaigns.CreateVariant(r.Context(), in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toAPIEmailVariantDetail(detail))
}

// GetEmailVariant implements GET /email-variants/{id}.
func (s *Server) GetEmailVariant(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	detail, err := s.campaigns.GetVariant(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIEmailVariantDetail(detail))
}

// UpdateEmailVariant implements PATCH /email-variants/{id}.
func (s *Server) UpdateEmailVariant(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var body gen.VariantUpdate
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	in := campaignsvc.VariantInput{
		Name:       body.Name,
		Step:       body.Step,
		Components: toSlotRefs(body.Components),
		Notes:      body.Notes,
	}
	detail, err := s.campaigns.UpdateVariant(r.Context(), id, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIEmailVariantDetail(detail))
}

// SetEmailVariantStatus implements PUT /email-variants/{id}/status.
func (s *Server) SetEmailVariantStatus(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var body gen.ContentStatusRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	detail, err := s.campaigns.SetVariantStatus(r.Context(), id, string(body.Status))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIEmailVariantDetail(detail))
}

// PreviewEmailVariant implements POST /email-variants/{id}/preview.
func (s *Server) PreviewEmailVariant(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var body gen.VariantPreviewRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	preview, err := s.campaigns.PreviewVariant(r.Context(), id, body.ContactId)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIPreview(preview))
}

// toSlotRefs maps the wire slot list onto the service's. A nil list stays nil: on
// an update that is what tells the service to leave the assembly alone.
func toSlotRefs(in *[]gen.VariantSlot) []campaignsvc.SlotRef {
	if in == nil {
		return nil
	}
	out := make([]campaignsvc.SlotRef, 0, len(*in))
	for _, slot := range *in {
		out = append(out, campaignsvc.SlotRef{Slot: string(slot.Slot), ComponentID: slot.ComponentId})
	}
	return out
}
