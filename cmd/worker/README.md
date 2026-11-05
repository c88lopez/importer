# importer-worker

Processes the events importer-api publishes to RabbitMQ. For each `item.created` it marks the item `done` in the shared Postgres database (`status`, `processed_at`).

```
importer-api ─► exchange "importer" (topic) ─► item.created ─► queue importer-worker.item-created ─► importer-worker
                                                                   │ rejected                           │
                                                                   ▼                                     ▼
                                                    importer-worker.item-created.dlq     UPDATE items SET status = 'done'
```

The API writes events to an outbox table in the same transaction as the item, then publishes them (see the [main README](../../README.md), "Events and the outbox"). Delivery is **at least once**, so the same event can arrive twice.

## How a message is handled

The worker declares its queues on every connect: a [quorum queue](https://www.rabbitmq.com/docs/quorum-queues) bound to `item.created`, and a dead-letter queue (DLQ) for messages it gives up on. It takes one message at a time, with up to 10 more prefetched, and acks each only after the database update commits. If a pod dies mid-message, RabbitMQ hands the message to another pod.

| What happens | Result |
| --- | --- |
| Item is pending | Marked `done`, message acked |
| Item already done, or deleted | Nothing to do, message acked. This makes duplicates harmless. |
| Malformed body or unknown type | Rejected straight to the DLQ: retrying can't fix it |
| Any other error (e.g. database down) | Waits 1s, 2s, 4s… (up to 30s), then requeues. After 10 attempts, rejected to the DLQ. |
| Delivered again after the 10th attempt (the pod died processing it) | Rejected to the DLQ without trying again |

The worker counts attempts itself, using the `x-acquired-count` header that quorum queues set on redeliveries. RabbitMQ's own `x-delivery-limit` doesn't help here: in RabbitMQ 4, a requeue with AMQP 0.9.1 `basic.nack` doesn't count toward it, so a failing message would be retried forever. `TestConsumerGivesUpAfterMaxDeliveries` checks this.

Messages in the DLQ wait for a human. Inspect them in the management UI (Queues > `importer-worker.item-created.dlq` > Get messages). To retry them once the cause is fixed, use a [shovel](https://www.rabbitmq.com/docs/shovel) back to the `importer` exchange, or the management UI's "Move messages".

Queue arguments are fixed when a queue is created. Changing one in code makes the declare fail with `PRECONDITION_FAILED`, so delete the queue first, or set it with a policy instead.

## The shared database

The worker updates the items table through `internal/items` (`Store.MarkDone`), the same store the API uses, and parses events into the same `items.Item` the API publishes. The tests run the real migrations (`internal/database/dbtest`), so a schema change that breaks the worker fails its tests. A release's migration runs before either service rolls out.

## Run locally

Use the commands from the [main README](../../README.md) ("Run locally"): `make tunnel`, then `make run-worker` (probes and metrics on :9465) next to `make run-api`. The worker runs on the real queue, so it takes its share of messages alongside the cluster's pods. Its logs show which items it processed.

## Probes, metrics and tracing

There's no API, so one port (`PORT`, default 9464) serves:

| Path | What |
| --- | --- |
| `/healthz` | Liveness: the process answers. Outages don't fail it, because the consumer reconnects on its own. |
| `/readyz` | Readiness: consuming from RabbitMQ and Postgres answers. A rollout waits for new pods to be ready, so a worker that can't consume never replaces one that can. |
| `/metrics` | Prometheus, scraped via `k8s/worker/40-podmonitor.yaml` |

| Metric | What |
| --- | --- |
| `messaging_process_duration_seconds` | Messages processed, by `outcome` (`done`, `requeued`, `dead_lettered`) |
| `db_client_operation_duration_seconds`, `pgxpool_*` | Database calls and the connection pool |

Queue metrics come from RabbitMQ itself. Its default `/metrics` (the ServiceMonitor in `default`) adds up all queues, so `k8s/worker/50-rabbitmq-queues.yaml` also scrapes `/metrics/detailed`, which has one series per queue, and keeps only the worker's queues:

| Metric | What |
| --- | --- |
| `rabbitmq_detailed_queue_messages_ready{queue}` | Waiting for a worker. Should stay near 0. |
| `rabbitmq_detailed_queue_messages_unacked{queue}` | Delivered to a worker, not acked yet (in progress or prefetched) |
| `rabbitmq_detailed_queue_consumers{queue}` | Consumers: one per worker pod |
| `rabbitmq_detailed_queue_messages{queue="importer-worker.item-created.dlq"}` | Messages waiting in the dead-letter queue |

The **Importer Worker** dashboard in Grafana (folder *Services*, source `k8s/dashboards/importer-worker.json`) puts these together with the worker's own metrics and logs. Things to watch:

- **In DLQ** above 0: a message was given up on. The *Errors and warnings* logs say why, and the red annotations mark when.
- **Waiting in queue** growing: the workers aren't keeping up, or aren't consuming (check *Consumers* and *Ready pods*).
- **API outbox backlog** growing: events aren't reaching RabbitMQ at all, so the worker has nothing to do. See the Importer API dashboard.

Each message gets a `process importer-worker.item-created` span, which continues the trace from the message's `traceparent` header. In Tempo, a `POST /api/items` trace shows the request, the API's `send importer`, the worker's `process`, and the worker's `UPDATE`.

## Deploy

The worker is released and deployed with the API: one version, one `make release`, and Argo CD rolls out both, after the migration (see the main README). It runs in the `importer` namespace with its own Secret, `importer-worker-deps`, and has no Service. `make logs APP=worker` streams its logs.

On shutdown (SIGTERM), the worker stops taking messages and finishes the one in hand, with up to 30s for it. Prefetched messages go back to the queue.
