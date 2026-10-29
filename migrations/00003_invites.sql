-- +goose Up

-- An invite brings a user into a guild and points the client at a channel.
-- max_uses 0 and expires_at NULL mean no limit.
CREATE TABLE invites (
    code       text PRIMARY KEY CHECK (code ~ '^[A-Za-z0-9]{8,16}$'),
    guild_id   uuid NOT NULL REFERENCES guilds (id) ON DELETE CASCADE,
    channel_id uuid NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    inviter_id uuid REFERENCES users (id) ON DELETE SET NULL,
    max_uses   int  NOT NULL DEFAULT 0 CHECK (max_uses BETWEEN 0 AND 100),
    uses       int  NOT NULL DEFAULT 0 CHECK (uses >= 0),
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX invites_guild ON invites (guild_id);

-- +goose Down
DROP TABLE invites;
