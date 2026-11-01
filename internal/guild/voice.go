package guild

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// VoiceState says who is in a voice channel (docs/adr/0025-voice.md). In a
// VOICE_STATE_UPDATE a null ChannelID means the user left.
type VoiceState struct {
	UserID    uuid.UUID  `json:"user_id"`
	GuildID   uuid.UUID  `json:"guild_id"`
	ChannelID *uuid.UUID `json:"channel_id"`
	SelfMute  bool       `json:"self_mute"`
	SelfDeaf  bool       `json:"self_deaf"`

	participant string // LiveKit participant sid
}

var (
	errNotVoice   = httpx.NewError(http.StatusBadRequest, "not_voice_channel", "Это не голосовой канал")
	errVoiceFull  = httpx.NewError(http.StatusConflict, "channel_full", "В голосовом канале нет мест")
	errNotInVoice = httpx.NewError(http.StatusConflict, "not_in_voice", "Вы не в этом голосовом канале")
)

// VoiceTarget is a voice channel a user may enter and what they may do there.
type VoiceTarget struct {
	GuildID   uuid.UUID
	ChannelID uuid.UUID
	Perms     perm.Permissions
}

// VoiceAccess checks that userID may enter a voice channel now: it is a
// voice channel they can view and connect to, and it has room (members
// with MOVE_MEMBERS enter a full channel, like in Discord). Someone else's
// or hidden channel is 404.
func (s *Service) VoiceAccess(ctx context.Context, userID, channelID uuid.UUID) (VoiceTarget, error) {
	ch, err := getChannel(ctx, s.pg, channelID)
	if err != nil {
		return VoiceTarget{}, notFound(err)
	}
	a, err := loadAccess(ctx, s.pg, ch.GuildID, userID)
	if err != nil {
		return VoiceTarget{}, notFound(err)
	}
	return voiceTarget(ctx, s.pg, a, ch, userID)
}

func voiceTarget(ctx context.Context, q querier, a access, ch Channel, userID uuid.UUID) (VoiceTarget, error) {
	p := a.in(ch)
	if !p.Has(perm.ViewChannel) {
		return VoiceTarget{}, httpx.ErrNotFound
	}
	if ch.Type != ChannelVoice {
		return VoiceTarget{}, errNotVoice
	}
	if !p.Has(perm.Connect) {
		return VoiceTarget{}, httpx.ErrForbidden
	}
	if ch.UserLimit > 0 && !p.Has(perm.MoveMembers) {
		var n int
		if err := q.QueryRow(ctx,
			`SELECT count(*) FROM voice_states WHERE channel_id = $1 AND user_id <> $2`, ch.ID, userID).Scan(&n); err != nil {
			return VoiceTarget{}, err
		}
		if n >= ch.UserLimit {
			return VoiceTarget{}, errVoiceFull
		}
	}
	return VoiceTarget{GuildID: ch.GuildID, ChannelID: ch.ID, Perms: p}, nil
}

// VoiceJoined records that LiveKit connected userID to a voice channel as
// participant. It checks access again: the token was issued up to a minute
// ago, and LiveKit accepts a reconnect even with an expired token. ok=false
// means the user must not be there and should be removed from the room.
// oldChannel is the voice channel the user was in before (in this or
// another guild), whose room the caller leaves.
func (s *Service) VoiceJoined(ctx context.Context, userID, channelID uuid.UUID, participant string) (bool, *uuid.UUID, error) {
	var cleared *uuid.UUID // a state cleared in another guild
	for range 2 {
		ok, old, other, err := s.voiceJoined(ctx, userID, channelID, participant)
		if err != nil {
			return false, nil, err
		}
		if other == nil {
			if old == nil {
				old = cleared
			}
			return ok, old, nil
		}
		// The state is in another guild: clear it under that guild's lock,
		// then try again.
		if _, err := s.VoiceLeft(ctx, userID, *other.ChannelID, other.participant); err != nil {
			return false, nil, err
		}
		cleared = other.ChannelID
	}
	return false, nil, errors.New("guild: voice state keeps moving between guilds")
}

