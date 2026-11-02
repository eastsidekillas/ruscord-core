-- +goose Up
-- Calls in DM channels, 1:1 and groups (docs/adr/0032-dm-calls.md): at most
-- one per channel, in LiveKit room call:<id> (ADR-0007). The social module
-- owns them like the channels. LiveKit is the truth about who is in a call;
-- members follow its webhooks and a periodic check.
CREATE TABLE dm_calls (
    id         uuid PRIMARY KEY,
    channel_id uuid NOT NULL UNIQUE REFERENCES dm_channels (id) ON DELETE CASCADE,
    started_by uuid NOT NULL REFERENCES users (id),
    started_at timestamptz NOT NULL DEFAULT now(),
    -- Recipients being rung until ring_until; answering or declining takes
    -- one out. Clients stop ringing at ring_until by themselves.
    ringing    uuid[] NOT NULL DEFAULT '{}',
    ring_until timestamptz NOT NULL
);

CREATE TABLE dm_call_members (
    call_id     uuid NOT NULL REFERENCES dm_calls (id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users (id),
    -- The LiveKit participant sid: a "left" of an older connection must not
    -- clear a newer one.
    participant text NOT NULL CHECK (char_length(participant) <= 64),
    joined_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (call_id, user_id)
);

-- +goose Down
DROP TABLE dm_call_members;
DROP TABLE dm_calls;
