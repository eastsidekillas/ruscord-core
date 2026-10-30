// Package social owns relationships between users (friends, requests,
// blocks) and direct-message channels (1:1 and group DMs).
package social

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/platform/hooks"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/slicex"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
)

// RelType is stored per direction; see migrations/00004_social.sql.
type RelType int16

const (
	RelFriend   RelType = 1
	RelBlocked  RelType = 2
	RelIncoming RelType = 3
	RelOutgoing RelType = 4
)

const maxFriends = 1000

var (
	errSelf = httpx.NewError(http.StatusBadRequest, "self", "Нельзя добавить себя")
	// errRequestFailed hides why a request failed, so a user cannot learn
	// that someone blocked them.
	errRequestFailed  = httpx.NewError(http.StatusBadRequest, "request_failed", "Не удалось отправить запрос в друзья")
	errYouBlocked     = httpx.NewError(http.StatusBadRequest, "user_blocked", "Сначала разблокируйте пользователя")
	errTooManyFriends = httpx.NewError(http.StatusBadRequest, "too_many_friends", "Достигнут лимит друзей")
)

var ruleFriendRequest = ratelimit.Rule{Name: "friend_request", Limit: 30, Window: time.Hour}

// Guilds is what social needs from the guild module.
type Guilds interface {
	SharesGuild(ctx context.Context, a, b uuid.UUID) (bool, error)
}

// Users is what social needs from the auth module.
type Users interface {
	auth.Directory
	UserByUsername(ctx context.Context, username string) (auth.PublicUser, error)
	Bio(ctx context.Context, id uuid.UUID) (string, error)
}

type Service struct {
	pg      *pgxpool.Pool
	users   Users
	guilds  Guilds
	limiter *ratelimit.Limiter

	channelsDeleted hooks.ChannelsDeleted
}

// OnChannelsDeleted registers the hook run when a group DM is deleted.
func (s *Service) OnChannelsDeleted(h hooks.ChannelsDeleted) { s.channelsDeleted = h }

func NewService(pg *pgxpool.Pool, users Users, guilds Guilds, limiter *ratelimit.Limiter) *Service {
	return &Service{pg: pg, users: users, guilds: guilds, limiter: limiter}
}

type Relationship struct {
	ID    uuid.UUID        `json:"id"` // the other user
	Type  RelType          `json:"type"`
	Since time.Time        `json:"since"`
	User  *auth.PublicUser `json:"user,omitempty"`
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// dbtx is satisfied by both the pool and a transaction.
type dbtx interface {
	querier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// relType returns the type of the from→to row, 0 when there is none.
func relType(ctx context.Context, q querier, from, to uuid.UUID) (RelType, error) {
	var t RelType
	err := q.QueryRow(ctx, `SELECT type FROM relationships WHERE user_id = $1 AND target_id = $2`, from, to).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return t, err
}

// lockPair serialises every change between two users, in either direction,
// so two requests crossing each other become a friendship exactly once.
func lockPair(ctx context.Context, tx pgx.Tx, a, b uuid.UUID) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "rel:"+pairKey(a, b))
	return err
}

// pairKey is the same for (a, b) and (b, a).
func pairKey(a, b uuid.UUID) string {
	if a.String() > b.String() {
		a, b = b, a
	}
	return a.String() + ":" + b.String()
}

