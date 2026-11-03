package api

import (
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// TraceRequests starts a server span per request, named after the matched
// route (e.g. "GET /api/items/{id}"), and records the request in the
// http.server.request.duration histogram. Probes are skipped: they'd be most
// of the traces and skew the request counts.
func TraceRequests(next http.Handler) http.Handler {
	return otelhttp.NewHandler(next, "http.server",
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
		}),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if r.Pattern != "" {
				return r.Pattern
			}
			return r.Method
		}),
	)
}

// LogRequests logs one line per request, with its trace_id when traced.
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return // probes are noisy
		}
		slog.InfoContext(r.Context(), "request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start).String())
	})
}
