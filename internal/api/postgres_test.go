package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/vandelay-labs/importer/internal/database/dbtest"
	"github.com/vandelay-labs/importer/internal/items"
)

// End to end through the router: the JSON id stays a string, and two
// "pods" (routers with their own pools) see each other's writes.
func TestItemsAPIWithPostgres(t *testing.T) {
	storeA := items.NewStore(dbtest.New(t))
	poolB, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer poolB.Close()

	var ready atomic.Bool
	ready.Store(true)
	podA := httptest.NewServer(NewRouter(LoadConfig(), storeA, &ready, nil))
	defer podA.Close()
	podB := httptest.NewServer(NewRouter(LoadConfig(), items.NewStore(poolB), &ready, nil))
	defer podB.Close()

	res, err := http.Post(podA.URL+"/api/items", "application/json", strings.NewReader(`{"name":"latex","kind":"import"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create: got %d, want 201", res.StatusCode)
	}

	res, err = http.Get(podB.URL + "/api/items/1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"id":"1"`) {
		t.Fatalf("get from the other pod: %d %s", res.StatusCode, body)
	}
}

// A traced request gets its database work as child spans, so Tempo shows how
// much of the request was the query. Untraced calls (like the /readyz ping)
// must not start traces of their own.
func TestQueriesAreTraced(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp) // before the pool: its tracer picks up the provider
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	db := dbtest.New(t)
	s := items.NewStore(db)
	if err := db.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(spans.GetSpans()); n != 0 {
		t.Fatalf("got %d spans before any request, want 0: setup and ping must not be traced", n)
	}

	var ready atomic.Bool
	ready.Store(true)
	srv := httptest.NewServer(TraceRequests(NewRouter(LoadConfig(), s, &ready, nil)))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/api/items")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	// The server span ends after the handler returns, which can be just after
	// the client has the response.
	var server sdktrace.ReadOnlySpan
	for deadline := time.Now().Add(time.Second); server == nil && time.Now().Before(deadline); {
		for _, sp := range spans.GetSpans().Snapshots() {
			if sp.Name() == "GET /api/items" {
				server = sp
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if server == nil {
		t.Fatal("no server span")
	}

	children := map[string]sdktrace.ReadOnlySpan{}
	for _, sp := range spans.GetSpans().Snapshots() {
		if sp.SpanContext().SpanID() == server.SpanContext().SpanID() {
			continue
		}
		if sp.SpanContext().TraceID() != server.SpanContext().TraceID() {
			t.Errorf("span %q is in another trace", sp.Name())
		}
		if sp.Parent().SpanID() == server.SpanContext().SpanID() {
			children[sp.Name()] = sp
		}
	}

	query, ok := children["query SELECT"]
	if !ok {
		t.Fatalf("no query SELECT span under the request; children: %v", children)
	}
	if _, ok := children["pool.acquire"]; !ok {
		t.Errorf("no pool.acquire span under the request; children: %v", children)
	}
	var sql string
	for _, attr := range query.Attributes() {
		if attr.Key == "db.query.text" {
			sql = attr.Value.AsString()
		}
	}
	if !strings.Contains(sql, "FROM items") {
		t.Errorf("db.query.text = %q, want the SELECT on items", sql)
	}
}
