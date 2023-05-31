package main

import (
	"context"
	"expvar"
	"fmt"

	"github.com/kelseyhightower/envconfig"
	_ "github.com/lib/pq"
	"github.com/mvo-usc/local-golang-sandbox/internal/app/http"
	"github.com/mvo-usc/local-golang-sandbox/internal/app/storage"
	"github.com/mvo-usc/local-golang-sandbox/internal/app/storage/postgres"
	"github.com/mvo-usc/local-golang-sandbox/internal/app/todo"
	"github.com/urbansportsclub/gokit/log"
	"github.com/urbansportsclub/gokit/otel"
	"github.com/urbansportsclub/gokit/signal"
)

var version = "develop"

type config struct {
	LogLevel      string `split_words:"true" default:"info"`
	Database      storage.Config
	Observability otel.Config
	Web           http.Config
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := log.WithContext(ctx)
	defer func() {
		// ignore https://github.com/uber-go/zap/issues/328
		_ = logger.Sync()
	}()

	if err := run(ctx); err != nil {
		logger.Fatal(err)
	}
}

func run(ctx context.Context) error {
	// =========================================================================
	// Logging
	// =========================================================================
	var (
		logger = log.WithContext(ctx).Named("main")
		slog   = logger.Named("starting")
	)

	// =========================================================================
	// Configuration
	// =========================================================================
	var cfg config
	if err := envconfig.Process("", &cfg); err != nil {
		return fmt.Errorf("failed to load the env vars: %w", err)
	}

	log.SetLevel(cfg.LogLevel)

	// =========================================================================
	// App Starting
	// =========================================================================
	expvar.NewString("build").Set(version)
	slog.Infow("Application initializing", "version", version)

	defer logger.Info("completed")

	// =========================================================================
	// Start observability provider
	// =========================================================================
	shutdown, err := otel.InitProvider(ctx, cfg.Observability)
	if err != nil {
		return fmt.Errorf("failed to initialize the provider: %w", err)
	}

	defer shutdown()

	// =========================================================================
	// Setup Databases
	// =========================================================================
	logger.Debug("connecting to the database")

	db, err := storage.SetupDatabase(cfg.Database)
	if err != nil {
		return fmt.Errorf("could not connect to the database: %w", err)
	}

	defer func() {
		if err := db.Close(); err != nil {
			logger.Errorw("failed to close connection to the database", "error", err)
		}
	}()

	// =========================================================================
	// Start API Service

	// Make a channel to listen for errors coming from the listener. Use a
	// buffered channel so the goroutine can exit if we don't collect this error.
	// =========================================================================
	serverErrors := make(chan error, 1)

	var (
		tr  = postgres.NewTaskReadRepository(db)
		svc = todo.NewService(tr, c)

		server = http.SetupServer(ctx, cfg.Web, http.Router(ctx, svc))
	)

	go func() {
		slog.Infow("The service is ready to listen and serve", "host", cfg.Web.APIHost)
		serverErrors <- server.ListenAndServe()
	}()

	// =========================================================================
	// Start Probe API Service
	// =========================================================================
	probeServer := http.SetupProbeServer(ctx, cfg.Web)

	go func() {
		slog.Infow("Initializing probe API support", "host", cfg.Web.ProbeHost)
		serverErrors <- probeServer.ListenAndServe()
	}()

	done := signal.New(ctx)

	// Blocking main and waiting for shutdown.
	select {
	case err := <-serverErrors:
		return fmt.Errorf("server error: %w", err)
	case <-done.Done():
		logger.Infow("start shutdown")

		// Give outstanding requests a deadline for completion.
		ctx, cancel := context.WithTimeout(ctx, cfg.Web.ShutdownTimeout)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			logger.Errorw("failed to gracefully shutdown the server", "err", err)

			if err = server.Close(); err != nil {
				return fmt.Errorf("could not stop server gracefully: %w", err)
			}
		}

		if err := probeServer.Shutdown(ctx); err != nil {
			logger.Errorw("failed to gracefully shutdown the probe server", "err", err)

			if err = probeServer.Close(); err != nil {
				return fmt.Errorf("could not stop probe server gracefully: %w", err)
			}
		}
	}

	return nil
}
