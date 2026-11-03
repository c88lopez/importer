package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
)

// itemCreated is the part of importer-api's item.created event the worker
// uses. The body is the item as the API returns it; the id is a JSON string.
type itemCreated struct {
	ID   int64  `json:"id,string"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// itemProcessor handles item.created events against the items table, which
// importer-api owns: its migrations create the status and processed_at
// columns this relies on.
type itemProcessor struct {
	db *pgxpool.Pool
}

func newItemProcessor(db *pgxpool.Pool) *itemProcessor {
	return &itemProcessor{db: db}
}

// newDBPool opens a lazy connection pool whose queries are child spans of
// the message being processed.
func newDBPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(otelpgx.WithQuerySpanNamePrefix())
	return pgxpool.NewWithConfig(ctx, cfg)
}

// Handle marks the item done. It's idempotent, because RabbitMQ delivers at
// least once: a redelivery, or an item deleted before it was processed,
// matches no pending row and is acknowledged as is.
func (p *itemProcessor) Handle(ctx context.Context, d amqp.Delivery) error {
	if d.Type != "item.created" {
		return permanent(fmt.Errorf("unexpected message type %q", d.Type))
	}
	var ev itemCreated
	if err := json.Unmarshal(d.Body, &ev); err != nil || ev.ID == 0 {
		return permanent(fmt.Errorf("invalid item.created body: %v", err))
	}

	tag, err := p.db.Exec(ctx,
		"UPDATE items SET status = 'done', processed_at = now() WHERE id = $1 AND status = 'pending'", ev.ID)
	if err != nil {
		return err // transient (e.g. database down): retried
	}
	if tag.RowsAffected() == 0 {
		slog.InfoContext(ctx, "item already processed or deleted", "item_id", ev.ID)
		return nil
	}
	slog.InfoContext(ctx, "item processed", "item_id", ev.ID, "name", ev.Name, "kind", ev.Kind)
	return nil
}
