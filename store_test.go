package main

import (
	"context"
	"errors"
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
)

// newTestPgStore migrates the database at TEST_DATABASE_URL and empties the
// items and outbox tables. Never point it at real data: `make test` uses the
// cluster's importer_test database (see make dev-setup).
func newTestPgStore(t *testing.T) *pgStore {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	provider, closeProvider, err := newMigrationProvider(url)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	db, err := newDBPool(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Exec(ctx, "TRUNCATE items, outbox RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	return newPgStore(db)
}

func TestPgStoreCRUD(t *testing.T) {
	s := newTestPgStore(t)
	ctx := context.Background()

	latex, err := s.Create(ctx, "latex", "import")
	if err != nil {
		t.Fatal(err)
	}
	chips, err := s.Create(ctx, "potato chips", "export")
	if err != nil {
		t.Fatal(err)
	}
	if latex.ID == 0 || latex.CreatedAt.IsZero() {
		t.Fatalf("create didn't return generated columns: %+v", latex)
	}

	got, err := s.Get(ctx, latex.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != latex {
		t.Errorf("get = %+v, want %+v", got, latex)
	}

	items, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != latex.ID || items[1].ID != chips.ID {
		t.Errorf("list = %+v, want latex then chips", items)
	}

	if err := s.Delete(ctx, latex.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, latex.ID); !errors.Is(err, errNotFound) {
		t.Errorf("get after delete: err = %v, want errNotFound", err)
	}
	if err := s.Delete(ctx, latex.ID); !errors.Is(err, errNotFound) {
		t.Errorf("delete twice: err = %v, want errNotFound", err)
	}
}

// The event is committed with the item, and carries the item as JSON.
func TestCreateWritesOutbox(t *testing.T) {
	s := newTestPgStore(t)
	ctx := context.Background()
	notified := 0
	s.outboxWritten = func() { notified++ }

	it, err := s.Create(ctx, "latex", "import")
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != "pending" || it.ProcessedAt != nil {
		t.Errorf("new item = %+v, want status pending and no processedAt", it)
	}
	if notified != 1 {
		t.Errorf("outboxWritten called %d times, want 1", notified)
	}

	var key string
	var payload Item
	if err := s.db.QueryRow(ctx, "SELECT routing_key, payload FROM outbox").Scan(&key, &payload); err != nil {
		t.Fatal(err)
	}
	if key != "item.created" || payload.ID != it.ID || payload.Name != "latex" {
		t.Errorf("outbox row = %s %+v, want item.created for item %d", key, payload, it.ID)
	}
}

// A failed insert must not leave an event behind.
func TestCreateFailureWritesNoOutbox(t *testing.T) {
	s := newTestPgStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "x", "smuggle"); err == nil {
		t.Fatal("want a constraint violation")
	}
	var n int
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM outbox").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("outbox has %d rows after a failed create, want 0", n)
	}
}

// The handlers validate kind, but the CHECK constraint is the last line of
// defense; a violation must be a plain error (500), not errNotFound (404).
func TestPgStoreRejectsBadKind(t *testing.T) {
	s := newTestPgStore(t)
	_, err := s.Create(context.Background(), "x", "smuggle")
	if err == nil || errors.Is(err, errNotFound) {
		t.Fatalf("err = %v, want a constraint violation", err)
	}
}

// End to end through the router: the JSON id stays a string, and two
// "pods" (routers with their own pools) see each other's writes.
func TestItemsAPIWithPostgres(t *testing.T) {
	storeA := newTestPgStore(t)
	poolB, err := pgxpool.New(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer poolB.Close()

	var ready atomic.Bool
	ready.Store(true)
	podA := httptest.NewServer(newRouter(loadConfig(), storeA, &ready, nil))
	defer podA.Close()
	podB := httptest.NewServer(newRouter(loadConfig(), newPgStore(poolB), &ready, nil))
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

	s := newTestPgStore(t)
	if err := s.db.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(spans.GetSpans()); n != 0 {
		t.Fatalf("got %d spans before any request, want 0: setup and ping must not be traced", n)
	}

	var ready atomic.Bool
	ready.Store(true)
	srv := httptest.NewServer(traceRequests(newRouter(loadConfig(), s, &ready, nil)))
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
