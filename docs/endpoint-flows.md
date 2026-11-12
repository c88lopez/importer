# Endpoint flows

What each endpoint does, from the client to the database and back. The diagrams follow the code: `internal/api/handlers.go`, `internal/api/health.go`, `internal/gql/`, `internal/items/store.go`, `internal/outbox/` and `internal/worker/`.

Every request goes through the same chain in each pod:

```
Client ─► Kubernetes Service (NodePort) ─► a ready pod ─► TraceRequests ─► LogRequests ─► router ─► RateLimit ─► handler/resolver ─► items.Store ─► Postgres
```

- **Service**: kube-proxy picks one pod that passes `/readyz`. importer-api is on NodePort `30080`, importer-graphql on `30082`.
- **TraceRequests**: opens an OpenTelemetry span named after the route (`GET /api/items/{id}`). Probes are skipped.
- **LogRequests**: one log line per request, with its status and `trace_id`. Probes are skipped.
- **RateLimit**: only on `/api/items*`, `/graphql` and `POST /`. Counts the request for the client's IP in Redis (`ratelimit:<service>:<ip>`, a fixed window). Over the limit, it answers `429` with `Retry-After` and the handler never runs. If Redis doesn't answer, the request goes through. The diagrams below leave this step out.
- **items.Store**: the only code that talks to Postgres, through a `pgxpool` pool. Both services and the worker use it.

Store errors other than "not found" are logged with the trace and returned as a bare `internal error`, so database details don't leak to clients.

## importer-api (REST)

### GET /

Shows which pod answered. No dependencies.

```mermaid
sequenceDiagram
    autonumber
    actor C as Client
    participant S as K8s Service :30080
    participant M as Middleware
    participant H as Handler

    C->>S: GET /
    S->>M: forward to a ready pod
    M->>H: span "GET /"
    H->>H: os.Hostname()
    H-->>C: 200 {message, version, env, pod}
```

### GET /healthz

Liveness. Only checks that the process answers, so it never touches a dependency.

```mermaid
sequenceDiagram
    autonumber
    participant K as kubelet
    participant H as Handler

    K->>H: GET /healthz (not traced, not logged)
    H-->>K: 200 {"status":"ok"}
    Note over K: Failing repeatedly means the container restarts
```

### GET /readyz

Readiness. Pings every configured dependency at the same time, within 2s.

```mermaid
sequenceDiagram
    autonumber
    participant K as kubelet
    participant H as ReadinessHandler
    participant PG as Postgres
    participant R as Redis
    participant MQ as RabbitMQ

    K->>H: GET /readyz (not traced, not logged)
    alt pod is shutting down (SIGTERM received)
        H-->>K: 503 {"status":"shutting down"}
    else running
        par 2s budget for all checks
            H->>PG: pool.Ping()
            PG-->>H: ok / error
        and
            H->>R: PING (skipped if REDIS_URL is empty)
            R-->>H: PONG / error
        and
            H->>MQ: open AMQP connection, then close it
            MQ-->>H: ok / error
        end
        alt a critical dependency failed (default: postgres, rabbitmq)
            H-->>K: 503 {"status":"not ready", checks}
            Note over K: Pod removed from the Service, not restarted
        else only a non-critical one failed (redis)
            H-->>K: 200 {"status":"degraded", checks}
        else all ok
            H-->>K: 200 {"status":"ready", checks}
        end
    end
```

### GET /api/items

```mermaid
sequenceDiagram
    autonumber
    actor C as Client
    participant S as K8s Service :30080
    participant M as Middleware
    participant H as Handler
    participant ST as items.Store
    participant PG as Postgres

    C->>S: GET /api/items
    S->>M: forward to a ready pod
    M->>H: span "GET /api/items"
    H->>ST: List(ctx)
    ST->>PG: SELECT id, name, kind, priority, created_at, status, processed_at<br/>FROM items ORDER BY created_at, id
    alt query ok
        PG-->>ST: rows
        ST-->>H: []Item
        H-->>C: 200 [{id, name, kind, priority, createdAt, status, processedAt}]
    else database error
        PG-->>ST: error
        ST-->>H: error
        H->>H: log error with trace_id
        H-->>C: 500 {"error":"internal error"}
    end
```

