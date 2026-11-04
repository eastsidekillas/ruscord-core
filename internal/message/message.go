// Package message owns messages in guild text channels and direct messages.
// Channels belong to other modules; this one asks them for permissions
// through interfaces (ADR-0015).
package message

import (
	"context"
	"errors"
	"net/http"
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
	"github.com/eastsidekillas/ruscord-core/internal/platform/blob"
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
	errNotText   = httpx.NewError(http.StatusBadRequest, "not_text_channel", "Сообщения есть только в текстовых каналах: здесь их нельзя ни читать, ни писать")
	errNotAuthor = httpx.NewError(http.StatusForbidden, "forbidden", "Редактировать можно только свои сообщения")
	errBadReply  = httpx.Validation(map[string]string{"reply_to": "Сообщение не найдено в этом канале"})
	ruleSend     = ratelimit.Rule{Name: "message_send", Limit: 10, Window: 10 * time.Second}
)

// GuildChannels is what message needs from the guild module.
type GuildChannels interface {
	ChannelPermissions(ctx context.Context, userID, channelID uuid.UUID) (uuid.UUID, guild.ChannelType, perm.Permissions, error)
	// Mentions keeps the users who are members of the guild and the roles
	// of the guild the author may ping (any role when pingAll).
	Mentions(ctx context.Context, guildID uuid.UUID, users, roles []uuid.UUID, pingAll bool) ([]uuid.UUID, []uuid.UUID, error)
	// Readable lists the text channels the user may read (in one guild or
	// all) and their roles there.
	Readable(ctx context.Context, userID uuid.UUID, guildID *uuid.UUID) ([]uuid.UUID, []uuid.UUID, error)
}

// DMChannels is what message needs from the social module.
type DMChannels interface {
	Permissions(ctx context.Context, userID, channelID uuid.UUID) (perm.Permissions, []uuid.UUID, error)
	// ChannelIDs lists the DMs and groups of the user.
	ChannelIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
}

