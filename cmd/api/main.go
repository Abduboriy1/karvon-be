// Command api runs the Karvon HTTP API and, unless KARVON_WORKERS=false, the River
// job workers in the same process.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/bory/karvon-be/internal/app"
	"github.com/bory/karvon-be/internal/config"
	"github.com/bory/karvon-be/internal/logging"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	workers := flag.Bool("workers", true, "run River job workers in this process")
	envFile := flag.String("env-file", ".env", "path to a .env file (ignored when missing)")
	flag.Parse()

	cfg, err := config.Load(*envFile)
	if err != nil {
		return err
	}
	// An explicit --workers=false always wins over the environment.
	if isFlagSet("workers") {
		cfg.Workers = *workers
	}

	log := logging.New(cfg.LogLevel, cfg.Env)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	application, err := app.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer application.Close()

	return application.Run(ctx)
}

func isFlagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
