-- +goose Up
-- The gateway reads the newest event of a topic inside a snapshot
-- transaction (docs/adr/0016-gateway.md).
CREATE INDEX outbox_topic_id ON outbox (topic, id);

-- +goose Down
DROP INDEX outbox_topic_id;
