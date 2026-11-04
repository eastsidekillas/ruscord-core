-- +goose Up
-- How far each user has read each channel (docs/adr/0038-read-states.md).
-- Channels live in two modules (guild text channels and DMs), so there is
-- no foreign key to them; deleting channels deletes their rows.
CREATE TABLE read_states (
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    channel_id   uuid NOT NULL,
    last_read_id uuid NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, channel_id)
);
CREATE INDEX read_states_channel ON read_states (channel_id);

-- +goose Down
DROP TABLE read_states;
