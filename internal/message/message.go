// Package message owns messages in guild text channels and direct messages.
// Channels belong to other modules; this one asks them for permissions
// through interfaces (ADR-0015).
package message

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/slicex"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
)

const (
	maxContent   = 4000
	maxNonce     = 64
	defaultLimit = 50
	maxLimit     = 100
)

var (
	errNotText    = httpx.NewError(http.StatusBadRequest, "not_text_channel", "Сообщения есть только в текстовых каналах: здесь их нельзя ни читать, ни писать")
	errNotAuthor  = httpx.NewError(http.StatusForbidden, "forbidden", "Редактировать можно только свои сообщения")
	errBadReply   = httpx.Validation(map[string]string{"reply_to": "Сообщение не найдено в этом канале"})
	ruleSend      = ratelimit.Rule{Name: "message_send", Limit: 10, Window: 10 * time.Second}
	everyoneRegex = regexp.MustCompile(`@(everyone|here)\b`)
)

// GuildChannels is what message needs from the guild module.
type GuildChannels interface {
	ChannelPermissions(ctx context.Context, userID, channelID uuid.UUID) (uuid.UUID, guild.ChannelType, perm.Permissions, error)
}

// DMChannels is what message needs from the social module.
type DMChannels interface {
	Permissions(ctx context.Context, userID, channelID uuid.UUID) (perm.Permissions, []uuid.UUID, error)
}

type Service struct {
	pg      *pgxpool.Pool
	users   auth.Directory
	guilds  GuildChannels
	dms     DMChannels
	limiter *ratelimit.Limiter
	eph     events.EphemeralPublisher
}

func NewService(pg *pgxpool.Pool, users auth.Directory, guilds GuildChannels, dms DMChannels, limiter *ratelimit.Limiter, eph events.EphemeralPublisher) *Service {
	return &Service{pg: pg, users: users, guilds: guilds, dms: dms, limiter: limiter, eph: eph}
}

type Message struct {
	ID              uuid.UUID        `json:"id"`
	ChannelID       uuid.UUID        `json:"channel_id"`
	GuildID         *uuid.UUID       `json:"guild_id,omitempty"`
	AuthorID        *uuid.UUID       `json:"-"`
	Author          *auth.PublicUser `json:"author"`
	Content         string           `json:"content"`
	Nonce           *string          `json:"nonce,omitempty"`
	ReplyTo         *uuid.UUID       `json:"reply_to,omitempty"`
	MentionEveryone bool             `json:"mention_everyone"`
	CreatedAt       time.Time        `json:"created_at"`
	EditedAt        *time.Time       `json:"edited_at,omitempty"`
}

// channel is a resolved channel: where it lives, what the caller may do in
// it and which topic its events go to.
type channel struct {
	id      uuid.UUID
	guildID *uuid.UUID
	perms   perm.Permissions
	topic   string
	// ephTopic carries ephemeral events (typing) of the channel.
	ephTopic string
}

// resolve finds the channel in the guild module, then in the social module.
// Channel ids are UUIDv7 in both, so they never collide.
func (s *Service) resolve(ctx context.Context, userID, channelID uuid.UUID) (channel, error) {
	gid, typ, p, err := s.guilds.ChannelPermissions(ctx, userID, channelID)
	if err == nil {
		if typ != guild.ChannelText {
			return channel{}, errNotText
		}
		return channel{id: channelID, guildID: &gid, perms: p, topic: events.GuildTopic(gid), ephTopic: events.EphGuildTopic(gid)}, nil
	}
	if !errors.Is(err, httpx.ErrNotFound) {
		return channel{}, err
	}
	p, _, err = s.dms.Permissions(ctx, userID, channelID)
	if err != nil {
		return channel{}, err
	}
	return channel{id: channelID, perms: p, topic: events.ChannelTopic(channelID), ephTopic: events.EphChannelTopic(channelID)}, nil
}

const msgCols = `id, channel_id, guild_id, author_id, content, nonce, reply_to, mention_everyone, created_at, edited_at`

