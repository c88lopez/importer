# importer

Two Go services, for learning how apps get from source code to running in Kubernetes:

- **importer-api**: a REST API for items (standard library HTTP). Creating an item announces it on RabbitMQ.
- **importer-worker**: consumes those events and processes the items. See [cmd/worker/README.md](cmd/worker/README.md).

They share a Postgres database, its schema, the event contract and a version, so they live in one repo and one Go module:

```
cmd/api/              importer-api: wiring (config, servers, shutdown); `importer-api migrate` runs migrations
cmd/worker/           importer-worker: wiring
internal/api/         HTTP routes, health checks, middleware
internal/worker/      RabbitMQ consumer and the item processor
internal/items/       the Item model and its Postgres store, used by both
internal/outbox/      transactional outbox and its relay to RabbitMQ
internal/events/      the RabbitMQ contract: exchange, routing keys, trace headers
internal/database/    connection pool and migrations (dbtest: a test database for tests)
internal/telemetry/   tracing, metrics and logs, set up the same way in both
migrations/           the schema, embedded in importer-api
k8s/                  the "importer" namespace: kustomization.yaml (what a deploy applies, and the version), api/, worker/, jobs/ (migrations), dashboards/, dev/
```

`internal/` packages can only be imported from this module, so the split stays private to the repo.

## The path from code to cluster

```
cmd/api ──go build──▶ binary ──docker build──▶ image ──Argo CD──▶ Deployment ──▶ ReplicaSet ──▶ Pods
                                                                         Service ──routes traffic──▶ ready Pods
```

1. **Build an image.** The `Dockerfile` compiles the binary in a Go image, then copies only the binary into a small distroless image.
2. **Make the image available to the cluster.** Docker Desktop's Kubernetes uses your local Docker images, so no registry is needed. On a real cluster you would `docker push` to a registry and the nodes would pull from it.
3. **Describe the desired state.** The YAML files in `k8s/` describe what should exist. Argo CD applies them (with `kubectl apply`, in effect) whenever they change on `main`.
4. **Controllers reconcile.** The Deployment controller creates a ReplicaSet, the ReplicaSet creates Pods, the scheduler places each Pod on a node, and the kubelet on that node starts the container.
5. **Probes gate traffic.** A Pod is added to the Service only after `/readyz` passes. If `/healthz` keeps failing, the container is restarted.

## API

| Method | Path              | Description                                    |
| ------ | ----------------- | ---------------------------------------------- |
| GET    | `/`               | Message, version, env, and **which pod answered** |
| GET    | `/healthz`        | Liveness probe (process only)                  |
| GET    | `/readyz`         | Readiness probe + Postgres/Redis/RabbitMQ pings |
| GET    | `/api/items`      | List items                                     |
| POST   | `/api/items`      | Create `{"name": "latex", "kind": "import"}`; announces it on RabbitMQ |
| GET    | `/api/items/{id}` | Get one item                                   |
| DELETE | `/api/items/{id}` | Delete one item                                |

Config comes from env vars: `PORT`, `APP_ENV`, `WELCOME_MESSAGE`, plus the dependency URLs below.

### Dependency checks

`/readyz` pings each configured dependency at the same time, with a 2s budget:

| Env var        | Example                                 |
| -------------- | --------------------------------------- |
| `DATABASE_URL` | `postgres://user:pass@host:5432/db`     |
| `REDIS_URL`    | `redis://:pass@host:6379/0`             |
| `RABBITMQ_URL` | `amqp://user:pass@host:5672/`           |

`CRITICAL_DEPENDENCIES` (comma-separated, default `postgres,rabbitmq`) decides what a failure means:

| Situation                        | HTTP | `status`    | Effect in Kubernetes                    |
| -------------------------------- | ---- | ----------- | --------------------------------------- |
| Everything up                    | 200  | `ready`     | Pod gets traffic                        |
| A **non-critical** dependency down (Redis) | 200  | `degraded`  | Pod still gets traffic; failure is visible in the response |
| A **critical** dependency down (Postgres, RabbitMQ) | 503  | `not ready` | Pod is removed from the Service         |

Setting it to an empty string makes nothing critical. An unknown name, like `postgress`, stops the app at startup, so a typo can't quietly turn off the check.

`DATABASE_URL` and `RABBITMQ_URL` are required: the app won't start without them. If `REDIS_URL` isn't set, its check is reported as `"skipped"` and doesn't fail the endpoint.

```json
{"status":"degraded","checks":{
  "postgres":{"status":"ok","critical":true,"latency":"1.2ms"},
  "redis":{"status":"error","critical":false,"latency":"2ms","error":"dial tcp ...: connection refused"},
  "rabbitmq":{"status":"skipped","critical":true}}}
```

