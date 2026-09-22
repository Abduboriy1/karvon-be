// Package httpapi is the HTTP transport: the chi router, the middleware wiring and the
// handlers that translate requests into service calls. All business logic lives in the
// service packages; handlers only deal with HTTP concerns.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/http/gen"
	"github.com/bory/karvon-be/internal/http/middleware"
)

// maxRequestBody caps how much JSON a client may send.
const maxRequestBody = 1 << 20

// writeJSON sends a JSON response, logging an encode failure rather than panicking.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		middleware.LoggerFrom(r.Context()).Error("could not write response body", "error", err)
	}
}

// writeNoContent sends 204.
func writeNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// newErrorEnvelope builds the `{ "error": { "code", "message" } }` body every failing
// endpoint returns.
func newErrorEnvelope(appErr *apperr.Error) gen.Error {
	var envelope gen.Error
	envelope.Error.Code = string(appErr.Code)
	envelope.Error.Message = appErr.Message
	if len(appErr.Fields) > 0 {
		details := make([]gen.FieldError, 0, len(appErr.Fields))
		for _, f := range appErr.Fields {
			details = append(details, gen.FieldError{Field: f.Field, Message: f.Message})
		}
		envelope.Error.Details = &details
	}
	return envelope
}

// WriteError maps any error to the shared envelope. Internal causes are logged and
// never leak to the client.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	appErr := apperr.From(err)
	if appErr == nil {
		return
	}

	log := middleware.LoggerFrom(r.Context())
	switch {
	case appErr.Status >= 500:
		log.Error("request failed", "code", appErr.Code, "error", appErr.Error())
	case appErr.Cause != nil:
		log.Warn("request rejected", "code", appErr.Code, "error", appErr.Error())
	}

	writeJSON(w, r, appErr.Status, newErrorEnvelope(appErr))
}

// writeUnauthorized is handed to the auth middleware so 401s use the same envelope.
func writeUnauthorized(w http.ResponseWriter, r *http.Request, message string) {
	WriteError(w, r, apperr.Unauthorized("%s", message))
}

// decodeJSON reads a JSON body into dst with a size cap and strict field checking.
func decodeJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return apperr.BadRequest("a JSON request body is required")
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		return badRequestFromDecodeError(err)
	}
	// Reject trailing content so "{}{}" is not silently accepted.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return apperr.BadRequest("request body must contain exactly one JSON object")
	}
	return nil
}

func badRequestFromDecodeError(err error) error {
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.Is(err, io.EOF):
		return apperr.BadRequest("a JSON request body is required")
	case errors.As(err, &typeErr):
		return apperr.Validation("request body is invalid", apperr.FieldError{
			Field:   typeErr.Field,
			Message: fmt.Sprintf("must be of type %s", typeErr.Type.String()),
		})
	case strings.Contains(err.Error(), "unknown field"):
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return apperr.Validation("request body is invalid", apperr.FieldError{
			Field:   field,
			Message: "is not a recognised field",
		})
	default:
		return apperr.BadRequest("request body is not valid JSON")
	}
}
