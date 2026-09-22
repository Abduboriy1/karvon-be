package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/bory/karvon-be/internal/db"
)

// Publisher appends events and notifies listeners in a single transaction, so a
// subscriber is never woken for a row it cannot yet read.
type Publisher struct {
	store *db.Store
}

// NewPublisher builds a Publisher over a store.
func NewPublisher(store *db.Store) *Publisher {
	return &Publisher{store: store}
}

// Publish stores one event and returns its id.
func (p *Publisher) Publish(ctx context.Context, jobID uuid.UUID, eventType Type, payload any) (int64, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("events: marshal payload: %w", err)
	}

	var id int64
	err = p.store.InTxRaw(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO job_events (job_id, type, data) VALUES ($1, $2, $3) RETURNING id`,
			jobID, string(eventType), data).Scan(&id); err != nil {
			return fmt.Errorf("events: insert: %w", err)
		}
		// Delivered to listeners only when this transaction commits.
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, Channel, jobID.String()); err != nil {
			return fmt.Errorf("events: notify: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// Progress publishes a progress event.
func (p *Publisher) Progress(ctx context.Context, jobID uuid.UUID, progress Progress) error {
	_, err := p.Publish(ctx, jobID, TypeProgress, progress)
	return err
}

// Log publishes a log line.
func (p *Publisher) Log(ctx context.Context, jobID uuid.UUID, level Level, format string, args ...any) error {
	_, err := p.Publish(ctx, jobID, TypeLog, Log{
		Level: level,
		Msg:   fmt.Sprintf(format, args...),
		TS:    time.Now().UTC(),
	})
	return err
}

// Status publishes a status change.
func (p *Publisher) Status(ctx context.Context, jobID uuid.UUID, status Status) error {
	_, err := p.Publish(ctx, jobID, TypeStatus, status)
	return err
}
