-- +goose Up
-- Owner statistics (docs/adr/0042-owner-stats.md): counts only, nothing
-- about what anyone wrote or where they came from beyond a link tag.

-- The tag of the link a user signed up through (?ref=stream).
ALTER TABLE users ADD COLUMN ref text CHECK (ref ~ '^[a-z0-9_-]{1,32}$');

-- The days (Moscow time) a user opened the app, for daily and weekly
-- active users. Owned by auth; rows older than 90 days are dropped.
CREATE TABLE user_days (
    day     date NOT NULL,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    PRIMARY KEY (day, user_id)
);

-- Visits of the site per day and link tag ('' without one), with no
-- cookies and no addresses. Owned by stats.
CREATE TABLE site_visits (
    day   date    NOT NULL,
    ref   text    NOT NULL CHECK (ref ~ '^[a-z0-9_-]{0,32}$'),
    count integer NOT NULL DEFAULT 0,
    PRIMARY KEY (day, ref)
);

-- +goose Down
DROP TABLE site_visits;
DROP TABLE user_days;
ALTER TABLE users DROP COLUMN ref;
