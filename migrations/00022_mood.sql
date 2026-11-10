-- +goose Up
-- Profile mood (a state, an optional short text and an optional expiry) and
-- the colour of the profile card. A mood without a state carries no text or
-- expiry.
ALTER TABLE users
    ADD COLUMN mood_state text CHECK (mood_state IN
        ('happy', 'laughing', 'thinking', 'sleeping', 'listening', 'surprised', 'confused', 'angry')),
    ADD COLUMN mood_text text NOT NULL DEFAULT '' CHECK (char_length(mood_text) <= 80),
    ADD COLUMN mood_expires_at timestamptz,
    ADD COLUMN profile_color text NOT NULL DEFAULT 'accent' CHECK (profile_color IN
        ('accent', 'success', 'warning', 'danger', 'neutral')),
    ADD CONSTRAINT users_mood_needs_state CHECK (mood_state IS NOT NULL OR (mood_text = '' AND mood_expires_at IS NULL));

-- +goose Down
ALTER TABLE users
    DROP CONSTRAINT users_mood_needs_state,
    DROP COLUMN profile_color,
    DROP COLUMN mood_expires_at,
    DROP COLUMN mood_text,
    DROP COLUMN mood_state;
