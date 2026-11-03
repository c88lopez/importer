package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
)

// The Grafana dashboard (k8s/dashboards/importer-api.json) queries these
// exact names and labels; if this test breaks, the dashboard does too.
func TestMetrics(t *testing.T) {
	prev := otel.GetMeterProvider()
	handler, shutdown, err := setupMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = shutdown(context.Background())
		otel.SetMeterProvider(prev)
	})

	var ready atomic.Bool
	ready.Store(true)
	srv := httptest.NewServer(traceRequests(newRouter(loadConfig(), newMemStore(), &ready, nil)))
	defer srv.Close()
	for _, path := range []string{"/api/items", "/api/items/42", "/healthz", "/readyz"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	metrics := string(body)

	for _, want := range []string{
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="200",http_route="/api/items"`,
		`http_server_request_duration_seconds_count{http_request_method="GET",http_response_status_code="404",http_route="/api/items/{id}"`,
		`http_server_request_duration_seconds_bucket{http_request_method="GET",http_response_status_code="200",http_route="/api/items",`, // sub-ms bucket below
		`le="0.0005"`,
		`target_info{service_name="importer-api",service_version="dev"`,
		`go_goroutines `,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("/metrics is missing %s", want)
		}
	}
	for _, probe := range []string{`http_route="/healthz"`, `http_route="/readyz"`} {
		if strings.Contains(metrics, probe) {
			t.Errorf("/metrics counts probe requests (%s); they'd swamp the request rate", probe)
		}
	}
}
