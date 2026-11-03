// Package worker is importer-worker: it consumes item.created events and
// processes them. cmd/worker wires it to the database and the ops server.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/vandelay-labs/importer/internal/events"
)

// Topology. importer-api publishes to events.Exchange; the worker owns its
// queues and declares them on every connect (declaring is idempotent, but
// changing a queue's arguments later fails with PRECONDITION_FAILED: delete
// the queue first, or move the setting to a policy).
const (
	itemsQueue     = "importer-worker.item-created" // bound to item.created
	maxDeliveries  = 10                             // attempts before a message is dead-lettered
	processTimeout = 30 * time.Second
)

// errPermanent marks a message that can never succeed (malformed, unknown
// type). It goes straight to the dead-letter queue instead of being retried.
var errPermanent = errors.New("permanent failure")

func permanent(err error) error { return errors.Join(errPermanent, err) }

// Consumer reads item.created events one at a time and acks each one only
// after handle succeeds, so a crash mid-message means a redelivery, not a
// loss. Scale out with replicas: each pod gets its own share of the queue.
type Consumer struct {
	url           string
	exchange      string
	queue         string
	maxDeliveries int // attempts before a message is dead-lettered
	prefetch      int // unacked messages the broker sends ahead; they go back to the queue if the pod dies
	handle        func(context.Context, amqp.Delivery) error

	connected atomic.Bool // consuming right now; see Connected
	tracer    trace.Tracer
	duration  metric.Float64Histogram
}

// NewConsumer returns a consumer that passes each message to handle. Call
// Run to start it.
func NewConsumer(url string, handle func(context.Context, amqp.Delivery) error) (*Consumer, error) {
	duration, err := otel.Meter("importer-worker/consumer").Float64Histogram("messaging.process.duration",
		metric.WithDescription("Time to process a message, by outcome."), metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	return &Consumer{
		url:           url,
		exchange:      events.Exchange,
		queue:         itemsQueue,
		maxDeliveries: maxDeliveries,
		prefetch:      10,
		handle:        handle,
		tracer:        otel.Tracer("importer-worker/consumer"),
		duration:      duration,
	}, nil
}

// Connected reports whether the consumer is consuming right now.
func (c *Consumer) Connected() bool { return c.connected.Load() }

func (c *Consumer) dlq() string { return c.queue + ".dlq" }

// declareTopology creates the exchange, the work queue, its dead-letter
// queue and the binding. Quorum queues are replicated and durable. A message
// the consumer rejects goes to the dead-letter queue, where it waits for a
// human.
func (c *Consumer) declareTopology(ch *amqp.Channel) error {
	if err := ch.ExchangeDeclare(c.exchange, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(c.dlq(), true, false, false, false,
		amqp.Table{amqp.QueueTypeArg: amqp.QueueTypeQuorum}); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(c.queue, true, false, false, false, amqp.Table{
		amqp.QueueTypeArg: amqp.QueueTypeQuorum,
		// Dead letters go through the default exchange, straight to the DLQ by name.
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": c.dlq(),
	}); err != nil {
		return err
	}
	return ch.QueueBind(c.queue, events.ItemCreated, c.exchange, false, nil)
}

// Run consumes until ctx is cancelled, reconnecting with backoff when the
// connection drops. It returns once the message in hand is finished.
func (c *Consumer) Run(ctx context.Context) {
	backoff := time.Second
	for {
		err := c.session(ctx, func() { backoff = time.Second })
		c.connected.Store(false)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("consumer disconnected from rabbitmq, retrying", "err", err, "in", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *Consumer) session(ctx context.Context, connected func()) error {
	hostname, _ := os.Hostname()
	conn, err := amqp.DialConfig(c.url, amqp.Config{
		Properties: amqp.Table{"connection_name": "importer-worker (" + hostname + ")"},
	})
	if err != nil {
		return err
	}
	// Closing the connection hands unacked prefetched messages back to the queue.
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	if err := c.declareTopology(ch); err != nil {
		return err
	}
	if err := ch.Qos(c.prefetch, 0, false); err != nil {
		return err
	}
	deliveries, err := ch.Consume(c.queue, hostname, false, false, false, false, nil)
	if err != nil {
		return err
	}
	connected()
	c.connected.Store(true)
	slog.Info("consuming", "queue", c.queue, "prefetch", c.prefetch)

	for {
		select {
		case <-ctx.Done():
			return nil
		case d, ok := <-deliveries:
			if !ok {
				return errors.New("delivery channel closed")
			}
			c.process(ctx, d)
		}
	}
}

// process handles one delivery under a consumer span that continues the
// producer's trace, then settles it:
//
//	success            ack: done
//	permanent failure  reject: straight to the dead-letter queue
//	other failure      wait, then requeue; reject on the last attempt
//
// The worker counts attempts itself: in RabbitMQ 4, a 0.9.1 nack with
// requeue doesn't count toward a quorum queue's x-delivery-limit, so the
// broker would retry a failing message forever.
func (c *Consumer) process(ctx context.Context, d amqp.Delivery) {
	start := time.Now()
	attempt := acquiredCount(d) + 1
	ctx = otel.GetTextMapPropagator().Extract(ctx, events.Headers(d.Headers))
	ctx, span := c.tracer.Start(ctx, "process "+c.queue,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "rabbitmq"),
			attribute.String("messaging.operation.type", "process"),
			attribute.String("messaging.destination.name", c.queue),
			attribute.String("messaging.rabbitmq.destination.routing_key", d.RoutingKey),
			attribute.String("messaging.message.id", d.MessageId),
			attribute.Int64("messaging.rabbitmq.attempt", attempt),
		))
	defer span.End()

	var err error
	if attempt > int64(c.maxDeliveries) {
		// Delivered again after the last attempt: the pod died while
		// processing it, maybe because of it. Don't try it again.
		err = permanent(fmt.Errorf("gave up after %d attempts", c.maxDeliveries))
	} else {
		// Finish the message even if shutdown starts meanwhile, within a bound.
		handleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), processTimeout)
		err = c.handle(handleCtx, d)
		cancel()
	}

	outcome := "done"
	switch {
	case err == nil:
		_ = d.Ack(false)
	case errors.Is(err, errPermanent) || attempt >= int64(c.maxDeliveries):
		outcome = "dead_lettered"
		slog.ErrorContext(ctx, "message rejected to the dead-letter queue", "message_id", d.MessageId,
			"err", err, "attempt", attempt)
		_ = d.Reject(false)
	default:
		outcome = "requeued"
		wait := retryDelay(attempt)
		slog.WarnContext(ctx, "message failed, requeueing", "message_id", d.MessageId, "err", err,
			"attempt", attempt, "in", wait.String())
		// Waiting here also slows this pod down while, say, the database is down.
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		_ = d.Nack(false, true)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	c.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
		attribute.String("messaging.destination.name", c.queue),
		attribute.String("outcome", outcome),
	))
}

// acquiredCount is how many times RabbitMQ handed this message out before:
// requeues, and deliveries to a consumer that died before settling it.
// Quorum queues set x-acquired-count on redeliveries.
func acquiredCount(d amqp.Delivery) int64 {
	n, _ := d.Headers["x-acquired-count"].(int64)
	return n
}

// retryDelay backs off 1s, 2s, 4s... up to 30s after a failed attempt, so
// the maxDeliveries attempts span a few minutes instead of milliseconds.
func retryDelay(attempt int64) time.Duration {
	return min(time.Second<<min(attempt-1, 5), 30*time.Second)
}
