-- +goose Up
-- Old pods insert without a priority during a rollout; they get 'normal'.
ALTER TABLE items
    ADD COLUMN priority TEXT NOT NULL DEFAULT 'normal'
        CONSTRAINT items_priority_check CHECK (priority IN ('low', 'normal', 'high'));

-- +goose Down
ALTER TABLE items DROP COLUMN priority;
