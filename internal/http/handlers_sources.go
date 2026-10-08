package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/bory/karvon-be/internal/http/gen"
	"github.com/bory/karvon-be/internal/source"
)

// sourceUpdateRequest mirrors the spec's SourceUpdate. api_key is decoded as raw JSON
// so "field absent" (keep the stored key) can be told apart from "field is null"
// (clear the stored key).
type sourceUpdateRequest struct {
	Name           *string         `json:"name" validate:"omitempty,min=1,max=100"`
	CostPer1kCents *int            `json:"cost_per_1k_cents" validate:"omitempty,gte=0,lte=1000000"`
	Enabled        *bool           `json:"enabled"`
	MaxActiveRuns  *int            `json:"max_active_runs" validate:"omitempty,gte=1,lte=64"`
	APIKey         json.RawMessage `json:"api_key"`
}

// ListSources implements GET /sources. Keys are never included in the response.
func (s *Server) ListSources(w http.ResponseWriter, r *http.Request) {
	rows, err := s.sources.List(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	payload := make([]gen.Source, 0, len(rows))
	for _, row := range rows {
		payload = append(payload, toAPISource(row))
	}
	writeJSON(w, r, http.StatusOK, payload)
}

// GetSource implements GET /sources/{id}.
func (s *Server) GetSource(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	row, err := s.sources.Get(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPISource(row))
}

// UpdateSource implements PUT /sources/{id}.
func (s *Server) UpdateSource(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var req sourceUpdateRequest
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if err := validateStruct(req); err != nil {
		WriteError(w, r, err)
		return
	}

	in := source.UpdateInput{
		Name:           req.Name,
		CostPer1kCents: req.CostPer1kCents,
		Enabled:        req.Enabled,
		MaxActiveRuns:  req.MaxActiveRuns,
	}
	if len(req.APIKey) > 0 {
		in.KeyPresent = true
		var key *string
		if err := json.Unmarshal(req.APIKey, &key); err != nil {
			WriteError(w, r, badRequestFromDecodeError(err))
			return
		}
		in.APIKey = key
	}

	row, err := s.sources.Update(r.Context(), id, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPISource(row))
}

// TestSource implements POST /sources/{id}/test.
func (s *Server) TestSource(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	result, err := s.sources.Test(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	listings := result.ListingsReturned
	testedAt := result.TestedAt
	writeJSON(w, r, http.StatusOK, gen.SourceTestResult{
		Ok:               result.OK,
		Kind:             gen.SourceKind(result.Kind),
		ListingsReturned: &listings,
		Credits:          result.Credits,
		TestedAt:         &testedAt,
	})
}
