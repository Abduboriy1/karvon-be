package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bory/karvon-be/internal/db/dbgen"
)

// Store bundles the pool with the generated queries and the transaction helper.
type Store struct {
	pool *pgxpool.Pool
	*dbgen.Queries
}

// NewStore wraps a pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, Queries: dbgen.New(pool)}
}

// Pool exposes the underlying pool for the few callers that need raw SQL
// (dynamic search, CSV streaming, LISTEN/NOTIFY).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// InTx runs fn inside a transaction, rolling back on error or panic.
func (s *Store) InTx(ctx context.Context, fn func(q *dbgen.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	defer func() {
		// Rollback is a no-op once the transaction has been committed.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()

	if err := fn(s.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}

// InTxRaw is InTx for callers that need the pgx.Tx itself.
func (s *Store) InTxRaw(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}
