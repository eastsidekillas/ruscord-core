-- +goose Up
-- Slow mode (docs/adr/0043-slow-mode.md): seconds a member waits between
-- messages in a text channel; 0 is off, at most 6 hours.
ALTER TABLE channels ADD COLUMN rate_limit_per_user integer NOT NULL DEFAULT 0
    CHECK (rate_limit_per_user BETWEEN 0 AND 21600);

-- +goose Down
ALTER TABLE channels DROP COLUMN rate_limit_per_user;
