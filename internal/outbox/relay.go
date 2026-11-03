package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/vandelay-labs/importer/internal/events"
)

type message struct {
	ID           int64
	RoutingKey   string
	Payload      []byte
	TraceHeaders map[string]string
	CreatedAt    time.Time
}

// errUnroutable means RabbitMQ accepted a message but had no queue to put it
// in: no consumer has declared its queue yet. The message stays in the outbox.
var errUnroutable = errors.New("no queue is bound for this routing key")

// Relay publishes outbox rows to RabbitMQ and deletes them once the
// broker confirms it has them. Every API pod runs one; FOR UPDATE SKIP
// LOCKED gives each row to a single pod at a time.
//
// Delivery is at least once: if a pod dies after the broker confirms but
// before the delete commits, the row is published again. Consumers must be
// idempotent.
type Relay struct {
	db        *pgxpool.Pool
	url       string
	exchange  string
	batchSize int
	interval  time.Duration // poll for rows that missed a Notify (e.g. written by another pod)

	nudge      chan struct{}
	tracer     trace.Tracer
	published  metric.Int64Counter
	unroutable bool // logged once per stretch, not on every poll
}

// NewRelay returns a relay publishing to events.Exchange. Call Run to start it.
func NewRelay(db *pgxpool.Pool, url string) (*Relay, error) {
	meter := otel.Meter("importer-api/outbox")
	published, err := meter.Int64Counter("messaging.client.sent.messages",
		metric.WithDescription("Messages published to RabbitMQ and confirmed by the broker."))
	if err != nil {
		return nil, err
	}
	r := &Relay{
		db:        db,
		url:       url,
		exchange:  events.Exchange,
		batchSize: 100,
		interval:  time.Second,
		nudge:     make(chan struct{}, 1),
		tracer:    otel.Tracer("importer-api/outbox"),
		published: published,
	}
	return r, r.observeBacklog(meter)
}

// observeBacklog reports how many events wait in the outbox and for how long.
// A growing age means messages aren't leaving: RabbitMQ is down, or nobody
// has declared a queue for them.
func (r *Relay) observeBacklog(meter metric.Meter) error {
	pending, err := meter.Int64ObservableGauge("outbox.pending",
		metric.WithDescription("Events in the outbox, not yet published."), metric.WithUnit("{message}"))
	if err != nil {
		return err
	}
	age, err := meter.Float64ObservableGauge("outbox.oldest.age",
		metric.WithDescription("Age of the oldest unpublished event."), metric.WithUnit("s"))
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var n int64
		var seconds float64
		err := r.db.QueryRow(ctx,
			"SELECT count(*), coalesce(extract(epoch FROM now() - min(created_at)), 0)::float8 FROM outbox").
			Scan(&n, &seconds)
		if err != nil {
			return err
		}
		o.ObserveInt64(pending, n)
		o.ObserveFloat64(age, seconds)
		return nil
	}, pending, age)
	return err
}

