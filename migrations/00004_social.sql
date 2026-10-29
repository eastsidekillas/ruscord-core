-- +goose Up

-- Direct messages live apart from guild channels (ADR-0014): the guild module
-- owns channels, the social module owns dm_channels. Guild channels lose the
-- DM types reserved in 00002, which were never written.
ALTER TABLE channels DROP CONSTRAINT channels_check;
ALTER TABLE channels DROP CONSTRAINT channels_type_check;
ALTER TABLE channels ALTER COLUMN guild_id SET NOT NULL;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN (0, 2, 4));

-- One row per direction, like Discord: a friendship is two type-1 rows, a
-- pending request is 4 (outgoing) on the sender and 3 (incoming) on the
-- receiver, a block is a single type-2 row on the blocker's side, invisible
-- to the blocked user.
CREATE TABLE relationships (
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    target_id  uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    type       smallint NOT NULL CHECK (type IN (1, 2, 3, 4)),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, target_id),
    CHECK (user_id <> target_id)
);
CREATE INDEX relationships_target ON relationships (target_id);

-- type: 1 dm, 3 group_dm (same numbers as the client protocol).
-- pair_key makes a 1:1 DM unique per pair of users.
CREATE TABLE dm_channels (
    id         uuid PRIMARY KEY,
    type       smallint NOT NULL CHECK (type IN (1, 3)),
    name       text CHECK (char_length(name) BETWEEN 1 AND 100),
    owner_id   uuid REFERENCES users (id) ON DELETE SET NULL,
    pair_key   text UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((type = 1) = (pair_key IS NOT NULL))
);

CREATE TABLE dm_recipients (
    channel_id uuid NOT NULL REFERENCES dm_channels (id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    joined_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (channel_id, user_id)
);
CREATE INDEX dm_recipients_user ON dm_recipients (user_id);

-- +goose Down
DROP TABLE dm_recipients;
DROP TABLE dm_channels;
DROP TABLE relationships;
ALTER TABLE channels DROP CONSTRAINT channels_type_check;
ALTER TABLE channels ALTER COLUMN guild_id DROP NOT NULL;
ALTER TABLE channels ADD CONSTRAINT channels_type_check CHECK (type IN (0, 1, 2, 3, 4));
ALTER TABLE channels ADD CONSTRAINT channels_check CHECK ((guild_id IS NULL) = (type IN (1, 3)));
