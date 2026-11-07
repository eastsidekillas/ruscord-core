package social

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// RingFor is how long a call rings for those who have not answered
// (decision of the owner, 2026-11-02).
const RingFor = 30 * time.Second

var (
	// errCannotCall does not say which rule failed, like errCannotDM.
	errCannotCall = httpx.NewError(http.StatusForbidden, "cannot_call", "Нельзя позвонить этому пользователю")
	errNoCall     = httpx.NewError(http.StatusNotFound, "no_call", "Звонка нет")
)

// Call is the call of a DM channel (docs/adr/0032-dm-calls.md). Members are
// those connected now; Ringing are those being called until RingUntil.
type Call struct {
	ID        uuid.UUID   `json:"id"`
	ChannelID uuid.UUID   `json:"channel_id"`
	StartedBy uuid.UUID   `json:"started_by"`
	StartedAt time.Time   `json:"started_at"`
	Ringing   []uuid.UUID `json:"ringing"`
	RingUntil time.Time   `json:"ring_until"`
	Members   []uuid.UUID `json:"members"`
}

// callDeleted is the payload of CALL_DELETE.
type callDeleted struct {
	ID        uuid.UUID `json:"id"`
	ChannelID uuid.UUID `json:"channel_id"`
}

const callSelect = `SELECT c.id, c.channel_id, c.started_by, c.started_at, c.ringing, c.ring_until,
        COALESCE(array_agg(m.user_id ORDER BY m.joined_at) FILTER (WHERE m.user_id IS NOT NULL), '{}')
 FROM dm_calls c LEFT JOIN dm_call_members m ON m.call_id = c.id `

func scanCall(r pgx.CollectableRow) (Call, error) {
	var c Call
	err := r.Scan(&c.ID, &c.ChannelID, &c.StartedBy, &c.StartedAt, &c.Ringing, &c.RingUntil, &c.Members)
	return c, err
}

// getCall loads the call of a channel (by channel) or by its own id; lock
// takes the call row lock that every change of the call holds.
func getCall(ctx context.Context, q dbtx, where string, id uuid.UUID, lock bool) (Call, error) {
	if lock {
		if _, err := q.Exec(ctx, `SELECT 1 FROM dm_calls c WHERE `+where+` FOR UPDATE`, id); err != nil {
			return Call{}, err
		}
	}
	rows, err := q.Query(ctx, callSelect+`WHERE `+where+` GROUP BY c.id`, id)
	if err != nil {
		return Call{}, err
	}
	c, err := pgx.CollectExactlyOneRow(rows, scanCall)
	if errors.Is(err, pgx.ErrNoRows) {
		return Call{}, errNoCall
	}
	return c, err
}

const (
	byChannel = `c.channel_id = $1`
	byID      = `c.id = $1`
)

// callEvent sends the call's current state (CALL_UPDATE or CALL_CREATE) to
// the channel's recipients.
func callEvent(ctx context.Context, tx pgx.Tx, c Call, typ string) error {
	c, err := getCall(ctx, tx, byID, c.ID, false)
	if err != nil {
		return err
	}
	return events.Write(ctx, tx, events.Event{Topic: events.ChannelTopic(c.ChannelID), Type: typ, Data: c})
}

// CallAccess returns what user may do in a call of the channel: the DM
// permissions (perm.InDM). Without CONNECT (a block in a 1:1) there is no
// call.
func (s *Service) CallAccess(ctx context.Context, user, channelID uuid.UUID) (perm.Permissions, error) {
	p, _, err := s.Permissions(ctx, user, channelID)
	if err != nil {
		return 0, err
	}
	if !p.Has(perm.Connect) {
		return 0, errCannotCall
	}
	return p, nil
}

