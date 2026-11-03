# importer-api

A small Go REST API (standard library only) for learning how an app gets from source code to running in Kubernetes.

## The path from code to cluster

```
main.go ──go build──▶ binary ──docker build──▶ image ──kubectl apply──▶ Deployment ──▶ ReplicaSet ──▶ Pods
                                                                         Service ──routes traffic──▶ ready Pods
```

1. **Build an image.** The `Dockerfile` compiles the binary in a Go image, then copies only the binary into a small distroless image.
2. **Make the image available to the cluster.** Docker Desktop's Kubernetes uses your local Docker images, so no registry is needed. On a real cluster you would `docker push` to a registry and the nodes would pull from it.
3. **Describe the desired state.** The YAML files in `k8s/` describe what should exist. `kubectl apply` sends them to the API server.
4. **Controllers reconcile.** The Deployment controller creates a ReplicaSet, the ReplicaSet creates Pods, the scheduler places each Pod on a node, and the kubelet on that node starts the container.
5. **Probes gate traffic.** A Pod is added to the Service only after `/readyz` passes. If `/healthz` keeps failing, the container is restarted.

## API

| Method | Path              | Description                                    |
| ------ | ----------------- | ---------------------------------------------- |
| GET    | `/`               | Message, version, env, and **which pod answered** |
| GET    | `/healthz`        | Liveness probe (process only)                  |
| GET    | `/readyz`         | Readiness probe + Postgres/Redis/RabbitMQ pings |
| GET    | `/api/items`      | List items                                     |
| POST   | `/api/items`      | Create `{"name": "latex", "kind": "import"}`   |
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

If a URL isn't set, that check is reported as `"skipped"` and doesn't fail the endpoint.

```json
{"status":"degraded","checks":{
  "postgres":{"status":"ok","critical":true,"latency":"1.2ms"},
  "redis":{"status":"error","critical":false,"latency":"2ms","error":"dial tcp ...: connection refused"},
  "rabbitmq":{"status":"skipped","critical":true}}}
```

In the cluster, the URLs contain passwords, so they come from a Secret instead of the ConfigMap. The Deployment marks it `optional`, so pods start without it:

```sh
kubectl -n importer-api create secret generic importer-api-deps \
  --from-literal=DATABASE_URL='postgres://user:pass@postgres:5432/db' \
  --from-literal=REDIS_URL='redis://redis:6379/0' \
  --from-literal=RABBITMQ_URL='amqp://guest:guest@rabbitmq:5672/'
make restart
```

### Why the checks are in readiness, not liveness

- **Liveness (`/healthz`)** answers "is this process stuck?" When it fails, Kubernetes **restarts** the container. It only checks the process itself, because restarting the pod can't fix a database that's down.
- **Readiness (`/readyz`)** answers "can this pod serve requests right now?" When it fails, Kubernetes **removes the pod from the Service** but leaves it running. Once the dependency comes back, the next probe passes and traffic returns, with no restarts.

Critical vs. non-critical is the same idea one level down: only take the pod out of rotation for something it truly can't work without. If the cache is down, it's usually better to serve slower than not at all.

Try it: point `DATABASE_URL` at a host that doesn't exist, then run `kubectl -n importer-api get pods -w`. The pods show `0/1 READY` but `RESTARTS` stays at 0, and `curl http://192.168.68.57:30080/` fails because the Service has no ready pods behind it. `kubectl -n importer-api get endpointslices` shows the pod IPs marked not ready.

## Run locally

Items are stored in Postgres. `compose.yaml` runs one locally (version 18, like the cluster), and the Makefile points `DATABASE_URL` at it unless you set your own.

```sh
make db-up          # start Postgres (docker compose)
make migrate-local  # create the tables
make run
curl -XPOST localhost:8080/api/items -d '{"name":"latex","kind":"import"}'
curl localhost:8080/api/items
make test           # also runs the Postgres tests against the importer_test database
make db-down
```

## Database migrations

