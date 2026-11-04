-- +goose Up
-- Search in messages (docs/adr/0039-search.md): Russian stems for words
-- ("кошки" finds "кошка") and the words as written for names, nicks and
-- anything the stemmer does not know. Generated, so it can never disagree
-- with the text.
ALTER TABLE messages ADD COLUMN search tsvector
    GENERATED ALWAYS AS (to_tsvector('russian', content) || to_tsvector('simple', content)) STORED;
CREATE INDEX messages_search ON messages USING gin (search);

-- +goose Down
DROP INDEX messages_search;
ALTER TABLE messages DROP COLUMN search;