### GET /api/items/{id}

```mermaid
sequenceDiagram
    autonumber
    actor C as Client
    participant S as K8s Service :30080
    participant M as Middleware
    participant H as Handler
    participant ST as items.Store
    participant PG as Postgres

    C->>S: GET /api/items/42
    S->>M: forward to a ready pod
    M->>H: span "GET /api/items/{id}"
    H->>H: parse {id} as int64
    alt id is not a number
        H-->>C: 404 {"error":"item not found"}
    else valid id
        H->>ST: Get(ctx, 42)
        ST->>PG: SELECT ... FROM items WHERE id = $1
        alt one row
            PG-->>ST: row
            ST-->>H: Item
            H-->>C: 200 {id, name, kind, priority, createdAt, status, processedAt}
        else no rows
            PG-->>ST: no rows
            ST-->>H: ErrNotFound
            H-->>C: 404 {"error":"item not found"}
        else database error
            ST-->>H: error
            H-->>C: 500 {"error":"internal error"}
        end
    end
```

### POST /api/items

The request itself only writes to Postgres: the item and its `item.created` event, in one transaction. Publishing to RabbitMQ and processing happen afterwards, in the background.

```mermaid
sequenceDiagram
    autonumber
    actor C as Client
    participant S as K8s Service :30080
    participant M as Middleware
    participant H as Handler
    participant ST as items.Store
    participant PG as Postgres
    participant RL as Outbox relay (same pod)

    C->>S: POST /api/items {"name":"latex","kind":"import","priority":"high"}
    S->>M: forward to a ready pod
    M->>H: span "POST /api/items"
    H->>H: decode JSON (max 1 MiB), trim name
    alt invalid JSON, empty name, kind not import/export, or priority not low/normal/high
        H-->>C: 400 {"error":"..."}
    else valid
        H->>ST: Create(ctx, name, kind, priority)
        ST->>PG: BEGIN
        ST->>PG: INSERT INTO items (name, kind, priority) VALUES ($1, $2, $3) RETURNING ...
        PG-->>ST: new row (status = 'pending')
        ST->>PG: INSERT INTO outbox (routing_key, payload, trace_headers)<br/>'item.created', item JSON, traceparent
        alt both inserts ok
            ST->>PG: COMMIT
            ST->>RL: Notify() (non-blocking nudge)
            ST-->>H: Item
            H-->>C: 201 {id, name, kind, priority, createdAt, status:"pending"}
        else any error
            ST->>PG: ROLLBACK (neither row is saved)
            ST-->>H: error
            H-->>C: 500 {"error":"internal error"}
        end
    end
```

Then, asynchronously:

```mermaid
sequenceDiagram
    autonumber
    participant RL as Outbox relay (any API or GraphQL pod)
    participant PG as Postgres
    participant MQ as RabbitMQ (exchange "importer")
    participant Q as Queue importer-worker.item-created
    participant W as importer-worker
    participant DLQ as ...item-created.dlq

    Note over RL: Wakes on Notify(), or polls every 1s
    RL->>PG: BEGIN
    RL->>PG: SELECT ... FROM outbox ORDER BY id LIMIT 100<br/>FOR UPDATE SKIP LOCKED
    PG-->>RL: rows (other pods skip the locked ones)
    loop each row, in order
        RL->>MQ: publish (persistent, mandatory, routing key item.created, traceparent header)
        MQ->>Q: route by item.created
        MQ-->>RL: publisher confirm (ack)
        Note over RL,MQ: If no queue is bound, the message is returned:<br/>the row stays, and the relay stops the batch there
    end
    RL->>PG: DELETE FROM outbox WHERE id = ANY(confirmed ids)
    RL->>PG: COMMIT

    Q->>W: deliver (prefetch 10)
    W->>W: span "process importer-worker.item-created",<br/>continues the request's trace
    alt wrong type or bad body
        W->>DLQ: reject (no retry)
    else valid item.created
        W->>PG: UPDATE items SET status = 'done', processed_at = now()<br/>WHERE id = $1 AND status = 'pending'
        alt 1 row updated, or 0 (already done or deleted)
            W->>Q: ack
        else database error, attempt below 10
            W->>W: wait 1s, 2s, 4s ... up to 30s
            W->>Q: nack, requeue
        else database error, attempt 10
            W->>DLQ: reject
        end
    end
```

