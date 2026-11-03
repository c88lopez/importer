-- +goose Up
CREATE TABLE items (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       TEXT NOT NULL CHECK (name <> ''),
    kind       TEXT NOT NULL CHECK (kind IN ('import', 'export')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- GET /api/items lists in creation order.
CREATE INDEX items_created_at_idx ON items (created_at);

-- +goose Down
DROP TABLE items;
