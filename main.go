package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Set at build time: go build -ldflags "-X main.version=1.0.0"
var version = "dev"

func main() {
	logger := slog.New(traceLogHandler{slog.NewJSONHandler(os.Stdout, nil)})
	slog.SetDefault(logger)

	cfg := loadConfig()

	shutdownTracing, err := setupTracing(context.Background())
	if err != nil {
		slog.Error("tracing setup failed", "err", err)
		os.Exit(1)
	}

	checks, closeChecks, err := newCheckers(cfg)
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
		Handler:           traceRequests(logRequests(newRouter(cfg, newStore(), &ready, checks))),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("server starting", "port", cfg.Port, "version", version)
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
	// Flush buffered spans
	if err := shutdownTracing(shutdownCtx); err != nil {
		slog.Error("tracing shutdown failed", "err", err)
	}
	slog.Info("server stopped")
}

type config struct {
	Port    string
	Message string
	Env     string

	// Dependency URLs. Leave one empty to skip its health check.
	DatabaseURL string // postgres://user:pass@host:5432/db
	RedisURL    string // redis://:pass@host:6379/0
	RabbitMQURL string // amqp://user:pass@host:5672/

	// Dependencies that make the pod not ready when down. The rest are reported only.
	CriticalDeps []string
}

func loadConfig() config {
	return config{
		Port:    getenv("PORT", "8080"),
		Message: getenv("WELCOME_MESSAGE", "Vandelay Industries: importing and exporting since 1990"),
		Env:     getenv("APP_ENV", "local"),

		DatabaseURL: os.Getenv("DATABASE_URL"),
		RedisURL:    os.Getenv("REDIS_URL"),
		RabbitMQURL: os.Getenv("RABBITMQ_URL"),

		CriticalDeps: criticalDeps(),
	}
}

// criticalDeps reads CRITICAL_DEPENDENCIES, a comma-separated list.
// Unset means postgres and rabbitmq; set to empty means none are critical.
func criticalDeps() []string {
	raw, ok := os.LookupEnv("CRITICAL_DEPENDENCIES")
	if !ok {
		return []string{"postgres", "rabbitmq"}
	}
	var names []string
	for _, n := range strings.Split(raw, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return names
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return // probes are noisy
		}
		slog.InfoContext(r.Context(), "request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start).String())
	})
}
