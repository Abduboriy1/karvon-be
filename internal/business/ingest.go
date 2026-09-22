package business

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
	"github.com/bory/karvon-be/internal/scraper/provider"
)

// EmailSource values match the business_emails.source check constraint.
const (
	EmailSourceMailto   = "mailto"
	EmailSourceRegex    = "regex"
	EmailSourceProvider = "provider"
)

// Ingestor turns provider listings and crawl results into deduplicated rows.
type Ingestor struct {
	store *db.Store
}

// NewIngestor builds an Ingestor.
func NewIngestor(store *db.Store) *Ingestor {
	return &Ingestor{store: store}
}

// IngestResult reports what one listing produced.
type IngestResult struct {
	BusinessID   uuid.UUID
	EmailsStored int
}

// IngestListing upserts one listing and links it to the job.
//
// Dedupe order follows the plan: a Google place id is authoritative; without one we
// fall back to the normalized domain, then to the (phone, zip) pair.
func (i *Ingestor) IngestListing(
	ctx context.Context,
	jobID uuid.UUID,
	queryID uuid.UUID,
	listing provider.Listing,
) (IngestResult, error) {
	website, domain, hasSite := NormalizeWebsite(listing.Website)
	var (
		websitePtr *string
		domainPtr  *string
	)
	if hasSite {
		websitePtr, domainPtr = &website, &domain
	}

	raw := []byte(listing.Raw)
	if len(raw) == 0 {
		raw = []byte("{}")
	}

	var result IngestResult
	err := i.store.InTx(ctx, func(q *dbgen.Queries) error {
		row, err := i.resolveBusiness(ctx, q, jobID, listing, websitePtr, domainPtr, raw)
		if err != nil {
			return err
		}
		result.BusinessID = row

		if err := q.UpsertJobResult(ctx, dbgen.UpsertJobResultParams{
			JobID:      jobID,
			BusinessID: row,
			QueryID:    uuid.NullUUID{UUID: queryID, Valid: queryID != uuid.Nil},
		}); err != nil {
			return fmt.Errorf("business: link job result: %w", err)
		}

		stored, err := saveEmails(ctx, q, row, domain, listing.Emails, EmailSourceProvider, nil)
		if err != nil {
			return err
		}
		result.EmailsStored = stored
		return nil
	})
	return result, err
}

