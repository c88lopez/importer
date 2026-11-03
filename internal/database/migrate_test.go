package database

import (
	"context"
	"os"
	"testing"
)

// Versions must run 1, 2, 3... with no gaps or duplicates. Catches two
// branches that each added "00002_..." and were both merged.
func TestMigrationVersionsAreSequential(t *testing.T) {
	provider, closeDB, err := NewMigrationProvider("postgres://unused") // never connects
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()

	sources := provider.ListSources()
	if len(sources) == 0 {
		t.Fatal("no migrations embedded")
	}
	for i, s := range sources {
		if want := int64(i + 1); s.Version != want {
			t.Errorf("migration %s has version %d, want %d", s.Path, s.Version, want)
		}
	}
}

// Applies every migration, rolls them all back, then applies them again, so a
// broken Down section fails here instead of in an incident. Needs a throwaway
// database: TEST_DATABASE_URL=postgres://... go test ./...
func TestMigrationsUpDownUp(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	provider, closeDB, err := NewMigrationProvider(url)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()

	ctx := context.Background()
	steps := []struct {
		name string
		run  func() error
	}{
		{"up", func() error { _, err := provider.Up(ctx); return err }},
		{"down to 0", func() error { _, err := provider.DownTo(ctx, 0); return err }},
		{"up again", func() error { _, err := provider.Up(ctx); return err }},
	}
	for _, step := range steps {
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
	}

	if pending, err := provider.HasPending(ctx); err != nil || pending {
		t.Fatalf("HasPending = %v, %v; want false, nil", pending, err)
	}
}
