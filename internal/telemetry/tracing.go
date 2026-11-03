// Package telemetry sets up traces, metrics and logs the same way for every
// importer service, so their telemetry lines up in Grafana.
package telemetry

import (
	"context"
	"io"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Service identifies the running binary on every span and metric.
type Service struct {
	Name    string // e.g. importer-api
	Version string // set at build time
}

// SetupTracing exports traces over OTLP/gRPC when OTEL_EXPORTER_OTLP_ENDPOINT
// is set (e.g. http://alloy.monitoring.svc:4317). Unset, tracing is a no-op so
// `go run` works without a collector. The exporter also reads the other
// standard OTEL_* env vars, and OTEL_SERVICE_NAME overrides the service name.
func SetupTracing(ctx context.Context, svc Service) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return noop, nil
	}

	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return noop, err
	}

	res, err := NewResource(ctx, svc)
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

// NewResource describes this process on every span and metric: service
// name and version (OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override).
func NewResource(ctx context.Context, svc Service) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", svc.Name),
			attribute.String("service.version", svc.Version),
		),
		resource.WithFromEnv(), // after the defaults so env vars win
		resource.WithTelemetrySDK(),
	)
}

// NewLogger writes JSON log lines to w. Lines written with a context that
// carries a span get trace_id and span_id, so Grafana can link them to traces.
func NewLogger(w io.Writer) *slog.Logger {
	return slog.New(traceLogHandler{slog.NewJSONHandler(w, nil)})
}

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
