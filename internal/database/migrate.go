package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path"
	"syscall"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/vandelay-labs/importer/migrations"
)

const migrateUsage = `usage: importer-api migrate <command>

  up            apply all pending migrations
  down          roll back the most recent migration
  status        list migrations and whether each is applied
  create NAME   add an empty SQL migration to ./migrations (no database needed)`

// MigrateMain runs `importer-api migrate ...` and returns the process exit
// code. SIGTERM (the Job being deleted) cancels the context, so goose stops
// and releases its lock.
func MigrateMain(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runMigrate(ctx, os.Getenv("DATABASE_URL"), args); err != nil {
		slog.Error("migrate failed", "err", err)
		return 1
	}
	return 0
}

// runMigrate handles `importer-api migrate ...`. It runs instead of the server
// (in the k8s Job, or via make migrate-local) and exits non-zero on failure,
// so a failed migration fails the Job and stops the deploy.
func runMigrate(ctx context.Context, databaseURL string, args []string) error {
	if len(args) == 0 {
		return errors.New(migrateUsage)
	}

	if args[0] == "create" {
		if len(args) != 2 {
			return errors.New(migrateUsage)
		}
		// Sequential versions (00002_...) instead of timestamps, so two branches
		// adding a migration collide on the file name and git flags it.
		goose.SetSequential(true)
		return goose.Create(nil, "migrations", args[1], "sql")
	}

	if databaseURL == "" {
		return errors.New("DATABASE_URL is not set")
	}
	provider, closeDB, err := NewMigrationProvider(databaseURL)
	if err != nil {
		return err
	}
	defer closeDB()

	switch args[0] {
	case "up":
		results, err := provider.Up(ctx)
		if err != nil {
			return err
		}
		logMigrationResults(results...)
		version, err := provider.GetDBVersion(ctx)
		if err != nil {
			return err
		}
		slog.Info("database is up to date", "version", version, "applied", len(results))
		return nil

	case "down":
		result, err := provider.Down(ctx)
		if err != nil {
			return err
		}
		logMigrationResults(result)
		return nil

	case "status":
		statuses, err := provider.Status(ctx)
		if err != nil {
			return err
		}
		for _, s := range statuses {
			attrs := []any{"version", s.Source.Version, "file", path.Base(s.Source.Path), "state", s.State}
			if !s.AppliedAt.IsZero() {
				attrs = append(attrs, "appliedAt", s.AppliedAt)
			}
			slog.Info("migration", attrs...)
		}
		return nil

	default:
		return fmt.Errorf("unknown migrate command %q\n\n%s", args[0], migrateUsage)
	}
}

// NewMigrationProvider opens the database and builds a goose provider over the
// embedded migrations. The session locker takes a Postgres advisory lock, so
// two runs at once (say, the Job and someone's laptop) wait for each other
// instead of applying the same migration twice.
func NewMigrationProvider(databaseURL string) (*goose.Provider, func(), error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, nil, err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS, goose.WithSessionLocker(locker))
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return provider, func() { _ = provider.Close() }, nil
}

func logMigrationResults(results ...*goose.MigrationResult) {
	for _, r := range results {
		slog.Info("migration applied",
			"version", r.Source.Version,
			"file", path.Base(r.Source.Path),
			"direction", r.Direction,
			"duration", r.Duration.String(),
		)
	}
}
