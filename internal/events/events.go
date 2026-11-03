// Package events is the RabbitMQ contract between importer-api, which
// publishes, and importer-worker, which consumes.
package events

import (
	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel/propagation"
)

// Exchange is the topic exchange events are published to, with the event
// name as the routing key. Publishers and consumers both declare it.
const Exchange = "importer"

// ItemCreated is published when an item is created. The body is the item as
// JSON (items.Item); the message type is the routing key.
const ItemCreated = "item.created"

// Headers carries trace context (traceparent) in AMQP message headers, so a
// consumer's span continues the producer's trace.
type Headers amqp.Table

var _ propagation.TextMapCarrier = Headers(nil)

func (h Headers) Get(key string) string {
	v, _ := h[key].(string)
	return v
}

func (h Headers) Set(key, value string) { h[key] = value }

func (h Headers) Keys() []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	return keys
}
