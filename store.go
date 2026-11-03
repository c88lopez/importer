package main

import (
	"context"
	"errors"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Item struct {
	// ",string" keeps the JSON id a string ("1"), as it was before the
	// database, so clients don't break. It's a BIGINT in Postgres.
	ID        int64     `json:"id,string"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"` // "import" or "export"
	CreatedAt time.Time `json:"createdAt"`
	// "pending" until importer-worker processes the item, then "done".
	Status      string     `json:"status"`
	ProcessedAt *time.Time `json:"processedAt,omitempty"`
}

var errNotFound = errors.New("not found")

// itemStore is what the handlers need. pgStore is the real one; tests use
// memStore. Get and Delete return errNotFound for an unknown id.
type itemStore interface {
	List(ctx context.Context) ([]Item, error)
	Get(ctx context.Context, id int64) (Item, error)
	Create(ctx context.Context, name, kind string) (Item, error)
	Delete(ctx context.Context, id int64) error
}

// pgStore keeps items in Postgres, so every pod sees the same data. The
// schema comes from migrations/, applied by `importer-api migrate up`.
type pgStore struct {
	db *pgxpool.Pool

	// Called after a transaction that wrote to the outbox commits, so the
	// relay publishes right away instead of on its next poll. Optional.
	outboxWritten func()
}

func newPgStore(db *pgxpool.Pool) *pgStore {
	return &pgStore{db: db, outboxWritten: func() {}}
}

// newDBPool opens a lazy connection pool that traces its work: inside a
// request span, each query gets a child span ("query SELECT", with the SQL in
// db.query.text but not the parameters), as does waiting for a connection
// ("pool.acquire"), opening one ("connect") and preparing a statement the
// first time a connection runs it ("prepare SELECT"). Calls outside a span,
// like the /readyz ping, aren't traced.
func newDBPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(otelpgx.WithQuerySpanNamePrefix())
	return pgxpool.NewWithConfig(ctx, cfg)
}

const itemColumns = "id, name, kind, created_at, status, processed_at"

func (s *pgStore) List(ctx context.Context) ([]Item, error) {
	rows, err := s.db.Query(ctx, "SELECT "+itemColumns+" FROM items ORDER BY created_at, id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanItem)
}

func (s *pgStore) Get(ctx context.Context, id int64) (Item, error) {
	rows, _ := s.db.Query(ctx, "SELECT "+itemColumns+" FROM items WHERE id = $1", id)
	return collectOneItem(rows)
}

// Create inserts the item and its item.created event in one transaction:
// either both are committed or neither is, so the worker never misses an item
// or hears about one that doesn't exist.
func (s *pgStore) Create(ctx context.Context, name, kind string) (Item, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Item{}, err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	rows, _ := tx.Query(ctx,
		"INSERT INTO items (name, kind) VALUES ($1, $2) RETURNING "+itemColumns, name, kind)
	it, err := collectOneItem(rows)
	if err != nil {
		return Item{}, err
	}
	if err := writeOutbox(ctx, tx, "item.created", it); err != nil {
		return Item{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Item{}, err
	}
	s.outboxWritten()
	return it, nil
}

func (s *pgStore) Delete(ctx context.Context, id int64) error {
	tag, err := s.db.Exec(ctx, "DELETE FROM items WHERE id = $1", id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

func scanItem(row pgx.CollectableRow) (Item, error) {
	var it Item
	err := row.Scan(&it.ID, &it.Name, &it.Kind, &it.CreatedAt, &it.Status, &it.ProcessedAt)
	it.CreatedAt = it.CreatedAt.UTC()
	if it.ProcessedAt != nil {
		*it.ProcessedAt = it.ProcessedAt.UTC()
	}
	return it, err
}

// collectOneItem reads a single row (a Query error surfaces here too) and
// maps "no rows" to errNotFound.
func collectOneItem(rows pgx.Rows) (Item, error) {
	it, err := pgx.CollectExactlyOneRow(rows, scanItem)
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, errNotFound
	}
	return it, err
}
