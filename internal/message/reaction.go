package message

import (
	"context"
	_ "embed"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
)

// Reactions and pins (docs/adr/0037-reactions-pins.md).
const (
	maxReactionEmoji = 20
	maxPins          = 250
	defaultReactors  = 25
	maxReactors      = 100
)

//go:embed emoji.txt
var emojiList string

// emojiSet holds every emoji a reaction may be: fully-qualified Unicode
// emoji, skin tones included (scripts/gen-emoji.py).
var emojiSet = func() map[string]bool {
	set := map[string]bool{}
	for _, line := range strings.Split(emojiList, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			set[line] = true
		}
	}
	return set
}()

var (
	ruleReact          = ratelimit.Rule{Name: "reaction", Limit: 20, Window: 5 * time.Second}
	errBadEmoji        = httpx.Validation(map[string]string{"emoji": "Нужен один эмодзи Unicode"})
	errTooManyReaction = httpx.Validation(map[string]string{"emoji": "Не больше 20 разных реакций на сообщение"})
	errTooManyPins     = httpx.Validation(map[string]string{"message": "В канале не больше 250 закреплённых сообщений"})
)

// Reaction is one emoji under a message: how many put it and whether the
// reader is among them.
type Reaction struct {
	Emoji string `json:"emoji"`
	Count int    `json:"count"`
	Me    bool   `json:"me"`
}

// ReactionEvent is the payload of MESSAGE_REACTION_*: emoji is empty when
// all reactions went, user_id is set when one person's reaction changed.
type ReactionEvent struct {
	ChannelID uuid.UUID  `json:"channel_id"`
	GuildID   *uuid.UUID `json:"guild_id,omitempty"`
	MessageID uuid.UUID  `json:"message_id"`
	Emoji     string     `json:"emoji,omitempty"`
	UserID    *uuid.UUID `json:"user_id,omitempty"`
}

// inMessage resolves the channel, checks that the caller may read its
// history and runs fn in a transaction with the message locked.
func (s *Service) inMessage(ctx context.Context, userID, channelID, messageID uuid.UUID, fn func(tx pgx.Tx, ch channel, m Message) error) error {
	ch, err := s.resolve(ctx, userID, channelID)
	if err != nil {
		return err
	}
	if !ch.perms.Has(perm.ReadMessageHistory) {
		return httpx.ErrForbidden
	}
	return pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		m, err := getMessage(ctx, tx, channelID, messageID, true)
		if err != nil {
			return err
		}
		return fn(tx, ch, m)
	})
}