In the cluster, the URLs contain passwords, so they come from the `importer-api-deps` Secret instead of the ConfigMap (`make secrets` creates it).

### Why the checks are in readiness, not liveness

- **Liveness (`/healthz`)** answers "is this process stuck?" When it fails, Kubernetes **restarts** the container. It only checks the process itself, because restarting the pod can't fix a database that's down.
- **Readiness (`/readyz`)** answers "can this pod serve requests right now?" When it fails, Kubernetes **removes the pod from the Service** but leaves it running. Once the dependency comes back, the next probe passes and traffic returns, with no restarts.

Critical vs. non-critical is the same idea one level down: only take the pod out of rotation for something it truly can't work without. If the cache is down, it's usually better to serve slower than not at all.

Try it: point `DATABASE_URL` at a host that doesn't exist, then run `kubectl -n importer get pods -w`. The pods show `0/1 READY` but `RESTARTS` stays at 0, and `curl http://192.168.68.57:30080/` fails because the Service has no ready pods behind it. `kubectl -n importer get endpointslices` shows the pod IPs marked not ready.

## Run locally

There are no local copies of the dependencies: `make run-api`, `make run-worker` and `make test` use the cluster's Postgres and RabbitMQ through `kubectl port-forward`. The Makefile reads the credentials from their Secrets in `default`.

| | Postgres | RabbitMQ |
| --- | --- | --- |
| `make run-api`, `make run-worker`, `make migrate-local` | `app`, the real database | vhost `/`, the real exchange |
| `make test` | `importer_test` | vhost `importer-test` |

The tests truncate tables and create and delete exchanges, so they get their own database and vhost. `k8s/dev/test-database.yaml` declares the database as a CloudNativePG `Database` owned by the `app` user.

```sh
make dev-setup      # once: create importer_test and the importer-test vhost
make tunnel         # in its own terminal: Postgres on 5433, RabbitMQ on 5673 (UI on 15673)
make run-api        # :8080, metrics on :9464
make run-worker     # optional, in another terminal: probes and metrics on :9465
curl -XPOST localhost:8080/api/items -d '{"name":"latex","kind":"import"}'
curl localhost:8080/api/items
make test           # -p 1: packages share the test database
```

Locally created items are real: they're in the same table as the cluster's, and the cluster's importer-worker processes them (unless your local worker gets there first). `make migrate-local` applies migrations to the real database, so a migration you're still writing is live before its release. Prefer releasing: each deploy runs the released image's migrations as a Job.

RabbitMQ's management UI is at http://localhost:15673, with the credentials in the `rabbitmq-default-user` Secret.

CI can't reach the cluster, so it runs the same tests against throwaway Postgres and RabbitMQ containers.

## Database migrations

