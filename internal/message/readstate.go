package message

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
)

// Unread messages and read marks (docs/adr/0038-read-states.md).

// mentionWindow is how many of the newest unread messages of a channel are
// looked at for mentions: the count is exact up to it, so clients show
// "99+" from there on, and a long-unread busy channel costs one short
// index scan.
const mentionWindow = 100

// ruleAck bounds read marks; clients send one at most every few seconds
// per channel, so above it they are dropped silently, like typing.
var ruleAck = ratelimit.Rule{Name: "ack", Limit: 30, Window: 10 * time.Second}

// ReadState is how far the user has read a channel and what is newer.
type ReadState struct {
	ChannelID uuid.UUID `json:"channel_id"`
	// LastReadID is the newest message the user has seen; null if never.
	LastReadID *uuid.UUID `json:"last_read_id"`
	// LastMessageID is the newest message of the channel; null if empty.
	LastMessageID *uuid.UUID `json:"last_message_id"`
	// MentionCount counts unread messages that ping the user, among the
	// newest mentionWindow unread ones.
	MentionCount int `json:"mention_count"`
}

// Ack is the payload of MESSAGE_ACK, sent to the user's own sessions.
type Ack struct {
	ChannelID uuid.UUID `json:"channel_id"`
	MessageID uuid.UUID `json:"message_id"`
}

// ReadStates returns the read state of every channel the user may read:
// guild text channels with READ_MESSAGE_HISTORY and their DMs.
func (s *Service) ReadStates(ctx context.Context, userID uuid.UUID) ([]ReadState, error) {
	channels, roles, err := s.guilds.Readable(ctx, userID, nil)
	if err != nil {
		return nil, err
	}
	dms, err := s.dms.ChannelIDs(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.Query(ctx,
		`SELECT c.id, rs.last_read_id,
		        (SELECT m.id FROM messages m WHERE m.channel_id = c.id ORDER BY m.id DESC LIMIT 1),
		        (SELECT count(*) FROM (
		            SELECT m.author_id, m.mention_everyone, m.mentions, m.mention_roles FROM messages m
		            WHERE m.channel_id = c.id AND (rs.last_read_id IS NULL OR m.id > rs.last_read_id)
		            ORDER BY m.id DESC LIMIT $4) u
		         WHERE u.author_id IS DISTINCT FROM $1
		           AND (u.mention_everyone OR $1 = ANY (u.mentions) OR u.mention_roles && $2::uuid[]))
		 FROM unnest($3::uuid[]) AS c(id)
		 LEFT JOIN read_states rs ON rs.user_id = $1 AND rs.channel_id = c.id`,
		userID, roles, append(channels, dms...), mentionWindow)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ReadState, error) {
		var st ReadState
		err := r.Scan(&st.ChannelID, &st.LastReadID, &st.LastMessageID, &st.MentionCount)
		return st, err
	})
}

// Ack marks a channel read up to a message of it. Marks only move forward;
// a mark that moves tells the user's other sessions (MESSAGE_ACK).
func (s *Service) Ack(ctx context.Context, userID, channelID, messageID uuid.UUID) error {
	ch, err := s.readable(ctx, userID, channelID)
	if err != nil {
		return err
	}
	if ok, _, err := s.limiter.Allow(ctx, ruleAck, userID.String()); err != nil || !ok {
		return err
	}
	return pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		if err := s.lockChannel(ctx, tx, ch); err != nil {
			return err
		}
		if _, err := getMessage(ctx, tx, channelID, messageID, false); err != nil {
			return err
		}
		return ack(ctx, tx, userID, []uuid.UUID{channelID}, true, `$3::uuid`, messageID)
	})
}

// AckGuild marks every channel of a guild the user may read as read up to
// its newest message ("Отметить как прочитанное" in the guild menu).
func (s *Service) AckGuild(ctx context.Context, userID, guildID uuid.UUID) error {
	channels, _, err := s.guilds.Readable(ctx, userID, &guildID)
	if err != nil {
		return err
	}
	if len(channels) == 0 {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		channels, err := s.guilds.LockChannels(ctx, tx, channels)
		if err != nil {
			return err
		}
		return ack(ctx, tx, userID, channels, true,
			`(SELECT m.id FROM messages m WHERE m.channel_id = c.id ORDER BY m.id DESC LIMIT 1)`)
	})
}

// ack moves the user's marks in channels to the message newest (an SQL
// expression over c.id and args after $2) where that is further. With
// notify it writes MESSAGE_ACK for each mark that moved; a sender's own
// message needs none, their sessions get MESSAGE_CREATE.
func ack(ctx context.Context, tx pgx.Tx, userID uuid.UUID, channels []uuid.UUID, notify bool, newest string, args ...any) error {
	rows, err := tx.Query(ctx,
		`INSERT INTO read_states AS rs (user_id, channel_id, last_read_id)
		 SELECT $1, c.id, n.id FROM unnest($2::uuid[]) AS c(id), LATERAL (SELECT `+newest+` AS id) n
		 WHERE n.id IS NOT NULL
		 ON CONFLICT (user_id, channel_id) DO UPDATE SET last_read_id = EXCLUDED.last_read_id, updated_at = now()
		 WHERE rs.last_read_id < EXCLUDED.last_read_id
		 RETURNING channel_id, last_read_id`, append([]any{userID, channels}, args...)...)
	if err != nil {
		return err
	}
	moved, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Ack, error) {
		var a Ack
		err := r.Scan(&a.ChannelID, &a.MessageID)
		return a, err
	})
	if err != nil || !notify {
		return err
	}
	for _, a := range moved {
		if err := events.Write(ctx, tx, events.Event{Topic: events.UserTopic(userID), Type: events.MessageAck, Data: a}); err != nil {
			return err
		}
	}
	return nil
}
