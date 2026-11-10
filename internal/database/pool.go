// Package database opens the shared importer database and migrates it.
// importer-api owns the schema (../../migrations); importer-worker uses it.
package database

import (
	"context"
	"fmt"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool opens a lazy connection pool that traces its work: inside a
// request or message span, each query gets a child span ("query SELECT",
// with the SQL in db.query.text but not the parameters), as does waiting for
// a connection ("pool.acquire"), opening one ("connect") and preparing a
// statement the first time a connection runs it ("prepare SELECT"). Calls
// outside a span, like the /readyz ping, aren't traced.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(otelpgx.WithQuerySpanNamePrefix())
	return pgxpool.NewWithConfig(ctx, cfg)
}

// Open is NewPool for the services: it also reports the pool's usage as
// pgxpool_* metrics. Like NewPool it doesn't connect, so a database that's
// down shows up in /readyz, not here.
func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	db, err := NewPool(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid DATABASE_URL: %w", err)
	}
	if err := otelpgx.RecordStats(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("pool metrics setup failed: %w", err)
	}
	return db, nil
}
