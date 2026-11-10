// importer-graphql serves the same items as importer-api, over GraphQL, so
// the two can be compared. It shares the items table and the outbox: an item
// created here is announced on RabbitMQ and processed by importer-worker too.
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
	"github.com/redis/go-redis/v9"

	"github.com/vandelay-labs/importer/internal/api"
	"github.com/vandelay-labs/importer/internal/database"
	"github.com/vandelay-labs/importer/internal/gql"
	"github.com/vandelay-labs/importer/internal/items"
	"github.com/vandelay-labs/importer/internal/outbox"
	"github.com/vandelay-labs/importer/internal/ratelimit"
	"github.com/vandelay-labs/importer/internal/telemetry"
)

// Set at build time: go build -ldflags "-X main.version=1.0.0"
var version = "dev"

func main() {
	slog.SetDefault(telemetry.NewLogger(os.Stdout))

	// Same settings as the API (PORT, METRICS_PORT, DATABASE_URL,
	// RABBITMQ_URL, REDIS_URL, CRITICAL_DEPENDENCIES, RATE_LIMIT_*).
	cfg := api.LoadConfig()
	svc := telemetry.Service{Name: "importer-graphql", Version: version}

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

	// createItem writes an outbox row like POST /api/items does. This
	// service publishes its own rows; the API's relay would also pick them up
	// within a second, and SKIP LOCKED keeps the two from sending one twice.
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

	// Redis holds the rate limit counts. It's optional: without it, /readyz
	// skips its check and requests aren't limited.
	var rdb *redis.Client
	var limiter *ratelimit.Limiter
	if cfg.RedisURL != "" {
		if rdb, err = api.NewRedisClient(cfg.RedisURL); err != nil {
			slog.Error("invalid REDIS_URL", "err", err)
			os.Exit(1)
		}
		defer rdb.Close()
		if cfg.RateLimit > 0 {
			limiter = ratelimit.New(rdb, "importer-graphql", cfg.RateLimit, cfg.RateLimitWindow)
		}
	}
	var limit func(http.Handler) http.Handler
	if limiter != nil {
		limit = gql.RateLimit(limiter)
		slog.Info("rate limiting per client IP", "limit", cfg.RateLimit, "window", cfg.RateLimitWindow.String())
	} else {
		slog.Warn("rate limiting is off: set REDIS_URL and RATE_LIMIT_REQUESTS to turn it on")
	}

	checks, err := api.NewCheckers(cfg, db, rdb)
	if err != nil {
		slog.Error("invalid dependency config", "err", err)
		os.Exit(1)
	}

	// ready flips to false on shutdown so the readiness probe fails and
	// Kubernetes stops routing new traffic to this pod before it exits.
	var ready atomic.Bool

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           api.TraceRequests(api.LogRequests(gql.NewRouter(store, version, api.ReadinessHandler(&ready, checks), limit))),
		ReadHeaderTimeout: 5 * time.Second,
	}

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
	stopRelay()
	select {
	case <-relayDone:
	case <-shutdownCtx.Done():
	}
	if err := shutdownTracing(shutdownCtx); err != nil {
		slog.Error("tracing shutdown failed", "err", err)
	}
	_ = metricsSrv.Shutdown(shutdownCtx)
	_ = shutdownMetrics(shutdownCtx)
	slog.Info("server stopped")
}
