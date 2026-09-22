package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/bory/karvon-be/migrations"
)

// Migrate applies the application schema (goose) and the River queue schema.
// It is safe to call concurrently from several replicas: goose takes a lock.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if err := migrateApp(ctx, pool); err != nil {
		return err
	}
	return MigrateQueue(ctx, pool)
}

func migrateApp(ctx context.Context, pool *pgxpool.Pool) error {
	goose.SetBaseFS(migrations.FS)
	goose.SetTableName("schema_migrations")
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("db: goose dialect: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("db: goose up: %w", err)
	}
	return nil
}

// MigrateQueue brings the River job tables up to date.
func MigrateQueue(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("db: river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("db: river migrate: %w", err)
	}
	return nil
}

// RollbackApp reverts the most recent application migration. Used by `make migrate-down`.
func RollbackApp(ctx context.Context, pool *pgxpool.Pool) error {
	goose.SetBaseFS(migrations.FS)
	goose.SetTableName("schema_migrations")
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("db: goose dialect: %w", err)
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	if err := goose.DownContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("db: goose down: %w", err)
	}
	return nil
}

// AppStatus prints the current migration status to the goose logger.
func AppStatus(ctx context.Context, pool *pgxpool.Pool) error {
	goose.SetBaseFS(migrations.FS)
	goose.SetTableName("schema_migrations")
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("db: goose dialect: %w", err)
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	return goose.StatusContext(ctx, sqlDB, ".")
}
