package ratelimit

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Allower is what the middleware needs: a *Limiter in production, a fake in tests.
type Allower interface {
	Allow(ctx context.Context, key string) (Result, error)
}

// RejectFunc writes the 429 status and body; each service has its own error
// shape. The rate limit headers are already set when it's called.
type RejectFunc func(w http.ResponseWriter, r *http.Request)

// Middleware limits requests per client IP. Every limited route's response
// carries RateLimit-Limit, RateLimit-Remaining and RateLimit-Reset (seconds);
// a request over the limit also gets Retry-After and goes to reject instead
// of next.
//
// It fails open: if Redis can't answer, the request goes through and the
// error is logged and counted, so a Redis outage doesn't take the API down.
//
// Create it after telemetry.SetupMetrics: the decision counter comes from
// the global MeterProvider.
func Middleware(l Allower, reject RejectFunc) func(http.Handler) http.Handler {
	// Creating an instrument only fails on an invalid name.
	decisions, _ := otel.Meter("github.com/vandelay-labs/importer/internal/ratelimit").Int64Counter(
		"http.server.ratelimit.decisions",
		metric.WithDescription("Rate limit decisions, by outcome: allowed, limited, or error (Redis failed; the request went through)."))
	record := func(ctx context.Context, outcome string) {
		decisions.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			res, err := l.Allow(r.Context(), clientIP(r))
			if err != nil {
				record(r.Context(), "error")
				slog.WarnContext(r.Context(), "rate limit check failed, allowing the request", "err", err)
				next.ServeHTTP(w, r)
				return
			}

			reset := ceilSeconds(res.Reset)
			h := w.Header()
			h.Set("RateLimit-Limit", strconv.Itoa(res.Limit))
			h.Set("RateLimit-Remaining", strconv.Itoa(res.Remaining))
			h.Set("RateLimit-Reset", reset)
			if !res.Allowed {
				record(r.Context(), "limited")
				h.Set("Retry-After", reset)
				reject(w, r)
				return
			}
			record(r.Context(), "allowed")
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP is the address the connection came from. Headers like
// X-Forwarded-For are ignored: any client can set them, and nothing trusted
// sits in front of the pods (the Services use externalTrafficPolicy: Local
// to keep the client's address).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ceilSeconds rounds d up to whole seconds, at least 1: a client told to
// retry in 0 seconds would retry right away and be limited again.
func ceilSeconds(d time.Duration) string {
	return strconv.FormatInt(max(int64((d+time.Second-1)/time.Second), 1), 10)
}
