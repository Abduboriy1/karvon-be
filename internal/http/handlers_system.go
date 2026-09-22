package httpapi

import (
	"net/http"
	"sync"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/http/gen"
)

var (
	specOnce  sync.Once
	specBytes []byte
	specErr   error
)

// GetHealth implements GET /healthz. It returns 503 when a dependency is down so a
// load balancer can take the instance out of rotation.
func (s *Server) GetHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	dbOK := s.health == nil || s.health.CheckDB(ctx) == nil
	queueOK := s.health == nil || s.health.CheckQueue(ctx) == nil

	version := s.cfg.Version
	payload := gen.Health{
		Ok:      dbOK && queueOK,
		Db:      dbOK,
		Queue:   queueOK,
		Version: &version,
	}

	status := http.StatusOK
	if !payload.Ok {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, r, status, payload)
}

// GetOpenapiSpec implements GET /openapi.json. The frontend generates its types from
// this document, so it is served without authentication.
func (s *Server) GetOpenapiSpec(w http.ResponseWriter, r *http.Request) {
	specOnce.Do(func() {
		swagger, err := gen.GetSwagger()
		if err != nil {
			specErr = err
			return
		}
		specBytes, specErr = swagger.MarshalJSON()
	})
	if specErr != nil {
		WriteError(w, r, apperr.Internal(specErr))
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	if _, err := w.Write(specBytes); err != nil {
		s.log.Warn("could not write openapi spec", "error", err)
	}
}

// GetScraperStats implements GET /stats/scraper.
func (s *Server) GetScraperStats(w http.ResponseWriter, r *http.Request) {
	result, err := s.stats.Scraper(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	payload, err := toAPIStats(result)
	if err != nil {
		WriteError(w, r, apperr.Internal(err))
		return
	}
	writeJSON(w, r, http.StatusOK, payload)
}
