-- +goose Up
-- Who is in which voice channel of a guild (docs/adr/0025-voice.md). LiveKit
-- is the truth; these rows follow its webhooks and a periodic check, and
-- live under the guild lock like the rest of a guild's state, so snapshots
-- and VOICE_STATE_UPDATE events agree (ADR-0016). One room per user.
CREATE TABLE voice_states (
    user_id     uuid PRIMARY KEY REFERENCES users (id),
    guild_id    uuid NOT NULL REFERENCES guilds (id) ON DELETE CASCADE,
    channel_id  uuid NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    -- The LiveKit participant sid: a "left" of an older connection must
    -- not clear the state of a newer one.
    participant text NOT NULL CHECK (char_length(participant) <= 64),
    self_mute   boolean NOT NULL DEFAULT false,
    self_deaf   boolean NOT NULL DEFAULT false,
    joined_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX voice_states_guild ON voice_states (guild_id);
CREATE INDEX voice_states_channel ON voice_states (channel_id);

-- +goose Down
DROP TABLE voice_states;