type Service struct {
	pg      *pgxpool.Pool
	users   auth.Directory
	guilds  GuildChannels
	dms     DMChannels
	limiter *ratelimit.Limiter
	eph     events.EphemeralPublisher
	// files and signer are set by WithFiles; nil turns attachments off.
	files  *blob.Collector
	signer *blob.Signer
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
	ReplyMention    bool             `json:"-"`
	MentionEveryone bool             `json:"mention_everyone"`
	// Mentions and MentionRoles are the users and roles the message pings,
	// checked when it was sent or last edited (ADR-0035).
	Mentions     []uuid.UUID `json:"mentions"`
	MentionRoles []uuid.UUID `json:"mention_roles"`
	// Attachments are the files of the message, in upload order (ADR-0036).
	Attachments []Attachment `json:"attachments"`
	// Reactions as the reader sees them; null in events (ADR-0037).
	Reactions []Reaction `json:"reactions"`
	// PinnedAt is when the message was pinned; absent if it is not.
	PinnedAt  *time.Time `json:"pinned_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	EditedAt  *time.Time `json:"edited_at,omitempty"`
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
	// recipients are the members of a DM; empty for guild channels.
	recipients []uuid.UUID
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
	p, recipients, err := s.dms.Permissions(ctx, userID, channelID)
	if err != nil {
		return channel{}, err
	}
	return channel{id: channelID, perms: p, topic: events.ChannelTopic(channelID), ephTopic: events.EphChannelTopic(channelID),
		recipients: recipients}, nil
}

const msgCols = `id, channel_id, guild_id, author_id, content, nonce, reply_to, reply_mention, mention_everyone,
 mentions, mention_roles, created_at, edited_at, pinned_at`

func scanMessage(r pgx.CollectableRow) (Message, error) {
	var m Message
	err := r.Scan(&m.ID, &m.ChannelID, &m.GuildID, &m.AuthorID, &m.Content, &m.Nonce, &m.ReplyTo, &m.ReplyMention,
		&m.MentionEveryone, &m.Mentions, &m.MentionRoles, &m.CreatedAt, &m.EditedAt, &m.PinnedAt)
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

// fill completes messages read from q for clients: authors and attachments.
func (s *Service) fill(ctx context.Context, q querier, ms []Message) error {
	err := auth.Attach(ctx, s.users, slicex.Ptrs(ms), func(m *Message) *uuid.UUID { return m.AuthorID },
		func(m *Message, u auth.PublicUser) { m.Author = &u })
	if err != nil {
		return err
	}
	return s.withAttachments(ctx, q, ms)
}

func (s *Service) fillOne(ctx context.Context, q querier, m Message) (Message, error) {
	ms := []Message{m}
	err := s.fill(ctx, q, ms)
	return ms[0], err
}

// validContent trims the text, checks its length and finds its mentions.
// The text may be empty only when the message has files.
func validContent(content string, files bool) (string, parsed, error) {
	content = strings.TrimSpace(content)
	fields := httpx.Fields{}
	minLen := 1
	if files {
		minLen = 0
	}
	fields.Length("content", content, minLen, maxContent)
	if err := fields.Err(); err != nil {
		return "", parsed{}, err
	}
	p, err := parseMentions(content)
	return content, p, err
}

type SendInput struct {
	Content string     `json:"content"`
	Nonce   *string    `json:"nonce"`
	ReplyTo *uuid.UUID `json:"reply_to"`
	// ReplyMention pings the author of the replied message; true when absent.
	ReplyMention *bool `json:"reply_mention"`
	// Attachments are ids of the sender's uploads to this channel.
	Attachments []uuid.UUID `json:"attachments"`
}

// replyAuthor returns the author of the message a reply answers, to ping
// them, or nil when the reply does not ping or the author is gone or is
// the sender.
func replyAuthor(ctx context.Context, q querier, channelID, replyTo, sender uuid.UUID) (*uuid.UUID, error) {
	r, err := getMessage(ctx, q, channelID, replyTo, false)
	if err != nil {
		return nil, err
	}
	if r.AuthorID == nil || *r.AuthorID == sender {
		return nil, nil
	}
	return r.AuthorID, nil
}

// Send posts a message. A retry with the same nonce returns the message the
// first attempt created (created = false) instead of a duplicate.
func (s *Service) Send(ctx context.Context, userID, channelID uuid.UUID, in SendInput) (Message, bool, error) {
	if err := validAttachmentIDs(in.Attachments); err != nil {
		return Message{}, false, err
	}
	content, parsed, err := validContent(in.Content, len(in.Attachments) > 0)
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
		ReplyMention: in.ReplyTo != nil && (in.ReplyMention == nil || *in.ReplyMention),
	}
	err = pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		var pinged *uuid.UUID
		if in.ReplyTo != nil {
			author, err := replyAuthor(ctx, tx, channelID, *in.ReplyTo, userID)
			if errors.Is(err, httpx.ErrNotFound) {
				return errBadReply
			} else if err != nil {
				return err
			}
			if m.ReplyMention {
				pinged = author
			}
		}
		mn, err := s.resolveMentions(ctx, ch, parsed, pinged)
		if err != nil {
			return err
		}
		mn.apply(&m)
		if err := tx.QueryRow(ctx,
			`INSERT INTO messages (id, channel_id, guild_id, author_id, content, nonce, reply_to, reply_mention,
			                       mention_everyone, mentions, mention_roles)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING created_at`,
			m.ID, m.ChannelID, m.GuildID, m.AuthorID, m.Content, m.Nonce, m.ReplyTo, m.ReplyMention,
			m.MentionEveryone, m.Mentions, m.MentionRoles).
			Scan(&m.CreatedAt); err != nil {
			return err
		}
		if err := attach(ctx, tx, &m, userID, in.Attachments); err != nil {
			return err
		}
		// One's own message is read.
		if err := ack(ctx, tx, userID, []uuid.UUID{channelID}, false, `$3::uuid`, m.ID); err != nil {
			return err
		}
		if m, err = s.fillOne(ctx, tx, m); err != nil {
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
	return s.fillOne(ctx, s.pg, m)
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
	if err := s.fill(ctx, s.pg, ms); err != nil {
		return nil, err
	}
	return ms, withReactions(ctx, s.pg, ms, userID)
}

// Edit changes the content of the caller's own message.
func (s *Service) Edit(ctx context.Context, userID, channelID, id uuid.UUID, content string) (Message, error) {
	// Emptiness is checked once it is known whether the message has files.
	content, parsed, err := validContent(content, true)
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
		if content == "" {
			var files bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attachments WHERE message_id = $1)`, id).Scan(&files); err != nil {
				return err
			}
			if !files {
				return httpx.Validation(map[string]string{"content": "От 1 до 4000 символов"})
			}
		}
		var pinged *uuid.UUID
		if m.ReplyTo != nil && m.ReplyMention {
			// The replied message may be gone by now; then nobody is pinged for it.
			if pinged, err = replyAuthor(ctx, tx, channelID, *m.ReplyTo, userID); errors.Is(err, httpx.ErrNotFound) {
				pinged = nil
			} else if err != nil {
				return err
			}
		}
		mn, err := s.resolveMentions(ctx, ch, parsed, pinged)
		if err != nil {
			return err
		}
		m.Content = content
		mn.apply(&m)
		if err := tx.QueryRow(ctx,
			`UPDATE messages SET content = $2, mention_everyone = $3, mentions = $4, mention_roles = $5, edited_at = now()
			 WHERE id = $1 RETURNING edited_at`,
			id, m.Content, m.MentionEveryone, m.Mentions, m.MentionRoles).Scan(&m.EditedAt); err != nil {
			return err
		}
		if m, err = s.fillOne(ctx, tx, m); err != nil {
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
		if err := dropAttachments(ctx, tx, `message_id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM messages WHERE id = $1`, id); err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{Topic: ch.topic, Type: events.MessageDelete,
			Data: map[string]any{"id": id, "channel_id": channelID, "guild_id": ch.guildID}})
	})
}

// DeleteChannels is the hooks.ChannelsDeleted for guild and social.
// Attachments go first, pending uploads included, with their files scheduled.
func DeleteChannels(ctx context.Context, tx pgx.Tx, channelIDs []uuid.UUID) error {
	if err := dropAttachments(ctx, tx, `channel_id = ANY($1)`, channelIDs); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM read_states WHERE channel_id = ANY($1)`, channelIDs); err != nil {
		return err
	}
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
