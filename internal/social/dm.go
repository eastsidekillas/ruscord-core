package social

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/slicex"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
)

type DMType int16

const (
	DMDirect DMType = 1
	DMGroup  DMType = 3
)

const maxGroupSize = 10 // including the owner

var (
	// errCannotDM does not say which rule failed, so blocks stay invisible.
	errCannotDM     = httpx.NewError(http.StatusForbidden, "cannot_dm", "Нельзя написать этому пользователю")
	errNotFriends   = httpx.NewError(http.StatusBadRequest, "not_friends", "В группу можно добавить только друзей")
	errGroupFull    = httpx.NewError(http.StatusBadRequest, "group_full", "В группе не больше 10 участников")
	errNotGroup     = httpx.NewError(http.StatusBadRequest, "not_group", "Это действие доступно только в групповом чате")
	errNotOwner     = httpx.NewError(http.StatusForbidden, "forbidden", "Удалять участников может только владелец группы")
	errAlreadyInDM  = httpx.NewError(http.StatusConflict, "already_recipient", "Пользователь уже в группе")
	ruleGroupCreate = ratelimit.Rule{Name: "group_dm_create", Limit: 10, Window: time.Hour}
)

type DM struct {
	ID         uuid.UUID         `json:"id"`
	Type       DMType            `json:"type"`
	Name       *string           `json:"name,omitempty"`
	OwnerID    *uuid.UUID        `json:"owner_id,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	Recipients []auth.PublicUser `json:"recipients"`

	recipientIDs []uuid.UUID
}

const dmSelect = `SELECT c.id, c.type, c.name, c.owner_id, c.created_at,
        array_agg(r.user_id ORDER BY r.joined_at, r.user_id)
 FROM dm_channels c JOIN dm_recipients r ON r.channel_id = c.id `

func scanDM(r pgx.CollectableRow) (DM, error) {
	var d DM
	err := r.Scan(&d.ID, &d.Type, &d.Name, &d.OwnerID, &d.CreatedAt, &d.recipientIDs)
	return d, err
}

func (d DM) has(user uuid.UUID) bool {
	for _, id := range d.recipientIDs {
		if id == user {
			return true
		}
	}
	return false
}

// getDM loads a DM with its recipients; lock takes the channel row lock that
// every membership change holds.
func getDM(ctx context.Context, q dbtx, id uuid.UUID, lock bool) (DM, error) {
	if lock {
		if _, err := q.Exec(ctx, `SELECT 1 FROM dm_channels WHERE id = $1 FOR UPDATE`, id); err != nil {
			return DM{}, err
		}
	}
	rows, err := q.Query(ctx, dmSelect+`WHERE c.id = $1 GROUP BY c.id`, id)
	if err != nil {
		return DM{}, err
	}
	d, err := pgx.CollectExactlyOneRow(rows, scanDM)
	if errors.Is(err, pgx.ErrNoRows) {
		return DM{}, httpx.ErrNotFound
	}
	return d, err
}

// withRecipients fills Recipients from recipientIDs.
func (s *Service) withRecipients(ctx context.Context, ds []DM) error {
	var ids []uuid.UUID
	for _, d := range ds {
		ids = append(ids, d.recipientIDs...)
	}
	users, err := s.users.Users(ctx, ids)
	if err != nil {
		return err
	}
	for _, d := range slicex.Ptrs(ds) {
		d.Recipients = make([]auth.PublicUser, 0, len(d.recipientIDs))
		for _, id := range d.recipientIDs {
			if u, ok := users[id]; ok {
				d.Recipients = append(d.Recipients, u)
			}
		}
	}
	return nil
}

// fill returns d with Recipients set.
func (s *Service) fill(ctx context.Context, d DM) (DM, error) {
	ds := []DM{d}
	err := s.withRecipients(ctx, ds)
	return ds[0], err
}

// dmEvent loads the channel with recipients and addresses an event about it
// to every recipient's user topic.
func (s *Service) dmEvent(ctx context.Context, tx pgx.Tx, id uuid.UUID, typ string) (DM, []events.Event, error) {
	d, err := getDM(ctx, tx, id, false)
	if err != nil {
		return DM{}, nil, err
	}
	if d, err = s.fill(ctx, d); err != nil {
		return DM{}, nil, err
	}
	evs := make([]events.Event, 0, len(d.recipientIDs))
	for _, u := range d.recipientIDs {
		evs = append(evs, events.Event{Topic: events.UserTopic(u), Type: typ, Data: d})
	}
	return d, evs, nil
}

// OpenDM returns the 1:1 DM with other, creating it when needed. It is
// allowed between friends and between members of a common guild, unless
// either user blocked the other.
func (s *Service) OpenDM(ctx context.Context, me, other uuid.UUID) (DM, error) {
	if me == other {
		return DM{}, errCannotDM
	}
	blocked, err := s.Blocked(ctx, me, other)
	if err != nil {
		return DM{}, err
	}
	if blocked {
		return DM{}, errCannotDM
	}
	ok, err := areFriends(ctx, s.pg, me, other)
	if err != nil {
		return DM{}, err
	}
	if !ok {
		if ok, err = s.guilds.SharesGuild(ctx, me, other); err != nil {
			return DM{}, err
		}
	}
	if !ok {
		return DM{}, errCannotDM
	}

	var out DM
	err = pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		key := pairKey(me, other)
		var id uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM dm_channels WHERE pair_key = $1`, key).Scan(&id)
		created := false
		if errors.Is(err, pgx.ErrNoRows) {
			id = uuid.Must(uuid.NewV7())
			tag, err := tx.Exec(ctx,
				`INSERT INTO dm_channels (id, type, pair_key) VALUES ($1, 1, $2) ON CONFLICT (pair_key) DO NOTHING`, id, key)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 { // created concurrently
				if err := tx.QueryRow(ctx, `SELECT id FROM dm_channels WHERE pair_key = $1`, key).Scan(&id); err != nil {
					return err
				}
			} else {
				created = true
				if _, err := tx.Exec(ctx,
					`INSERT INTO dm_recipients (channel_id, user_id) VALUES ($1, $2), ($1, $3)`, id, me, other); err != nil {
					return err
				}
			}
		} else if err != nil {
			return err
		}
		if !created {
			d, err := getDM(ctx, tx, id, false)
			if err != nil {
				return err
			}
			out, err = s.fill(ctx, d)
			return err
		}
		d, evs, err := s.dmEvent(ctx, tx, id, events.ChannelCreate)
		out = d
		if err != nil {
			return err
		}
		return events.Write(ctx, tx, evs...)
	})
	return out, err
}

