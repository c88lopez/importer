// Package dbtest gives tests a migrated, empty database.
package dbtest

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vandelay-labs/importer/internal/database"
)

// New migrates the database at TEST_DATABASE_URL to the latest schema, empties
// its tables and returns a pool on it. It skips the test when the variable
// isn't set. Never point it at real data: `make test` uses the cluster's
// importer_test database (see make dev-setup).
//
// Packages share the database, so `make test` runs them one at a time (-p 1).
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	provider, closeProvider, err := database.NewMigrationProvider(url)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	db, err := database.NewPool(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Exec(ctx, "TRUNCATE items, outbox RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	return db
}
