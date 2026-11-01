// Package voice connects users to LiveKit rooms (docs/adr/0001-livekit-sfu.md,
// docs/adr/0025-voice.md). Who may enter a voice channel and who is in it
// is the guild module's; this package issues LiveKit tokens, follows
// LiveKit's webhooks and checks its rooms against the recorded states.
package voice

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
)

// tokenTTL bounds the first connection with a token (ADR-0007); LiveKit
// accepts reconnects of a connected participant after it.
const tokenTTL = time.Minute

// roomPrefix names the LiveKit room of a voice channel (ADR-0007).
const roomPrefix = "voice:"

var ruleJoin = ratelimit.Rule{Name: "voice_join", Limit: 20, Window: time.Minute}

// Guilds is what voice needs from the guild module; guild.Service implements it.
type Guilds interface {
	VoiceAccess(ctx context.Context, userID, channelID uuid.UUID) (guild.VoiceTarget, error)
	VoiceJoined(ctx context.Context, userID, channelID uuid.UUID, participant string) (bool, *uuid.UUID, error)
	VoiceLeft(ctx context.Context, userID, channelID uuid.UUID, participant string) (bool, error)
	SetSelfVoice(ctx context.Context, userID, channelID uuid.UUID, mute, deaf bool) (guild.VoiceState, error)
	VoiceRooms(ctx context.Context) (map[uuid.UUID][]guild.VoiceParticipant, error)
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
}

type Service struct {
	rooms     Rooms
	guilds    Guilds
	users     Users
	limiter   *ratelimit.Limiter
	publicURL string
	log       *slog.Logger
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
	grant := perm.Voice(t.Perms)
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
	room := roomOf(channelID)
	token, err := s.rooms.JoinToken(Join{Room: room, Identity: userID.String(), Name: name, Metadata: string(meta),
		Sources: sources, TTL: tokenTTL})
	if err != nil {
		return JoinInfo{}, err
	}
	return JoinInfo{URL: s.publicURL, Token: token, Room: room, Sources: grant.Sources, PushToTalk: grant.PushToTalk}, nil
}

// SelfVoice records the user's own mute and deafen in their voice channel.
func (s *Service) SelfVoice(ctx context.Context, userID, channelID uuid.UUID, mute, deaf bool) (guild.VoiceState, error) {
	return s.guilds.SetSelfVoice(ctx, userID, channelID, mute, deaf)
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
	channelID, ok := channelOf(ev.Room.Name)
	if !ok {
		return nil
	}
	userID, err := uuid.Parse(ev.Participant.Identity)
	if err != nil {
		return nil // not one of our users
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
	return nil
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
