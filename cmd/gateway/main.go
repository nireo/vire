package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nireo/vire/gateway"
	"github.com/nireo/vire/internal/accountdb"
)

type config struct {
	addr          string
	registry      string
	metricsAddr   string
	databaseURL   string
	insecureDev   bool
	backendAPIKey string
	maxInflight   int
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("gateway failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg := parseConfig()
	if err := cfg.validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var accounts gateway.AccountStore
	if cfg.databaseURL != "" {
		store, err := openAccountStore(ctx, cfg.databaseURL)
		if err != nil {
			return fmt.Errorf("account database initialization failed: %w", err)
		}
		defer store.Close()
		accounts = store
		go maintainAccounts(ctx, store, logger)
	} else {
		logger.Warn("unauthenticated development mode enabled")
	}

	server, err := gateway.NewServerWithOptions(cfg.addr, cfg.registry, gateway.Options{
		MetricsAddr:   cfg.metricsAddr,
		Logger:        logger,
		AccountStore:  accounts,
		BackendAPIKey: cfg.backendAPIKey,
		MaxInflight:   cfg.maxInflight,
	})
	if err != nil {
		return fmt.Errorf("gateway initialization failed: %w", err)
	}
	if err := server.Run(ctx); err != nil {
		return fmt.Errorf("gateway stopped with error: %w", err)
	}
	return nil
}

func parseConfig() config {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	registry := flag.String("registry", "models.json", "path to the model registry")
	metricsAddr := flag.String("metrics-addr", "127.0.0.1:9091", "private metrics listen address (empty disables listener)")
	databaseURL := flag.String("database-url", os.Getenv("VIRE_DATABASE_URL"), "Postgres URL for accounts and usage (or VIRE_DATABASE_URL)")
	insecureDev := flag.Bool("insecure-dev", false, "allow unauthenticated requests for local development only")
	backendAPIKey := flag.String("backend-api-key", os.Getenv("VIRE_BACKEND_API_KEY"), "separate credential sent to inference backends")
	maxInflight := flag.Int("max-inflight", 32, "maximum simultaneous metered inference requests per gateway")
	flag.Parse()
	return config{
		addr:          *addr,
		registry:      *registry,
		metricsAddr:   *metricsAddr,
		databaseURL:   *databaseURL,
		insecureDev:   *insecureDev,
		backendAPIKey: *backendAPIKey,
		maxInflight:   *maxInflight,
	}
}

func (cfg config) validate() error {
	if (cfg.databaseURL == "" && !cfg.insecureDev) || (cfg.databaseURL != "" && cfg.insecureDev) {
		return errors.New("configure exactly one of -database-url or -insecure-dev")
	}
	if cfg.maxInflight <= 0 {
		return errors.New("-max-inflight must be positive")
	}
	return nil
}

func openAccountStore(ctx context.Context, databaseURL string) (*accountdb.Store, error) {
	setupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	store, err := accountdb.Open(setupCtx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := store.Migrate(setupCtx); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func maintainAccounts(ctx context.Context, store *accountdb.Store, logger *slog.Logger) {
	for {
		maintenanceCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		if err := store.Maintain(maintenanceCtx); err != nil && ctx.Err() == nil {
			logger.Error("usage maintenance failed", "error", err)
		}
		cancel()
		timer := time.NewTimer(time.Hour)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
