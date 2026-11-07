package guild

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
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
	// Mute and Deaf are set by a moderator (MUTE_MEMBERS, DEAFEN_MEMBERS);
	// the core enforces them in LiveKit, the user cannot undo them.
	Mute bool `json:"mute"`
	Deaf bool `json:"deaf"`
	// User is the public profile (auth.PublicUser) of someone in a channel;
	// absent when they left.
	User any `json:"user,omitempty"`

	participant string // LiveKit participant sid
}

var (
	errNotVoice   = httpx.NewError(http.StatusBadRequest, "not_voice_channel", "Это не голосовой канал")
	errVoiceFull  = httpx.NewError(http.StatusConflict, "channel_full", "В голосовом канале нет мест")
	errNotInVoice = httpx.NewError(http.StatusConflict, "not_in_voice", "Вы не в этом голосовом канале")
	// errTargetNotInVoice: the member is not in a voice channel the actor
	// can see.
	errTargetNotInVoice = httpx.NewError(http.StatusNotFound, "not_in_voice", "Участник не в голосовом канале")
	// errCannotMove: the member could not enter the channel themself.
	errCannotMove   = httpx.NewError(http.StatusConflict, "cannot_move", "Участник не может зайти в этот канал")
	errAlreadyThere = httpx.NewError(http.StatusConflict, "already_in_channel", "Участник уже в этом канале")
)

// VoiceTarget is a voice channel a user may enter and what they may do there.
type VoiceTarget struct {
	GuildID   uuid.UUID
	ChannelID uuid.UUID
	Perms     perm.Permissions
	// Muted and deafened by a moderator.
	ServerMute bool
	ServerDeaf bool
}

// memberVoice selects, for a voice_states row, whether a moderator muted
// and deafened its user (members.voice_mute, voice_deaf).
const memberVoice = `COALESCE((SELECT m.voice_mute FROM members m WHERE m.guild_id = voice_states.guild_id AND m.user_id = voice_states.user_id), false),
	COALESCE((SELECT m.voice_deaf FROM members m WHERE m.guild_id = voice_states.guild_id AND m.user_id = voice_states.user_id), false)`

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
	t := VoiceTarget{GuildID: ch.GuildID, ChannelID: ch.ID, Perms: p}
	err := q.QueryRow(ctx, `SELECT voice_mute, voice_deaf FROM members WHERE guild_id = $1 AND user_id = $2`, ch.GuildID, userID).
		Scan(&t.ServerMute, &t.ServerDeaf)
	if errors.Is(err, pgx.ErrNoRows) {
		return VoiceTarget{}, httpx.ErrNotFound
	}
	return t, err
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
			 RETURNING self_mute, self_deaf, `+memberVoice, userID, ch.GuildID, channelID, participant).
			Scan(&st.SelfMute, &st.SelfDeaf, &st.Mute, &st.Deaf)
		if err != nil {
			return err
		}
		st.UserID, st.GuildID, st.ChannelID = userID, ch.GuildID, &channelID
		return s.writeVoiceState(ctx, tx, st)
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
		return s.writeVoiceState(ctx, tx, VoiceState{UserID: userID, GuildID: ch.GuildID})
	})
	return cleared, err
}

// VoiceStateOf is the voice channel the user is in and the LiveKit
// participant of that connection; ok is false when the user is in none.
func (s *Service) VoiceStateOf(ctx context.Context, userID uuid.UUID) (channelID uuid.UUID, participant string, ok bool, err error) {
	err = s.pg.QueryRow(ctx, `SELECT channel_id, participant FROM voice_states WHERE user_id = $1`, userID).Scan(&channelID, &participant)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", false, nil
	}
	return channelID, participant, err == nil, err
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
			 RETURNING self_mute, self_deaf, `+memberVoice, userID, channelID, mute, deaf).
			Scan(&st.SelfMute, &st.SelfDeaf, &st.Mute, &st.Deaf)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotInVoice
		}
		if err != nil {
			return err
		}
		st.UserID, st.GuildID, st.ChannelID = userID, ch.GuildID, &channelID
		return s.writeVoiceState(ctx, tx, st)
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

