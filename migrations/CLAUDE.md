# Migrations

goose SQL files, embedded in the importer-api binary (`importer-api migrate ...`). Create one with `make migration NAME=add_x` from the repo root.

- Tests check versions are sequential and, with a test DB, run every migration up, down and up.
- Old pods run against the new schema during a rollout, so migrations must be backward compatible (add a column, ship code, drop later).
- Applied migrations are never edited, and the cluster only rolls forward.
- Migrations reach the cluster through the PreSync migration Job on deploy. `make migrate-local` hits the **real** `app` database: don't run it with an unreleased migration unless asked.
