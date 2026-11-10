// importer-api serves the items API and announces new items on RabbitMQ.
// `importer-api migrate ...` runs database migrations instead.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/vandelay-labs/importer/internal/api"
	"github.com/vandelay-labs/importer/internal/app"
	"github.com/vandelay-labs/importer/internal/database"
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
	if err := run(); err != nil {
		slog.Error("importer-api failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// Kubernetes sends SIGTERM when it deletes a pod (rollout, scale down, etc).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := api.LoadConfig()
	cfg.Version = version
	svc := telemetry.Service{Name: "importer-api", Version: version}

	// Before the service and router: they read the global MeterProvider when created.
	metrics, shutdownTelemetry, err := telemetry.Setup(ctx, svc)
	if err != nil {
		return err
	}
	s, err := app.NewItemsService(ctx, cfg, svc, api.RateLimit)
	if err != nil {
		return err
	}
	defer s.Close()

	router := api.NewRouter(cfg, s.Store, s.Readyz, s.Limit)
	return s.Serve(ctx, api.TraceRequests(api.LogRequests(router)), metrics, shutdownTelemetry)
}