func setRel(ctx context.Context, tx pgx.Tx, from, to uuid.UUID, t RelType) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO relationships (user_id, target_id, type) VALUES ($1, $2, $3)
		 ON CONFLICT (user_id, target_id) DO UPDATE SET type = $3, created_at = now()`, from, to, t)
	return err
}

func deleteRel(ctx context.Context, tx pgx.Tx, from, to uuid.UUID) error {
	_, err := tx.Exec(ctx, `DELETE FROM relationships WHERE user_id = $1 AND target_id = $2`, from, to)
	return err
}

// relAdded tells owner about a new relationship with other; it carries
// other's public profile, as the relationship list does, so a client can show
// a new request or friend without another lookup.
func relAdded(owner uuid.UUID, other pairUser, t RelType) events.Event {
	return events.Event{Topic: events.UserTopic(owner), Type: events.RelationshipAdd,
		Data: Relationship{ID: other.ID, Type: t, Since: other.since, User: &other.PublicUser}}
}

// pairUser is one side of a relationship being written, with the time the
// rows get (setRel stores now(), the transaction's start).
type pairUser struct {
	auth.PublicUser
	since time.Time
}

// pair looks up the public profiles of both users of a relationship.
func (s *Service) pair(ctx context.Context, tx pgx.Tx, me, other uuid.UUID) (pairUser, pairUser, error) {
	var since time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&since); err != nil {
		return pairUser{}, pairUser{}, err
	}
	us, err := s.users.Users(ctx, []uuid.UUID{me, other})
	if err != nil {
		return pairUser{}, pairUser{}, err
	}
	a, okA := us[me]
	b, okB := us[other]
	if !okA || !okB {
		return pairUser{}, pairUser{}, errRequestFailed
	}
	return pairUser{a, since}, pairUser{b, since}, nil
}

func relRemoved(owner, other uuid.UUID, t RelType) events.Event {
	return events.Event{Topic: events.UserTopic(owner), Type: events.RelationshipRemove,
		Data: map[string]any{"id": other, "type": t}}
}

// withPair runs fn in a transaction holding the pair lock, with both
// directions of the relationship already read.
func (s *Service) withPair(ctx context.Context, me, other uuid.UUID, fn func(tx pgx.Tx, mine, theirs RelType) error) error {
	if me == other {
		return errSelf
	}
	return pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		if err := lockPair(ctx, tx, me, other); err != nil {
			return err
		}
		mine, err := relType(ctx, tx, me, other)
		if err != nil {
			return err
		}
		theirs, err := relType(ctx, tx, other, me)
		if err != nil {
			return err
		}
		return fn(tx, mine, theirs)
	})
}

// RequestByUsername sends a friend request to the user with that username.
func (s *Service) RequestByUsername(ctx context.Context, me uuid.UUID, username string) error {
	u, err := s.users.UserByUsername(ctx, username)
	if errors.Is(err, httpx.ErrNotFound) {
		return errRequestFailed
	}
	if err != nil {
		return err
	}
	return s.Request(ctx, me, u.ID)
}

// Request sends a friend request, or accepts the other user's pending one.
func (s *Service) Request(ctx context.Context, me, other uuid.UUID) error {
	return s.withPair(ctx, me, other, func(tx pgx.Tx, mine, theirs RelType) error {
		switch {
		case mine == RelBlocked:
			return errYouBlocked
		case theirs == RelBlocked:
			return errRequestFailed
		case mine == RelFriend, mine == RelOutgoing:
			return nil
		case mine == RelIncoming:
			if err := setRel(ctx, tx, me, other, RelFriend); err != nil {
				return err
			}
			if err := setRel(ctx, tx, other, me, RelFriend); err != nil {
				return err
			}
			a, b, err := s.pair(ctx, tx, me, other)
			if err != nil {
				return err
			}
			return events.Write(ctx, tx, relAdded(me, b, RelFriend), relAdded(other, a, RelFriend))
		}
		if err := s.limiter.Check(ctx, ruleFriendRequest, me.String()); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM relationships WHERE user_id = $1 AND type IN (1, 4)`, me).Scan(&n); err != nil {
			return err
		}
		if n >= maxFriends {
			return errTooManyFriends
		}
		if err := setRel(ctx, tx, me, other, RelOutgoing); err != nil {
			return err
		}
		if err := setRel(ctx, tx, other, me, RelIncoming); err != nil {
			return err
		}
		a, b, err := s.pair(ctx, tx, me, other)
		if err != nil {
			return err
		}
		return events.Write(ctx, tx, relAdded(me, b, RelOutgoing), relAdded(other, a, RelIncoming))
	})
}

// Block drops any friendship or request with the other user. The other user
// only sees the friendship or request disappear, never the block itself.
func (s *Service) Block(ctx context.Context, me, other uuid.UUID) error {
	return s.withPair(ctx, me, other, func(tx pgx.Tx, mine, theirs RelType) error {
		if mine == RelBlocked {
			return nil
		}
		evs := []events.Event{}
		if mine != 0 {
			evs = append(evs, relRemoved(me, other, mine))
		}
		if theirs != 0 && theirs != RelBlocked {
			if err := deleteRel(ctx, tx, other, me); err != nil {
				return err
			}
			evs = append(evs, relRemoved(other, me, theirs))
		}
		if err := setRel(ctx, tx, me, other, RelBlocked); err != nil {
			return err
		}
		_, b, err := s.pair(ctx, tx, me, other)
		if err != nil {
			return err
		}
		return events.Write(ctx, tx, append(evs, relAdded(me, b, RelBlocked))...)
	})
}

// Remove unfriends, cancels or declines a request, or unblocks.
func (s *Service) Remove(ctx context.Context, me, other uuid.UUID) error {
	return s.withPair(ctx, me, other, func(tx pgx.Tx, mine, theirs RelType) error {
		if mine == 0 {
			return httpx.ErrNotFound
		}
		if err := deleteRel(ctx, tx, me, other); err != nil {
			return err
		}
		evs := []events.Event{relRemoved(me, other, mine)}
		// The two sides of a friendship or request go together; a block is one-sided.
		if mine != RelBlocked && theirs != 0 && theirs != RelBlocked {
			if err := deleteRel(ctx, tx, other, me); err != nil {
				return err
			}
			evs = append(evs, relRemoved(other, me, theirs))
		}
		return events.Write(ctx, tx, evs...)
	})
}

// List returns the caller's relationships with public profiles.
func (s *Service) List(ctx context.Context, me uuid.UUID) ([]Relationship, error) {
	rows, err := s.pg.Query(ctx,
		`SELECT target_id, type, created_at FROM relationships WHERE user_id = $1 ORDER BY created_at, target_id`, me)
	if err != nil {
		return nil, err
	}
	rels, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Relationship, error) {
		var x Relationship
		err := r.Scan(&x.ID, &x.Type, &x.Since)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	return rels, auth.Attach(ctx, s.users, slicex.Ptrs(rels), func(r *Relationship) *uuid.UUID { return &r.ID },
		func(r *Relationship, u auth.PublicUser) { r.User = &u })
}

// Blocked reports whether either user blocked the other.
func (s *Service) Blocked(ctx context.Context, a, b uuid.UUID) (bool, error) {
	var ok bool
	err := s.pg.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM relationships
		                WHERE type = 2 AND ((user_id = $1 AND target_id = $2) OR (user_id = $2 AND target_id = $1)))`,
		a, b).Scan(&ok)
	return ok, err
}

func areFriends(ctx context.Context, q querier, a, b uuid.UUID) (bool, error) {
	t, err := relType(ctx, q, a, b)
	return t == RelFriend, err
}
