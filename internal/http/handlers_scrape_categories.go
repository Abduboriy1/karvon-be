package httpapi

import (
	"net/http"

	"github.com/bory/karvon-be/internal/category"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/http/gen"
)

// ListScrapeCategories implements GET /scrape-categories.
func (s *Server) ListScrapeCategories(w http.ResponseWriter, r *http.Request) {
	rows, err := s.categories.List(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	payload := make([]gen.ScrapeCategory, 0, len(rows))
	for _, row := range rows {
		payload = append(payload, toAPIScrapeCategory(row))
	}
	writeJSON(w, r, http.StatusOK, payload)
}

// CreateScrapeCategory implements POST /scrape-categories.
func (s *Server) CreateScrapeCategory(w http.ResponseWriter, r *http.Request) {
	var req gen.ScrapeCategoryCreate
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	terms := req.Terms
	if terms == nil {
		terms = []string{}
	}
	row, err := s.categories.Create(r.Context(), category.Input{Name: &req.Name, Terms: terms})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toAPIScrapeCategory(row))
}

// GetScrapeCategory implements GET /scrape-categories/{id}.
func (s *Server) GetScrapeCategory(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	row, err := s.categories.Get(r.Context(), id)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIScrapeCategory(row))
}

// UpdateScrapeCategory implements PUT /scrape-categories/{id}.
func (s *Server) UpdateScrapeCategory(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	var req gen.ScrapeCategoryUpdate
	if err := decodeJSON(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	in := category.Input{Name: req.Name}
	if req.Terms != nil {
		in.Terms = *req.Terms
		if in.Terms == nil {
			in.Terms = []string{}
		}
	}
	row, err := s.categories.Update(r.Context(), id, in)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIScrapeCategory(row))
}

// DeleteScrapeCategory implements DELETE /scrape-categories/{id}.
func (s *Server) DeleteScrapeCategory(w http.ResponseWriter, r *http.Request, id gen.IdPath) {
	if err := s.categories.Delete(r.Context(), id); err != nil {
		WriteError(w, r, err)
		return
	}
	writeNoContent(w)
}

func toAPIScrapeCategory(row dbgen.ScrapeCategory) gen.ScrapeCategory {
	terms := row.Terms
	if terms == nil {
		terms = []string{}
	}
	return gen.ScrapeCategory{
		Id:        row.ID,
		Name:      row.Name,
		Terms:     terms,
		IsDefault: row.IsDefault,
		CreatedAt: utc(row.CreatedAt),
		UpdatedAt: utc(row.UpdatedAt),
	}
}
