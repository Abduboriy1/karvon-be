// Command migrate applies, rolls back or reports the database schema. It is the
// production migration step; the API can also migrate on boot in development.
//
// Usage: migrate [up|down|status|queue]
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/bory/karvon-be/internal/config"
	"github.com/bory/karvon-be/internal/db"
	"github.com/bory/karvon-be/internal/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	cfg, err := config.Load(".env")
	if err != nil {
		return err
	}
	log := logging.New(cfg.LogLevel, cfg.Env)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := db.NewPool(ctx, db.PoolConfig{URL: cfg.DatabaseURL, MaxConns: 2})
	if err != nil {
		return err
	}
	defer pool.Close()

	switch command {
	case "up":
		if err := db.Migrate(ctx, pool); err != nil {
			return err
		}
		log.Info("migrations applied")
	case "down":
		if err := db.RollbackApp(ctx, pool); err != nil {
			return err
		}
		log.Info("last migration rolled back")
	case "status":
		return db.AppStatus(ctx, pool)
	case "queue":
		if err := db.MigrateQueue(ctx, pool); err != nil {
			return err
		}
		log.Info("queue schema applied")
	default:
		return fmt.Errorf("unknown command %q: expected up, down, status or queue", command)
	}
	return nil
}