Delivery is at least once: if a pod dies after RabbitMQ confirms but before the `DELETE` commits, the event is published again. The `AND status = 'pending'` condition makes the duplicate harmless.

### DELETE /api/items/{id}

```mermaid
sequenceDiagram
    autonumber
    actor C as Client
    participant S as K8s Service :30080
    participant M as Middleware
    participant H as Handler
    participant ST as items.Store
    participant PG as Postgres

    C->>S: DELETE /api/items/42
    S->>M: forward to a ready pod
    M->>H: span "DELETE /api/items/{id}"
    H->>H: parse {id} as int64
    alt id is not a number
        H-->>C: 404 {"error":"item not found"}
    else valid id
        H->>ST: Delete(ctx, 42)
        ST->>PG: DELETE FROM items WHERE id = $1
        alt 1 row affected
            PG-->>ST: DELETE 1
            ST-->>H: nil
            H-->>C: 204 No Content
        else 0 rows affected
            PG-->>ST: DELETE 0
            ST-->>H: ErrNotFound
            H-->>C: 404 {"error":"item not found"}
        else database error
            ST-->>H: error
            H-->>C: 500 {"error":"internal error"}
        end
    end
```

No event is published. If the item was still pending, the worker's `UPDATE` later matches no row and the message is acked.

## importer-graphql

`/healthz` and `/readyz` work the same as in importer-api: `/readyz` uses the same `ReadinessHandler` and checks.

### GET / (GraphiQL)

```mermaid
sequenceDiagram
    autonumber
    actor B as Browser
    participant S as K8s Service :30082
    participant G as importer-graphql

    B->>S: GET /
    S->>G: forward to a ready pod
    G-->>B: 200 GraphiQL HTML (title shows version and pod)
    B->>S: POST /graphql (introspection query, for autocomplete)
    S->>G: forward
    G-->>B: 200 {data: {__schema ...}}
```

### POST /graphql (and POST /): shared steps

Every operation goes through these steps before its resolver runs. The diagrams below start at the resolver.

```mermaid
sequenceDiagram
    autonumber
    actor C as Client
    participant S as K8s Service :30082
    participant M as Middleware
    participant RL as RateLimit
    participant RD as Redis
    participant CT as Content-Type check
    participant GQ as gqlgen handler
    participant R as Resolver

    C->>S: POST /graphql {"query": "...", "variables": {...}}
    S->>M: forward to a ready pod
    M->>RL: span "/graphql"
    RL->>RD: INCR ratelimit:importer-graphql:<ip> (Lua, sets the window's expiry)
    alt over the limit
        RL-->>C: 429 {"errors":[{"message":"rate limit exceeded","extensions":{"code":"RATE_LIMITED"}}]}<br/>Retry-After, RateLimit-*
    else within the limit, or Redis down (fail open)
    RL->>CT: forward
    end
    alt Content-Type is not application/json or application/graphql
        CT-->>C: 415 {"errors":[{"message":"POST needs Content-Type ..."}]}
    else JSON or GraphQL body (max 1 MiB)
        CT->>GQ: forward
        GQ->>GQ: parse and validate against schema.graphqls<br/>(LRU cache of parsed queries)
        alt syntax error, unknown field or bad enum
            GQ-->>C: 422 {"errors":[...]}
        else complexity above 200
            GQ-->>C: 422 {"errors":[complexity limit]}
        else valid
            GQ->>R: child span per operation, e.g. "query Items"
            R-->>GQ: data or error
            GQ-->>C: 200 {"data": ..., "errors": [...]?}
        end
    end
```

### query items

