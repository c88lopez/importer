package worker

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/vandelay-labs/importer/internal/database/dbtest"
	"github.com/vandelay-labs/importer/internal/items"
)

// newTestDB returns a pool on an empty test database with the real schema
// (importer-api's migrations), so the worker is tested against the table it
// actually updates in production.
func newTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return dbtest.New(t)
}

func insertItem(t *testing.T, db *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(context.Background(),
		"INSERT INTO items (name, kind) VALUES ('latex', 'import') RETURNING id").Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func itemStatus(t *testing.T, db *pgxpool.Pool, id int64) (string, *time.Time) {
	t.Helper()
	var status string
	var processedAt *time.Time
	err := db.QueryRow(context.Background(), "SELECT status, processed_at FROM items WHERE id = $1", id).
		Scan(&status, &processedAt)
	if err != nil {
		t.Fatal(err)
	}
	return status, processedAt
}

func itemCreatedDelivery(id int64) amqp.Delivery {
	return amqp.Delivery{
		Type: "item.created",
		Body: []byte(`{"id":"` + strconv.FormatInt(id, 10) + `","name":"latex","kind":"import","status":"pending"}`),
	}
}

func TestHandleMarksItemDone(t *testing.T) {
	db := newTestDB(t)
	p := NewItemProcessor(items.NewStore(db))
	id := insertItem(t, db)

	if err := p.Handle(context.Background(), itemCreatedDelivery(id)); err != nil {
		t.Fatal(err)
	}
	status, processedAt := itemStatus(t, db, id)
	if status != "done" || processedAt == nil {
		t.Fatalf("status = %s, processed_at = %v; want done with a time", status, processedAt)
	}

	// At-least-once delivery: the same event again changes nothing.
	if err := p.Handle(context.Background(), itemCreatedDelivery(id)); err != nil {
		t.Fatal(err)
	}
	if _, again := itemStatus(t, db, id); !again.Equal(*processedAt) {
		t.Errorf("redelivery changed processed_at from %v to %v", processedAt, again)
	}
}

// The item can be deleted through the API before the worker gets to it.
func TestHandleDeletedItemIsAcked(t *testing.T) {
	p := NewItemProcessor(items.NewStore(newTestDB(t)))
	if err := p.Handle(context.Background(), itemCreatedDelivery(999)); err != nil {
		t.Errorf("err = %v, want nil: there's nothing left to do", err)
	}
}

// A message that can never be processed is permanent, so it's dead-lettered
// instead of retried. It's checked before touching the database.
func TestHandleRejectsBadMessages(t *testing.T) {
	p := NewItemProcessor(nil)
	for name, d := range map[string]amqp.Delivery{
		"wrong type": {Type: "item.deleted", Body: []byte(`{"id":"1"}`)},
		"not json":   {Type: "item.created", Body: []byte(`{`)},
		"numeric id": {Type: "item.created", Body: []byte(`{"id":1}`)},
		"no id":      {Type: "item.created", Body: []byte(`{"name":"latex"}`)},
	} {
		if err := p.Handle(context.Background(), d); !errors.Is(err, errPermanent) {
			t.Errorf("%s: err = %v, want a permanent error", name, err)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	for attempt, want := range map[int64]time.Duration{1: time.Second, 2: 2 * time.Second, 5: 16 * time.Second, 6: 30 * time.Second, 50: 30 * time.Second} {
		if got := retryDelay(attempt); got != want {
			t.Errorf("retryDelay(%d) = %v, want %v", attempt, got, want)
		}
	}
}