func (i *Ingestor) resolveBusiness(
	ctx context.Context,
	q *dbgen.Queries,
	jobID uuid.UUID,
	listing provider.Listing,
	website, domain *string,
	raw []byte,
) (uuid.UUID, error) {
	if listing.PlaceID != "" {
		row, err := q.UpsertBusinessByPlaceID(ctx, dbgen.UpsertBusinessByPlaceIDParams{
			ID:         ids.New(),
			PlaceID:    &listing.PlaceID,
			Name:       listing.Name,
			Category:   nullable(listing.Category),
			Address:    nullable(listing.Address),
			City:       nullable(listing.City),
			State:      nullable(listing.State),
			Zip:        nullable(listing.Zip),
			Phone:      nullable(listing.Phone),
			Website:    website,
			Domain:     domain,
			Rating:     listing.Rating,
			Reviews:    listing.Reviews,
			Lat:        listing.Lat,
			Lng:        listing.Lng,
			Raw:        raw,
			FirstJobID: uuid.NullUUID{UUID: jobID, Valid: true},
		})
		if err != nil {
			return uuid.Nil, fmt.Errorf("business: upsert by place id: %w", err)
		}
		return row.ID, nil
	}

	if existing, ok, err := findExisting(ctx, q, domain, listing); err != nil {
		return uuid.Nil, err
	} else if ok {
		updated, err := q.UpdateBusinessFromListing(ctx, dbgen.UpdateBusinessFromListingParams{
			ID:       existing,
			Name:     listing.Name,
			Category: nullable(listing.Category),
			Address:  nullable(listing.Address),
			City:     nullable(listing.City),
			State:    nullable(listing.State),
			Zip:      nullable(listing.Zip),
			Phone:    nullable(listing.Phone),
			Website:  website,
			Domain:   domain,
			Rating:   listing.Rating,
			Reviews:  listing.Reviews,
			Lat:      listing.Lat,
			Lng:      listing.Lng,
		})
		if err != nil {
			return uuid.Nil, fmt.Errorf("business: update from listing: %w", err)
		}
		return updated.ID, nil
	}

	inserted, err := q.InsertBusiness(ctx, dbgen.InsertBusinessParams{
		ID:         ids.New(),
		PlaceID:    nil,
		Name:       listing.Name,
		Category:   nullable(listing.Category),
		Address:    nullable(listing.Address),
		City:       nullable(listing.City),
		State:      nullable(listing.State),
		Zip:        nullable(listing.Zip),
		Phone:      nullable(listing.Phone),
		Website:    website,
		Domain:     domain,
		Rating:     listing.Rating,
		Reviews:    listing.Reviews,
		Lat:        listing.Lat,
		Lng:        listing.Lng,
		Raw:        raw,
		FirstJobID: uuid.NullUUID{UUID: jobID, Valid: true},
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("business: insert: %w", err)
	}
	return inserted.ID, nil
}

// findExisting implements the no-place-id dedupe fallbacks.
func findExisting(ctx context.Context, q *dbgen.Queries, domain *string, listing provider.Listing) (uuid.UUID, bool, error) {
	if domain != nil {
		row, err := q.FindBusinessByDomain(ctx, domain)
		switch {
		case err == nil:
			return row.ID, true, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return uuid.Nil, false, fmt.Errorf("business: find by domain: %w", err)
		}
	}
	if listing.Phone != "" && listing.Zip != "" {
		row, err := q.FindBusinessByPhoneZip(ctx, dbgen.FindBusinessByPhoneZipParams{
			Phone: &listing.Phone,
			Zip:   &listing.Zip,
		})
		switch {
		case err == nil:
			return row.ID, true, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return uuid.Nil, false, fmt.Errorf("business: find by phone/zip: %w", err)
		}
	}
	return uuid.Nil, false, nil
}

// FoundEmail is one crawl hit carrying its own source and page URL.
type FoundEmail struct {
	Email   string
	Source  string
	PageURL string
}

// SaveFound stores a mixed batch of crawl hits in one transaction.
func (i *Ingestor) SaveFound(ctx context.Context, businessID uuid.UUID, siteDomain string, found []FoundEmail) (int, error) {
	var stored int
	err := i.store.InTx(ctx, func(q *dbgen.Queries) error {
		for _, f := range found {
			email, ok := AcceptEmail(f.Email)
			if !ok {
				continue
			}
			pageURL := nullable(f.PageURL)
			if _, err := q.InsertBusinessEmail(ctx, dbgen.InsertBusinessEmailParams{
				ID:         ids.New(),
				BusinessID: businessID,
				Email:      email,
				Source:     f.Source,
				PageUrl:    pageURL,
			}); err != nil {
				return fmt.Errorf("business: insert email: %w", err)
			}
			stored++
		}
		if stored == 0 {
			return nil
		}
		return repickPrimary(ctx, q, businessID, siteDomain)
	})
	return stored, err
}

func saveEmails(
	ctx context.Context,
	q *dbgen.Queries,
	businessID uuid.UUID,
	siteDomain string,
	emails []string,
	source string,
	pageURLs []string,
) (int, error) {
	stored := 0
	for idx, raw := range emails {
		email, ok := AcceptEmail(raw)
		if !ok {
			continue
		}
		var pageURL *string
		if idx < len(pageURLs) {
			pageURL = nullable(pageURLs[idx])
		}
		if _, err := q.InsertBusinessEmail(ctx, dbgen.InsertBusinessEmailParams{
			ID:         ids.New(),
			BusinessID: businessID,
			Email:      email,
			Source:     source,
			PageUrl:    pageURL,
		}); err != nil {
			return stored, fmt.Errorf("business: insert email: %w", err)
		}
		stored++
	}
	if stored == 0 {
		return 0, nil
	}
	return stored, repickPrimary(ctx, q, businessID, siteDomain)
}

// repickPrimary recomputes which address is primary for a business.
func repickPrimary(ctx context.Context, q *dbgen.Queries, businessID uuid.UUID, siteDomain string) error {
	rows, err := q.ListBusinessEmails(ctx, businessID)
	if err != nil {
		return fmt.Errorf("business: list emails: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	addresses := make([]string, len(rows))
	for i, row := range rows {
		addresses[i] = row.Email
	}
	best := PickPrimary(addresses, siteDomain)
	if best < 0 {
		return nil
	}
	if rows[best].IsPrimary {
		return nil
	}
	if err := q.ClearPrimaryEmail(ctx, businessID); err != nil {
		return fmt.Errorf("business: clear primary: %w", err)
	}
	if err := q.SetPrimaryEmail(ctx, rows[best].ID); err != nil {
		return fmt.Errorf("business: set primary: %w", err)
	}
	return nil
}

// RepickPrimary recomputes which address is primary for a business after its
// addresses changed outside the crawl path, such as when a typo correction rewrites
// one of them.
func (i *Ingestor) RepickPrimary(ctx context.Context, businessID uuid.UUID) error {
	return i.store.InTx(ctx, func(q *dbgen.Queries) error {
		biz, err := q.GetBusiness(ctx, businessID)
		if err != nil {
			return fmt.Errorf("business: load for primary pick: %w", err)
		}
		domain := ""
		if biz.Domain != nil {
			domain = *biz.Domain
		}
		return repickPrimary(ctx, q, businessID, domain)
	})
}

// CopyEmailsFrom duplicates a sibling business's addresses onto another business that
// shares its domain, so a domain is crawled once per retention window.
func (i *Ingestor) CopyEmailsFrom(ctx context.Context, sourceID, targetID uuid.UUID, siteDomain string) (int, error) {
	var copied int
	err := i.store.InTx(ctx, func(q *dbgen.Queries) error {
		rows, err := q.ListBusinessEmails(ctx, sourceID)
		if err != nil {
			return fmt.Errorf("business: list sibling emails: %w", err)
		}
		for _, row := range rows {
			if _, err := q.InsertBusinessEmail(ctx, dbgen.InsertBusinessEmailParams{
				ID:         ids.New(),
				BusinessID: targetID,
				Email:      row.Email,
				Source:     row.Source,
				PageUrl:    row.PageUrl,
			}); err != nil {
				return fmt.Errorf("business: copy email: %w", err)
			}
			copied++
		}
		if copied == 0 {
			return nil
		}
		return repickPrimary(ctx, q, targetID, siteDomain)
	})
	return copied, err
}

// nullable maps an empty string to a NULL column value.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