```mermaid
sequenceDiagram
    autonumber
    actor C as Client
    participant GQ as gqlgen handler
    participant R as Query.items
    participant ST as items.Store
    participant PG as Postgres

    C->>GQ: query Items { items { id name kind status } }
    GQ->>R: Items(ctx)
    R->>ST: List(ctx)
    ST->>PG: SELECT ... FROM items ORDER BY created_at, id
    alt ok
        PG-->>ST: rows
        ST-->>R: []Item
        R-->>GQ: items
        GQ->>GQ: Item.kind, Item.priority and Item.status resolvers<br/>map "import" to IMPORT, "high" to HIGH, "pending" to PENDING
        GQ-->>C: 200 {"data":{"items":[...]}}
    else database error
        ST-->>R: error
        R->>R: log error with trace_id
        GQ-->>C: 200 {"data":null,"errors":[{"message":"internal error"}]}
    end
```

### query item(id)

```mermaid
sequenceDiagram
    autonumber
    actor C as Client
    participant GQ as gqlgen handler
    participant R as Query.item
    participant ST as items.Store
    participant PG as Postgres

    C->>GQ: query Item($id: ID!) { item(id: $id) { ... } }
    GQ->>GQ: ID scalar parses $id as int64
    GQ->>R: Item(ctx, 42)
    R->>ST: Get(ctx, 42)
    ST->>PG: SELECT ... FROM items WHERE id = $1
    alt found
        PG-->>ST: row
        ST-->>R: Item
        GQ-->>C: 200 {"data":{"item":{...}}}
    else not found
        ST-->>R: ErrNotFound
        R-->>GQ: nil (no error)
        GQ-->>C: 200 {"data":{"item":null}}
    else database error
        ST-->>R: error
        GQ-->>C: 200 {"data":{"item":null},"errors":[{"message":"internal error"}]}
    end
```

### mutation createItem

Same store call, so the same transaction and outbox as `POST /api/items`. importer-graphql runs its own outbox relay, and the rest of the asynchronous flow is the one shown under `POST /api/items`.

```mermaid
sequenceDiagram
    autonumber
    actor C as Client
    participant GQ as gqlgen handler
    participant R as Mutation.createItem
    participant ST as items.Store
    participant PG as Postgres
    participant RL as Outbox relay (same pod)

    C->>GQ: mutation { createItem(input: {name: "latex", kind: IMPORT}) { id status } }
    Note over GQ: The schema already rejected a kind or priority outside its enum (422)<br/>and filled in priority: NORMAL if it was left out
    GQ->>R: CreateItem(ctx, input)
    R->>R: trim name
    alt name is empty
        R-->>GQ: BAD_USER_INPUT
        GQ-->>C: 200 {"data":null,"errors":[{"message":"name is required"}]}
    else valid
        R->>ST: Create(ctx, name, lower(kind), lower(priority))
        ST->>PG: BEGIN
        ST->>PG: INSERT INTO items ... RETURNING ...
        ST->>PG: INSERT INTO outbox ('item.created', item JSON, traceparent)
        ST->>PG: COMMIT
        ST->>RL: Notify()
        ST-->>R: Item
        GQ-->>C: 200 {"data":{"createItem":{"id":"43","status":"PENDING"}}}
    end
```

### mutation deleteItem

```mermaid
sequenceDiagram
    autonumber
    actor C as Client
    participant GQ as gqlgen handler
    participant R as Mutation.deleteItem
    participant ST as items.Store
    participant PG as Postgres

    C->>GQ: mutation { deleteItem(id: "42") }
    GQ->>R: DeleteItem(ctx, 42)
    R->>ST: Delete(ctx, 42)
    ST->>PG: DELETE FROM items WHERE id = $1
    alt 1 row affected
        ST-->>R: nil
        GQ-->>C: 200 {"data":{"deleteItem":true}}
    else 0 rows affected
        ST-->>R: ErrNotFound
        GQ-->>C: 200 {"data":{"deleteItem":false}}
    else database error
        ST-->>R: error
        GQ-->>C: 200 {"data":null,"errors":[{"message":"internal error"}]}
    end
```
