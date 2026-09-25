package business

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/bory/karvon-be/internal/db/dbgen"
	"github.com/bory/karvon-be/internal/ids"
)

// FoundSocial is one social profile a crawl turned up, already in canonical form.
type FoundSocial struct {
	Network string
	Handle  string
	URL     string
	PageURL string
}

// SaveSocials stores the social profiles found for a business in one transaction and
// reports how many were new. A profile already on record is left untouched.
func (i *Ingestor) SaveSocials(ctx context.Context, businessID uuid.UUID, found []FoundSocial) (int, error) {
	if len(found) == 0 {
		return 0, nil
	}
	var stored int
	err := i.store.InTx(ctx, func(q *dbgen.Queries) error {
		for _, f := range found {
			inserted, err := q.InsertBusinessSocial(ctx, dbgen.InsertBusinessSocialParams{
				ID:         ids.New(),
				BusinessID: businessID,
				Network:    f.Network,
				Handle:     f.Handle,
				Url:        f.URL,
				PageUrl:    nullable(f.PageURL),
			})
			if err != nil {
				return fmt.Errorf("business: insert social: %w", err)
			}
			stored += int(inserted)
		}
		return nil
	})
	return stored, err
}

// CopySocialsFrom duplicates a sibling business's social profiles onto another
// business that shares its domain, alongside CopyEmailsFrom.
func (i *Ingestor) CopySocialsFrom(ctx context.Context, sourceID, targetID uuid.UUID) (int, error) {
	var copied int
	err := i.store.InTx(ctx, func(q *dbgen.Queries) error {
		rows, err := q.ListBusinessSocials(ctx, sourceID)
		if err != nil {
			return fmt.Errorf("business: list sibling socials: %w", err)
		}
		for _, row := range rows {
			inserted, err := q.InsertBusinessSocial(ctx, dbgen.InsertBusinessSocialParams{
				ID:         ids.New(),
				BusinessID: targetID,
				Network:    row.Network,
				Handle:     row.Handle,
				Url:        row.Url,
				PageUrl:    row.PageUrl,
			})
			if err != nil {
				return fmt.Errorf("business: copy social: %w", err)
			}
			copied += int(inserted)
		}
		return nil
	})
	return copied, err
}
