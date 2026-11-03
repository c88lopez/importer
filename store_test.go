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

	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestPgStore migrates the database at TEST_DATABASE_URL and empties the
// items table. Use a throwaway database: `make db-up` creates importer_test.
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

	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Exec(ctx, "TRUNCATE items RESTART IDENTITY"); err != nil {
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
