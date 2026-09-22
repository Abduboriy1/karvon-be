package middleware

import (
	"log/slog"
	"net/http"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// Logger writes one structured line per request and attaches a request-scoped logger
// carrying the request id to the context.
func Logger(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			reqID := chimw.GetReqID(r.Context())

			log := base.With("request_id", reqID)
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r.WithContext(WithLogger(r.Context(), log)))

			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(),
				"remote_ip", r.RemoteAddr,
			}
			switch {
			case ww.Status() >= 500:
				log.Error("request failed", attrs...)
			case ww.Status() >= 400:
				log.Warn("request rejected", attrs...)
			default:
				log.Info("request", attrs...)
			}
		})
	}
}
