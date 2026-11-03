// importer-api serves the items API and announces new items on RabbitMQ.
// `importer-api migrate ...` runs database migrations instead.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/exaring/otelpgx"

	"github.com/vandelay-labs/importer/internal/api"
	"github.com/vandelay-labs/importer/internal/database"
	"github.com/vandelay-labs/importer/internal/items"
	"github.com/vandelay-labs/importer/internal/outbox"
	"github.com/vandelay-labs/importer/internal/telemetry"
)

// Set at build time: go build -ldflags "-X main.version=1.0.0"
var version = "dev"

func main() {
	slog.SetDefault(telemetry.NewLogger(os.Stdout))

	// `importer-api migrate ...` runs migrations instead of the server.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		os.Exit(database.MigrateMain(os.Args[2:]))
	}

	cfg := api.LoadConfig()
	cfg.Version = version
	svc := telemetry.Service{Name: "importer-api", Version: version}

	shutdownTracing, err := telemetry.SetupTracing(context.Background(), svc)
	if err != nil {
		slog.Error("tracing setup failed", "err", err)
		os.Exit(1)
	}

	// Before the pool and router: they read the global MeterProvider when created.
	metricsHandler, shutdownMetrics, err := telemetry.SetupMetrics(context.Background(), svc)
	if err != nil {
		slog.Error("metrics setup failed", "err", err)
		os.Exit(1)
	}

	// Items live in Postgres, so the API can't run without it. The pool
	// connects lazily: a database that's down shows up in /readyz, not here.
	if cfg.DatabaseURL == "" {
		slog.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	db, err := database.NewPool(context.Background(), cfg.DatabaseURL)
	if err != nil {
		slog.Error("invalid DATABASE_URL", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := otelpgx.RecordStats(db); err != nil {
		slog.Error("pool metrics setup failed", "err", err)
		os.Exit(1)
	}

	// New items are announced on RabbitMQ (see internal/outbox), so it's
	// required too. Like the pool, the relay connects in the background and retries.
	if cfg.RabbitMQURL == "" {
		slog.Error("RABBITMQ_URL is required")
		os.Exit(1)
	}
	relay, err := outbox.NewRelay(db, cfg.RabbitMQURL)
	if err != nil {
		slog.Error("outbox relay setup failed", "err", err)
		os.Exit(1)
	}
	store := items.NewStore(db)
	store.OutboxWritten = relay.Notify
	relayCtx, stopRelay := context.WithCancel(context.Background())
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		relay.Run(relayCtx)
	}()

	checks, closeChecks, err := api.NewCheckers(cfg, db)
	if err != nil {
		slog.Error("invalid dependency config", "err", err)
		os.Exit(1)
	}
	defer closeChecks()

	// ready flips to false on shutdown so the readiness probe fails and
	// Kubernetes stops routing new traffic to this pod before it exits.
	var ready atomic.Bool

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           api.TraceRequests(api.LogRequests(api.NewRouter(cfg, store, &ready, checks))),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Metrics get their own port, so they aren't exposed through the Service
	// (NodePort) and scrapes don't show up as API traffic.
	metricsSrv := &http.Server{
		Addr:              ":" + cfg.MetricsPort,
		Handler:           metricsHandler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server failed", "err", err)
			os.Exit(1)
		}
	}()

	go func() {
		slog.Info("server starting", "port", cfg.Port, "metricsPort", cfg.MetricsPort, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()
	ready.Store(true)

	// Kubernetes sends SIGTERM when it deletes a pod (rollout, scale down, etc).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	slog.Info("shutdown signal received, draining connections")
	ready.Store(false)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
	// After the server: requests that finished draining may have written
	// events. Anything left in the outbox is published by another pod.
	stopRelay()
	select {
	case <-relayDone:
	case <-shutdownCtx.Done():
	}
	// Flush buffered spans
	if err := shutdownTracing(shutdownCtx); err != nil {
		slog.Error("tracing shutdown failed", "err", err)
	}
	_ = metricsSrv.Shutdown(shutdownCtx)
	_ = shutdownMetrics(shutdownCtx)
	slog.Info("server stopped")
}
