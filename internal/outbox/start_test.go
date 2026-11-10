package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/vandelay-labs/importer/internal/database"
)

// With RabbitMQ unreachable the relay sits in its reconnect backoff; stop
// must still end it right away. Neither Postgres nor RabbitMQ is needed.
func TestStartStop(t *testing.T) {
	db, err := database.NewPool(context.Background(), "postgres://u:p@127.0.0.1:1/db") // never connects
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	relay, err := NewRelay(db, "amqp://u:p@127.0.0.1:1/")
	if err != nil {
		t.Fatal(err)
	}

	stop := relay.Start()
	time.Sleep(50 * time.Millisecond) // let it fail to dial and start waiting
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
}