Schema changes are SQL files in `migrations/`, applied in version order by [goose](https://github.com/pressly/goose). They're embedded in the binary, so each image carries the schema its code expects. goose records what it applied in the `goose_db_version` table.

```sh
make migration NAME=add_items_owner           # creates migrations/00003_add_items_owner.sql
make migrate-local                             # apply pending ones
make migrate-local MIGRATE=status
make migrate-local MIGRATE=down                # roll back the latest one
```

In the cluster, every Argo CD sync first runs `k8s/jobs/migrate.yaml`, a Job that runs `importer-api migrate up` once with the version being deployed (a PreSync hook). If the migration fails, the Job fails, the sync stops, and the old pods keep running; the Job's logs are in Argo CD and on the API dashboard. A Postgres advisory lock stops two runs from applying the same migration at once.

Rules that keep rolling updates safe:

- **Old pods keep running against the new schema** until the rollout finishes, so a migration must work with both versions of the code. To rename or drop a column, add the new one first, ship code that uses it, and remove the old one in a later release.
- **Never edit a migration that has been applied anywhere.** Add a new one instead.
- **Roll forward in the cluster.** `down` is for local work.
- Build indexes on big tables with `CREATE INDEX CONCURRENTLY` and `-- +goose NO TRANSACTION`.

`go test ./...` checks that versions are sequential. With `TEST_DATABASE_URL` set (`make test` and CI set it), it also runs every migration up, down and up again.

## How the image is built

Pushing to GitHub runs `.github/workflows/build.yml`. It tests once, then builds one image per service from the same `Dockerfile`, with the `APP` build argument picking the binary:

```
git push ──▶ GitHub Actions: go vet + go test ──▶ docker build (amd64 + arm64) ──▶ ghcr.io/c88lopez/importer-api
                                                                              └─▶ ghcr.io/c88lopez/importer-worker
                                                                                        │
kubectl apply ──▶ Deployment ──▶ kubelet on the node pulls the image ◀──────────────────┘
```

| You push            | Image tags                    |
| ------------------- | ----------------------------- |
| A commit to `main`  | `main`, `sha-<short>`         |
| A tag `v1.2.3`      | `1.2.3`, `1.2`, `latest`      |
| A pull request      | Built and tested, not pushed  |

Both services share one version: the `newTag` in `k8s/kustomization.yaml`, which pins it exactly. Ship a new version with:

```sh
make release VERSION=1.7.0   # pushes tag v1.7.0
```

That's the whole release:

1. CI builds and pushes both images.
2. Argo CD Image Updater sees the new `1.x.y` tags (it checks every 2 minutes) and commits them as the `newTag`s on `main`, as `argocd-image-updater`.
3. Argo CD syncs that commit: the migration Job first, then both Deployments roll out.

It takes about 2 minutes after CI finishes; `git pull` shows the commit, and the Argo CD UI shows the sync.

To roll back, first tell Image Updater to skip the bad version: add it to `ignoreTags` in `manifests/argocd/importer-image-updater.yaml` in the platform repo and `make argocd-apps` there. Then revert its commit here. Reverting alone doesn't stick: on its next check Image Updater sees the bad tag is still the newest and commits it again. Usually it's simpler to fix forward with the next patch release. Image Updater only moves forward within major version 1: a `2.0.0` needs its rule changed in the platform repo.

## Deploy

The cluster is Docker Desktop on another PC (`192.168.68.57`); `kubectl config current-context` prints `docker-desktop`. Postgres, RabbitMQ and Redis already run in its `default` namespace.

Both services run in the `importer` namespace. Each has its own Secret, with only the connection URLs it uses. Secrets aren't in Git, so they're the one thing you create by hand:

```sh
make secrets        # once: copies the connection URLs from the Secrets in `default`
```

Everything else is deployed by Argo CD (http://192.168.68.57:30081), from `k8s/` on `main`: the `importer` Application in the [platform repo](https://github.com/c88lopez/platform). Any commit that changes `k8s/` (a ConfigMap, a dashboard, a new version) is live within about 3 minutes.

```sh
make status
curl http://192.168.68.57:30080/readyz
```

## Tracing

Each request (except `/healthz` and `/readyz`) gets an OpenTelemetry span named after its route, like `GET /api/items/{id}`. Database work done for the request shows up as child spans, so a trace shows where the time went:

```
GET /api/items           21.30 ms
  pool.acquire             16.44 ms    waiting for a connection from the pool
    connect                  16.10 ms  opening a new one (first requests only)
  query SELECT              4.42 ms    the query, SQL in db.query.text
    prepare SELECT            2.52 ms  first time this connection runs the query
```

Query parameters are not recorded. Database calls outside a request, like the `/readyz` ping, aren't traced. Spans go over OTLP/gRPC to `OTEL_EXPORTER_OTLP_ENDPOINT`; in the cluster that's Alloy, which forwards them to Tempo (see `../monitoring-and-observability/k8s/`). With the variable unset, tracing is off.

Request log lines carry `trace_id` and `span_id`, so in Grafana a log line in Loki links to its trace in Tempo, and a trace links back to its logs.

To send traces from `make run-api` to the cluster:

```sh
kubectl -n monitoring port-forward svc/alloy 4317:4317 &
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317 make run-api
```

## Events and the outbox

Creating an item publishes an `item.created` event, which importer-worker ([cmd/worker](cmd/worker/README.md)) consumes to process the item and set its `status` from `pending` to `done`.

```
POST /api/items ─► one transaction: INSERT items + INSERT outbox
                                                      │
          relay (internal/outbox, in every API pod) ◄───────────┘ polls every 1s, or right after a create
            │ publish with confirms, then DELETE the row
            ▼
  exchange "importer" (topic) ─► routing key item.created ─► queues the consumers declare
```

Publishing right after the commit could lose the event: the pod can die, or RabbitMQ can be down, between the two. The **transactional outbox** avoids that. The event is a row written in the same transaction as the item, so both are saved or neither is. A relay then publishes outbox rows and deletes each one only after RabbitMQ confirms it has the message.

| What happens | Result |
| --- | --- |
| RabbitMQ is down | Creates still work. Events wait in the outbox, and the relay reconnects with backoff and sends them. |
| No queue is bound yet (the worker was never deployed) | Messages are published as `mandatory`, so RabbitMQ returns them instead of dropping them. They stay in the outbox until a queue exists. |
| A pod dies after the confirm but before the delete | The row is published again. Delivery is **at least once**, so consumers must be idempotent. |
| Two pods | `FOR UPDATE SKIP LOCKED` gives each row to one relay at a time. Order across pods isn't guaranteed. |

Messages are persistent JSON (the item as `GET /api/items/{id}` returns it), with `type` set to the routing key, `message_id` set to the outbox id, and a `traceparent` header. The publish shows up as a `send importer` span under the request that created the item, and the worker's spans continue the same trace.

The `00002` migration also queued an `item.created` for every existing item, so they go through the worker too.

## Metrics and dashboard

Each pod serves Prometheus metrics on port 9464 (`/metrics`), separate from the API port so they aren't exposed through the NodePort. `k8s/api/40-podmonitor.yaml` makes Prometheus scrape them every 15s. The main series:

| Metric | What |
| --- | --- |
| `http_server_request_duration_seconds` | Requests by `http_route`, `http_request_method` and `http_response_status_code` (histogram; `_count` for request counts). Probes aren't counted. |
| `db_client_operation_duration_seconds` | Database calls by `pgx_operation_type` (`query`, `acquire`, `connect`, `prepare`) |
| `pgxpool_*` | Connection pool: in use, idle, max |
| `messaging_client_sent_messages_total` | Events published and confirmed, by `messaging_rabbitmq_destination_routing_key` |
| `outbox_pending`, `outbox_oldest_age_seconds` | Events waiting in the outbox, and how long the oldest has waited. A growing age means events aren't leaving. |
| `go_*`, `process_*` | Go runtime and process |

Grafana has one dashboard per service, in the *Services* folder:

- **Importer API** shows requests, errors, latency, database calls, the connection pool, pod CPU and memory against their limits, restarts, version history, recent traces and logs. Deploy markers come from each pod's `server starting` log line.
- **Importer Worker** shows messages by outcome (done, requeued, dead-lettered), processing time, the queue, its consumers, the dead-letter queue, the API's outbox backlog, recent traces, the database, pods, versions and logs. See [cmd/worker/README.md](cmd/worker/README.md).

Their sources are `k8s/dashboards/*.json`; the kustomization makes one ConfigMap of each. To change one, edit in Grafana, export the JSON (Share > Export), save it over the file and push it to `main`; Argo CD loads it, with no release needed.

```sh
curl -s localhost:9464/metrics | grep http_server_request_duration_seconds_count   # with make run-api
```

## Experiments to try

**See load balancing.** Run this a few times and watch the `pod` field change:
```sh
for i in $(seq 6); do curl -s http://192.168.68.57:30080/ | grep -o '"pod":"[^"]*"'; done
```

**See that state is shared.** POST an item, then list items a few times. Requests hit different pods (check `/`), but every pod reads the same Postgres table, so the list is always the same. Before 1.2.0 items were kept in each pod's memory, and half the requests showed an empty list.

**Self-healing.** Delete a pod and watch a new one replace it:
```sh
kubectl -n importer get pods -w        # in one terminal
kubectl -n importer delete pod <name>  # in another
```

**Scale.**
```sh
kubectl -n importer scale deployment/importer-api --replicas=4
```

**Change config.** Edit `WELCOME_MESSAGE` in `k8s/api/10-configmap.yaml` and push it to `main`. Once Argo CD has synced it:
```sh
make restart
```
Pods read env vars only at startup, so they need a restart to pick up the change.

**Rolling update.** Release a new version and watch old pods drain while new ones come up:
```sh
kubectl -n importer get pods -w   # in one terminal
make release VERSION=1.7.0        # in another
kubectl -n importer rollout history deployment/importer-api
```
To roll back, see [How the image is built](#how-the-image-is-built). `kubectl rollout undo` works too, but only until the next sync.

**Break a probe.** `kubectl -n importer edit deployment importer-api` and change `readinessProbe.path` to `/nope`. The new pod never becomes Ready, and because of `maxUnavailable: 0` the old pods keep serving. Check `kubectl -n importer describe pod <name>` to see the events.

**Inspect.**
```sh
make logs            # the API; make logs APP=worker for the worker
kubectl -n importer describe deployment importer-api
kubectl -n importer get events --sort-by=.lastTimestamp
```

## Clean up

Remove `manifests/argocd/importer*.yaml` from the platform repo, so `make argocd-apps` doesn't bring it back. Then delete the `importer` Application in the Argo CD UI with *Foreground* propagation: that deletes everything it deployed, the namespace and its Secrets included. `kubectl delete application importer` deletes only the Application and leaves the app running.

## Next steps

- Package the manifests as a **Helm chart** (`helm create`), then compare it with the plain YAML.
- Add an **Ingress** (for example, ingress-nginx) instead of NodePort.
