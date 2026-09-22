package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// streamingSuffixes are routes that must not be cut off by the request timeout: the
// SSE stream stays open for the life of a job and CSV exports stream large result sets.
var streamingSuffixes = []string{"/events", "/export.csv", "/export"}

// IsStreaming reports whether a path is exempt from the request timeout.
func IsStreaming(path string) bool {
	for _, suffix := range streamingSuffixes {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// Timeout bounds ordinary requests, leaving streaming routes untouched.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d <= 0 || IsStreaming(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