Schema changes are SQL files in `migrations/`, applied in version order by [goose](https://github.com/pressly/goose). They're embedded in the binary, so each image carries the schema its code expects. goose records what it applied in the `goose_db_version` table.

```sh
make migration NAME=add_items_owner           # creates migrations/00002_add_items_owner.sql
make migrate-local                             # apply pending ones
make migrate-local MIGRATE=status
make migrate-local MIGRATE=down                # roll back the latest one
```

In the cluster, `make deploy` and `make rollout` first run `make migrate`. That starts `k8s/jobs/migrate.yaml`, a Job that runs `importer-api migrate up` once with the `$(VERSION)` image, and waits for it. If the migration fails, the Job fails, its logs are printed, and nothing is deployed. A Postgres advisory lock stops two runs from applying the same migration at once.

Rules that keep rolling updates safe:

- **Old pods keep running against the new schema** until the rollout finishes, so a migration must work with both versions of the code. To rename or drop a column, add the new one first, ship code that uses it, and remove the old one in a later release.
- **Never edit a migration that has been applied anywhere.** Add a new one instead.
- **Roll forward in the cluster.** `down` is for local work.
- Build indexes on big tables with `CREATE INDEX CONCURRENTLY` and `-- +goose NO TRANSACTION`.

`go test ./...` checks that versions are sequential. With `TEST_DATABASE_URL` set (`make test` and CI set it), it also runs every migration up, down and up again.

## How the image is built

Pushing to GitHub runs `.github/workflows/build.yml`:

```
git push ──▶ GitHub Actions: go vet + go test ──▶ docker build (amd64 + arm64) ──▶ ghcr.io/c88lopez/importer-api
                                                                                        │
kubectl apply ──▶ Deployment ──▶ kubelet on the node pulls the image ◀──────────────────┘
```

| You push            | Image tags                    |
| ------------------- | ----------------------------- |
| A commit to `main`  | `main`, `sha-<short>`         |
| A tag `v1.2.3`      | `1.2.3`, `1.2`, `latest`      |
| A pull request      | Built and tested, not pushed  |

The Deployment pins an exact version (`1.0.0`), so the cluster runs only what you chose. Ship a new version with:

```sh
make release VERSION=1.1.0   # pushes tag v1.1.0; wait for the Actions run to finish
make rollout VERSION=1.1.0   # points the Deployment at the new image
```

## Deploy

The cluster is Docker Desktop on another PC (`192.168.68.57`); `kubectl config current-context` prints `docker-desktop`. Postgres, RabbitMQ and Redis already run in its `default` namespace.

```sh
kubectl apply -f k8s/00-namespace.yaml -f k8s/10-configmap.yaml

# Copy the dependency connection URLs from the existing Secrets in `default`
kubectl -n importer-api create secret generic importer-api-deps \
  --from-literal=DATABASE_URL="$(kubectl -n default get secret postgres-app -o jsonpath='{.data.fqdn-uri}' | base64 -d)" \
  --from-literal=RABBITMQ_URL="$(kubectl -n default get secret rabbitmq-default-user -o jsonpath='{.data.connection_string}' | base64 -d)" \
  --from-literal=REDIS_URL="redis://:$(kubectl -n default get secret redis-secret -o jsonpath='{.data.REDIS_PASSWORD}' | base64 -d)@redis.default.svc.cluster.local:6379/0"

make deploy
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

To send traces from `make run` to the cluster:

```sh
kubectl -n monitoring port-forward svc/alloy 4317:4317 &
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317 make run
```

## Experiments to try

**See load balancing.** Run this a few times and watch the `pod` field change:
```sh
for i in $(seq 6); do curl -s http://192.168.68.57:30080/ | grep -o '"pod":"[^"]*"'; done
```

**See that state is shared.** POST an item, then list items a few times. Requests hit different pods (check `/`), but every pod reads the same Postgres table, so the list is always the same. Before 1.2.0 items were kept in each pod's memory, and half the requests showed an empty list.

**Self-healing.** Delete a pod and watch a new one replace it:
```sh
kubectl -n importer-api get pods -w        # in one terminal
kubectl -n importer-api delete pod <name>  # in another
```

**Scale.**
```sh
kubectl -n importer-api scale deployment/importer-api --replicas=4
```

**Change config.** Edit `WELCOME_MESSAGE` in `k8s/10-configmap.yaml`, then:
```sh
kubectl apply -f k8s/ && make restart
```
Pods read env vars only at startup, so they need a restart to pick up the change.

**Rolling update.** Ship a "new version" and watch old pods drain while new ones come up:
```sh
make rollout VERSION=1.1.0
kubectl -n importer-api rollout history deployment/importer-api
kubectl -n importer-api rollout undo deployment/importer-api   # roll back
```
`make rollout` changes the image in the cluster only. Update `k8s/20-deployment.yaml` too, or the next `kubectl apply` puts the old version back.

**Break a probe.** Change `readinessProbe.path` to `/nope` and apply. The new pod never becomes Ready, and because of `maxUnavailable: 0` the old pods keep serving. Check `kubectl -n importer-api describe pod <name>` to see the events.

**Inspect.**
```sh
make logs
kubectl -n importer-api describe deployment importer-api
kubectl -n importer-api get events --sort-by=.lastTimestamp
```

## Clean up

```sh
make undeploy
```

## Next steps

- Package the manifests as a **Helm chart** (`helm create`), then compare it with the plain YAML.
- Add an **Ingress** (for example, ingress-nginx) instead of NodePort.
- Push the image to a registry (GHCR or Docker Hub) and deploy with a GitOps tool like **Argo CD**.
