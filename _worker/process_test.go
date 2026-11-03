package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
)

// itemsContract is the part of importer-api's items table the worker relies
// on (its migrations 00001 and 00002). importer-api owns the real schema; if
// it changes these columns, this has to change too, and so may the worker.
const itemsContract = `
CREATE TABLE IF NOT EXISTS items (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name         TEXT NOT NULL,
    kind         TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done')),
    processed_at TIMESTAMPTZ
);
TRUNCATE items RESTART IDENTITY;`

// newTestDB connects to TEST_DATABASE_URL, a throwaway database
// (`make test` uses the cluster's importer_worker_test, see make dev-setup),
// and empties the items table.
func newTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := newDBPool(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Exec(context.Background(), itemsContract); err != nil {
		t.Fatal(err)
	}
	return db
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
	p := newItemProcessor(db)
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
	p := newItemProcessor(newTestDB(t))
	if err := p.Handle(context.Background(), itemCreatedDelivery(999)); err != nil {
		t.Errorf("err = %v, want nil: there's nothing left to do", err)
	}
}

// A message that can never be processed is permanent, so it's dead-lettered
// instead of retried. It's checked before touching the database.
func TestHandleRejectsBadMessages(t *testing.T) {
	p := newItemProcessor(nil)
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
