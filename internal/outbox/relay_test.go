package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/vandelay-labs/importer/internal/database/dbtest"
)

// newTestRelay returns a relay on an empty test database, publishing to an
// exchange of its own on the RabbitMQ at TEST_RABBITMQ_URL (`make test` uses
// the cluster's importer-test vhost).
func newTestRelay(t *testing.T) (*Relay, *pgxpool.Pool, *amqp.Channel) {
	t.Helper()
	url := os.Getenv("TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("TEST_RABBITMQ_URL not set")
	}
	db := dbtest.New(t)
	relay, err := NewRelay(db, url)
	if err != nil {
		t.Fatal(err)
	}
	relay.exchange = fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano())
	relay.interval = 50 * time.Millisecond

	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.ExchangeDeclare(relay.exchange, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.ExchangeDelete(relay.exchange, false, false) })
	return relay, db, ch
}

// bindQueue plays the consumer: a temporary queue bound to item.created.
func bindQueue(t *testing.T, ch *amqp.Channel, exchange string) <-chan amqp.Delivery {
	t.Helper()
	q, err := ch.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.QueueBind(q.Name, "item.created", exchange, false, nil); err != nil {
		t.Fatal(err)
	}
	deliveries, err := ch.Consume(q.Name, "", true, true, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return deliveries
}

// event is what the tests publish: shaped like an item.created body.
type event struct {
	ID int64 `json:"id,string"`
}

// write commits one event to the outbox, as a store would with its change.
func write(t *testing.T, ctx context.Context, db *pgxpool.Pool, r *Relay, id int64) {
	t.Helper()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := Write(ctx, tx, "item.created", event{ID: id}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r.Notify()
}

func runRelay(t *testing.T, relay *Relay) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		relay.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
}

func outboxCount(t *testing.T, db *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), "SELECT count(*) FROM outbox").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func receive(t *testing.T, deliveries <-chan amqp.Delivery) amqp.Delivery {
	t.Helper()
	select {
	case d := <-deliveries:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("no message within 5s")
		return amqp.Delivery{}
	}
}

func TestRelayPublishesAndDeletes(t *testing.T) {
	relay, db, ch := newTestRelay(t)
	deliveries := bindQueue(t, ch, relay.exchange)
	runRelay(t, relay)

	write(t, context.Background(), db, relay, 1)
	d := receive(t, deliveries)

	var got event
	if err := json.Unmarshal(d.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != 1 || d.RoutingKey != "item.created" || d.Type != "item.created" ||
		d.ContentType != "application/json" || d.DeliveryMode != amqp.Persistent {
		t.Errorf("got %s %s %s mode %d, body %+v; want a persistent JSON item.created for item %d",
			d.RoutingKey, d.Type, d.ContentType, d.DeliveryMode, got, 1)
	}

	// The delete commits just after the confirm.
	for deadline := time.Now().Add(2 * time.Second); outboxCount(t, db) > 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if n := outboxCount(t, db); n != 0 {
		t.Errorf("outbox has %d rows after publishing, want 0", n)
	}
}

// With no queue bound, RabbitMQ would drop the message; mandatory makes it
// come back, and the event waits in the outbox until a consumer shows up.
func TestRelayKeepsUnroutableEvents(t *testing.T) {
	relay, db, ch := newTestRelay(t)
	runRelay(t, relay)

	write(t, context.Background(), db, relay, 1)
	time.Sleep(300 * time.Millisecond) // several polls
	if n := outboxCount(t, db); n != 1 {
		t.Fatalf("outbox has %d rows with no queue bound, want 1", n)
	}

	deliveries := bindQueue(t, ch, relay.exchange)
	var got event
	if err := json.Unmarshal(receive(t, deliveries).Body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != 1 {
		t.Errorf("got event %d, want 1", got.ID)
	}
}

// The publish span is a child of the request that wrote the event, and the
// message carries the publish span's context for the consumer.
func TestPublishJoinsTheRequestTrace(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) })

	relay, db, ch := newTestRelay(t)
	relay.tracer = tp.Tracer("test") // newOutboxRelay ran before the provider was set
	deliveries := bindQueue(t, ch, relay.exchange)

	ctx, request := tp.Tracer("test").Start(context.Background(), "POST /api/items")
	write(t, ctx, db, relay, 1)
	request.End()
	runRelay(t, relay)
	d := receive(t, deliveries)

	// The span ends after the broker's confirm, which can be just after the
	// message reaches the queue.
	var send sdktrace.ReadOnlySpan
	for deadline := time.Now().Add(time.Second); send == nil && time.Now().Before(deadline); {
		for _, sp := range spans.GetSpans().Snapshots() {
			if sp.Name() == "send "+relay.exchange {
				send = sp
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if send == nil {
		t.Fatal("no send span")
	}
	if send.Parent().SpanID() != request.SpanContext().SpanID() {
		t.Errorf("send span's parent = %s, want the request span %s", send.Parent().SpanID(), request.SpanContext().SpanID())
	}

	carrier := propagation.MapCarrier{}
	for k, v := range d.Headers {
		carrier[k], _ = v.(string)
	}
	want := fmt.Sprintf("00-%s-%s-01", send.SpanContext().TraceID(), send.SpanContext().SpanID())
	if got := carrier.Get("traceparent"); got != want {
		t.Errorf("message traceparent = %q, want %q", got, want)
	}
}