// StartCall starts the channel's call, ringing everyone else for RingFor,
// or returns the call going on, taking user out of its ringing (they
// answer). Being in the call starts when LiveKit reports the connection
// (CallJoined). The caller checks CallAccess first.
func (s *Service) StartCall(ctx context.Context, user, channelID uuid.UUID) (Call, error) {
	var out Call
	err := pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		d, err := getDM(ctx, tx, channelID, true)
		if err != nil {
			return err
		}
		if !d.has(user) {
			return httpx.ErrNotFound
		}
		c, err := getCall(ctx, tx, byChannel, channelID, true)
		if errors.Is(err, errNoCall) {
			id := uuid.Must(uuid.NewV7())
			ring := slices.DeleteFunc(slices.Clone(d.recipientIDs), func(u uuid.UUID) bool { return u == user })
			if _, err := tx.Exec(ctx, `INSERT INTO dm_calls (id, channel_id, started_by, ringing, ring_until)
				VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))`, id, channelID, user, ring, RingFor.Seconds()); err != nil {
				return err
			}
			if err := callEvent(ctx, tx, Call{ID: id}, events.CallCreate); err != nil {
				return err
			}
			out, err = getCall(ctx, tx, byID, id, false)
			return err
		}
		if err != nil {
			return err
		}
		if out, err = stopRinging(ctx, tx, c, user); err != nil {
			return err
		}
		return nil
	})
	return out, err
}

// stopRinging takes user out of the call's ringing; others hear of it.
func stopRinging(ctx context.Context, tx pgx.Tx, c Call, user uuid.UUID) (Call, error) {
	if !slices.Contains(c.Ringing, user) {
		return c, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE dm_calls SET ringing = array_remove(ringing, $2) WHERE id = $1`, c.ID, user); err != nil {
		return Call{}, err
	}
	if err := callEvent(ctx, tx, c, events.CallUpdate); err != nil {
		return Call{}, err
	}
	return getCall(ctx, tx, byID, c.ID, false)
}

// DeclineCall stops the ringing for user; the call goes on for the others.
func (s *Service) DeclineCall(ctx context.Context, user, channelID uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		d, err := getDM(ctx, tx, channelID, false)
		if err != nil {
			return err
		}
		if !d.has(user) {
			return httpx.ErrNotFound
		}
		c, err := getCall(ctx, tx, byChannel, channelID, true)
		if err != nil {
			return err
		}
		_, err = stopRinging(ctx, tx, c, user)
		return err
	})
}

// CallJoined records that LiveKit connected user to a call as participant.
// It checks access again (a token lives a minute, a group may have changed);
// ok=false means the user must not be there and should be removed.
func (s *Service) CallJoined(ctx context.Context, user, callID uuid.UUID, participant string) (bool, error) {
	var channelID uuid.UUID
	err := s.pg.QueryRow(ctx, `SELECT channel_id FROM dm_calls WHERE id = $1`, callID).Scan(&channelID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // the call is over
	}
	if err != nil {
		return false, err
	}
	if _, err := s.CallAccess(ctx, user, channelID); err != nil {
		if errors.Is(err, errCannotCall) || errors.Is(err, httpx.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	ok := true
	err = pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		c, err := getCall(ctx, tx, byID, callID, true)
		if errors.Is(err, errNoCall) {
			ok = false
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO dm_call_members (call_id, user_id, participant) VALUES ($1, $2, $3)
			ON CONFLICT (call_id, user_id) DO UPDATE SET participant = EXCLUDED.participant`, callID, user, participant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE dm_calls SET ringing = array_remove(ringing, $2) WHERE id = $1`, callID, user); err != nil {
			return err
		}
		return callEvent(ctx, tx, c, events.CallUpdate)
	})
	return ok, err
}

// CallLeft clears user's membership if it still belongs to participant. The
// call ends when nobody is left in it (ended=true): CALL_DELETE.
func (s *Service) CallLeft(ctx context.Context, user, callID uuid.UUID, participant string) (ended bool, err error) {
	err = pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		c, err := getCall(ctx, tx, byID, callID, true)
		if errors.Is(err, errNoCall) {
			return nil
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM dm_call_members WHERE call_id = $1 AND user_id = $2 AND participant = $3`,
			callID, user, participant)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if len(c.Members) > 1 {
			return callEvent(ctx, tx, c, events.CallUpdate)
		}
		ended = true
		return endCall(ctx, tx, c)
	})
	return ended, err
}