// Notify wakes the relay up. It never blocks.
func (r *Relay) Notify() {
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

// Run publishes until ctx is cancelled, reconnecting to RabbitMQ with
// backoff when the connection drops. Events written meanwhile wait in the
// outbox, so a RabbitMQ outage delays them but loses nothing.
func (r *Relay) Run(ctx context.Context) {
	backoff := time.Second
	for {
		err := r.session(ctx, func() { backoff = time.Second })
		if ctx.Err() != nil {
			return
		}
		slog.Warn("outbox relay disconnected from rabbitmq, retrying", "err", err, "in", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// session runs one RabbitMQ connection until it fails or ctx is cancelled.
func (r *Relay) session(ctx context.Context, connected func()) error {
	hostname, _ := os.Hostname()
	conn, err := amqp.DialConfig(r.url, amqp.Config{
		// Shows up in the management UI's connection list.
		Properties: amqp.Table{"connection_name": "importer-api outbox relay (" + hostname + ")"},
	})
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	if err := ch.ExchangeDeclare(r.exchange, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
		return err
	}
	// Publisher confirms: the broker acks each message once it has it, so a
	// row is only deleted after RabbitMQ is responsible for the message.
	if err := ch.Confirm(false); err != nil {
		return err
	}
	returns := ch.NotifyReturn(make(chan amqp.Return, 1))
	closed := conn.NotifyClose(make(chan *amqp.Error, 1))
	connected()
	slog.Info("outbox relay connected to rabbitmq", "exchange", r.exchange)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		for {
			n, err := r.publishBatch(ctx, ch, returns)
			if err != nil {
				return err
			}
			if n < r.batchSize {
				break // drained, or stopped at an unroutable message
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-closed:
			return err
		case <-ticker.C:
		case <-r.nudge:
		}
	}
}

// publishBatch publishes up to batchSize rows in order and deletes the ones
// the broker confirmed, in one transaction. It returns how many it published.
func (r *Relay) publishBatch(ctx context.Context, ch *amqp.Channel, returns <-chan amqp.Return) (int, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	rows, _ := tx.Query(ctx, `
		SELECT id, routing_key, payload, trace_headers, created_at
		FROM outbox ORDER BY id LIMIT $1
		FOR UPDATE SKIP LOCKED`, r.batchSize)
	msgs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[message])
	if err != nil || len(msgs) == 0 {
		return 0, err
	}

	var done []int64
	for _, m := range msgs {
		err := r.publish(ctx, ch, returns, m)
		if errors.Is(err, errUnroutable) {
			if !r.unroutable {
				slog.Warn("outbox event is unroutable, keeping it until a queue is bound",
					"exchange", r.exchange, "routing_key", m.RoutingKey, "outbox_id", m.ID)
			}
			r.unroutable = true
			break // keep the order: retry from this one on the next poll
		}
		if err != nil {
			return 0, err
		}
		r.unroutable = false
		done = append(done, m.ID)
	}

	if len(done) > 0 {
		if _, err := tx.Exec(ctx, "DELETE FROM outbox WHERE id = ANY($1)", done); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(done), nil
}

// publish sends one message and waits for the broker's confirm. The span is
// a child of the request that wrote the event, and its context travels in
// the message headers, so the consumer's span is a child of this one.
func (r *Relay) publish(ctx context.Context, ch *amqp.Channel, returns <-chan amqp.Return, m message) error {
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(m.TraceHeaders))
	ctx, span := r.tracer.Start(ctx, "send "+r.exchange,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "rabbitmq"),
			attribute.String("messaging.operation.type", "send"),
			attribute.String("messaging.destination.name", r.exchange),
			attribute.String("messaging.rabbitmq.destination.routing_key", m.RoutingKey),
			attribute.String("messaging.message.id", strconv.FormatInt(m.ID, 10)),
			attribute.Float64("outbox.wait_seconds", time.Since(m.CreatedAt).Seconds()),
		))
	defer span.End()

	err := r.publishConfirmed(ctx, ch, returns, m)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	r.published.Add(ctx, 1, metric.WithAttributes(
		attribute.String("messaging.destination.name", r.exchange),
		attribute.String("messaging.rabbitmq.destination.routing_key", m.RoutingKey),
	))
	return nil
}

func (r *Relay) publishConfirmed(ctx context.Context, ch *amqp.Channel, returns <-chan amqp.Return, m message) error {
	headers := amqp.Table{}
	otel.GetTextMapPropagator().Inject(ctx, events.Headers(headers))

	msgID := strconv.FormatInt(m.ID, 10)
	confirm, err := ch.PublishWithDeferredConfirmWithContext(ctx, r.exchange, m.RoutingKey,
		true,  // mandatory: return the message if no queue is bound, instead of dropping it
		false, // immediate (unsupported by RabbitMQ)
		amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent, // survives a broker restart, if the queue is durable
			MessageId:    msgID,
			Type:         m.RoutingKey,
			Timestamp:    m.CreatedAt,
			AppId:        "importer-api",
			Headers:      headers,
			Body:         m.Payload,
		})
	if err != nil {
		return err
	}
	ok, err := confirm.WaitContext(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("broker nacked message %s", msgID)
	}
	// The broker sends basic.return before the ack, and the library delivers
	// it to the buffered channel before resolving the confirm.
	select {
	case ret := <-returns:
		if ret.MessageId == msgID {
			return errUnroutable
		}
	default:
	}
	return nil
}