func scanMessage(r pgx.CollectableRow) (Message, error) {
	var m Message
	err := r.Scan(&m.ID, &m.ChannelID, &m.GuildID, &m.AuthorID, &m.Content, &m.Nonce, &m.ReplyTo,
		&m.MentionEveryone, &m.CreatedAt, &m.EditedAt)
	return m, err
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func getMessage(ctx context.Context, q querier, channelID, id uuid.UUID, lock bool) (Message, error) {
	sql := `SELECT ` + msgCols + ` FROM messages WHERE channel_id = $1 AND id = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	rows, err := q.Query(ctx, sql, channelID, id)
	if err != nil {
		return Message{}, err
	}
	m, err := pgx.CollectExactlyOneRow(rows, scanMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, httpx.ErrNotFound
	}
	return m, err
}

func (s *Service) withAuthors(ctx context.Context, ms []Message) error {
	return auth.Attach(ctx, s.users, slicex.Ptrs(ms), func(m *Message) *uuid.UUID { return m.AuthorID },
		func(m *Message, u auth.PublicUser) { m.Author = &u })
}

func (s *Service) withAuthor(ctx context.Context, m Message) (Message, error) {
	ms := []Message{m}
	err := s.withAuthors(ctx, ms)
	return ms[0], err
}

func validContent(content string) (string, error) {
	content = strings.TrimSpace(content)
	fields := httpx.Fields{}
	fields.Length("content", content, 1, maxContent)
	return content, fields.Err()
}

type SendInput struct {
	Content string     `json:"content"`
	Nonce   *string    `json:"nonce"`
	ReplyTo *uuid.UUID `json:"reply_to"`
}

// Send posts a message. A retry with the same nonce returns the message the
// first attempt created (created = false) instead of a duplicate.
func (s *Service) Send(ctx context.Context, userID, channelID uuid.UUID, in SendInput) (Message, bool, error) {
	content, err := validContent(in.Content)
	if err != nil {
		return Message{}, false, err
	}
	if in.Nonce != nil && (len(*in.Nonce) == 0 || len(*in.Nonce) > maxNonce || strings.ContainsRune(*in.Nonce, 0)) {
		return Message{}, false, httpx.Validation(map[string]string{"nonce": "От 1 до 64 байт"})
	}
	ch, err := s.resolve(ctx, userID, channelID)
	if err != nil {
		return Message{}, false, err
	}
	if !ch.perms.Has(perm.SendMessages) {
		return Message{}, false, httpx.ErrForbidden
	}
	if in.Nonce != nil {
		if m, err := s.byNonce(ctx, channelID, userID, *in.Nonce); err == nil {
			return m, false, nil
		} else if !errors.Is(err, httpx.ErrNotFound) {
			return Message{}, false, err
		}
	}
	if err := s.limiter.Check(ctx, ruleSend, userID.String()); err != nil {
		return Message{}, false, err
	}

	m := Message{
		ID: uuid.Must(uuid.NewV7()), ChannelID: channelID, GuildID: ch.guildID, AuthorID: &userID,
		Content: content, Nonce: in.Nonce, ReplyTo: in.ReplyTo,
		MentionEveryone: ch.perms.Has(perm.MentionEveryone) && everyoneRegex.MatchString(content),
	}
	err = pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		if in.ReplyTo != nil {
			if _, err := getMessage(ctx, tx, channelID, *in.ReplyTo, false); err != nil {
				if errors.Is(err, httpx.ErrNotFound) {
					return errBadReply
				}
				return err
			}
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO messages (id, channel_id, guild_id, author_id, content, nonce, reply_to, mention_everyone)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING created_at`,
			m.ID, m.ChannelID, m.GuildID, m.AuthorID, m.Content, m.Nonce, m.ReplyTo, m.MentionEveryone).
			Scan(&m.CreatedAt); err != nil {
			return err
		}
		var err error
		if m, err = s.withAuthor(ctx, m); err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{Topic: ch.topic, Type: events.MessageCreate, Data: m})
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && in.Nonce != nil {
		// Two retries raced; the other one won.
		m, err := s.byNonce(ctx, channelID, userID, *in.Nonce)
		return m, false, err
	}
	return m, err == nil, err
}

func (s *Service) byNonce(ctx context.Context, channelID, userID uuid.UUID, nonce string) (Message, error) {
	rows, err := s.pg.Query(ctx,
		`SELECT `+msgCols+` FROM messages WHERE channel_id = $1 AND author_id = $2 AND nonce = $3`, channelID, userID, nonce)
	if err != nil {
		return Message{}, err
	}
	m, err := pgx.CollectExactlyOneRow(rows, scanMessage)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, httpx.ErrNotFound
	}
	if err != nil {
		return Message{}, err
	}
	return s.withAuthor(ctx, m)
}

type Page struct {
	Before *uuid.UUID
	After  *uuid.UUID
	Limit  int
}

