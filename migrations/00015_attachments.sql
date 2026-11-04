-- +goose Up
-- Attachments of messages (docs/adr/0036-attachments.md). A file is uploaded
-- first (message_id NULL) and attached when the message is sent; the object
-- is attachments/<channel_id>/<id>.<ext> in S3. An upload nobody attaches
-- within an hour is collected with its row.
CREATE TABLE attachments (
    id           uuid PRIMARY KEY,
    channel_id   uuid NOT NULL,
    uploader_id  uuid REFERENCES users (id) ON DELETE SET NULL,
    -- No cascade: deleting a message deletes its attachments first, so
    -- their objects are scheduled for collection in the same transaction.
    message_id   uuid REFERENCES messages (id),
    filename     text NOT NULL CHECK (char_length(filename) BETWEEN 1 AND 255),
    size         bigint NOT NULL CHECK (size > 0),
    -- What the server sniffed: the extension of the key and how it is shown.
    ext          text NOT NULL CHECK (ext IN ('png', 'jpg', 'gif', 'webp', 'mp4', 'webm', 'bin')),
    content_type text NOT NULL,
    width        integer,
    height       integer,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX attachments_message ON attachments (message_id) WHERE message_id IS NOT NULL;
CREATE INDEX attachments_channel ON attachments (channel_id);

-- A message may be only attachments, without text.
ALTER TABLE messages DROP CONSTRAINT messages_content_check,
    ADD CONSTRAINT messages_content_check CHECK (char_length(content) <= 4000);

-- +goose Down
ALTER TABLE messages DROP CONSTRAINT messages_content_check,
    ADD CONSTRAINT messages_content_check CHECK (char_length(content) BETWEEN 1 AND 4000);
DROP TABLE attachments;
