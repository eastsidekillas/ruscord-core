-- +goose Up

-- channel_id points at either channels (guild) or dm_channels, so there is no
-- foreign key; owners delete messages through hooks.ChannelsDeleted (ADR-0015).
-- guild_id is NULL in direct messages.
CREATE TABLE messages (
    id               uuid PRIMARY KEY,
    channel_id       uuid NOT NULL,
    guild_id         uuid,
    author_id        uuid REFERENCES users (id) ON DELETE SET NULL,
    content          text NOT NULL CHECK (char_length(content) BETWEEN 1 AND 4000),
    nonce            text CHECK (char_length(nonce) <= 64),
    reply_to         uuid,
    mention_everyone boolean NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    edited_at        timestamptz
);
-- History is read newest first by channel; UUIDv7 ids sort by time.
CREATE INDEX messages_channel ON messages (channel_id, id DESC);
-- A retried send with the same nonce returns the first message.
CREATE UNIQUE INDEX messages_nonce ON messages (channel_id, author_id, nonce) WHERE nonce IS NOT NULL;

-- +goose Down
DROP TABLE messages;