// writeVoiceState sends a VOICE_STATE_UPDATE; someone in a channel comes
// with their profile, so clients can show them.
func (s *Service) writeVoiceState(ctx context.Context, tx pgx.Tx, st VoiceState) error {
	if st.ChannelID != nil {
		if err := s.attachVoiceUsers(ctx, []*VoiceState{&st}); err != nil {
			return err
		}
	}
	return events.Write(ctx, tx, events.Event{Topic: events.GuildTopic(st.GuildID), Type: events.VoiceStateUpdate, Data: st})
}

func (s *Service) attachVoiceUsers(ctx context.Context, sts []*VoiceState) error {
	return auth.Attach(ctx, s.users, sts, func(st *VoiceState) *uuid.UUID { return &st.UserID },
		func(st *VoiceState, u auth.PublicUser) { st.User = u })
}

// voiceStates loads the voice states of guilds with the users' profiles.
func (s *Service) voiceStates(ctx context.Context, q querier, guildIDs []uuid.UUID) (map[uuid.UUID][]VoiceState, error) {
	byGuild, err := voiceStatesIn(ctx, q, guildIDs)
	if err != nil {
		return nil, err
	}
	var all []*VoiceState
	for _, sts := range byGuild {
		for i := range sts {
			all = append(all, &sts[i])
		}
	}
	return byGuild, s.attachVoiceUsers(ctx, all)
}

