-- +goose Up
-- The address a session was last refreshed from; ip stays the sign-in address.
ALTER TABLE sessions ADD COLUMN last_ip inet;

-- +goose Down
ALTER TABLE sessions DROP COLUMN last_ip;
