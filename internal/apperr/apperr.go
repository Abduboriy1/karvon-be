// Package apperr defines the error type that travels from the service layer to the
// HTTP layer. Every API error the client sees is produced from one of these, so the
// error envelope stays consistent across the whole surface.
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// Code is a stable, machine-readable error identifier. The frontend switches on these.
type Code string

// The codes the API can return. They are part of the contract with the frontend.
const (
	CodeBadRequest       Code = "bad_request"
	CodeValidationFailed Code = "validation_failed"
	CodeUnauthorized     Code = "unauthorized"
	CodeNotFound         Code = "not_found"
	CodeConflict         Code = "conflict"
	CodeProviderAuth     Code = "provider_auth"
	CodeProviderError    Code = "provider_error"
	CodeAIPlanLimit      Code = "ai_plan_limit"
	CodeInternal         Code = "internal"
)

// FieldError attaches a validation message to a specific request field.
type FieldError struct {
	Field   string
	Message string
}

// Error is an application error carrying an HTTP status and a client-safe message.
type Error struct {
	Code    Code
	Message string
	Status  int
	Fields  []FieldError
	// Cause is logged but never sent to the client.
	Cause error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// From extracts an *Error from err, or wraps it as an internal error.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr
	}
	return Internal(err)
}

// New builds an error with an explicit code and status.
func New(code Code, status int, format string, args ...any) *Error {
	return &Error{Code: code, Status: status, Message: fmt.Sprintf(format, args...)}
}

// WithCause attaches an underlying error for logging.
func (e *Error) WithCause(err error) *Error {
	e.Cause = err
	return e
}

// BadRequest reports a request the server could not parse.
func BadRequest(format string, args ...any) *Error {
	return New(CodeBadRequest, http.StatusBadRequest, format, args...)
}

// Unauthorized reports a missing or invalid API key.
func Unauthorized(format string, args ...any) *Error {
	return New(CodeUnauthorized, http.StatusUnauthorized, format, args...)
}

// NotFound describes a missing resource, e.g. NotFound("job").
func NotFound(resource string) *Error {
	return New(CodeNotFound, http.StatusNotFound, "%s not found", resource)
}

// Conflict reports an action that the resource's current state does not allow.
func Conflict(format string, args ...any) *Error {
	return New(CodeConflict, http.StatusConflict, format, args...)
}

// Validation reports a request that parsed but failed the rules.
func Validation(message string, fields ...FieldError) *Error {
	return &Error{
		Code:    CodeValidationFailed,
		Status:  http.StatusUnprocessableEntity,
		Message: message,
		Fields:  fields,
	}
}

// ProviderAuth marks a provider rejecting the stored API key.
func ProviderAuth(format string, args ...any) *Error {
	return New(CodeProviderAuth, http.StatusBadGateway, format, args...)
}

// ProviderError marks a provider failing for any other reason.
func ProviderError(format string, args ...any) *Error {
	return New(CodeProviderError, http.StatusBadGateway, format, args...)
}

// Internal hides an unexpected failure behind a generic message.
func Internal(cause error) *Error {
	return &Error{
		Code:    CodeInternal,
		Status:  http.StatusInternalServerError,
		Message: "internal server error",
		Cause:   cause,
	}
}