type GroupInput struct {
	Recipients []uuid.UUID `json:"recipients"`
	Name       *string     `json:"name"`
}

// CreateGroup makes a group DM owned by the caller with up to nine friends.
func (s *Service) CreateGroup(ctx context.Context, me uuid.UUID, in GroupInput) (DM, error) {
	fields := httpx.Fields{}
	seen := map[uuid.UUID]bool{me: true}
	others := make([]uuid.UUID, 0, len(in.Recipients))
	for _, id := range in.Recipients {
		if !seen[id] {
			seen[id] = true
			others = append(others, id)
		}
	}
	if len(others) < 1 || len(others) > maxGroupSize-1 {
		fields["recipients"] = "От 1 до 9 друзей"
	}
	if in.Name != nil {
		*in.Name = strings.TrimSpace(*in.Name)
		fields.Length("name", *in.Name, 1, 100)
	}
	if err := fields.Err(); err != nil {
		return DM{}, err
	}
	if err := s.limiter.Check(ctx, ruleGroupCreate, me.String()); err != nil {
		return DM{}, err
	}
	var out DM
	err := pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		for _, id := range others {
			ok, err := areFriends(ctx, tx, me, id)
			if err != nil {
				return err
			}
			if !ok {
				return errNotFriends
			}
		}
		id := uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx, `INSERT INTO dm_channels (id, type, name, owner_id) VALUES ($1, 3, $2, $3)`, id, in.Name, me); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO dm_recipients (channel_id, user_id) SELECT $1, unnest($2::uuid[])`, id, append([]uuid.UUID{me}, others...)); err != nil {
			return err
		}
		d, evs, err := s.dmEvent(ctx, tx, id, events.ChannelCreate)
		out = d
		if err != nil {
			return err
		}
		return events.Write(ctx, tx, evs...)
	})
	return out, err
}

// ListDMs returns the caller's DM channels, newest first.
func (s *Service) ListDMs(ctx context.Context, me uuid.UUID) ([]DM, error) {
	rows, err := s.pg.Query(ctx,
		dmSelect+`WHERE c.id IN (SELECT channel_id FROM dm_recipients WHERE user_id = $1)
		 GROUP BY c.id ORDER BY c.id DESC`, me)
	if err != nil {
		return nil, err
	}
	ds, err := pgx.CollectRows(rows, scanDM)
	if err != nil {
		return nil, err
	}
	return ds, s.withRecipients(ctx, ds)
}

// withGroup runs fn on a group DM the caller belongs to, under its lock.
func (s *Service) withGroup(ctx context.Context, me, id uuid.UUID, fn func(tx pgx.Tx, d DM) error) error {
	return pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		d, err := getDM(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if !d.has(me) {
			return httpx.ErrNotFound
		}
		if d.Type != DMGroup {
			return errNotGroup
		}
		return fn(tx, d)
	})
}

// RenameGroup sets or clears (empty or null name) the group's name.
func (s *Service) RenameGroup(ctx context.Context, me, id uuid.UUID, name *string) (DM, error) {
	if name != nil {
		n := strings.TrimSpace(*name)
		name = &n
		if n == "" {
			name = nil
		}
	}
	if name != nil {
		fields := httpx.Fields{}
		fields.Length("name", *name, 1, 100)
		if err := fields.Err(); err != nil {
			return DM{}, err
		}
	}
	var out DM
	err := s.withGroup(ctx, me, id, func(tx pgx.Tx, d DM) error {
		if _, err := tx.Exec(ctx, `UPDATE dm_channels SET name = $2 WHERE id = $1`, id, name); err != nil {
			return err
		}
		var evs []events.Event
		var err error
		out, evs, err = s.dmEvent(ctx, tx, id, events.ChannelUpdate)
		if err != nil {
			return err
		}
		return events.Write(ctx, tx, evs...)
	})
	return out, err
}

// AddRecipient adds one of the caller's friends to a group the caller is in.
func (s *Service) AddRecipient(ctx context.Context, me, id, user uuid.UUID) error {
	return s.withGroup(ctx, me, id, func(tx pgx.Tx, d DM) error {
		if d.has(user) {
			return errAlreadyInDM
		}
		if len(d.recipientIDs) >= maxGroupSize {
			return errGroupFull
		}
		ok, err := areFriends(ctx, tx, me, user)
		if err != nil {
			return err
		}
		if !ok {
			return errNotFriends
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dm_recipients (channel_id, user_id) VALUES ($1, $2)`, id, user); err != nil {
			return err
		}
		return s.writeRecipientChange(ctx, tx, id, user, events.RecipientAdd)
	})
}

