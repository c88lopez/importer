// importer-worker processes the events importer-api publishes to RabbitMQ.
// For each item.created it marks the item done in the shared database.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vandelay-labs/importer/internal/app"
	"github.com/vandelay-labs/importer/internal/database"
	"github.com/vandelay-labs/importer/internal/items"
	"github.com/vandelay-labs/importer/internal/telemetry"
	"github.com/vandelay-labs/importer/internal/worker"
)

// Set at build time: go build -ldflags "-X main.version=1.0.0"
var version = "dev"

func main() {
	slog.SetDefault(telemetry.NewLogger(os.Stdout))

	cfg := loadConfig()
	svc := telemetry.Service{Name: "importer-worker", Version: version}

	// Before the pool and the consumer: they read the global MeterProvider when created.
	metricsHandler, shutdownTelemetry, err := telemetry.Setup(context.Background(), svc)
	if err != nil {
		slog.Error("telemetry setup failed", "err", err)
		os.Exit(1)
	}

	// Both dependencies are required. Neither connects here: a database or
	// broker that's down shows up in /readyz, and the consumer retries.
	if err := app.Require(app.Setting{Name: "DATABASE_URL", Value: cfg.DatabaseURL}, app.Setting{Name: "RABBITMQ_URL", Value: cfg.RabbitMQURL}); err != nil {
		slog.Error("invalid config", "err", err)
		os.Exit(1)
	}
	db, err := database.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		slog.Error("database setup failed", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	c, err := worker.NewConsumer(cfg.RabbitMQURL, worker.NewItemProcessor(items.NewStore(db)).Handle)
	if err != nil {
		slog.Error("consumer setup failed", "err", err)
		os.Exit(1)
	}

	// No API, so one server for probes and Prometheus.
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           worker.NewOpsRouter(metricsHandler, c, db),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("ops server failed", "err", err)
			os.Exit(1)
		}
	}()

	// Kubernetes sends SIGTERM when it deletes a pod (rollout, scale down, etc).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	slog.Info("worker starting", "port", cfg.Port, "version", version)
	c.Run(ctx) // returns after finishing the message in hand

	slog.Info("worker stopped consuming, shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if err := shutdownTelemetry(shutdownCtx); err != nil {
		slog.Error("telemetry shutdown failed", "err", err)
	}
	slog.Info("worker stopped")
}

type config struct {
	Port        string // probes and /metrics
	DatabaseURL string // postgres://user:pass@host:5432/db, the same database as importer-api
	RabbitMQURL string // amqp://user:pass@host:5672/
}

func loadConfig() config {
	return config{
		Port:        getenv("PORT", "9464"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		RabbitMQURL: os.Getenv("RABBITMQ_URL"),
	}
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
