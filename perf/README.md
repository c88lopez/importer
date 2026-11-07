# Performance tests

[k6](https://k6.io) scripts for the three services. They run in Docker (`grafana/k6`), so there's nothing to install:

| Script | Target | What it measures |
| --- | --- | --- |
| `rest.js` | importer-api | Create, get and delete an item, plus a list in 1 of every 10 iterations. Latency per route. |
| `graphql.js` | importer-graphql | The same steps as `rest.js`, so you can compare the two. Requests are tagged by operation name (`CreateItem`, `Item`, ...). |
| `worker.js` | importer-worker (RabbitMQ) | How long it takes from creating an item to the worker marking it `done`: outbox, relay, exchange, queue, consumer. |

```sh
make perf-rest                       # smoke: 1 iteration/s for 30s
make perf-graphql PROFILE=load       # ramp to 20 iterations/s, hold for 3m
make perf-rest PROFILE=stress        # climb to 200 iterations/s, to find where it breaks
make perf-rest RATE=400 RAMP=2m DURATION=5m   # your own peak: ramp to RATE iterations/s, hold it, ramp down

make perf-worker RATE=20 DURATION=3m # latency: a steady rate of creates
make perf-worker MODE=backlog COUNT=2000   # throughput: a burst, then wait for all of it
```

They target the cluster's NodePorts by default. To test `make run-api` / `make run-graphql` instead, run `API_URL=http://host.docker.internal:8080 GRAPHQL_URL=http://host.docker.internal:8081/graphql make perf-rest`.

**The data is real.** There's one database, so test items go into the same table as everything else. Each one is named `perf-<run id>-...` and is deleted by the end of its run. If you stop a run with Ctrl+C before it finishes, delete the `perf-` items by hand.

## Thresholds

A run fails (non-zero exit) if any of these is missed:

- Fewer than 99% of checks pass, or more than 1% of requests fail.
- p95 latency is over 300ms for create, 150ms for get, 200ms for delete, or 1s for list. The list isn't paginated, so it gets slower as the table grows.
- `worker.js`: p95 `item_processing_ms` is over 2s, or any item is still `pending` after `TIMEOUT` (60s by default).

## The worker test

`item_processing_ms` is `processedAt - createdAt`. Both timestamps come from the server, so how often k6 polls doesn't affect the result.

- **latency** creates `RATE` items per second. Each iteration polls its own item until it's `done`, then deletes it.
- **backlog** creates `COUNT` items in `setup()`, 50 at a time, then lists items once a second until all of them are `done`. It reports:
  - `backlog_drain_s`: from the first create to the last item processed.
  - `backlog_drain_after_creates_s`: from the last create to the last item processed.
  - `backlog_throughput_per_s`: items processed per second.

  When the worker keeps up with the creates, the throughput only tells you how fast the API creates items, and the "after creates" time stays close to 0. To measure the worker alone, build up a queue first: `kubectl -n importer scale deployment/importer-worker --replicas=0`, run the backlog test, then scale it back to 2. Scaling up ends the test. Argo CD may scale the worker back to 2 by itself if self-heal is on.

While a test runs, watch the **Importer API**, **Importer GraphQL** and **Importer Worker** dashboards in Grafana for CPU against limits, the connection pool, outbox backlog and queue depth. They show where the time is going. k6 only shows how long requests took.
