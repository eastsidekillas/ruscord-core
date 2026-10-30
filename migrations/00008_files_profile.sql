-- +goose Up
-- Files (docs/adr/0019-files.md): columns hold object keys, clients get URLs.
ALTER TABLE users
    ADD COLUMN avatar text,
    ADD COLUMN bio text NOT NULL DEFAULT '' CHECK (char_length(bio) <= 190);
ALTER TABLE guilds ADD COLUMN icon text;
-- The blob collector asks "does anything still use this key?" per key.
CREATE INDEX users_avatar ON users (avatar) WHERE avatar IS NOT NULL;
CREATE INDEX guilds_icon ON guilds (icon) WHERE icon IS NOT NULL;

-- Objects that may be garbage: fresh uploads until a row refers to them,
-- replaced and removed files after a grace period. internal/platform/blob.
CREATE TABLE blob_garbage (
    key          text PRIMARY KEY,
    delete_after timestamptz NOT NULL,
    lease        uuid
);
CREATE INDEX blob_garbage_due ON blob_garbage (delete_after);

-- +goose Down
DROP TABLE blob_garbage;
ALTER TABLE guilds DROP COLUMN icon;
ALTER TABLE users DROP COLUMN bio, DROP COLUMN avatar;
