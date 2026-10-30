-- +goose Up
-- Bans (docs/adr/0021-bans-account-deletion.md): a banned user cannot join
-- the guild again, by any invite, until the ban is lifted.
CREATE TABLE guild_bans (
    guild_id     uuid NOT NULL REFERENCES guilds (id) ON DELETE CASCADE,
    user_id      uuid NOT NULL REFERENCES users (id),
    reason       text NOT NULL DEFAULT '' CHECK (char_length(reason) <= 512),
    moderator_id uuid REFERENCES users (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (guild_id, user_id)
);

-- Deleted accounts: hidden and unusable at once; their data stays until the
-- retention question is settled (docs/legal/russia.md, L2). cleaned_at marks
-- the last pass that took the account out of guilds, groups and friends.
ALTER TABLE users
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN cleaned_at timestamptz;
CREATE INDEX users_cleanup ON users (deleted_at) WHERE deleted_at IS NOT NULL;

-- +goose Down
DROP INDEX users_cleanup;
ALTER TABLE users DROP COLUMN deleted_at, DROP COLUMN cleaned_at;
DROP TABLE guild_bans;
