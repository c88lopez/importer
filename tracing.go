package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// setupTracing exports traces over OTLP/gRPC when OTEL_EXPORTER_OTLP_ENDPOINT
// is set (e.g. http://alloy.monitoring.svc:4317). Unset, tracing is a no-op so
// `go run .` works without a collector. The exporter also reads the other
// standard OTEL_* env vars, and OTEL_SERVICE_NAME overrides the service name.
func setupTracing(ctx context.Context) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return noop, nil
	}

	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return noop, err
	}

	res, err := newResource(ctx)
	if err != nil {
		return noop, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp.Shutdown, nil
}

// newResource describes this process on every span and metric: service
// name and version (OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override).
func newResource(ctx context.Context) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", "importer-api"),
			attribute.String("service.version", version),
		),
		resource.WithFromEnv(), // after the defaults so env vars win
		resource.WithTelemetrySDK(),
	)
}

// traceRequests starts a server span per request, named after the matched
// route (e.g. "GET /api/items/{id}"), and records the request in the
// http.server.request.duration histogram (see metrics.go). Probes are
// skipped: they'd be most of the traces and skew the request counts.
func traceRequests(next http.Handler) http.Handler {
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

// traceLogHandler adds trace_id and span_id to log records written with a
// context that carries a span, so Grafana can link log lines to traces.
type traceLogHandler struct{ slog.Handler }

func (h traceLogHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceLogHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceLogHandler) WithGroup(name string) slog.Handler {
	return traceLogHandler{h.Handler.WithGroup(name)}
}
