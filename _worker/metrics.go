package main

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// setupMetrics makes the OpenTelemetry metrics that the consumer and
// otelpgx record available for Prometheus to scrape, and returns the
// /metrics handler. Call it before building the consumer and the database
// pool: they pick up the global MeterProvider when they're created.
//
// Main series:
//
//	messaging_process_duration_seconds     messages processed, by outcome
//	db_client_operation_duration_seconds   database calls by operation
//	pgxpool_*                              connection pool usage
//	go_*, process_*                        Go runtime and process
func setupMetrics(ctx context.Context) (http.Handler, func(context.Context) error, error) {
	// Own registry instead of the global one, so tests can call this twice.
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	exporter, err := otelprom.New(
		otelprom.WithRegisterer(reg),
		// Pinned so names stay messaging_process_duration_seconds even if
		// the library default moves to UTF-8 names like messaging.process.duration.
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
	)
	if err != nil {
		return nil, nil, err
	}
	res, err := newResource(ctx)
	if err != nil {
		return nil, nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithResource(res),
		sdkmetric.WithView(latencyBuckets("messaging.process.duration"), latencyBuckets("db.client.operation.duration")),
	)
	otel.SetMeterProvider(mp)

	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), mp.Shutdown, nil
}

// latencyBuckets overrides an instrument's histogram buckets (seconds). The
// defaults start at 5ms, and most requests here take under 1ms, so p95 would
// always read as "somewhere below 5ms".
func latencyBuckets(instrument string) sdkmetric.View {
	return sdkmetric.NewView(
		sdkmetric.Instrument{Name: instrument},
		sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
			Boundaries: []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}},
	)
}
