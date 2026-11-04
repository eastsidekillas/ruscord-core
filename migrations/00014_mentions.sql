-- +goose Up
-- Mentions of a message (docs/adr/0035-mentions.md): the users and roles it
-- pings, checked against the channel when it is sent or edited. The text
-- keeps them as <@id> and <@&id>; these columns are what notifications and
-- unread counters read, so they never hold an id the author may not ping.
ALTER TABLE messages
    ADD COLUMN mentions      uuid[] NOT NULL DEFAULT '{}',
    ADD COLUMN mention_roles uuid[] NOT NULL DEFAULT '{}',
    -- A reply pings the author of the message it answers unless the sender
    -- turned it off; an edit keeps that choice.
    ADD COLUMN reply_mention boolean NOT NULL DEFAULT false;

-- Anyone may ping a mentionable role; other roles only with MENTION_EVERYONE.
ALTER TABLE roles ADD COLUMN mentionable boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE roles DROP COLUMN mentionable;
ALTER TABLE messages DROP COLUMN reply_mention, DROP COLUMN mention_roles, DROP COLUMN mentions;