func (s *Service) voiceJoined(ctx context.Context, userID, channelID uuid.UUID, participant string) (ok bool, oldChannel *uuid.UUID, other *VoiceState, err error) {
	ch, err := getChannel(ctx, s.pg, channelID)
	if err != nil {
		return false, nil, nil, ignoreNotFound(err)
	}
	err = s.mutate(ctx, ch.GuildID, userID, func(tx pgx.Tx, a access) error {
		ch, err := getChannel(ctx, tx, channelID)
		if err != nil {
			return err
		}
		if _, err := voiceTarget(ctx, tx, a, ch, userID); err != nil {
			var apiErr *httpx.Error
			if errors.As(err, &apiErr) {
				return nil // not allowed (any more): ok stays false
			}
			return err
		}
		ok = true
		var prev VoiceState
		err = tx.QueryRow(ctx, `SELECT guild_id, channel_id, participant FROM voice_states WHERE user_id = $1 FOR UPDATE`, userID).
			Scan(&prev.GuildID, &prev.ChannelID, &prev.participant)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		case prev.GuildID != ch.GuildID:
			other = &prev
			return nil
		case *prev.ChannelID == channelID && prev.participant == participant:
			return nil // a repeated webhook
		case *prev.ChannelID != channelID:
			oldChannel = prev.ChannelID
		}
		var st VoiceState
		err = tx.QueryRow(ctx,
			`INSERT INTO voice_states (user_id, guild_id, channel_id, participant) VALUES ($1, $2, $3, $4)
			 ON CONFLICT (user_id) DO UPDATE SET channel_id = $3, participant = $4, joined_at = now()
			 RETURNING self_mute, self_deaf`, userID, ch.GuildID, channelID, participant).Scan(&st.SelfMute, &st.SelfDeaf)
		if err != nil {
			return err
		}
		st.UserID, st.GuildID, st.ChannelID = userID, ch.GuildID, &channelID
		return writeVoiceState(ctx, tx, st)
	})
	if errors.Is(err, httpx.ErrNotFound) {
		return false, nil, nil, nil // no such guild or member any more
	}
	return ok, oldChannel, other, err
}

// VoiceLeft clears userID's voice state if it still belongs to participant
// in channelID: a "left" of an older connection must not clear a newer one.
// It reports whether a state was cleared.
func (s *Service) VoiceLeft(ctx context.Context, userID, channelID uuid.UUID, participant string) (bool, error) {
	ch, err := getChannel(ctx, s.pg, channelID)
	if err != nil {
		return false, ignoreNotFound(err)
	}
	cleared := false
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := lockGuild(ctx, tx, ch.GuildID); err != nil {
			return ignoreNotFound(err)
		}
		tag, err := tx.Exec(ctx,
			`DELETE FROM voice_states WHERE user_id = $1 AND channel_id = $2 AND participant = $3`, userID, channelID, participant)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		cleared = true
		return writeVoiceState(ctx, tx, VoiceState{UserID: userID, GuildID: ch.GuildID})
	})
	return cleared, err
}

// SetSelfVoice records that userID muted or deafened themself in the voice
// channel they are in; others see it next to their name.
func (s *Service) SetSelfVoice(ctx context.Context, userID, channelID uuid.UUID, mute, deaf bool) (VoiceState, error) {
	ch, err := getChannel(ctx, s.pg, channelID)
	if err != nil {
		return VoiceState{}, notFound(err)
	}
	var st VoiceState
	err = s.mutate(ctx, ch.GuildID, userID, func(tx pgx.Tx, _ access) error {
		err := tx.QueryRow(ctx,
			`UPDATE voice_states SET self_mute = $3, self_deaf = $4 WHERE user_id = $1 AND channel_id = $2
			 RETURNING self_mute, self_deaf`, userID, channelID, mute, deaf).Scan(&st.SelfMute, &st.SelfDeaf)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotInVoice
		}
		if err != nil {
			return err
		}
		st.UserID, st.GuildID, st.ChannelID = userID, ch.GuildID, &channelID
		return writeVoiceState(ctx, tx, st)
	})
	return st, err
}

// VoiceParticipant is a voice state as LiveKit knows it.
type VoiceParticipant struct {
	UserID      uuid.UUID
	Participant string
}

// VoiceRooms lists the voice channels that have someone in them, with their
// participants, for the periodic check against LiveKit.
func (s *Service) VoiceRooms(ctx context.Context) (map[uuid.UUID][]VoiceParticipant, error) {
	rows, err := s.pg.Query(ctx, `SELECT channel_id, user_id, participant FROM voice_states ORDER BY channel_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]VoiceParticipant{}
	for rows.Next() {
		var ch uuid.UUID
		var p VoiceParticipant
		if err := rows.Scan(&ch, &p.UserID, &p.Participant); err != nil {
			return nil, err
		}
		out[ch] = append(out[ch], p)
	}
	return out, rows.Err()
}

func writeVoiceState(ctx context.Context, tx pgx.Tx, st VoiceState) error {
	return events.Write(ctx, tx, events.Event{Topic: events.GuildTopic(st.GuildID), Type: events.VoiceStateUpdate, Data: st})
}

func voiceStatesIn(ctx context.Context, q querier, guildIDs []uuid.UUID) (map[uuid.UUID][]VoiceState, error) {
	rows, err := q.Query(ctx,
		`SELECT user_id, guild_id, channel_id, self_mute, self_deaf FROM voice_states
		 WHERE guild_id = ANY($1) ORDER BY guild_id, joined_at, user_id`, guildIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]VoiceState{}
	for rows.Next() {
		var st VoiceState
		if err := rows.Scan(&st.UserID, &st.GuildID, &st.ChannelID, &st.SelfMute, &st.SelfDeaf); err != nil {
			return nil, err
		}
		out[st.GuildID] = append(out[st.GuildID], st)
	}
	return out, rows.Err()
}

func ignoreNotFound(err error) error {
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}
