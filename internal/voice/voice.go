// Package voice connects users to LiveKit rooms (docs/adr/0001-livekit-sfu.md,
// docs/adr/0025-voice.md). Who may enter a voice channel and who is in it
// is the guild module's; this package issues LiveKit tokens, follows
// LiveKit's webhooks and checks its rooms against the recorded states.
package voice

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
	"github.com/eastsidekillas/ruscord-core/internal/social"
)

// tokenTTL bounds the first connection with a token (ADR-0007); LiveKit
// accepts reconnects of a connected participant after it.
const tokenTTL = time.Minute

// roomPrefix names the LiveKit room of a voice channel (ADR-0007).
const roomPrefix = "voice:"

var (
	ruleJoin = ratelimit.Rule{Name: "voice_join", Limit: 20, Window: time.Minute}
	// ruleSelf bounds mute/deafen toggles: each is an event to the guild.
	ruleSelf = ratelimit.Rule{Name: "voice_self", Limit: 30, Window: time.Minute}
	// ruleModerate bounds a moderator's voice actions.
	ruleModerate = ratelimit.Rule{Name: "voice_moderate", Limit: 60, Window: time.Minute}
)

// Guilds is what voice needs from the guild module; guild.Service implements it.
type Guilds interface {
	VoiceAccess(ctx context.Context, userID, channelID uuid.UUID) (guild.VoiceTarget, error)
	VoiceJoined(ctx context.Context, userID, channelID uuid.UUID, participant string) (bool, *uuid.UUID, error)
	VoiceLeft(ctx context.Context, userID, channelID uuid.UUID, participant string) (bool, error)
	SetSelfVoice(ctx context.Context, userID, channelID uuid.UUID, mute, deaf bool) (guild.VoiceState, error)
	VoiceRooms(ctx context.Context) (map[uuid.UUID][]guild.VoiceParticipant, error)
	VoiceGuildIDs(ctx context.Context) ([]uuid.UUID, error)
	VoiceChecks(ctx context.Context, guildID uuid.UUID) ([]guild.VoiceCheck, error)
	VoiceModerate(ctx context.Context, actorID, guildID, targetID uuid.UUID, need perm.Permissions) (guild.VoiceSubject, error)
	VoiceMove(ctx context.Context, actorID, guildID, targetID, channelID uuid.UUID) (guild.VoiceSubject, error)
	SetServerVoice(ctx context.Context, actorID, guildID, targetID uuid.UUID, in guild.ServerVoiceInput) (guild.VoiceState, error)
	VoiceStateOf(ctx context.Context, userID uuid.UUID) (uuid.UUID, string, bool, error)
}

// Users gives the public profiles shown in a room; auth.Service implements it.
type Users interface {
	Users(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]auth.PublicUser, error)
}

// Rooms is the LiveKit API voice uses; *LiveKit implements it.
type Rooms interface {
	JoinToken(j Join) (string, error)
	ListRooms(ctx context.Context) ([]string, error)
	ListParticipants(ctx context.Context, room string) ([]Participant, error)
	RemoveParticipant(ctx context.Context, room, identity string) error
	SetPermission(ctx context.Context, room, identity string, p Permission) error
	DeleteRoom(ctx context.Context, room string) error
}

type Service struct {
	rooms     Rooms
	guilds    Guilds
	users     Users
	limiter   *ratelimit.Limiter
	publicURL string
	log       *slog.Logger
	calls     Calls
}

// NewService: publicURL is the LiveKit address clients connect to; empty
// means the client's own origin under /livekit (proxied by its nginx).
func NewService(rooms Rooms, guilds Guilds, users Users, limiter *ratelimit.Limiter, publicURL string, log *slog.Logger) *Service {
	return &Service{rooms: rooms, guilds: guilds, users: users, limiter: limiter, publicURL: publicURL, log: log}
}

func roomOf(channelID uuid.UUID) string { return roomPrefix + channelID.String() }

