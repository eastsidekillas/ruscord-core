-- +goose Up
-- Muted or deafened by a moderator (MUTE_MEMBERS, DEAFEN_MEMBERS). It belongs
-- to the member, not to the connection, so leaving and coming back does not
-- undo it, like in Discord. The core takes the microphone or the incoming
-- sound away in LiveKit's permissions and in the join token, so a client
-- cannot undo it either (docs/adr/0026-voice-moderation.md).
ALTER TABLE members
    ADD COLUMN voice_mute boolean NOT NULL DEFAULT false,
    ADD COLUMN voice_deaf boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE members DROP COLUMN voice_mute, DROP COLUMN voice_deaf;
