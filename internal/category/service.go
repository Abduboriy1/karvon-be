// Package category manages scrape categories: named sets of search terms that
// pre-fill the Create scrape form. The seeded defaults can be edited but not deleted.
package category

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bory/karvon-be/internal/apperr"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
	"github.com/bory/karvon-be/internal/scraper"
)

// MaxNameLen bounds a category name.
const MaxNameLen = 100

// uniqueViolation is the Postgres SQLSTATE for a duplicate key.
const uniqueViolation = "23505"

// Service implements the /scrape-categories endpoints.
type Service struct {
	store *db.Store
}

// NewService builds the category service.
func NewService(store *db.Store) *Service {
	return &Service{store: store}
}

// Input carries a create or a partial update. A nil field is left unchanged on update
// and is required on create.
type Input struct {
	Name  *string
	Terms []string
}

// List returns every category, defaults first.
func (s *Service) List(ctx context.Context) ([]dbgen.ScrapeCategory, error) {
	rows, err := s.store.ListScrapeCategories(ctx)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	return rows, nil
}

// Get returns one category or a 404.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (dbgen.ScrapeCategory, error) {
	row, err := s.store.GetScrapeCategory(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.ScrapeCategory{}, apperr.NotFound("scrape category")
	}
	if err != nil {
		return dbgen.ScrapeCategory{}, apperr.Internal(err)
	}
	return row, nil
}

// Create stores a new, non-default category.
func (s *Service) Create(ctx context.Context, in Input) (dbgen.ScrapeCategory, error) {
	in = in.normalize()
	if err := in.validate(true); err != nil {
		return dbgen.ScrapeCategory{}, err
	}
	row, err := s.store.CreateScrapeCategory(ctx, dbgen.CreateScrapeCategoryParams{
		ID:    ids.New(),
		Name:  *in.Name,
		Terms: in.Terms,
	})
	if err != nil {
		return dbgen.ScrapeCategory{}, mapWriteError(err, *in.Name)
	}
	return row, nil
}

// Update renames a category or replaces its terms. Defaults are editable too.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in Input) (dbgen.ScrapeCategory, error) {
	in = in.normalize()
	if err := in.validate(false); err != nil {
		return dbgen.ScrapeCategory{}, err
	}
	row, err := s.store.UpdateScrapeCategory(ctx, dbgen.UpdateScrapeCategoryParams{
		ID:    id,
		Name:  in.Name,
		Terms: in.Terms,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.ScrapeCategory{}, apperr.NotFound("scrape category")
	}
	if err != nil {
		name := ""
		if in.Name != nil {
			name = *in.Name
		}
		return dbgen.ScrapeCategory{}, mapWriteError(err, name)
	}
	return row, nil
}

// Delete removes a user-created category. A default category is a 409.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	row, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if row.IsDefault {
		return apperr.Conflict("default categories cannot be deleted")
	}
	if _, err := s.store.DeleteScrapeCategory(ctx, id); err != nil {
		return apperr.Internal(err)
	}
	return nil
}

func (in Input) normalize() Input {
	if in.Name != nil {
		name := strings.Join(strings.Fields(*in.Name), " ")
		in.Name = &name
	}
	if in.Terms != nil {
		// An all-blank list stays non-nil so validate reports it rather than
		// treating it as "leave unchanged".
		terms := scraper.NormalizeTerms(in.Terms)
		if terms == nil {
			terms = []string{}
		}
		in.Terms = terms
	}
	return in
}

func (in Input) validate(create bool) error {
	var fields []apperr.FieldError
	switch {
	case in.Name == nil && create:
		fields = append(fields, apperr.FieldError{Field: "name", Message: "is required"})
	case in.Name != nil && (*in.Name == "" || len(*in.Name) > MaxNameLen):
		fields = append(fields, apperr.FieldError{
			Field:   "name",
			Message: fmt.Sprintf("must be between 1 and %d characters", MaxNameLen),
		})
	}
	switch {
	case in.Terms == nil && create, in.Terms != nil && len(in.Terms) == 0:
		fields = append(fields, apperr.FieldError{Field: "terms", Message: "at least one search term is required"})
	case len(in.Terms) > scraper.MaxTerms:
		fields = append(fields, apperr.FieldError{
			Field:   "terms",
			Message: fmt.Sprintf("at most %d search terms are allowed", scraper.MaxTerms),
		})
	}
	for _, term := range in.Terms {
		if len(term) > scraper.MaxTermLen {
			fields = append(fields, apperr.FieldError{
				Field:   "terms",
				Message: fmt.Sprintf("each search term must be at most %d characters", scraper.MaxTermLen),
			})
			break
		}
	}
	if len(fields) > 0 {
		return apperr.Validation("scrape category is invalid", fields...)
	}
	return nil
}

func mapWriteError(err error, name string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return apperr.Conflict("a category named %q already exists", name)
	}
	return apperr.Internal(err)
}