func endCall(ctx context.Context, tx pgx.Tx, c Call) error {
	if _, err := tx.Exec(ctx, `DELETE FROM dm_calls WHERE id = $1`, c.ID); err != nil {
		return err
	}
	return events.Write(ctx, tx, events.Event{Topic: events.ChannelTopic(c.ChannelID), Type: events.CallDelete,
		Data: callDeleted{ID: c.ID, ChannelID: c.ChannelID}})
}

// EndIdleCalls ends the calls nobody connected to while they rang (the
// caller's connection failed or they gave up); it returns their ids, whose
// rooms the caller closes.
func (s *Service) EndIdleCalls(ctx context.Context) ([]uuid.UUID, error) {
	var ended []uuid.UUID
	err := pgx.BeginFunc(ctx, s.pg, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, channel_id FROM dm_calls c WHERE ring_until < now()
			AND NOT EXISTS (SELECT 1 FROM dm_call_members m WHERE m.call_id = c.id)
			FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		idle, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Call, error) {
			var c Call
			err := r.Scan(&c.ID, &c.ChannelID)
			return c, err
		})
		if err != nil {
			return err
		}
		for _, c := range idle {
			if err := endCall(ctx, tx, c); err != nil {
				return err
			}
			ended = append(ended, c.ID)
		}
		return nil
	})
	return ended, err
}

// CallMember is someone recorded in a call, for the periodic check against
// LiveKit; Allowed is false when they may not be there any more (left the
// group, blocked in a 1:1).
type CallMember struct {
	CallID      uuid.UUID
	UserID      uuid.UUID
	Participant string
	Allowed     bool
}

// CallMembers lists everyone recorded in a call now.
// CallsOf are the calls the user is in, with the participant of each.
func (s *Service) CallsOf(ctx context.Context, userID uuid.UUID) ([]CallMember, error) {
	rows, err := s.pg.Query(ctx, `SELECT call_id, user_id, participant FROM dm_call_members WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CallMember, error) {
		var m CallMember
		err := r.Scan(&m.CallID, &m.UserID, &m.Participant)
		return m, err
	})
}

func (s *Service) CallMembers(ctx context.Context) ([]CallMember, error) {
	rows, err := s.pg.Query(ctx, `SELECT m.call_id, m.user_id, m.participant, c.channel_id
		FROM dm_call_members m JOIN dm_calls c ON c.id = m.call_id`)
	if err != nil {
		return nil, err
	}
	type row struct {
		CallMember
		channel uuid.UUID
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.CallID, &x.UserID, &x.Participant, &x.channel)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	out := make([]CallMember, len(all))
	for i, x := range all {
		_, err := s.CallAccess(ctx, x.UserID, x.channel)
		if err != nil && !errors.Is(err, errCannotCall) && !errors.Is(err, httpx.ErrNotFound) {
			return nil, err
		}
		x.Allowed = err == nil
		out[i] = x.CallMember
	}
	return out, nil
}

// withCalls fills the Call of channels that have one going on.
func (s *Service) withCalls(ctx context.Context, ds []DM) error {
	if len(ds) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(ds))
	for i, d := range ds {
		ids[i] = d.ID
	}
	rows, err := s.pg.Query(ctx, callSelect+`WHERE c.channel_id = ANY($1) GROUP BY c.id`, ids)
	if err != nil {
		return err
	}
	calls, err := pgx.CollectRows(rows, scanCall)
	if err != nil {
		return err
	}
	for i := range ds {
		for j := range calls {
			if calls[j].ChannelID == ds[i].ID {
				ds[i].Call = &calls[j]
			}
		}
	}
	return nil
}
