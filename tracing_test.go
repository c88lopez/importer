package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTraceRequests(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	var logs bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(traceLogHandler{slog.NewJSONHandler(&logs, nil)}))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	var ready atomic.Bool
	ready.Store(true)
	srv := httptest.NewServer(traceRequests(logRequests(newRouter(loadConfig(), newStore(), &ready, nil))))
	t.Cleanup(srv.Close)

	for _, path := range []string{"/healthz", "/readyz", "/api/items/42"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}

	got := spans.GetSpans()
	if len(got) != 1 {
		t.Fatalf("got %d spans, want 1 (probes should not be traced)", len(got))
	}
	if got[0].Name != "GET /api/items/{id}" {
		t.Errorf("span name: got %q, want %q", got[0].Name, "GET /api/items/{id}")
	}

	var line map[string]any
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("want one JSON request log line, got %q: %v", logs.String(), err)
	}
	if want := got[0].SpanContext.TraceID().String(); line["trace_id"] != want {
		t.Errorf("log trace_id: got %v, want %s", line["trace_id"], want)
	}
}
