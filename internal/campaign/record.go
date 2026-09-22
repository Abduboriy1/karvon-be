package campaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/db/dbgen"
)

// EventInput is one timeline entry to record.
type EventInput struct {
	ContactID       uuid.UUID
	CampaignID      *uuid.UUID
	CampaignLeadID  *uuid.UUID
	AssignmentID    *uuid.UUID
	VariantID       *uuid.UUID
	SendID          *uuid.UUID
	Step            *int
	Type            string
	OccurredAt      time.Time
	Source          string
	ProviderEventID *uuid.UUID
	StageBefore     Stage
	StageAfter      Stage
	Data            map[string]any
}

// RecordEvent appends a timeline entry. An entry keyed by a provider event that was
// already recorded is silently skipped, which is what makes a replayed webhook safe.
func RecordEvent(ctx context.Context, q *dbgen.Queries, in EventInput) (dbgen.ContactEvent, bool, error) {
	if in.OccurredAt.IsZero() {
		in.OccurredAt = time.Now().UTC()
	}
	data := in.Data
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return dbgen.ContactEvent{}, false, fmt.Errorf("campaign: encode event data: %w", err)
	}
	params := dbgen.InsertContactEventParams{
		ContactID:       in.ContactID,
		CampaignID:      nullUUID(in.CampaignID),
		CampaignLeadID:  nullUUID(in.CampaignLeadID),
		AssignmentID:    nullUUID(in.AssignmentID),
		VariantID:       nullUUID(in.VariantID),
		SendID:          nullUUID(in.SendID),
		Type:            in.Type,
		OccurredAt:      in.OccurredAt,
		Source:          in.Source,
		ProviderEventID: nullUUID(in.ProviderEventID),
		Data:            raw,
	}
	if in.Step != nil {
		step := int32(*in.Step) //nolint:gosec // G115: steps are 1..5
		params.Step = &step
	}
	if in.StageBefore != "" {
		before := string(in.StageBefore)
		params.StageBefore = &before
	}
	if in.StageAfter != "" {
		after := string(in.StageAfter)
		params.StageAfter = &after
	}
	row, err := q.InsertContactEvent(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.ContactEvent{}, false, nil
	}
	if err != nil {
		return dbgen.ContactEvent{}, false, fmt.Errorf("campaign: record event: %w", err)
	}
	if err := q.TouchContactEvent(ctx, dbgen.TouchContactEventParams{ID: in.ContactID, At: &in.OccurredAt}); err != nil {
		return dbgen.ContactEvent{}, false, fmt.Errorf("campaign: touch contact: %w", err)
	}
	return row, true, nil
}

// Advance moves a contact to next when the funnel rules allow it, returning the
// stage it is now in and whether it changed. The database trigger is the last
// line of defence; this is the first.
func Advance(ctx context.Context, q *dbgen.Queries, contact dbgen.Contact, next Stage) (dbgen.Contact, bool, error) {
	cur := Stage(contact.LifecycleStage)
	target, changed := AdvanceStage(cur, next)
	if !changed {
		return contact, false, nil
	}
	if target.Newsletter() {
		if _, err := q.GetActiveConsent(ctx, contact.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Interest is not consent. Without a consent record the contact
				// stays where it is; the trigger would refuse anyway.
				return contact, false, nil
			}
			return contact, false, fmt.Errorf("campaign: check consent: %w", err)
		}
	}
	updated, err := q.SetContactStage(ctx, dbgen.SetContactStageParams{ID: contact.ID, Stage: string(target)})
	if err != nil {
		return contact, false, fmt.Errorf("campaign: advance %s → %s: %w", cur, target, err)
	}
	return updated, true, nil
}

func nullUUID(id *uuid.UUID) uuid.NullUUID {
	if id == nil {
		return uuid.NullUUID{}
	}
	return uuid.NullUUID{UUID: *id, Valid: true}
}

// NullUUID wraps an optional id for sqlc parameters.
func NullUUID(id *uuid.UUID) uuid.NullUUID { return nullUUID(id) }

// UUIDPtr unwraps a sqlc NullUUID.
func UUIDPtr(id uuid.NullUUID) *uuid.UUID {
	if !id.Valid {
		return nil
	}
	v := id.UUID
	return &v
}

// Ptr returns a pointer to v.
func Ptr[T any](v T) *T { return &v }

// Optional returns nil for an empty string.
func Optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Deref returns the value behind p or the zero value.
func Deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// Int32 narrows an int to what a Postgres integer column accepts.
func Int32(v int) int32 {
	switch {
	case v < 0:
		return 0
	case v > 1<<31-1:
		return 1<<31 - 1
	default:
		return int32(v)
	}
}
