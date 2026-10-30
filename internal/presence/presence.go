// Package presence keeps who is online and tells the right people when it
// changes (docs/adr/0018-ephemeral-events.md).
//
// The status a user chose (online, idle, dnd, invisible) lives in Postgres
// (auth). Whether they are connected lives here, in Valkey: one key per
// gateway session with a TTL, so a crashed node cannot leave anyone online
// forever. Changes go out as ephemeral PRESENCE_UPDATE events to the user's
// friends (eph.presence.<id>) and to their guilds of up to MaxGuildMembers.
package presence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/eastsidekillas/ruscord-core/internal/events"
)

type Status string

const (
	Online    Status = "online"
	Idle      Status = "idle"
	DND       Status = "dnd"
	Invisible Status = "invisible" // chosen by the user; others see Offline
	Offline   Status = "offline"
)

// ValidChoice reports whether s may be chosen by a user as their status.
func ValidChoice(s Status) bool {
	return s == Online || s == Idle || s == DND || s == Invisible
}

// ValidSession reports whether s may be reported by one connection (op 3):
// a device is either in use or idle; the rest is the user's choice.
func ValidSession(s Status) bool { return s == Online || s == Idle }

// Visible is what other people see, given the user's chosen status and the
// states of their live sessions.
func Visible(choice Status, sessions []Status) Status {
	if len(sessions) == 0 || choice == Invisible {
		return Offline
	}
	switch choice {
	case DND, Idle:
		return choice
	}
	for _, s := range sessions {
		if s == Online {
			return Online
		}
	}
	return Idle // every device idle
}

// MaxGuildMembers bounds which guilds get presence: in a guild of n members
// every change reaches n people, so the total grows with n². Bigger guilds
// need a lazy subscription to the visible part of the member list (after
// the MVP, docs/plans/mvp-client-features.md).
const MaxGuildMembers = 250

// Publisher sends ephemeral events.
type Publisher = events.EphemeralPublisher

// Guilds is what presence needs from the guild module.
type Guilds interface {
	// SmallGuilds returns the guilds of user with at most max members.
	SmallGuilds(ctx context.Context, user uuid.UUID, max int) ([]uuid.UUID, error)
}

// Update is the payload of PRESENCE_UPDATE.
type Update struct {
	UserID uuid.UUID `json:"user_id"`
	Status Status    `json:"status"`
}

type Service struct {
	rdb    *redis.Client
	pub    Publisher
	guilds Guilds
	// ttl is how long a session counts as live without a heartbeat. It must
	// outlast the gateway's resume window, so a dropped connection does not
	// blink to offline while it can still resume.
	ttl time.Duration
}

func NewService(rdb *redis.Client, pub Publisher, guilds Guilds, ttl time.Duration) *Service {
	return &Service{rdb: rdb, pub: pub, guilds: guilds, ttl: ttl}
}

// TTL is how long a session stays live without a heartbeat.
func (s *Service) TTL() time.Duration { return s.ttl }

func sessionKey(user uuid.UUID, sid string) string {
	return "presence:sess:" + user.String() + ":" + sid
}
func indexKey(user uuid.UUID) string { return "presence:idx:" + user.String() }
func lastKey(user uuid.UUID) string  { return "presence:last:" + user.String() }

// Touch records that session sid of user is live in state (online or idle)
// for another TTL. Heartbeats call it.
func (s *Service) Touch(ctx context.Context, user uuid.UUID, sid string, state Status) error {
	if !ValidSession(state) {
		return fmt.Errorf("presence: invalid session state %q", state)
	}
	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, sessionKey(user, sid), string(state), s.ttl)
	pipe.SAdd(ctx, indexKey(user), sid)
	pipe.Expire(ctx, indexKey(user), s.ttl)
	pipe.Expire(ctx, lastKey(user), s.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("presence: touch: %w", err)
	}
	return nil
}

// Drop forgets a session that ended.
func (s *Service) Drop(ctx context.Context, user uuid.UUID, sid string) error {
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, sessionKey(user, sid))
	pipe.SRem(ctx, indexKey(user), sid)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("presence: drop: %w", err)
	}
	return nil
}

// Refresh recomputes what others see of user and, if it changed since the
// last time anyone published it, publishes PRESENCE_UPDATE. Every session
// of the user calls it after a change; the swap of the last published value
// makes the publication happen once.
func (s *Service) Refresh(ctx context.Context, user uuid.UUID, choice Status) (Status, error) {
	sids, err := s.rdb.SMembers(ctx, indexKey(user)).Result()
	if err != nil {
		return "", fmt.Errorf("presence: sessions: %w", err)
	}
	states := make([]Status, 0, len(sids))
	if len(sids) > 0 {
		keys := make([]string, len(sids))
		for i, sid := range sids {
			keys[i] = sessionKey(user, sid)
		}
		vals, err := s.rdb.MGet(ctx, keys...).Result()
		if err != nil {
			return "", fmt.Errorf("presence: session states: %w", err)
		}
		var gone []any
		for i, v := range vals {
			if str, ok := v.(string); ok {
				states = append(states, Status(str))
			} else {
				gone = append(gone, sids[i]) // expired: its node died or it never said goodbye
			}
		}
		if len(gone) > 0 {
			if err := s.rdb.SRem(ctx, indexKey(user), gone...).Err(); err != nil {
				return "", fmt.Errorf("presence: prune: %w", err)
			}
		}
	}
	visible := Visible(choice, states)

	old, err := s.rdb.SetArgs(ctx, lastKey(user), string(visible), redis.SetArgs{Get: true, TTL: s.ttl}).Result()
	if errors.Is(err, redis.Nil) {
		old, err = string(Offline), nil // nothing published yet means offline
	}
	if err != nil {
		return "", fmt.Errorf("presence: swap: %w", err)
	}
	if Status(old) == visible {
		return visible, nil
	}
	if err := s.publish(ctx, user, visible); err != nil {
		// Not delivered: put the old value back so the next refresh tries
		// again rather than believing everyone already knows.
		if rerr := s.rdb.Set(ctx, lastKey(user), old, s.ttl).Err(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("presence: restore: %w", rerr))
		}
		return visible, err
	}
	return visible, nil
}

func (s *Service) publish(ctx context.Context, user uuid.UUID, st Status) error {
	upd := Update{UserID: user, Status: st}
	if err := s.pub.Publish(events.EphPresenceTopic(user), events.PresenceUpdate, upd); err != nil {
		return err
	}
	guilds, err := s.guilds.SmallGuilds(ctx, user, MaxGuildMembers)
	if err != nil {
		return fmt.Errorf("presence: guilds: %w", err)
	}
	for _, g := range guilds {
		if err := s.pub.Publish(events.EphGuildTopic(g), events.PresenceUpdate, upd); err != nil {
			return err
		}
	}
	return nil
}

// Statuses returns what others currently see of users; unknown means offline.
func (s *Service) Statuses(ctx context.Context, users []uuid.UUID) (map[uuid.UUID]Status, error) {
	out := make(map[uuid.UUID]Status, len(users))
	if len(users) == 0 {
		return out, nil
	}
	keys := make([]string, len(users))
	for i, u := range users {
		keys[i] = lastKey(u)
	}
	vals, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("presence: statuses: %w", err)
	}
	for i, v := range vals {
		st := Offline
		if str, ok := v.(string); ok {
			st = Status(str)
		}
		out[users[i]] = st
	}
	return out, nil
}
