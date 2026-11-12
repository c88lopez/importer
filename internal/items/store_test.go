package items

import (
	"context"
	"errors"
	"testing"

	"github.com/vandelay-labs/importer/internal/database/dbtest"
)

// newTestStore returns a store on an empty, migrated test database.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(dbtest.New(t))
}

func TestStoreCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	latex, err := s.Create(ctx, "latex", "import", PriorityLow)
	if err != nil {
		t.Fatal(err)
	}
	chips, err := s.Create(ctx, "potato chips", "export", PriorityHigh)
	if err != nil {
		t.Fatal(err)
	}
	if latex.ID == 0 || latex.CreatedAt.IsZero() {
		t.Fatalf("create didn't return generated columns: %+v", latex)
	}

	got, err := s.Get(ctx, latex.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != latex || got.Priority != PriorityLow {
		t.Errorf("get = %+v, want %+v with priority low", got, latex)
	}

	items, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0] != latex || items[1] != chips || items[1].Priority != PriorityHigh {
		t.Errorf("list = %+v, want latex (low) then chips (high)", items)
	}

	if err := s.Delete(ctx, latex.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, latex.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete: err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, latex.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice: err = %v, want ErrNotFound", err)
	}
}

// The event is committed with the item, and carries the item as JSON.
func TestCreateWritesOutbox(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	notified := 0
	s.OutboxWritten = func() { notified++ }

	it, err := s.Create(ctx, "latex", "import", "normal")
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != "pending" || it.ProcessedAt != nil {
		t.Errorf("new item = %+v, want status pending and no processedAt", it)
	}
	if notified != 1 {
		t.Errorf("outboxWritten called %d times, want 1", notified)
	}

	var key string
	var payload Item
	if err := s.db.QueryRow(ctx, "SELECT routing_key, payload FROM outbox").Scan(&key, &payload); err != nil {
		t.Fatal(err)
	}
	if key != "item.created" || payload.ID != it.ID || payload.Name != "latex" {
		t.Errorf("outbox row = %s %+v, want item.created for item %d", key, payload, it.ID)
	}
}

// A failed insert must not leave an event behind.
func TestCreateFailureWritesNoOutbox(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "x", "smuggle", "normal"); err == nil {
		t.Fatal("want a constraint violation")
	}
	var n int
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM outbox").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("outbox has %d rows after a failed create, want 0", n)
	}
}

// The handlers validate kind, but the CHECK constraint is the last line of
// defense; a violation must be a plain error (500), not ErrNotFound (404).
func TestStoreRejectsBadKind(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Create(context.Background(), "x", "smuggle", "normal")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want a constraint violation", err)
	}
}

// Like kind, priority is validated by the handlers and by a CHECK constraint.
func TestStoreRejectsBadPriority(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Create(context.Background(), "x", "import", "urgent")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want a constraint violation", err)
	}
}

// Every priority the code accepts passes items_priority_check, so a new one
// can't ship without its migration.
func TestStoreAcceptsEveryPriority(t *testing.T) {
	s := newTestStore(t)
	for _, p := range Priorities {
		if _, err := s.Create(context.Background(), "x", "import", p); err != nil {
			t.Errorf("priority %q: %v", p, err)
		}
	}
}

// The worker's update: once, then a no-op for redeliveries and deleted items.
func TestMarkDone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	it, err := s.Create(ctx, "latex", "import", "normal")
	if err != nil {
		t.Fatal(err)
	}

	if done, err := s.MarkDone(ctx, it.ID); err != nil || !done {
		t.Fatalf("MarkDone = %v, %v; want true, nil", done, err)
	}
	got, err := s.Get(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" || got.ProcessedAt == nil {
		t.Fatalf("after MarkDone: %+v, want status done with processedAt", got)
	}

	if done, err := s.MarkDone(ctx, it.ID); err != nil || done {
		t.Errorf("MarkDone again = %v, %v; want false, nil", done, err)
	}
	if again, _ := s.Get(ctx, it.ID); !again.ProcessedAt.Equal(*got.ProcessedAt) {
		t.Errorf("a second MarkDone changed processedAt from %v to %v", got.ProcessedAt, again.ProcessedAt)
	}
	if done, err := s.MarkDone(ctx, 999); err != nil || done {
		t.Errorf("MarkDone(unknown id) = %v, %v; want false, nil", done, err)
	}
}