// React puts the caller's emoji under a message. A new emoji needs
// ADD_REACTIONS; joining one already there needs only to see the message,
// as in Discord, except in a DM, where a block takes reactions away.
func (s *Service) React(ctx context.Context, userID, channelID, messageID uuid.UUID, emoji string) error {
	if !emojiSet[emoji] {
		return errBadEmoji
	}
	if err := s.limiter.Check(ctx, ruleReact, userID.String()); err != nil {
		return err
	}
	return s.inMessage(ctx, userID, channelID, messageID, func(tx pgx.Tx, ch channel, _ Message) error {
		var there, distinct int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE emoji = $2), count(DISTINCT emoji) FROM message_reactions WHERE message_id = $1`,
			messageID, emoji).Scan(&there, &distinct); err != nil {
			return err
		}
		if (there == 0 || ch.guildID == nil) && !ch.perms.Has(perm.AddReactions) {
			return httpx.ErrForbidden
		}
		if there == 0 && distinct >= maxReactionEmoji {
			return errTooManyReaction
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO message_reactions (message_id, emoji, user_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			messageID, emoji, userID)
		if err != nil || tag.RowsAffected() == 0 {
			return err // already there: nothing changes
		}
		return events.Write(ctx, tx, events.Event{Topic: ch.topic, Type: events.ReactionAdd,
			Data: ReactionEvent{ChannelID: channelID, GuildID: ch.guildID, MessageID: messageID, Emoji: emoji, UserID: &userID}})
	})
}

// Unreact takes the caller's own emoji off a message.
func (s *Service) Unreact(ctx context.Context, userID, channelID, messageID uuid.UUID, emoji string) error {
	return s.inMessage(ctx, userID, channelID, messageID, func(tx pgx.Tx, ch channel, _ Message) error {
		tag, err := tx.Exec(ctx, `DELETE FROM message_reactions WHERE message_id = $1 AND emoji = $2 AND user_id = $3`,
			messageID, emoji, userID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return events.Write(ctx, tx, events.Event{Topic: ch.topic, Type: events.ReactionRemove,
			Data: ReactionEvent{ChannelID: channelID, GuildID: ch.guildID, MessageID: messageID, Emoji: emoji, UserID: &userID}})
	})
}

// ClearReactions is moderation: everyone's emoji, or every reaction when
// emoji is empty. Needs MANAGE_MESSAGES, so it does not exist in DMs.
func (s *Service) ClearReactions(ctx context.Context, userID, channelID, messageID uuid.UUID, emoji string) error {
	return s.inMessage(ctx, userID, channelID, messageID, func(tx pgx.Tx, ch channel, _ Message) error {
		if !ch.perms.Has(perm.ManageMessages) {
			return httpx.ErrForbidden
		}
		typ := events.ReactionRemoveAll
		sql, args := `DELETE FROM message_reactions WHERE message_id = $1`, []any{messageID}
		if emoji != "" {
			typ = events.ReactionRemoveEmoji
			sql, args = sql+` AND emoji = $2`, append(args, emoji)
		}
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return events.Write(ctx, tx, events.Event{Topic: ch.topic, Type: typ,
			Data: ReactionEvent{ChannelID: channelID, GuildID: ch.guildID, MessageID: messageID, Emoji: emoji}})
	})
}

// Reactors lists who put emoji under a message, earliest first.
func (s *Service) Reactors(ctx context.Context, userID, channelID, messageID uuid.UUID, emoji string, limit int) ([]auth.PublicUser, error) {
	if limit <= 0 || limit > maxReactors {
		limit = defaultReactors
	}
	var ids []uuid.UUID
	err := s.inMessage(ctx, userID, channelID, messageID, func(tx pgx.Tx, _ channel, _ Message) error {
		rows, err := tx.Query(ctx,
			`SELECT user_id FROM message_reactions WHERE message_id = $1 AND emoji = $2 ORDER BY created_at, user_id LIMIT $3`,
			messageID, emoji, limit)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	if err != nil {
		return nil, err
	}
	users, err := s.users.Users(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]auth.PublicUser, 0, len(ids))
	for _, id := range ids {
		if u, ok := users[id]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}

// withReactions fills in the reactions of messages as viewer sees them.
// Events carry none (null): who reads them differs, so clients keep what
// they have and follow MESSAGE_REACTION_* instead.
func withReactions(ctx context.Context, q querier, ms []Message, viewer uuid.UUID) error {
	ids := make([]uuid.UUID, len(ms))
	at := make(map[uuid.UUID]int, len(ms))
	for i := range ms {
		ids[i], at[ms[i].ID] = ms[i].ID, i
		ms[i].Reactions = []Reaction{}
	}
	rows, err := q.Query(ctx,
		`SELECT message_id, emoji, count(*), bool_or(user_id = $2) FROM message_reactions
		 WHERE message_id = ANY($1) GROUP BY message_id, emoji ORDER BY message_id, min(created_at), emoji`, ids, viewer)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var r Reaction
		if err := rows.Scan(&id, &r.Emoji, &r.Count, &r.Me); err != nil {
			return err
		}
		ms[at[id]].Reactions = append(ms[at[id]].Reactions, r)
	}
	return rows.Err()
}

// Pin pins (pin = true) or unpins a message. Needs PIN_MESSAGES and to see
// the message; the change goes out as MESSAGE_UPDATE with pinned_at.
func (s *Service) Pin(ctx context.Context, userID, channelID, messageID uuid.UUID, pin bool) error {
	return s.inMessage(ctx, userID, channelID, messageID, func(tx pgx.Tx, ch channel, m Message) error {
		if !ch.perms.Has(perm.PinMessages) {
			return httpx.ErrForbidden
		}
		if (m.PinnedAt != nil) == pin {
			return nil
		}
		if pin {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM messages WHERE channel_id = $1 AND pinned_at IS NOT NULL`, channelID).Scan(&n); err != nil {
				return err
			}
			if n >= maxPins {
				return errTooManyPins
			}
		}
		if err := tx.QueryRow(ctx,
			`UPDATE messages SET pinned_at = CASE WHEN $2 THEN now() END WHERE id = $1 RETURNING pinned_at`,
			messageID, pin).Scan(&m.PinnedAt); err != nil {
			return err
		}
		m, err := s.fillOne(ctx, tx, m)
		if err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{Topic: ch.topic, Type: events.MessageUpdate, Data: m})
	})
}

// Pins lists the pinned messages of a channel, last pinned first.
func (s *Service) Pins(ctx context.Context, userID, channelID uuid.UUID) ([]Message, error) {
	ch, err := s.resolve(ctx, userID, channelID)
	if err != nil {
		return nil, err
	}
	if !ch.perms.Has(perm.ReadMessageHistory) {
		return nil, httpx.ErrForbidden
	}
	rows, err := s.pg.Query(ctx,
		`SELECT `+msgCols+` FROM messages WHERE channel_id = $1 AND pinned_at IS NOT NULL ORDER BY pinned_at DESC LIMIT $2`,
		channelID, maxPins)
	if err != nil {
		return nil, err
	}
	ms, err := pgx.CollectRows(rows, scanMessage)
	if err != nil {
		return nil, err
	}
	if err := s.fill(ctx, s.pg, ms); err != nil {
		return nil, err
	}
	return ms, withReactions(ctx, s.pg, ms, userID)
}
