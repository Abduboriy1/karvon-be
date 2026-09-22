package httpapi

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/go-playground/validator/v10"

	"github.com/bory/karvon-be/internal/apperr"
)

var (
	validateOnce sync.Once
	validate     *validator.Validate
)

// validator returns the shared validator, configured to report JSON field names so
// clients see the same names they sent.
func requestValidator() *validator.Validate {
	validateOnce.Do(func() {
		validate = validator.New(validator.WithRequiredStructEnabled())
		validate.RegisterTagNameFunc(func(field reflect.StructField) string {
			name := strings.SplitN(field.Tag.Get("json"), ",", 2)[0]
			if name == "-" || name == "" {
				return field.Name
			}
			return name
		})
	})
	return validate
}

// validateStruct runs struct tag validation and converts failures into the shared
// validation error, one FieldError per rule that failed.
func validateStruct(v any) error {
	err := requestValidator().Struct(v)
	if err == nil {
		return nil
	}

	var invalid *validator.InvalidValidationError
	if errors.As(err, &invalid) {
		return apperr.Internal(err)
	}

	var validationErrs validator.ValidationErrors
	if !errors.As(err, &validationErrs) {
		return apperr.Internal(err)
	}

	fields := make([]apperr.FieldError, 0, len(validationErrs))
	for _, fieldErr := range validationErrs {
		fields = append(fields, apperr.FieldError{
			Field:   jsonPath(fieldErr.Namespace()),
			Message: ruleMessage(fieldErr),
		})
	}
	return apperr.Validation("request body is invalid", fields...)
}

// jsonPath turns "CreateJobRequest.config.terms[0]" into "config.terms[0]".
func jsonPath(namespace string) string {
	if idx := strings.Index(namespace, "."); idx >= 0 {
		return namespace[idx+1:]
	}
	return namespace
}

func ruleMessage(fieldErr validator.FieldError) string {
	switch fieldErr.Tag() {
	case "required":
		return "is required"
	case "min":
		return fmt.Sprintf("must have at least %s", fieldErr.Param())
	case "max":
		return fmt.Sprintf("must have at most %s", fieldErr.Param())
	case "gte":
		return fmt.Sprintf("must be at least %s", fieldErr.Param())
	case "lte":
		return fmt.Sprintf("must be at most %s", fieldErr.Param())
	case "oneof":
		return fmt.Sprintf("must be one of: %s", strings.ReplaceAll(fieldErr.Param(), " ", ", "))
	case "uuid", "uuid4", "uuid7":
		return "must be a valid UUID"
	case "email":
		return "must be a valid email address"
	default:
		return fmt.Sprintf("failed the %q rule", fieldErr.Tag())
	}
}
