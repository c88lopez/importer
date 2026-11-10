package telemetry

import (
	"context"
	"errors"
	"net/http"
)

// Setup sets up tracing, then metrics, for a service, and returns the
// /metrics handler and one shutdown that flushes both. Call it before
// building routers, pools and consumers (see SetupMetrics).
func Setup(ctx context.Context, svc Service) (metrics http.Handler, shutdown func(context.Context) error, err error) {
	shutdownTracing, err := SetupTracing(ctx, svc)
	if err != nil {
		return nil, nil, errors.Join(errors.New("tracing setup failed"), err)
	}
	metrics, shutdownMetrics, err := SetupMetrics(ctx, svc)
	if err != nil {
		_ = shutdownTracing(ctx)
		return nil, nil, errors.Join(errors.New("metrics setup failed"), err)
	}
	return metrics, func(ctx context.Context) error {
		// Spans first: they're buffered, metrics are only read on scrape.
		return errors.Join(shutdownTracing(ctx), shutdownMetrics(ctx))
	}, nil
}
