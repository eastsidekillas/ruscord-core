-- +goose Up

-- A guild is what the UI calls a "server".
CREATE TABLE guilds (
    id         uuid PRIMARY KEY,
    name       text NOT NULL CHECK (char_length(name) BETWEEN 2 AND 100),
    owner_id   uuid NOT NULL REFERENCES users (id),
    icon_key   text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX guilds_owner ON guilds (owner_id);

-- The @everyone role has the same id as its guild.
CREATE TABLE roles (
    id          uuid PRIMARY KEY,
    guild_id    uuid NOT NULL REFERENCES guilds (id) ON DELETE CASCADE,
    name        text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    position    int  NOT NULL CHECK (position >= 0),
    permissions bigint NOT NULL DEFAULT 0,
    color       int  NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX roles_guild ON roles (guild_id, position);

CREATE TABLE members (
    guild_id  uuid NOT NULL REFERENCES guilds (id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    nick      text CHECK (char_length(nick) BETWEEN 1 AND 32),
    joined_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (guild_id, user_id)
);
CREATE INDEX members_user ON members (user_id);

CREATE TABLE member_roles (
    guild_id uuid NOT NULL,
    user_id  uuid NOT NULL,
    role_id  uuid NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    PRIMARY KEY (guild_id, user_id, role_id),
    FOREIGN KEY (guild_id, user_id) REFERENCES members (guild_id, user_id) ON DELETE CASCADE
);

-- type: 0 text, 2 voice, 4 category, 1 dm, 3 group_dm.
-- Guild channels have guild_id; DMs do not.
CREATE TABLE channels (
    id         uuid PRIMARY KEY,
    guild_id   uuid REFERENCES guilds (id) ON DELETE CASCADE,
    type       smallint NOT NULL CHECK (type IN (0, 1, 2, 3, 4)),
    name       text CHECK (char_length(name) BETWEEN 1 AND 100),
    topic      text CHECK (char_length(topic) <= 1024),
    parent_id  uuid REFERENCES channels (id) ON DELETE SET NULL,
    position   int NOT NULL DEFAULT 0,
    user_limit int NOT NULL DEFAULT 0 CHECK (user_limit BETWEEN 0 AND 99),
    bitrate    int NOT NULL DEFAULT 64000 CHECK (bitrate BETWEEN 8000 AND 384000),
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((guild_id IS NULL) = (type IN (1, 3)))
);
CREATE INDEX channels_guild ON channels (guild_id, position);

-- target_type: 1 role, 2 member (matches perm.TargetType).
CREATE TABLE permission_overwrites (
    channel_id  uuid NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    target_type smallint NOT NULL CHECK (target_type IN (1, 2)),
    target_id   uuid NOT NULL,
    allow       bigint NOT NULL DEFAULT 0,
    deny        bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (channel_id, target_type, target_id)
);

-- Events written in the same transaction as the change that caused them.
-- A relay publishes them to NATS in id order and marks published_at.
CREATE TABLE outbox (
    id           bigserial PRIMARY KEY,
    topic        text NOT NULL,
    type         text NOT NULL,
    payload      jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);
CREATE INDEX outbox_unpublished ON outbox (id) WHERE published_at IS NULL;

-- +goose Down
DROP TABLE outbox;
DROP TABLE permission_overwrites;
DROP TABLE channels;
DROP TABLE member_roles;
DROP TABLE members;
DROP TABLE roles;
DROP TABLE guilds;
