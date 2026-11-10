package database

import (
	"context"
	"strings"
	"testing"
)

func TestOpen(t *testing.T) {
	// Lazy: an unreachable database still opens, and fails on first use.
	db, err := Open(context.Background(), "postgres://u:p@127.0.0.1:1/db?connect_timeout=1")
	if err != nil {
		t.Fatalf("unreachable database: got %v, want a lazy pool", err)
	}
	db.Close()

	_, err = Open(context.Background(), "postgres://u:p@host:notaport/db")
	if err == nil || !strings.Contains(err.Error(), "invalid DATABASE_URL") {
		t.Fatalf("bad URL: got %v, want an invalid DATABASE_URL error", err)
	}
}
