// importer-graphql serves the same items as importer-api, over GraphQL, so
// the two can be compared. It shares the items table and the outbox: an item
// created here is announced on RabbitMQ and processed by importer-worker too.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/vandelay-labs/importer/internal/api"
	"github.com/vandelay-labs/importer/internal/app"
	"github.com/vandelay-labs/importer/internal/gql"
	"github.com/vandelay-labs/importer/internal/telemetry"
)

// Set at build time: go build -ldflags "-X main.version=1.0.0"
var version = "dev"

func main() {
	slog.SetDefault(telemetry.NewLogger(os.Stdout))
	if err := run(); err != nil {
		slog.Error("importer-graphql failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// Kubernetes sends SIGTERM when it deletes a pod (rollout, scale down, etc).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Same settings as the API (PORT, METRICS_PORT, DATABASE_URL,
	// RABBITMQ_URL, REDIS_URL, CRITICAL_DEPENDENCIES, RATE_LIMIT_*).
	cfg := api.LoadConfig()
	svc := telemetry.Service{Name: "importer-graphql", Version: version}

	// Before the service and router: they read the global MeterProvider when created.
	metrics, shutdownTelemetry, err := telemetry.Setup(ctx, svc)
	if err != nil {
		return err
	}
	// createItem writes an outbox row like POST /api/items does. This
	// service publishes its own rows; the API's relay would also pick them up
	// within a second, and SKIP LOCKED keeps the two from sending one twice.
	s, err := app.NewItemsService(ctx, cfg, svc, gql.RateLimit)
	if err != nil {
		return err
	}
	defer s.Close()

	router := gql.NewRouter(s.Store, version, s.Readyz, s.Limit)
	return s.Serve(ctx, api.TraceRequests(api.LogRequests(router)), metrics, shutdownTelemetry)
}
