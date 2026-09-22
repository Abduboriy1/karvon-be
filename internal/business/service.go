package business

import (
	"context"
	"errors"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
)

// MaxBulkIDs bounds a single bulk action.
const MaxBulkIDs = 5000

// MaxNotesLength bounds the free-text note on a business.
const MaxNotesLength = 2000

// Service implements the /businesses endpoints.
type Service struct {
	store *db.Store
}

// NewService builds the business service.
func NewService(store *db.Store) *Service {
	return &Service{store: store}
}

// ListResult is one page of businesses.
type ListResult struct {
	Rows  []db.BusinessRow
	Total int64
}

// List returns a filtered, sorted page.
func (s *Service) List(ctx context.Context, filter db.BusinessFilter, sort string, page, perPage int) (ListResult, error) {
	total, err := s.store.CountBusinesses(ctx, filter)
	if err != nil {
		return ListResult{}, apperr.Internal(err)
	}
	rows, err := s.store.ListBusinesses(ctx, filter, sort, perPage, (page-1)*perPage)
	if err != nil {
		return ListResult{}, apperr.Internal(err)
	}
	return ListResult{Rows: rows, Total: total}, nil
}

// Detail is a business with every address it has.
type Detail struct {
	Business dbgen.GetBusinessRow
	Emails   []dbgen.ListBusinessEmailsWithVerificationRow
}

// Get returns one business with its addresses.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Detail, error) {
	row, err := s.store.GetBusiness(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, apperr.NotFound("business")
	}
	if err != nil {
		return Detail{}, apperr.Internal(err)
	}
	emails, err := s.store.ListBusinessEmailsWithVerification(ctx, id)
	if err != nil {
		return Detail{}, apperr.Internal(err)
	}
	return Detail{Business: row, Emails: emails}, nil
}

// UpdateInput carries the patchable fields. SetNotes distinguishes "clear the note"
// from "leave it alone".
type UpdateInput struct {
	Suppressed *bool
	SetNotes   bool
	Notes      *string
}

// Update patches suppression and notes.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (Detail, error) {
	if in.Notes != nil && len(*in.Notes) > MaxNotesLength {
		return Detail{}, apperr.Validation("business update is invalid",
			apperr.FieldError{Field: "notes", Message: "must be at most 2000 characters"})
	}
	if in.Suppressed == nil && !in.SetNotes {
		return Detail{}, apperr.Validation("business update is invalid",
			apperr.FieldError{Field: "body", Message: "at least one of suppressed or notes is required"})
	}

	_, err := s.store.UpdateBusinessFlags(ctx, dbgen.UpdateBusinessFlagsParams{
		ID:         id,
		Suppressed: in.Suppressed,
		SetNotes:   in.SetNotes,
		Notes:      in.Notes,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Detail{}, apperr.NotFound("business")
	}
	if err != nil {
		return Detail{}, apperr.Internal(err)
	}
	return s.Get(ctx, id)
}

// BulkAction is the action of POST /businesses/bulk.
type BulkAction string

const (
	// BulkSuppress hides businesses from the default list.
	BulkSuppress BulkAction = "suppress"
	// BulkUnsuppress restores them.
	BulkUnsuppress BulkAction = "unsuppress"
)

// Bulk suppresses or unsuppresses many businesses and reports how many changed.
func (s *Service) Bulk(ctx context.Context, ids []uuid.UUID, action BulkAction) (int64, error) {
	var fields []apperr.FieldError
	switch {
	case len(ids) == 0:
		fields = append(fields, apperr.FieldError{Field: "ids", Message: "at least one id is required"})
	case len(ids) > MaxBulkIDs:
		fields = append(fields, apperr.FieldError{Field: "ids", Message: "at most 5000 ids are allowed"})
	}
	if action != BulkSuppress && action != BulkUnsuppress {
		fields = append(fields, apperr.FieldError{Field: "action", Message: `must be "suppress" or "unsuppress"`})
	}
	if len(fields) > 0 {
		return 0, apperr.Validation("bulk action is invalid", fields...)
	}

	updated, err := s.store.BulkSetSuppressed(ctx, dbgen.BulkSetSuppressedParams{
		Suppressed: action == BulkSuppress,
		Ids:        ids,
	})
	if err != nil {
		return 0, apperr.Internal(err)
	}
	return updated, nil
}

// Export streams a filtered set as CSV. Rows are written as they arrive from the
// database, so memory stays flat no matter how large the export is.
func (s *Service) Export(ctx context.Context, filter db.BusinessFilter, dst io.Writer, flush func()) (int, error) {
	writer, err := NewCSVWriter(dst, flush)
	if err != nil {
		return 0, apperr.Internal(err)
	}

	err = s.store.StreamBusinessesForExport(ctx, filter, func(row db.BusinessRow) error {
		return writer.Write(toCSVRow(row))
	})
	if err != nil {
		return writer.Rows(), err
	}
	if err := writer.Close(); err != nil {
		return writer.Rows(), err
	}
	return writer.Rows(), nil
}

func toCSVRow(row db.BusinessRow) CSVRow {
	return CSVRow{
		ID:           row.ID.String(),
		Name:         row.Name,
		Category:     deref(row.Category),
		Address:      deref(row.Address),
		City:         deref(row.City),
		State:        deref(row.State),
		Zip:          deref(row.Zip),
		Phone:        deref(row.Phone),
		Website:      deref(row.Website),
		Domain:       deref(row.Domain),
		PrimaryEmail: deref(row.PrimaryEmail),
		EmailSource:  deref(row.PrimaryEmailSource),
		AllEmails:    deref(row.AllEmails),
		EmailsCount:  row.EmailsCount,
		Rating:       row.Rating,
		Reviews:      row.Reviews,
		Suppressed:   row.Suppressed,
		FirstSeenAt:  row.CreatedAt,
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
