-- +goose Up
-- The status a user chose; whether they are connected lives in Valkey
-- (internal/presence, docs/adr/0018-ephemeral-events.md).
ALTER TABLE users ADD COLUMN status text NOT NULL DEFAULT 'online'
    CHECK (status IN ('online', 'idle', 'dnd', 'invisible'));

-- +goose Down
ALTER TABLE users DROP COLUMN status;