// RemoveRecipient removes user from the group: the owner may remove anyone,
// everyone may remove themself (leave). An owner who leaves hands the group
// to the longest-standing recipient; the last one out deletes it.
func (s *Service) RemoveRecipient(ctx context.Context, me, id, user uuid.UUID) error {
	return s.withGroup(ctx, me, id, func(tx pgx.Tx, d DM) error {
		if !d.has(user) {
			return httpx.ErrNotFound
		}
		isOwner := d.OwnerID != nil && *d.OwnerID == me
		if user != me && !isOwner {
			return errNotOwner
		}
		if _, err := tx.Exec(ctx, `DELETE FROM dm_recipients WHERE channel_id = $1 AND user_id = $2`, id, user); err != nil {
			return err
		}
		if len(d.recipientIDs) == 1 {
			if _, err := tx.Exec(ctx, `DELETE FROM dm_channels WHERE id = $1`, id); err != nil {
				return err
			}
			return events.Write(ctx, tx, events.Event{Topic: events.UserTopic(user), Type: events.ChannelDelete,
				Data: map[string]any{"id": id}})
		}
		if d.OwnerID != nil && *d.OwnerID == user {
			if _, err := tx.Exec(ctx,
				`UPDATE dm_channels SET owner_id = (SELECT user_id FROM dm_recipients WHERE channel_id = $1
				                                    ORDER BY joined_at, user_id LIMIT 1)
				 WHERE id = $1`, id); err != nil {
				return err
			}
		}
		if err := s.writeRecipientChange(ctx, tx, id, user, events.RecipientRemove); err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{Topic: events.UserTopic(user), Type: events.ChannelDelete,
			Data: map[string]any{"id": id}})
	})
}

// writeRecipientChange tells the remaining recipients (and the changed user)
// about the new member list.
func (s *Service) writeRecipientChange(ctx context.Context, tx pgx.Tx, id, user uuid.UUID, typ string) error {
	d, evs, err := s.dmEvent(ctx, tx, id, typ)
	if err != nil {
		return err
	}
	users, err := s.users.Users(ctx, []uuid.UUID{user})
	if err != nil {
		return err
	}
	data := map[string]any{"channel_id": id, "user": users[user], "channel": d}
	for i := range evs {
		evs[i].Data = data
	}
	if typ == events.RecipientAdd {
		// The new member gets the whole channel, like on creation.
		for i := range evs {
			if evs[i].Topic == events.UserTopic(user) {
				evs[i].Type, evs[i].Data = events.ChannelCreate, d
			}
		}
	}
	return events.Write(ctx, tx, evs...)
}

// Permissions returns what user may do in a DM channel (perm.InDM): nothing
// when not a recipient, read-only when a 1:1 pair has a block. Messages use
// it in step 4c-2.
func (s *Service) Permissions(ctx context.Context, user, id uuid.UUID) (perm.Permissions, []uuid.UUID, error) {
	d, err := getDM(ctx, s.pg, id, false)
	if err != nil {
		return 0, nil, err
	}
	if !d.has(user) {
		return 0, nil, httpx.ErrNotFound
	}
	blocked := false
	if d.Type == DMDirect {
		for _, other := range d.recipientIDs {
			if other != user {
				if blocked, err = s.Blocked(ctx, user, other); err != nil {
					return 0, nil, err
				}
			}
		}
	}
	return perm.InDM(true, blocked), d.recipientIDs, nil
}
