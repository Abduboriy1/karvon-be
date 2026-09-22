// Package middleware holds the HTTP middleware stack: request ids, structured access
// logs, panic recovery, CORS, API-key authentication and request timeouts.
package middleware

import (
	"context"
	"log/slog"
	"net/http"

	chimw "github.com/go-chi/chi/v5/middleware"
)

type ctxKey int

const loggerKey ctxKey = iota

// WithLogger stores a request-scoped logger on the context.
func WithLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, log)
}

// LoggerFrom returns the request-scoped logger, or the default logger.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if log, ok := ctx.Value(loggerKey).(*slog.Logger); ok && log != nil {
		return log
	}
	return slog.Default()
}

// RequestIDFrom returns the chi request id, if any.
func RequestIDFrom(r *http.Request) string {
	return chimw.GetReqID(r.Context())
}
