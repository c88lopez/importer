package app

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/vandelay-labs/importer/internal/api"
	"github.com/vandelay-labs/importer/internal/database"
	"github.com/vandelay-labs/importer/internal/items"
	"github.com/vandelay-labs/importer/internal/outbox"
	"github.com/vandelay-labs/importer/internal/ratelimit"
	"github.com/vandelay-labs/importer/internal/telemetry"
)

// ItemsService is what importer-api and importer-graphql share: the items in
// Postgres, the outbox relay that announces new ones on RabbitMQ, the rate
// limit in Redis, the readiness check, and the API and metrics servers.
// Each binary adds only its router.
//
// Nothing connects when it's built: a dependency that's down shows up in
// /readyz, and the relay retries in the background once Serve starts it.
type ItemsService struct {
	cfg api.Config
	svc telemetry.Service

	// Store is the item store to give the router.
	Store *items.Store
	// Readyz is the readiness check to give the router. It fails until
	// Serve starts and again once shutdown begins.
	Readyz http.Handler
	// Limit is the rate limit middleware to give the router, or nil when
	// limiting is off (no REDIS_URL, or RATE_LIMIT_REQUESTS=0).
	Limit func(http.Handler) http.Handler

	db    *pgxpool.Pool
	rdb   *redis.Client // nil without REDIS_URL
	relay *outbox.Relay
	ready atomic.Bool
}

// NewItemsService opens the service's dependencies from cfg. rateLimit
// builds the service's own rate limit middleware (api.RateLimit,
// gql.RateLimit); svc.Name also keys its counts in Redis. Call Close when done.
func NewItemsService(ctx context.Context, cfg api.Config, svc telemetry.Service, rateLimit func(ratelimit.Allower) func(http.Handler) http.Handler) (*ItemsService, error) {
	// Items live in Postgres and new ones are announced on RabbitMQ (see
	// internal/outbox), so the service can't run without either.
	if err := Require(Setting{"DATABASE_URL", cfg.DatabaseURL}, Setting{"RABBITMQ_URL", cfg.RabbitMQURL}); err != nil {
		return nil, err
	}
	s := &ItemsService{cfg: cfg, svc: svc}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()

	var err error
	if s.db, err = database.Open(ctx, cfg.DatabaseURL); err != nil {
		return nil, err
	}
	if s.relay, err = outbox.NewRelay(s.db, cfg.RabbitMQURL); err != nil {
		return nil, err
	}
	s.Store = items.NewStore(s.db)
	s.Store.OutboxWritten = s.relay.Notify

	// Redis holds the rate limit counts. It's optional: without it, /readyz
	// skips its check and requests aren't limited.
	if cfg.RedisURL != "" {
		if s.rdb, err = api.NewRedisClient(cfg.RedisURL); err != nil {
			return nil, err
		}
		if cfg.RateLimit > 0 {
			s.Limit = rateLimit(ratelimit.New(s.rdb, svc.Name, cfg.RateLimit, cfg.RateLimitWindow))
		}
	}
	if s.Limit != nil {
		slog.Info("rate limiting per client IP", "limit", cfg.RateLimit, "window", cfg.RateLimitWindow.String())
	} else {
		slog.Warn("rate limiting is off: set REDIS_URL and RATE_LIMIT_REQUESTS to turn it on")
	}

	checks, err := api.NewCheckers(cfg, s.db, s.rdb)
	if err != nil {
		return nil, err
	}
	s.Readyz = api.ReadinessHandler(&s.ready, checks)
	ok = true
	return s, nil
}

// Serve starts the outbox relay, serves handler on PORT and metrics on
// METRICS_PORT until ctx is cancelled, then shuts down (see Run). The relay
// stops after the servers drain, since requests that finished draining may
// have written events; shutdownTelemetry then flushes the spans.
func (s *ItemsService) Serve(ctx context.Context, handler, metrics http.Handler, shutdownTelemetry func(context.Context) error) error {
	stopRelay := s.relay.Start()
	servers := []*http.Server{
		{Addr: ":" + s.cfg.Port, Handler: handler, ReadHeaderTimeout: 5 * time.Second},
		// Metrics get their own port, so they aren't exposed through the
		// Service (NodePort) and scrapes don't show up as API traffic.
		{Addr: ":" + s.cfg.MetricsPort, Handler: metrics, ReadHeaderTimeout: 5 * time.Second},
	}
	slog.Info("server starting", "port", s.cfg.Port, "metricsPort", s.cfg.MetricsPort, "version", s.svc.Version)
	return Run(ctx, &s.ready, servers, stopRelay, shutdownTelemetry)
}

// Close closes the Redis client and the database pool.
func (s *ItemsService) Close() {
	if s.rdb != nil {
		_ = s.rdb.Close()
	}
	if s.db != nil {
		s.db.Close()
	}
}
