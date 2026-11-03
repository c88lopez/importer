-- +goose Up
-- importer-worker processes each new item and marks it done.
ALTER TABLE items
    ADD COLUMN status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done')),
    ADD COLUMN processed_at TIMESTAMPTZ;

-- Transactional outbox: an event is written in the same transaction as the
-- change it describes, so it's committed if and only if the change is. The
-- API's relay (outbox.go) publishes rows to RabbitMQ and deletes them.
CREATE TABLE outbox (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    routing_key   TEXT NOT NULL,
    payload       JSONB NOT NULL,
    -- W3C trace context of the request that wrote the event (traceparent),
    -- so the publish and the worker join the request's trace.
    trace_headers JSONB NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Items created before the worker existed go through it too.
INSERT INTO outbox (routing_key, payload)
SELECT 'item.created', jsonb_build_object(
           'id', id::text, 'name', name, 'kind', kind, 'createdAt', created_at, 'status', status)
FROM items
ORDER BY id;

-- +goose Down
DROP TABLE outbox;
ALTER TABLE items DROP COLUMN processed_at, DROP COLUMN status;
