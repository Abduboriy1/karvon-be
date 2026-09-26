package httpapi

import (
	"net/http"

	campaignsvc "github.com/bory/karvon-be/internal/campaign/service"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/http/gen"
)

// ListContacts implements GET /contacts.
func (s *Server) ListContacts(w http.ResponseWriter, r *http.Request, params gen.ListContactsParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	filter := db.ContactFilter{
		Suppressed: params.Suppressed,
		HasConsent: params.HasConsent,
		Excluded:   params.Excluded,
		CampaignID: params.CampaignId,
		Q:          params.Q,
	}
	if params.Stage != nil {
		for _, stage := range *params.Stage {
			filter.Stages = append(filter.Stages, string(stage))
		}
	}

	sort, err := resolveSortParam(params.Sort, db.ContactSortKeys())
	if err != nil {
		WriteError(w, r, err)
		return
	}

	result, err := s.campaigns.ListContacts(r.Context(), filter, sort, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	data := make([]gen.Contact, 0, len(result.Rows))
	for _, row := range result.Rows {
		data = append(data, toAPIContact(row))
	}
	writeJSON(w, r, http.StatusOK, gen.ContactList{
		Data: data,
		Meta: pageMeta(page, perPage, result.Total),
	})
}

// GetContact implements GET /contacts/{id}.
func (s *Server) GetContact(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	detail, err := s.campaigns.GetContact(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIContactDetail(detail))
}

// UpdateContact implements PATCH /contacts/{id}. The address is immutable; every
// other profile field can be set, and an explicit null clears it.
func (s *Server) UpdateContact(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var body gen.ContactUpdate
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	in := campaignsvc.ContactUpdate{
		FirstName:  body.FirstName,
		LastName:   body.LastName,
		Company:    body.Company,
		Title:      body.Title,
		Phone:      body.Phone,
		Website:    body.Website,
		Attributes: derefMap(body.Attributes),
	}
	detail, err := s.campaigns.UpdateContact(r.Context(), id, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIContactDetail(detail))
}

// GetContactTimeline implements GET /contacts/{id}/timeline.
func (s *Server) GetContactTimeline(w http.ResponseWriter, r *http.Request, id gen.IdPath, params gen.GetContactTimelineParams) {
	page, perPage := paginate(params.Page, params.PerPage)

	result, err := s.campaigns.ContactTimeline(r.Context(), id, page, perPage)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, apiContactEventList(result, page, perPage))
}

// RequestContactPermission implements POST /contacts/{id}/request-permission.
func (s *Server) RequestContactPermission(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var body gen.PermissionRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	detail, err := s.campaigns.RequestPermission(r.Context(), id, deref(body.Note))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIContactDetail(detail))
}

// CreateContactConsent implements POST /contacts/{id}/consents.
func (s *Server) CreateContactConsent(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var body gen.ConsentCreate
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	in := campaignsvc.ConsentInput{
		Source:         string(body.Source),
		Evidence:       body.Evidence,
		CapturedBy:     body.CapturedBy,
		CampaignLeadID: body.CampaignLeadId,
	}
	// An omitted capture time means now, which the service fills in.
	if body.CapturedAt != nil {
		in.CapturedAt = *body.CapturedAt
	}

	consent, err := s.campaigns.CaptureConsent(r.Context(), id, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toAPIConsent(consent))
}

// RevokeContactConsent implements POST /contacts/{id}/consents/{consent_id}/revoke.
func (s *Server) RevokeContactConsent(w http.ResponseWriter, r *http.Request, id gen.IdPath, consentID gen.ConsentIdPath) {
	var body gen.ConsentRevokeRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	detail, err := s.campaigns.RevokeConsent(r.Context(), id, consentID, deref(body.Reason))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIContactDetail(detail))
}

// SuppressContact implements POST /contacts/{id}/suppress.
func (s *Server) SuppressContact(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var body gen.SuppressRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	detail, err := s.campaigns.SuppressContact(r.Context(), id, string(body.Reason), deref(body.Note))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIContactDetail(detail))
}

// LiftContactSuppression implements POST
// /contacts/{id}/suppressions/{suppression_id}/lift.
func (s *Server) LiftContactSuppression(w http.ResponseWriter, r *http.Request, id gen.IdPath, suppressionID gen.SuppressionIdPath) {
	var body gen.SuppressionLiftRequest
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}

	detail, err := s.campaigns.LiftSuppression(r.Context(), id, suppressionID, body.Note)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIContactDetail(detail))
}

// deref reads an optional string body field, treating an omitted one as empty.
func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