// History returns messages newest first. With After it returns the messages
// right after that id (still newest first), for catching up.
func (s *Service) History(ctx context.Context, userID, channelID uuid.UUID, pg Page) ([]Message, error) {
	ch, err := s.resolve(ctx, userID, channelID)
	if err != nil {
		return nil, err
	}
	if !ch.perms.Has(perm.ReadMessageHistory) {
		return nil, httpx.ErrForbidden
	}
	if pg.Limit <= 0 || pg.Limit > maxLimit {
		pg.Limit = defaultLimit
	}
	var rows pgx.Rows
	switch {
	case pg.After != nil:
		rows, err = s.pg.Query(ctx,
			`SELECT * FROM (SELECT `+msgCols+` FROM messages WHERE channel_id = $1 AND id > $2 ORDER BY id LIMIT $3) m ORDER BY id DESC`,
			channelID, *pg.After, pg.Limit)
	case pg.Before != nil:
		rows, err = s.pg.Query(ctx,
			`SELECT `+msgCols+` FROM messages WHERE channel_id = $1 AND id < $2 ORDER BY id DESC LIMIT $3`,
			channelID, *pg.Before, pg.Limit)
	default:
		rows, err = s.pg.Query(ctx,
			`SELECT `+msgCols+` FROM messages WHERE channel_id = $1 ORDER BY id DESC LIMIT $2`, channelID, pg.Limit)
	}
	if err != nil {
		return nil, err
	}
	ms, err := pgx.CollectRows(rows, scanMessage)
	if err != nil {
		return nil, err
	}
	return ms, s.withAuthors(ctx, ms)
}

// Edit changes the content of the caller's own message.
func (s *Service) Edit(ctx context.Context, userID, channelID, id uuid.UUID, content string) (Message, error) {
	content, err := validContent(content)
	if err != nil {
		return Message{}, err
	}
	ch, err := s.resolve(ctx, userID, channelID)
	if err != nil {
		return Message{}, err
	}
	var m Message
	err = pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		var err error
		if m, err = getMessage(ctx, tx, channelID, id, true); err != nil {
			return err
		}
		if m.AuthorID == nil || *m.AuthorID != userID {
			return errNotAuthor
		}
		// Editing needs the right to write here now, not only when it was sent.
		if !ch.perms.Has(perm.SendMessages) {
			return httpx.ErrForbidden
		}
		m.Content = content
		m.MentionEveryone = ch.perms.Has(perm.MentionEveryone) && everyoneRegex.MatchString(content)
		if err := tx.QueryRow(ctx,
			`UPDATE messages SET content = $2, mention_everyone = $3, edited_at = now() WHERE id = $1 RETURNING edited_at`,
			id, m.Content, m.MentionEveryone).Scan(&m.EditedAt); err != nil {
			return err
		}
		if m, err = s.withAuthor(ctx, m); err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{Topic: ch.topic, Type: events.MessageUpdate, Data: m})
	})
	return m, err
}

// Delete removes a message: the author may delete their own, MANAGE_MESSAGES
// allows deleting anyone's in a guild channel.
func (s *Service) Delete(ctx context.Context, userID, channelID, id uuid.UUID) error {
	ch, err := s.resolve(ctx, userID, channelID)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		m, err := getMessage(ctx, tx, channelID, id, true)
		if err != nil {
			return err
		}
		own := m.AuthorID != nil && *m.AuthorID == userID
		if !own && !ch.perms.Has(perm.ManageMessages) {
			return httpx.ErrForbidden
		}
		if _, err := tx.Exec(ctx, `DELETE FROM messages WHERE id = $1`, id); err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{Topic: ch.topic, Type: events.MessageDelete,
			Data: map[string]any{"id": id, "channel_id": channelID, "guild_id": ch.guildID}})
	})
}

// DeleteChannels is the hooks.ChannelsDeleted for guild and social.
func DeleteChannels(ctx context.Context, tx pgx.Tx, channelIDs []uuid.UUID) error {
	_, err := tx.Exec(ctx, `DELETE FROM messages WHERE channel_id = ANY($1)`, channelIDs)
	return err
}

// ruleTyping is how often one user's typing in one channel is relayed;
// clients show the indicator for 10 s and resend while typing.
var ruleTyping = ratelimit.Rule{Name: "typing", Limit: 1, Window: 5 * time.Second}

// Typing is the payload of TYPING_START.
type Typing struct {
	ChannelID uuid.UUID  `json:"channel_id"`
	GuildID   *uuid.UUID `json:"guild_id,omitempty"`
	UserID    uuid.UUID  `json:"user_id"`
	Timestamp time.Time  `json:"timestamp"`
}

// Typing tells the people who see the channel that userID is typing. It
// needs SEND_MESSAGES there. Calls above the rate are dropped silently:
// the indicator is already showing, and a 429 would only make clients retry.
func (s *Service) Typing(ctx context.Context, userID, channelID uuid.UUID) error {
	ch, err := s.resolve(ctx, userID, channelID)
	if err != nil {
		return err
	}
	if !ch.perms.Has(perm.SendMessages) {
		return httpx.ErrForbidden
	}
	ok, _, err := s.limiter.Allow(ctx, ruleTyping, userID.String()+":"+channelID.String())
	if err != nil || !ok {
		return err
	}
	return s.eph.Publish(ch.ephTopic, events.TypingStart,
		Typing{ChannelID: channelID, GuildID: ch.guildID, UserID: userID, Timestamp: time.Now().UTC()})
}