func voiceStatesIn(ctx context.Context, q querier, guildIDs []uuid.UUID) (map[uuid.UUID][]VoiceState, error) {
	rows, err := q.Query(ctx,
		`SELECT user_id, guild_id, channel_id, self_mute, self_deaf, `+memberVoice+` FROM voice_states
		 WHERE guild_id = ANY($1) ORDER BY guild_id, joined_at, user_id`, guildIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]VoiceState{}
	for rows.Next() {
		var st VoiceState
		if err := rows.Scan(&st.UserID, &st.GuildID, &st.ChannelID, &st.SelfMute, &st.SelfDeaf, &st.Mute, &st.Deaf); err != nil {
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

// VoiceEnforcer applies changes of who may do what in voice to the rooms
// themselves (voice.Service implements it): someone who lost access is
// removed, someone who lost a source or was muted by a moderator gets
// narrower LiveKit permissions (docs/adr/0026-voice-moderation.md).
type VoiceEnforcer interface {
	Enforce(ctx context.Context, guildID uuid.UUID) error
	CloseRooms(ctx context.Context, channelIDs []uuid.UUID) error
}

// WithVoice connects the voice module; without it nothing is enforced in
// rooms (tests of this package).
func (s *Service) WithVoice(v VoiceEnforcer) { s.voice = v }

// voiceTimeout bounds the LiveKit calls after a change; the periodic check
// (voice.Reconcile) catches what does not get through.
const voiceTimeout = 10 * time.Second

// enforceVoice runs after a committed change that may change voice access
// in a guild. A failure is logged, not returned: the change itself stands.
func (s *Service) enforceVoice(ctx context.Context, guildID uuid.UUID) {
	if s.voice == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceTimeout)
	defer cancel()
	if err := s.voice.Enforce(ctx, guildID); err != nil {
		slog.WarnContext(ctx, "guild: voice access not enforced yet; the periodic check will", "guild", guildID, "err", err)
	}
}

func (s *Service) closeVoiceRooms(ctx context.Context, channelIDs []uuid.UUID) {
	if s.voice == nil || len(channelIDs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceTimeout)
	defer cancel()
	if err := s.voice.CloseRooms(ctx, channelIDs); err != nil {
		slog.WarnContext(ctx, "guild: voice rooms of deleted channels not closed", "channels", channelIDs, "err", err)
	}
}

// mutateVoice is mutate for changes that can take voice access away
// (members, roles, overwrites): rooms follow once the change is committed.
func (s *Service) mutateVoice(ctx context.Context, guildID, userID uuid.UUID, fn func(pgx.Tx, access) error) error {
	err := s.mutate(ctx, guildID, userID, fn)
	if err == nil {
		s.enforceVoice(ctx, guildID)
	}
	return err
}

// withChannelVoice is withChannel for such changes in one channel.
func (s *Service) withChannelVoice(ctx context.Context, userID, channelID uuid.UUID, fn func(pgx.Tx, access, Channel, perm.Permissions) error) error {
	var guildID uuid.UUID
	err := s.withChannel(ctx, userID, channelID, func(tx pgx.Tx, a access, ch Channel, p perm.Permissions) error {
		guildID = ch.GuildID
		return fn(tx, a, ch, p)
	})
	if err == nil {
		s.enforceVoice(ctx, guildID)
	}
	return err
}

// VoiceCheck is someone in a voice channel and what they may do there now.
type VoiceCheck struct {
	UserID      uuid.UUID
	ChannelID   uuid.UUID
	Participant string
	// Allowed: still a member who can view the channel and connect.
	Allowed    bool
	Perms      perm.Permissions
	ServerMute bool
	ServerDeaf bool
}

// VoiceChecks evaluates everyone in the guild's voice channels against the
// permissions as they are now (the user limit aside: nobody is thrown out
// of a channel that became smaller).
func (s *Service) VoiceChecks(ctx context.Context, guildID uuid.UUID) ([]VoiceCheck, error) {
	rows, err := s.pg.Query(ctx,
		`SELECT user_id, channel_id, participant, `+memberVoice+` FROM voice_states WHERE guild_id = $1`, guildID)
	if err != nil {
		return nil, err
	}
	checks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (VoiceCheck, error) {
		var c VoiceCheck
		err := r.Scan(&c.UserID, &c.ChannelID, &c.Participant, &c.ServerMute, &c.ServerDeaf)
		return c, err
	})
	if err != nil {
		return nil, err
	}
	for i := range checks {
		c := &checks[i]
		ch, err := getChannel(ctx, s.pg, c.ChannelID)
		if err != nil {
			if errors.Is(err, errNotFound) {
				continue // deleted meanwhile; its states went with it
			}
			return nil, err
		}
		a, err := loadAccess(ctx, s.pg, guildID, c.UserID)
		if errors.Is(err, errNotFound) {
			continue // not a member any more: Allowed stays false
		}
		if err != nil {
			return nil, err
		}
		c.Perms = a.in(ch)
		c.Allowed = c.Perms.Has(perm.ViewChannel | perm.Connect)
	}
	return checks, nil
}

// VoiceGuildIDs lists the guilds with someone in voice, for the periodic check.
func (s *Service) VoiceGuildIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.pg.Query(ctx, `SELECT DISTINCT guild_id FROM voice_states`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// VoiceTarget of a moderator action: the state of the target and the
// participant to act on in LiveKit.
type VoiceSubject struct {
	State       VoiceState
	Participant string
}

// voiceSubject loads targetID's voice state in guildID and checks that the
// actor has need in that channel.
func voiceSubject(ctx context.Context, q querier, a access, guildID, targetID uuid.UUID, need perm.Permissions, lock bool) (VoiceSubject, error) {
	sql := `SELECT channel_id, participant, self_mute, self_deaf, ` + memberVoice + ` FROM voice_states
	        WHERE guild_id = $1 AND user_id = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	var vs VoiceSubject
	st := &vs.State
	err := q.QueryRow(ctx, sql, guildID, targetID).Scan(&st.ChannelID, &vs.Participant, &st.SelfMute, &st.SelfDeaf, &st.Mute, &st.Deaf)
	if errors.Is(err, pgx.ErrNoRows) {
		return VoiceSubject{}, errTargetNotInVoice
	}
	if err != nil {
		return VoiceSubject{}, err
	}
	ch, err := getChannel(ctx, q, *st.ChannelID)
	if err != nil {
		return VoiceSubject{}, notFound(err)
	}
	p := a.in(ch)
	if !p.Has(perm.ViewChannel) {
		return VoiceSubject{}, errTargetNotInVoice
	}
	if !p.Has(need) {
		return VoiceSubject{}, httpx.ErrForbidden
	}
	st.UserID, st.GuildID = targetID, guildID
	return vs, nil
}

// VoiceModerate checks that actorID may act on targetID's voice connection
// with need (MOVE_MEMBERS to disconnect) and returns it.
func (s *Service) VoiceModerate(ctx context.Context, actorID, guildID, targetID uuid.UUID, need perm.Permissions) (VoiceSubject, error) {
	a, err := loadAccess(ctx, s.pg, guildID, actorID)
	if err != nil {
		return VoiceSubject{}, notFound(err)
	}
	return voiceSubject(ctx, s.pg, a, guildID, targetID, need, false)
}

// VoiceMoveEvent tells a user's clients that a moderator moved them: the
// one in FromChannelID joins ChannelID.
type VoiceMoveEvent struct {
	GuildID       uuid.UUID `json:"guild_id"`
	ChannelID     uuid.UUID `json:"channel_id"`
	FromChannelID uuid.UUID `json:"from_channel_id"`
}

// VoiceMove checks that actorID may move targetID from their voice channel
// to channelID and tells the target's clients to go there (VOICE_MOVE on
// the user topic). The actor needs MOVE_MEMBERS in both channels and
// CONNECT in the new one, like in Discord; the target must be able to
// enter it themself, limit included (docs/adr/0031-voice-move.md). The
// caller takes the target out of the old room.
func (s *Service) VoiceMove(ctx context.Context, actorID, guildID, targetID, channelID uuid.UUID) (VoiceSubject, error) {
	var vs VoiceSubject
	err := s.mutate(ctx, guildID, actorID, func(tx pgx.Tx, a access) error {
		var err error
		if vs, err = voiceSubject(ctx, tx, a, guildID, targetID, perm.MoveMembers, true); err != nil {
			return err
		}
		ch, err := getChannel(ctx, tx, channelID)
		if err != nil {
			return notFound(err)
		}
		p := a.in(ch)
		if ch.GuildID != guildID || !p.Has(perm.ViewChannel) {
			return httpx.ErrNotFound
		}
		if ch.Type != ChannelVoice {
			return errNotVoice
		}
		if !p.Has(perm.Connect | perm.MoveMembers) {
			return httpx.ErrForbidden
		}
		if ch.ID == *vs.State.ChannelID {
			return errAlreadyThere
		}
		ta, err := loadAccess(ctx, tx, guildID, targetID)
		if err != nil {
			return notFound(err)
		}
		if _, err := voiceTarget(ctx, tx, ta, ch, targetID); err != nil {
			if errors.Is(err, httpx.ErrNotFound) || errors.Is(err, httpx.ErrForbidden) {
				return errCannotMove
			}
			return err // errVoiceFull included
		}
		return events.Write(ctx, tx, events.Event{Topic: events.UserTopic(targetID), Type: events.VoiceMove,
			Data: VoiceMoveEvent{GuildID: guildID, ChannelID: ch.ID, FromChannelID: *vs.State.ChannelID}})
	})
	return vs, err
}

// ServerVoiceInput mutes or deafens someone in voice; absent fields stay.
type ServerVoiceInput struct {
	Mute *bool `json:"mute"`
	Deaf *bool `json:"deaf"`
}

// SetServerVoice mutes or deafens targetID in their voice channel:
// MUTE_MEMBERS and DEAFEN_MEMBERS there, like in Discord. The rooms follow
// (enforceVoice). It belongs to the member: it stays across channels
// and reconnects, and the next join token carries it.
func (s *Service) SetServerVoice(ctx context.Context, actorID, guildID, targetID uuid.UUID, in ServerVoiceInput) (VoiceState, error) {
	need := perm.Permissions(0)
	if in.Mute != nil {
		need |= perm.MuteMembers
	}
	if in.Deaf != nil {
		need |= perm.DeafenMembers
	}
	if need == 0 {
		return VoiceState{}, httpx.Validation(map[string]string{"": "Укажите mute или deaf"})
	}
	var st VoiceState
	err := s.mutateVoice(ctx, guildID, actorID, func(tx pgx.Tx, a access) error {
		vs, err := voiceSubject(ctx, tx, a, guildID, targetID, need, true)
		if err != nil {
			return err
		}
		st = vs.State
		if in.Mute != nil {
			st.Mute = *in.Mute
		}
		if in.Deaf != nil {
			st.Deaf = *in.Deaf
		}
		if _, err := tx.Exec(ctx, `UPDATE members SET voice_mute = $3, voice_deaf = $4 WHERE guild_id = $1 AND user_id = $2`,
			guildID, targetID, st.Mute, st.Deaf); err != nil {
			return err
		}
		return s.writeVoiceState(ctx, tx, st)
	})
	return st, err
}
