package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// newTestConsumer returns a consumer with its own exchange and queues on the
// RabbitMQ at TEST_RABBITMQ_URL (`make test` uses the importer-test vhost), and a channel
// to publish with. Everything is deleted when the test ends.
func newTestConsumer(t *testing.T, handle func(context.Context, amqp.Delivery) error) (*consumer, *amqp.Channel) {
	t.Helper()
	url := os.Getenv("TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("TEST_RABBITMQ_URL not set")
	}
	c, err := newConsumer(url, handle)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	c.exchange, c.queue = "test-"+suffix, "test-worker-"+suffix

	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = ch.QueueDelete(c.queue, false, false, false)
		_, _ = ch.QueueDelete(c.dlq(), false, false, false)
		_ = ch.ExchangeDelete(c.exchange, false, false)
	})
	return c, ch
}

func runConsumer(t *testing.T, c *consumer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "consumer to connect", c.connected.Load)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// publish sends a message the way importer-api's outbox relay does.
func publish(t *testing.T, ch *amqp.Channel, exchange string, msg amqp.Publishing) {
	t.Helper()
	msg.DeliveryMode = amqp.Persistent
	if err := ch.PublishWithContext(context.Background(), exchange, "item.created", true, false, msg); err != nil {
		t.Fatal(err)
	}
}

func queueDepthOf(t *testing.T, ch *amqp.Channel, queue string) int {
	t.Helper()
	q, err := ch.QueueDeclarePassive(queue, true, false, false, false,
		amqp.Table{amqp.QueueTypeArg: amqp.QueueTypeQuorum})
	if err != nil {
		t.Fatal(err)
	}
	return q.Messages
}

func TestConsumerProcessesItemCreated(t *testing.T) {
	db := newTestDB(t)
	c, ch := newTestConsumer(t, newItemProcessor(db).Handle)
	runConsumer(t, c)

	id := insertItem(t, db)
	d := itemCreatedDelivery(id)
	publish(t, ch, c.exchange, amqp.Publishing{Type: d.Type, ContentType: "application/json", Body: d.Body})

	waitFor(t, "item to be done", func() bool {
		status, _ := itemStatus(t, db, id)
		return status == "done"
	})
}

// A malformed message goes to the dead-letter queue on its first delivery,
// and the consumer moves on to the next one.
func TestConsumerDeadLettersBadMessages(t *testing.T) {
	db := newTestDB(t)
	c, ch := newTestConsumer(t, newItemProcessor(db).Handle)
	runConsumer(t, c)

	publish(t, ch, c.exchange, amqp.Publishing{Type: "item.created", Body: []byte("not json")})
	id := insertItem(t, db)
	d := itemCreatedDelivery(id)
	publish(t, ch, c.exchange, amqp.Publishing{Type: d.Type, Body: d.Body})

	waitFor(t, "the good message to be processed", func() bool {
		status, _ := itemStatus(t, db, id)
		return status == "done"
	})
	waitFor(t, "the bad message in the DLQ", func() bool { return queueDepthOf(t, ch, c.dlq()) == 1 })
}

// The consumer's span continues the trace from the message's traceparent,
// and the database work is under it.
func TestConsumerContinuesTheTrace(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp) // before the pool: its tracer picks up the provider
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) })

	db := newTestDB(t)
	c, ch := newTestConsumer(t, newItemProcessor(db).Handle)
	c.tracer = tp.Tracer("test") // newConsumer ran before the provider was set
	runConsumer(t, c)

	_, send := tp.Tracer("test").Start(context.Background(), "send importer")
	send.End()
	headers := amqp.Table{"traceparent": fmt.Sprintf("00-%s-%s-01", send.SpanContext().TraceID(), send.SpanContext().SpanID())}
	d := itemCreatedDelivery(insertItem(t, db))
	publish(t, ch, c.exchange, amqp.Publishing{Type: d.Type, Body: d.Body, Headers: headers})

	var process sdktrace.ReadOnlySpan
	waitFor(t, "the process span", func() bool {
		for _, sp := range spans.GetSpans().Snapshots() {
			if sp.Name() == "process "+c.queue {
				process = sp
			}
		}
		return process != nil
	})
	if process.Parent().SpanID() != send.SpanContext().SpanID() {
		t.Errorf("process span's parent = %s, want the send span %s", process.Parent().SpanID(), send.SpanContext().SpanID())
	}
	var update bool
	for _, sp := range spans.GetSpans().Snapshots() {
		if sp.Name() == "query UPDATE" && sp.Parent().SpanID() == process.SpanContext().SpanID() {
			update = true
		}
	}
	if !update {
		t.Error("no query UPDATE span under the process span")
	}
}

// A transient failure is retried after a backoff; the redelivery carries
// how many times the message was handed out before.
func TestConsumerRetriesTransientFailures(t *testing.T) {
	counts := make(chan int64, 2)
	c, ch := newTestConsumer(t, func(_ context.Context, d amqp.Delivery) error {
		counts <- acquiredCount(d)
		if acquiredCount(d) == 0 {
			return fmt.Errorf("database is down")
		}
		return nil
	})
	runConsumer(t, c)

	start := time.Now()
	publish(t, ch, c.exchange, amqp.Publishing{Type: "item.created", Body: []byte(`{"id":"1"}`)})
	for want := range int64(2) {
		select {
		case got := <-counts:
			if got != want {
				t.Fatalf("delivery %d has x-acquired-count %d, want %d", want+1, got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no delivery %d", want+1)
		}
	}
	if waited := time.Since(start); waited < retryDelay(1) {
		t.Errorf("retried after %v, want a backoff of at least %v", waited, retryDelay(1))
	}
	waitFor(t, "the queue to be empty", func() bool { return queueDepthOf(t, ch, c.queue) == 0 })
}

// A message that keeps failing is dead-lettered after maxDeliveries attempts,
// not retried forever.
func TestConsumerGivesUpAfterMaxDeliveries(t *testing.T) {
	attempts := make(chan struct{}, 10)
	c, ch := newTestConsumer(t, func(context.Context, amqp.Delivery) error {
		attempts <- struct{}{}
		return fmt.Errorf("database is down")
	})
	c.maxDeliveries = 2
	runConsumer(t, c)

	publish(t, ch, c.exchange, amqp.Publishing{Type: "item.created", Body: []byte(`{"id":"1"}`)})
	waitFor(t, "the message in the DLQ", func() bool { return queueDepthOf(t, ch, c.dlq()) == 1 })
	if n := len(attempts); n != 2 {
		t.Errorf("handled %d times, want 2", n)
	}
}