func channelOf(room string) (uuid.UUID, bool) {
	rest, ok := strings.CutPrefix(room, roomPrefix)
	if !ok {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(rest)
	return id, err == nil
}

// JoinInfo is what a client needs to enter a voice channel.
type JoinInfo struct {
	// URL of LiveKit; empty means the client's origin under /livekit.
	URL   string `json:"url"`
	Token string `json:"token"`
	Room  string `json:"room"`
	// Sources the user may publish there; the token allows only these.
	Sources    []perm.MediaSource `json:"sources"`
	PushToTalk bool               `json:"push_to_talk"`

	// HighQuality: the screen may be shared above 720p (STREAM_HQ). LiveKit
	// cannot cap a resolution, so the client picks it.
	HighQuality bool `json:"high_quality"`

	// Call is the DM call joined (ADR-0032); absent for voice channels.
	Call *social.Call `json:"call,omitempty"`
}

// Join issues a token for a voice channel the user may enter now. Being in
// the channel starts when LiveKit reports the connection (Webhook).
func (s *Service) Join(ctx context.Context, userID, channelID uuid.UUID) (JoinInfo, error) {
	if err := s.limiter.Check(ctx, ruleJoin, userID.String()); err != nil {
		return JoinInfo{}, err
	}
	t, err := s.guilds.VoiceAccess(ctx, userID, channelID)
	if err != nil {
		return JoinInfo{}, err
	}
	return s.token(ctx, userID, roomOf(channelID), t.Perms, allowedSources(t.Perms, t.ServerMute), t.ServerDeaf)
}

// token issues a LiveKit token for room with the profile as the
// participant's name and metadata; perms give push-to-talk and quality,
// sources what may be published, deaf no incoming sound.
func (s *Service) token(ctx context.Context, userID uuid.UUID, room string, perms perm.Permissions,
	allowed []perm.MediaSource, deaf bool) (JoinInfo, error) {
	grant := perm.Voice(perms)
	grant.Sources = allowed
	users, err := s.users.Users(ctx, []uuid.UUID{userID})
	if err != nil {
		return JoinInfo{}, err
	}
	u := users[userID]
	meta, err := json.Marshal(map[string]any{"avatar": u.Avatar, "username": u.Username})
	if err != nil {
		return JoinInfo{}, err
	}
	sources := make([]string, len(grant.Sources))
	for i, src := range grant.Sources {
		sources[i] = string(src)
	}
	name := u.DisplayName
	if name == "" {
		name = u.Username
	}
	token, err := s.rooms.JoinToken(Join{Room: room, Identity: userID.String(), Name: name, Metadata: string(meta),
		Sources: sources, Deaf: deaf, TTL: tokenTTL})
	if err != nil {
		return JoinInfo{}, err
	}
	return JoinInfo{URL: s.publicURL, Token: token, Room: room, Sources: grant.Sources, PushToTalk: grant.PushToTalk,
		HighQuality: grant.HighQuality}, nil
}

// SelfVoice records the user's own mute and deafen in their voice channel.
func (s *Service) SelfVoice(ctx context.Context, userID, channelID uuid.UUID, mute, deaf bool) (guild.VoiceState, error) {
	if err := s.limiter.Check(ctx, ruleSelf, userID.String()); err != nil {
		return guild.VoiceState{}, err
	}
	return s.guilds.SetSelfVoice(ctx, userID, channelID, mute, deaf)
}

// ServerVoice mutes or deafens someone (MUTE_MEMBERS, DEAFEN_MEMBERS); the
// guild module enforces it in the room right after (Enforce).
func (s *Service) ServerVoice(ctx context.Context, actorID, guildID, targetID uuid.UUID, in guild.ServerVoiceInput) (guild.VoiceState, error) {
	if err := s.limiter.Check(ctx, ruleModerate, actorID.String()); err != nil {
		return guild.VoiceState{}, err
	}
	return s.guilds.SetServerVoice(ctx, actorID, guildID, targetID, in)
}

// Disconnect takes someone out of their voice channel (MOVE_MEMBERS there).
func (s *Service) Disconnect(ctx context.Context, actorID, guildID, targetID uuid.UUID) error {
	if err := s.limiter.Check(ctx, ruleModerate, actorID.String()); err != nil {
		return err
	}
	vs, err := s.guilds.VoiceModerate(ctx, actorID, guildID, targetID, perm.MoveMembers)
	if err != nil {
		return err
	}
	return s.takeOut(ctx, targetID, *vs.State.ChannelID, vs.Participant)
}

// Move takes someone to another voice channel of the guild (ADR-0031).
// LiveKit cannot move a participant between rooms: the core tells their
// clients where to go (VOICE_MOVE) and takes them out of the old room at
// once, so a client that ignores the move does not stay there.
func (s *Service) Move(ctx context.Context, actorID, guildID, targetID, channelID uuid.UUID) error {
	if err := s.limiter.Check(ctx, ruleModerate, actorID.String()); err != nil {
		return err
	}
	vs, err := s.guilds.VoiceMove(ctx, actorID, guildID, targetID, channelID)
	if err != nil {
		return err
	}
	return s.takeOut(ctx, targetID, *vs.State.ChannelID, vs.Participant)
}

// SignedOut takes the user out of their voice channel and their DM calls
// (auth.SignOutListener; S4 of docs/audit/2026-11-04.md). After "sign out
// everywhere", a password change, a device signed out or the account
// deleted, a connection made with a revoked session must not stay: LiveKit
// keeps it alive by itself. Participants are named by user, not session,
// so every connection of the user goes; a device still signed in joins
// again.
func (s *Service) SignedOut(ctx context.Context, userID uuid.UUID) error {
	var errs []error
	if ch, participant, ok, err := s.guilds.VoiceStateOf(ctx, userID); err != nil {
		errs = append(errs, err)
	} else if ok {
		errs = append(errs, s.takeOut(ctx, userID, ch, participant))
	}
	if s.calls != nil {
		calls, err := s.calls.CallsOf(ctx, userID)
		errs = append(errs, err)
		for _, c := range calls {
			if err := s.rooms.RemoveParticipant(ctx, callRoom(c.CallID), userID.String()); err != nil {
				errs = append(errs, err)
				continue
			}
			errs = append(errs, s.callLeft(ctx, userID, c.CallID, c.Participant))
		}
	}
	return errors.Join(errs...)
}

// CleanupAccount takes a deleted account out of voice and calls
// (auth.AccountCleanup): it is retried until it succeeds.
func (s *Service) CleanupAccount(ctx context.Context, userID uuid.UUID) error {
	return s.SignedOut(ctx, userID)
}

// takeOut removes someone from a channel's room and clears their state.
func (s *Service) takeOut(ctx context.Context, userID, channelID uuid.UUID, participant string) error {
	if err := s.rooms.RemoveParticipant(ctx, roomOf(channelID), userID.String()); err != nil {
		return err
	}
	_, err := s.guilds.VoiceLeft(ctx, userID, channelID, participant)
	return err
}

// allowedSources are the sources someone may publish: what their
// permissions allow, without the microphone when a moderator muted them.
func allowedSources(perms perm.Permissions, serverMute bool) []perm.MediaSource {
	return slices.DeleteFunc(perm.Voice(perms).Sources, func(src perm.MediaSource) bool {
		return src == perm.SourceMicrophone && serverMute
	})
}

// permissionFor is what someone may do in a room: allowedSources (as
// LiveKit's TrackSource names), and no incoming sound when deafened.
func permissionFor(c guild.VoiceCheck) Permission {
	p := Permission{CanSubscribe: !c.ServerDeaf, CanPublishSources: []string{}}
	for _, src := range allowedSources(c.Perms, c.ServerMute) {
		p.CanPublishSources = append(p.CanPublishSources, strings.ToUpper(string(src)))
	}
	p.CanPublish = len(p.CanPublishSources) > 0
	return p
}

func samePermission(a *Permission, b Permission) bool {
	if a == nil {
		return false
	}
	x, y := slices.Clone(a.CanPublishSources), slices.Clone(b.CanPublishSources)
	slices.Sort(x)
	slices.Sort(y)
	return a.CanSubscribe == b.CanSubscribe && a.CanPublish == b.CanPublish && slices.Equal(x, y)
}

// Enforce brings the rooms of a guild in line with permissions as they are
// now (guild.VoiceEnforcer): whoever may not be in their channel any more
// is removed, the others get exactly the sources they may publish.
func (s *Service) Enforce(ctx context.Context, guildID uuid.UUID) error {
	checks, err := s.guilds.VoiceChecks(ctx, guildID)
	if err != nil {
		return err
	}
	live := map[uuid.UUID][]Participant{}
	var errs []error
	for _, c := range checks {
		room := roomOf(c.ChannelID)
		if !c.Allowed {
			s.log.InfoContext(ctx, "voice: removing someone who lost access", "user", c.UserID, "channel", c.ChannelID)
			if err := s.takeOut(ctx, c.UserID, c.ChannelID, c.Participant); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		ps, ok := live[c.ChannelID]
		if !ok {
			if ps, err = s.rooms.ListParticipants(ctx, room); err != nil {
				errs = append(errs, err)
				continue
			}
			live[c.ChannelID] = ps
		}
		i := slices.IndexFunc(ps, func(p Participant) bool { return p.Identity == c.UserID.String() })
		if i < 0 {
			continue // not connected now; the periodic check clears the state
		}
		if want := permissionFor(c); !samePermission(ps[i].Permission, want) {
			if err := s.rooms.SetPermission(ctx, room, c.UserID.String(), want); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// CloseRooms closes the rooms of deleted voice channels (guild.VoiceEnforcer).
func (s *Service) CloseRooms(ctx context.Context, channelIDs []uuid.UUID) error {
	var errs []error
	for _, ch := range channelIDs {
		if err := s.rooms.DeleteRoom(ctx, roomOf(ch)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// webhookEvent is the part of LiveKit's WebhookEvent voice reads.
type webhookEvent struct {
	Event string `json:"event"`
	Room  *struct {
		Name string `json:"name"`
	} `json:"room"`
	Participant *Participant `json:"participant"`
}

// Webhook applies a verified LiveKit webhook. A join is checked again
// against permissions; who may not be in the room is removed from it.
func (s *Service) Webhook(ctx context.Context, body []byte) error {
	var ev webhookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return err
	}
	if ev.Room == nil || ev.Participant == nil {
		return nil
	}
	userID, err := uuid.Parse(ev.Participant.Identity)
	if err != nil {
		return nil // not one of our users
	}
	if callID, ok := callOf(ev.Room.Name); ok {
		return s.callEvent(ctx, ev.Event, userID, callID, ev.Participant.Sid)
	}
	channelID, ok := channelOf(ev.Room.Name)
	if !ok {
		return nil
	}
	switch ev.Event {
	case "participant_joined":
		return s.joined(ctx, userID, channelID, ev.Participant.Sid)
	case "participant_left":
		_, err := s.guilds.VoiceLeft(ctx, userID, channelID, ev.Participant.Sid)
		return err
	}
	return nil
}

func (s *Service) joined(ctx context.Context, userID, channelID uuid.UUID, sid string) error {
	ok, old, err := s.guilds.VoiceJoined(ctx, userID, channelID, sid)
	if err != nil {
		return err
	}
	if !ok {
		s.log.InfoContext(ctx, "voice: removing a participant who may not be in the room", "user", userID, "channel", channelID)
		return s.rooms.RemoveParticipant(ctx, roomOf(channelID), userID.String())
	}
	if old != nil && *old != channelID {
		// One voice channel at a time: leave the previous room.
		return s.rooms.RemoveParticipant(ctx, roomOf(*old), userID.String())
	}
	return nil
}

// Reconcile brings the recorded states in line with LiveKit's rooms: webhooks
// are not guaranteed, and LiveKit forgets rooms when it restarts. States
// without a participant are cleared; participants without a state are
// recorded (or removed, if they may not be there).
func (s *Service) Reconcile(ctx context.Context) error {
	states, err := s.guilds.VoiceRooms(ctx)
	if err != nil {
		return err
	}
	names, err := s.rooms.ListRooms(ctx)
	if err != nil {
		return err
	}
	rooms := map[uuid.UUID]bool{}
	for ch := range states {
		rooms[ch] = true
	}
	for _, name := range names {
		if ch, ok := channelOf(name); ok {
			rooms[ch] = true
		}
	}
	for ch := range rooms {
		live, err := s.rooms.ListParticipants(ctx, roomOf(ch))
		if err != nil {
			return err
		}
		here := map[string]bool{}
		for _, p := range live {
			here[p.Sid] = true
		}
		recorded := map[string]bool{}
		for _, st := range states[ch] {
			recorded[st.Participant] = true
			if !here[st.Participant] {
				if _, err := s.guilds.VoiceLeft(ctx, st.UserID, ch, st.Participant); err != nil {
					return err
				}
			}
		}
		for _, p := range live {
			user, err := uuid.Parse(p.Identity)
			if err != nil || recorded[p.Sid] {
				continue
			}
			if err := s.joined(ctx, user, ch, p.Sid); err != nil {
				return err
			}
		}
	}
	// Changes whose enforcement failed (LiveKit was down) are applied now.
	guildIDs, err := s.guilds.VoiceGuildIDs(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, g := range guildIDs {
		if err := s.Enforce(ctx, g); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.reconcileCalls(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// RunReconcile checks the rooms every interval until ctx ends.
func (s *Service) RunReconcile(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
			s.log.WarnContext(ctx, "voice: reconcile with LiveKit", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
