// Package api is importer-api's HTTP side: routes, health checks and
// middleware. cmd/api wires it to the database, the outbox relay and the
// servers.
package api

import (
	"os"
	"strings"
)

type Config struct {
	Port        string
	MetricsPort string // serves /metrics for Prometheus
	Message     string
	Env         string
	Version     string // shown on GET /; set by cmd/api from the build

	// Dependency URLs. DatabaseURL and RabbitMQURL are required; leave
	// RedisURL empty to skip its health check.
	DatabaseURL string // postgres://user:pass@host:5432/db
	RedisURL    string // redis://:pass@host:6379/0
	RabbitMQURL string // amqp://user:pass@host:5672/

	// Dependencies that make the pod not ready when down. The rest are reported only.
	CriticalDeps []string
}

func LoadConfig() Config {
	return Config{
		Port:        getenv("PORT", "8080"),
		MetricsPort: getenv("METRICS_PORT", "9464"),
		Message:     getenv("WELCOME_MESSAGE", "Vandelay Industries: importing and exporting since 1990"),
		Env:         getenv("APP_ENV", "local"),
		Version:     "dev",

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
