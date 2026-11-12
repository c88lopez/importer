# Worker (importer-worker)

`cmd/worker/README.md` is the detailed reference.

- Consumes `item.created` from a quorum queue and marks the item `done` via `Store.MarkDone`. Delivery is at-least-once, so processing must stay idempotent.
- Retries are counted here from `x-acquired-count` before dead-lettering, because RabbitMQ 4's delivery limit doesn't count nacks.
- Queue arguments are fixed at creation: changing them in code makes the declare fail with `PRECONDITION_FAILED` against an existing queue.
