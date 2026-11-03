// Package outbox implements the transactional outbox: events are written to
// the outbox table in the same transaction as the change they describe, and
// a Relay publishes them to RabbitMQ afterwards.
package outbox

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// Write records an event in the outbox table, inside the caller's
// transaction. The relay publishes it once the transaction commits.
func Write(ctx context.Context, tx pgx.Tx, routingKey string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// Remember the request's trace so the publish (and the worker) join it.
	traceHeaders := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, traceHeaders)
	_, err = tx.Exec(ctx,
		"INSERT INTO outbox (routing_key, payload, trace_headers) VALUES ($1, $2, $3)",
		routingKey, body, map[string]string(traceHeaders))
	return err
}
