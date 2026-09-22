// Package db owns the Postgres connection pool, transaction helpers and the
// hand-written dynamic queries that sit next to the sqlc-generated code in dbgen.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig carries the subset of settings the pool needs.
type PoolConfig struct {
	URL      string
	MaxConns int32
}

// NewPool opens a pgx pool and verifies connectivity before returning.
func NewPool(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("db: parse database url: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	// citext is an extension type, so its OID is not known at compile time. Load it
	// once per connection and decode it as text.
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return registerCitext(ctx, conn)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

func registerCitext(ctx context.Context, conn *pgx.Conn) error {
	if _, ok := conn.TypeMap().TypeForName("citext"); ok {
		return nil
	}
	var oid uint32
	err := conn.QueryRow(ctx, "SELECT oid FROM pg_type WHERE typname = 'citext'").Scan(&oid)
	if err != nil {
		// The extension may not exist yet (first boot, before migrations run).
		return nil //nolint:nilerr // absence of citext is not a connection failure
	}
	conn.TypeMap().RegisterType(&pgtype.Type{Name: "citext", OID: oid, Codec: pgtype.TextCodec{}})
	conn.TypeMap().RegisterType(&pgtype.Type{Name: "_citext", OID: 0, Codec: pgtype.TextCodec{}})
	return nil
}
