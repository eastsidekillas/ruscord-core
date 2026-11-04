-- +goose Up
-- Reactions and pins (docs/adr/0037-reactions-pins.md).
CREATE TABLE message_reactions (
    message_id uuid NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    -- One fully-qualified Unicode emoji from internal/message/emoji.txt.
    emoji      text NOT NULL CHECK (octet_length(emoji) BETWEEN 1 AND 64),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, emoji, user_id)
);

ALTER TABLE messages ADD COLUMN pinned_at timestamptz;
CREATE INDEX messages_pinned ON messages (channel_id, pinned_at DESC) WHERE pinned_at IS NOT NULL;

-- PIN_MESSAGES (bit 26) is new: whoever could manage messages keeps being
-- able to pin, in roles and in channel overwrites alike.
UPDATE roles SET permissions = permissions | (1::bigint << 26) WHERE permissions & (1::bigint << 6) <> 0;
UPDATE permission_overwrites SET allow = allow | (1::bigint << 26) WHERE allow & (1::bigint << 6) <> 0;
UPDATE permission_overwrites SET deny = deny | (1::bigint << 26) WHERE deny & (1::bigint << 6) <> 0;

-- +goose Down
UPDATE permission_overwrites SET allow = allow & ~(1::bigint << 26), deny = deny & ~(1::bigint << 26);
UPDATE roles SET permissions = permissions & ~(1::bigint << 26);
DROP INDEX messages_pinned;
ALTER TABLE messages DROP COLUMN pinned_at;
DROP TABLE message_reactions;
