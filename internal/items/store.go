// Package items is the item model and its Postgres store, shared by
// importer-api (CRUD) and importer-worker (processing).
package items

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vandelay-labs/importer/internal/events"
	"github.com/vandelay-labs/importer/internal/outbox"
)

// Item is also the body of the item.created event.
type Item struct {
	// ",string" keeps the JSON id a string ("1"), as it was before the
	// database, so clients don't break. It's a BIGINT in Postgres.
	ID        int64     `json:"id,string"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`     // "import" or "export"
	Priority  string    `json:"priority"` // one of Priorities
	CreatedAt time.Time `json:"createdAt"`
	// "pending" until importer-worker processes the item, then "done".
	Status      string     `json:"status"`
	ProcessedAt *time.Time `json:"processedAt,omitempty"`
}

// Item priorities. Adding one also needs a migration that changes
// items_priority_check and a value in the GraphQL Priority enum; tests fail
// until both are done.
const (
	PriorityLow    = "low"
	PriorityNormal = "normal"
	PriorityHigh   = "high"
)

// Priorities lists every valid priority, lowest first.
var Priorities = []string{PriorityLow, PriorityNormal, PriorityHigh}

func ValidPriority(p string) bool { return slices.Contains(Priorities, p) }

var ErrNotFound = errors.New("not found")

// Store keeps items in Postgres, so every pod sees the same data. The
// schema comes from migrations/, applied by `importer-api migrate up`.
type Store struct {
	db *pgxpool.Pool

	// Called after a transaction that wrote to the outbox commits, so the
	// relay publishes right away instead of on its next poll. Optional.
	OutboxWritten func()
}

func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db, OutboxWritten: func() {}}
}

const itemColumns = "id, name, kind, priority, created_at, status, processed_at"

func (s *Store) List(ctx context.Context) ([]Item, error) {
	rows, err := s.db.Query(ctx, "SELECT "+itemColumns+" FROM items ORDER BY created_at, id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanItem)
}

// Get returns ErrNotFound for an unknown id.
func (s *Store) Get(ctx context.Context, id int64) (Item, error) {
	rows, _ := s.db.Query(ctx, "SELECT "+itemColumns+" FROM items WHERE id = $1", id)
	return collectOneItem(rows)
}

// Create inserts the item and its item.created event in one transaction:
// either both are committed or neither is, so the worker never misses an item
// or hears about one that doesn't exist.
func (s *Store) Create(ctx context.Context, name, kind, priority string) (Item, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Item{}, err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	rows, _ := tx.Query(ctx,
		"INSERT INTO items (name, kind, priority) VALUES ($1, $2, $3) RETURNING "+itemColumns, name, kind, priority)
	it, err := collectOneItem(rows)
	if err != nil {
		return Item{}, err
	}
	if err := outbox.Write(ctx, tx, events.ItemCreated, it); err != nil {
		return Item{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Item{}, err
	}
	s.OutboxWritten()
	return it, nil
}

// Delete returns ErrNotFound for an unknown id.
func (s *Store) Delete(ctx context.Context, id int64) error {
	tag, err := s.db.Exec(ctx, "DELETE FROM items WHERE id = $1", id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkDone records that the worker processed a pending item. It reports
// false, with no error, when there's nothing to do: the item is already done
// (a redelivered event) or was deleted before the worker got to it.
func (s *Store) MarkDone(ctx context.Context, id int64) (bool, error) {
	tag, err := s.db.Exec(ctx,
		"UPDATE items SET status = 'done', processed_at = now() WHERE id = $1 AND status = 'pending'", id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func scanItem(row pgx.CollectableRow) (Item, error) {
	var it Item
	err := row.Scan(&it.ID, &it.Name, &it.Kind, &it.Priority, &it.CreatedAt, &it.Status, &it.ProcessedAt)
	it.CreatedAt = it.CreatedAt.UTC()
	if it.ProcessedAt != nil {
		*it.ProcessedAt = it.ProcessedAt.UTC()
	}
	return it, err
}

// collectOneItem reads a single row (a Query error surfaces here too) and
// maps "no rows" to ErrNotFound.
func collectOneItem(rows pgx.Rows) (Item, error) {
	it, err := pgx.CollectExactlyOneRow(rows, scanItem)
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, ErrNotFound
	}
	return it, err
}
