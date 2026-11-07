-- +goose NO TRANSACTION
-- +goose Up
-- Searches by author or mention without text (S5 of docs/audit/2026-11-04.md)
-- read these instead of every message of a server. CONCURRENTLY: building
-- them does not stop sending.
CREATE INDEX CONCURRENTLY IF NOT EXISTS messages_channel_author ON messages (channel_id, author_id, id DESC);
CREATE INDEX CONCURRENTLY IF NOT EXISTS messages_mentions ON messages USING gin (mentions);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS messages_mentions;
DROP INDEX CONCURRENTLY IF EXISTS messages_channel_author;
