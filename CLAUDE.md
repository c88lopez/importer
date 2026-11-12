# CLAUDE.md

Three Go services in one module (`github.com/vandelay-labs/importer`). README.md is the detailed reference (endpoints, env vars, rate limiting, outbox, metrics, release flow); `cmd/worker/README.md` covers the worker and `docs/endpoint-flows.md` traces each endpoint through the code.

## Commands

There are no local copies of Postgres, RabbitMQ or Redis. Local runs and tests use the cluster's, through port-forwards, with credentials read from Secrets in the `data` namespace by the Makefile (so `kubectl` must point at the `docker-desktop` context).

```sh
make dev-setup     # once: importer_test database + importer-test vhost in the cluster
make tunnel        # keep running in its own terminal: Postgres :5433, RabbitMQ :5673 (UI :15673), Redis :6380
make test          # go test -p 1 ./... against importer_test, the importer-test vhost and Redis DB 1
make test GOFLAGS=-run=TestConsumerGivesUpAfterMaxDeliveries   # one test (make passes GOFLAGS through to go test)
make cover         # coverage.out, generated gqlgen code excluded
make run-api       # :8080, metrics :9464
make run-graphql   # :8081 (GraphiQL), metrics :9466
make run-worker    # probes and metrics :9465
make generate      # after editing internal/gql/schema.graphqls; CI fails if generated code is stale
make migration NAME=add_x   # new goose file in migrations/
go vet ./...       # CI runs this too; there is no separate linter
```

- Tests that need Postgres, RabbitMQ or Redis **skip** when `TEST_DATABASE_URL` / `TEST_RABBITMQ_URL` / `TEST_REDIS_URL` are unset, so a plain `go test ./...` passes while skipping most integration coverage. Use `make test` (or `-v` to see skips).
- Always `-p 1`: packages share one test database, and `dbtest.New` truncates `items` and `outbox`.
- `make run-*` and `make migrate-local` hit the **real** `app` database and `/` vhost, the same data the cluster serves, and the cluster's worker will process locally created items. Don't run `migrate-local` with an unreleased migration unless asked; the normal path is the PreSync migration Job on deploy.
- CI (`.github/workflows/build.yml`) runs vet, the gqlgen staleness check and `make cover` against throwaway containers, then builds one image per service from the same `Dockerfile` (`APP` build arg). Commits touching only `k8s/`, `httpRequests/` or `*.md` skip CI.

## Definition of done

A change isn't done until all of these pass, and the results are reported (with output if anything fails):

1. `gofmt -l .` prints nothing (no unformatted files).
2. `go vet ./...` is clean. This is the lint CI runs; there is no golangci-lint config.
3. `make test` passes with `make tunnel` running, so the Postgres, RabbitMQ and Redis tests actually run instead of skipping. If the tunnel or cluster isn't reachable, say so instead of reporting a plain `go test ./...` as passing.
4. Any extra steps in the `CLAUDE.md` of a directory the change touches (`internal/gql/`, `migrations/`, `internal/worker/`) are done too.

## Reviewing a branch or PR

When asked to review a branch or PR, spawn the reviewer agents in `.claude/agents/` in parallel (one message, several Agent calls) instead of reviewing inline. Pick them from `git diff --stat main...HEAD` (or the PR's files):

- `go-pr-reviewer` when any `.go` file changed.
- `graphql-reviewer` when `internal/gql/schema.graphqls` or `gqlgen.yml` changed.
- `migration-reviewer` when anything in `migrations/` changed.

Reviewers don't edit files. Combine their reports into one review, most severe first, and resolve any question one reviewer leaves open for another (e.g. "check the handler defaults this") by reading the code yourself. Reviewers don't run `make test`, so say whether the definition-of-done checks were run.

## Architecture

- **importer-api** (REST, stdlib `net/http`) and **importer-graphql** (gqlgen) serve the same items from the same table. Both are built by `internal/app.ItemsService` (`internal/app/items.go`), which owns the store, outbox relay, Redis rate limiter, `/readyz` and the API + metrics servers; each `cmd/*/main.go` only adds its router. `internal/app.Run` is the shared graceful shutdown (readiness flips false, servers drain, cleanup steps run, all within `ShutdownTimeout`, which must fit the Deployments' 20s grace period).
- **items.Store** (`internal/items`) is the only code that talks to Postgres, used by all three services. Creating an item writes the item and an `outbox` row in one transaction.
- **Outbox relay** (`internal/outbox`) runs in every API/GraphQL pod, claims rows with `FOR UPDATE SKIP LOCKED`, publishes `mandatory` with confirms to the `importer` topic exchange, and deletes the row only after the confirm. Delivery is at-least-once: consumers must be idempotent.
- **importer-worker** (`internal/worker`) consumes `item.created` from a quorum queue, marks the item `done` via `Store.MarkDone`, and dead-letters after a retry limit it counts itself. Details in `internal/worker/CLAUDE.md`.
- `internal/events` is the RabbitMQ contract (exchange, routing keys, `traceparent` header); `internal/telemetry` sets up OTel tracing, Prometheus metrics and slog the same way in each binary. `telemetry.Setup` must run before building the service and router, which read the global MeterProvider.
- `internal/api` middleware and health checks are reused by importer-graphql. The rate limit is Redis-backed, per client IP (connection address; `X-Forwarded-For` is ignored on purpose), fails open, and is off without `REDIS_URL`.
- `/readyz` checks dependencies; `/healthz` checks only the process. `CRITICAL_DEPENDENCIES` (default `postgres,rabbitmq`) decides which failures make the pod not ready; Redis only degrades it.

## GraphQL

Schema first, with generated code committed. The full rules are in `internal/gql/CLAUDE.md`.

## Deploy and release

Argo CD deploys `k8s/` from `main`. Releasing, rolling back, Secrets and the Postgres connection budget are in the `deploy-release` skill (`.claude/skills/deploy-release/SKILL.md`).
