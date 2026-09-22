package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/http/gen"
	"github.com/bory/karvon-be/internal/http/middleware"
)

// BasePath is where the API is mounted; the frontend's VITE_API_BASE matches it.
const BasePath = "/api/v1"

// RouterConfig configures the middleware stack.
type RouterConfig struct {
	APIKey         string
	CORSOrigins    []string
	RequestTimeout time.Duration
	Log            *slog.Logger
}

// NewRouter builds the full HTTP handler:
//
//	request id → real ip → access log → panic recovery → CORS → API key → timeout
//
// The timeout is skipped for the SSE and CSV routes, which are long-lived by design.
func NewRouter(server *Server, cfg RouterConfig) http.Handler {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}

	publicPaths := []string{
		"/healthz",
		BasePath + "/healthz",
		BasePath + "/openapi.json",
	}
	// Provider webhooks authenticate themselves: see middleware.PublicPrefix.
	auth := middleware.NewAuthenticator(cfg.APIKey, publicPaths...).
		PublicPrefix(BasePath + "/webhooks/")

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	// chi's RealIP is deliberately not used: it trusts X-Forwarded-For
	// unconditionally, and the access log is more useful with the true peer address.
	r.Use(middleware.Logger(log))
	r.Use(recoverer(log))
	r.Use(corsMiddleware(cfg.CORSOrigins))
	r.Use(auth.Middleware(writeUnauthorized))
	r.Use(middleware.Timeout(cfg.RequestTimeout))

	// Container and load-balancer probes commonly hit /healthz at the root.
	r.Get("/healthz", server.GetHealth)

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		WriteError(w, req, apperr.NotFound("route"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		WriteError(w, req, apperr.New(apperr.CodeBadRequest, http.StatusMethodNotAllowed,
			"method %s is not allowed for this route", req.Method))
	})

	return gen.HandlerWithOptions(server, gen.ChiServerOptions{
		BaseURL:          BasePath,
		BaseRouter:       r,
		ErrorHandlerFunc: bindingErrorHandler,
	})
}

// bindingErrorHandler turns a path or query binding failure from the generated code
// into the shared validation envelope.
func bindingErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	message := err.Error()
	field := "request"
	// Generated errors read "Invalid format for parameter id: ...".
	if rest, ok := strings.CutPrefix(message, "Invalid format for parameter "); ok {
		if name, _, found := strings.Cut(rest, ":"); found {
			field = name
		}
	}
	WriteError(w, r, apperr.Validation("request parameters are invalid",
		apperr.FieldError{Field: field, Message: message}))
}

// corsMiddleware allows the dashboard origin(s) only; a wildcard is accepted for
// local development but never implied.
func corsMiddleware(origins []string) func(http.Handler) http.Handler {
	allowed := make([]string, 0, len(origins))
	for _, origin := range origins {
		origin = strings.TrimSpace(origin)
		if origin != "" {
			allowed = append(allowed, origin)
		}
	}
	if len(allowed) == 0 {
		allowed = []string{"http://localhost:5173"}
	}

	return cors.Handler(cors.Options{
		AllowedOrigins:   allowed,
		AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "Last-Event-ID", "X-Requested-With"},
		ExposedHeaders:   []string{"Content-Disposition"},
		AllowCredentials: false,
		MaxAge:           300,
	})
}

// recoverer converts a panic into a 500 with the standard envelope, logging the stack.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// ErrAbortHandler is the server's own signal; re-panic so
					// net/http can handle it as designed.
					if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(rec)
					}
					log.Error("panic recovered",
						"error", fmt.Sprint(rec),
						"path", r.URL.Path,
						"request_id", middleware.RequestIDFrom(r))
					WriteError(w, r, apperr.Internal(fmt.Errorf("panic: %v", rec)))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
