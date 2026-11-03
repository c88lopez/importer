package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/vandelay-labs/importer/internal/events"
	"github.com/vandelay-labs/importer/internal/items"
)

// ItemProcessor handles item.created events.
type ItemProcessor struct {
	store *items.Store
}

func NewItemProcessor(store *items.Store) *ItemProcessor {
	return &ItemProcessor{store: store}
}

// Handle marks the item done. It's idempotent, because RabbitMQ delivers at
// least once: a redelivery, or an item deleted before it was processed,
// matches no pending row and is acknowledged as is.
func (p *ItemProcessor) Handle(ctx context.Context, d amqp.Delivery) error {
	if d.Type != events.ItemCreated {
		return permanent(fmt.Errorf("unexpected message type %q", d.Type))
	}
	var it items.Item
	if err := json.Unmarshal(d.Body, &it); err != nil || it.ID == 0 {
		return permanent(fmt.Errorf("invalid item.created body: %v", err))
	}

	done, err := p.store.MarkDone(ctx, it.ID)
	if err != nil {
		return err // transient (e.g. database down): retried
	}
	if !done {
		slog.InfoContext(ctx, "item already processed or deleted", "item_id", it.ID)
		return nil
	}
	slog.InfoContext(ctx, "item processed", "item_id", it.ID, "name", it.Name, "kind", it.Kind)
	return nil
}
