package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/soroworks/sorovault/internal/config"
	"github.com/soroworks/sorovault/internal/registry"
	"github.com/soroworks/sorovault/internal/spec"
	"github.com/soroworks/sorovault/internal/stellar"
	"github.com/soroworks/sorovault/internal/store"
)

// app bundles everything a command needs, so each command body can stay
// about what it does rather than how it was wired.
type app struct {
	cfg      *config.Config
	log      *slog.Logger
	registry *registry.Registry
	fetcher  stellar.Fetcher

	closers []func() error
}

// newApp loads configuration and connects the registry to Postgres and RPC.
// The configuration source is whatever --config named (or CONFIG_PATH),
// layered under the environment.
func newApp(ctx context.Context) (*app, error) {
	cfg, err := config.LoadWith(configPath)
	if err != nil {
		return nil, err
	}

	log := cfg.NewLogger(os.Stderr)
	slog.SetDefault(log)

	db, err := store.Open(ctx, cfg.DatabaseURL, store.PoolOptions{
		MaxConns: cfg.DatabaseMaxConns,
		MinConns: cfg.DatabaseMinConns,
	})
	if err != nil {
		return nil, err
	}

	// The pool in force is not always the one that was configured: pgx has
	// defaults of its own, and the connection string can carry a size too.
	// Logging the resolved numbers says which the process actually got.
	if pg, ok := db.(*store.Postgres); ok {
		pool := pg.PoolConfig()
		log.Info("database pool ready", "max_conns", pool.MaxConns, "min_conns", pool.MinConns)
	} else {
		log.Info("sqlite store ready")
	}

	fetcher := stellar.NewRPCFetcher(cfg.RPCURL, nil)

	reg := registry.New(fetcher, spec.XDRDecoder{}, db, registry.Options{
		ExpectPassphrase: cfg.NetworkPassphrase,
		Logger:           log,
		MaxConcurrent:    cfg.MaxConcurrentRegistrations,
	})

	return &app{
		cfg:      cfg,
		log:      log,
		registry: reg,
		fetcher:  fetcher,
		closers:  []func() error{fetcher.Close, db.Close},
	}, nil
}

// Close releases the app's resources in reverse order of acquisition.
func (a *app) Close() error {
	var firstErr error
	for i := len(a.closers) - 1; i >= 0; i-- {
		if err := a.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// withApp runs fn against a fully wired app, tearing it down afterwards.
func withApp(ctx context.Context, fn func(context.Context, *app) error) error {
	a, err := newApp(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := a.Close(); err != nil {
			a.log.Warn("shutting down", "error", err)
		}
	}()

	return fn(ctx, a)
}

// fetchContext bounds a single contract fetch by the configured timeout.
func (a *app) fetchContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, a.cfg.RPCTimeout)
}

// withStore runs fn against the database alone. Commands that only move
// registry data between stores use it so they cannot accidentally depend on
// RPC: export and import must work during an outage.
func withStore(ctx context.Context, fn func(context.Context, store.Store) error) error {
	cfg, err := config.LoadWith(configPath)
	if err != nil {
		return err
	}

	db, err := store.Open(ctx, cfg.DatabaseURL, store.PoolOptions{
		MaxConns: cfg.DatabaseMaxConns,
		MinConns: cfg.DatabaseMinConns,
	})
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	return fn(ctx, db)
}

// requireDatabaseURL produces the same guidance as it always has for
// commands that need only the database and skip the rest of the wiring —
// the wording callers pin on — while honoring the config file the way
// every command now does.
func requireDatabaseURL() (string, error) {
	url, err := config.DatabaseURL(configPath)
	if err != nil {
		return "", err
	}
	if url == "" {
		return "", fmt.Errorf("DATABASE_URL or DATABASE_URL_FILE is required")
	}
	return url, nil
}
