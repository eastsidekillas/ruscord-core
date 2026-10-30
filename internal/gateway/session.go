package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/presence"
)

// session is the logical connection of one device. It outlives the
// WebSocket: when the socket drops, the session keeps consuming events into
// its replay buffer for the resume window, and a RESUME on this node picks
// up exactly where the client stopped.
type session struct {
	id        string
	principal auth.Principal
	srv       *Server
	cancel    context.CancelFunc // stops the pump

	// routeMu serialises the router between the stream pump and ephemeral
	// deliveries; it is taken before mu, never after.
	routeMu sync.Mutex
	r       *router

	// pmu guards the ephemeral subscriptions and this device's state.
	pmu   sync.Mutex
	eph   map[string]bool
	state presence.Status // online or idle (op 3)

	mu         sync.Mutex
	buf        *replayBuffer
	conn       *conn // nil while detached
	detachedAt time.Time
	ended      bool
}

// emit numbers the dispatches, stores them for resume and queues them on
// the attached connection, all under one lock so the order is the same in
// the buffer and on the wire.
func (s *session) emit(ds []dispatch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range ds {
		f := dispatchFrame(s.buf.last()+1, d)
		s.buf.push(f)
		if s.conn != nil {
			s.conn.send(f)
		}
	}
}

// ack frees frames the client confirmed in a heartbeat.
func (s *session) ack(seq uint64) {
	s.mu.Lock()
	s.buf.ack(seq)
	s.mu.Unlock()
}

// detach is called when c is gone. The session stays resumable unless the
// close ended it. A connection replaced by a RESUME no longer speaks for the
// session, so its close changes nothing.
func (s *session) detach(c *conn, code websocket.StatusCode) {
	s.mu.Lock()
	current := s.conn == c
	if current {
		s.conn = nil
		s.detachedAt = s.srv.now()
	}
	s.mu.Unlock()
	if current && endsSession(code) {
		s.end(0, "")
	}
}

// resume attaches c and queues every frame after seq, then RESUMED. It
// fails when the session ended or the frames after seq are no longer kept.
func (s *session) resume(c *conn, seq uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	frames, ok := s.buf.since(seq)
	if !ok {
		return false
	}
	if s.conn != nil {
		// The old socket is usually already dead; make sure it stops.
		s.conn.kick(closeSessionInvalid, "resumed elsewhere")
	}
	s.buf.ack(seq)
	s.conn = c
	for _, f := range frames {
		c.send(f)
	}
	c.send(encode(outFrame{Op: opDispatch, T: typeResumed, D: mustJSON(struct{}{})}))
	return true
}

// expired reports whether the session sat detached longer than window.
func (s *session) expired(now time.Time, window time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn == nil && !s.detachedAt.IsZero() && now.Sub(s.detachedAt) > window
}

// ephemeral delivers an ephemeral event (typing, presence) from the hub.
func (s *session) ephemeral(ev event) {
	s.routeMu.Lock()
	res := s.r.routeEphemeral(ev)
	s.routeMu.Unlock()
	s.emit(res.out)
}

// syncEph subscribes to the ephemeral subjects the router now needs.
func (s *session) syncEph() {
	s.routeMu.Lock()
	want := s.r.ephSubjects()
	s.routeMu.Unlock()
	s.pmu.Lock()
	defer s.pmu.Unlock()
	// end() marks the session ended before it clears the subscriptions under
	// pmu, so checking here, under pmu, cannot resubscribe an ended session.
	s.mu.Lock()
	ended := s.ended
	s.mu.Unlock()
	if ended {
		want = nil
	}
	if s.eph == nil && want == nil {
		return
	}
	var err error
	s.eph, err = s.srv.hub.sync(s, s.eph, want)
	if err != nil {
		s.srv.log.Warn("gateway: ephemeral subscriptions", "user_id", s.principal.UserID, "err", err)
	}
}

// presenceTimeout bounds a presence call; a slow Valkey must not stall the
// connection, and a lost update only delays a status.
const presenceTimeout = 2 * time.Second

func (s *session) presenceCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), presenceTimeout)
}

// touch marks this device live in its state for another presence TTL.
func (s *session) touch() {
	s.pmu.Lock()
	state := s.state
	s.pmu.Unlock()
	ctx, cancel := s.presenceCtx()
	defer cancel()
	if err := s.srv.deps.Presence.Touch(ctx, s.principal.UserID, s.id, state); err != nil {
		s.srv.log.Warn("gateway: presence touch", "user_id", s.principal.UserID, "err", err)
	}
}

// refreshPresence recomputes what others see of the user and publishes it
// if it changed.
func (s *session) refreshPresence() {
	s.routeMu.Lock()
	choice := s.r.choice
	s.routeMu.Unlock()
	ctx, cancel := s.presenceCtx()
	defer cancel()
	if _, err := s.srv.deps.Presence.Refresh(ctx, s.principal.UserID, choice); err != nil {
		s.srv.log.Warn("gateway: presence refresh", "user_id", s.principal.UserID, "err", err)
	}
}

// setState records op 3 (this device is in use or idle).
func (s *session) setState(st presence.Status) {
	s.pmu.Lock()
	changed := s.state != st
	s.state = st
	s.pmu.Unlock()
	if changed {
		s.touch()
		s.refreshPresence()
	}
}

// sendStatuses delivers the current status of users the client has just
// started to care about (new friends): PRESENCE_UPDATE only reports changes.
func (s *session) sendStatuses(users []uuid.UUID) {
	ctx, cancel := s.presenceCtx()
	defer cancel()
	st, err := s.srv.deps.Presence.Statuses(ctx, users)
	if err != nil {
		s.srv.log.Warn("gateway: presence statuses", "user_id", s.principal.UserID, "err", err)
		return
	}
	var out []dispatch
	s.routeMu.Lock()
	for _, u := range users {
		if st[u] == presence.Offline {
			// Like READY: anyone not reported is offline, so this is not sent;
			// it is still recorded, so a later "offline" is not taken as news.
			s.r.seen[u] = presence.Offline
			continue
		}
		out = append(out, s.r.presence(presence.Update{UserID: u, Status: st[u]}).out...)
	}
	s.routeMu.Unlock()
	s.emit(out)
}

// end stops the session for good and closes its connection, if one is
// attached, with code. Code 0 means the caller expected no connection (an
// expired or evicted session); one that attached in the meantime is closed
// with 4006 rather than left open on a session that no longer delivers.
// Safe to call more than once.
func (s *session) end(code websocket.StatusCode, reason string) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	c := s.conn
	s.conn = nil
	s.mu.Unlock()

	s.cancel()
	if c != nil {
		if code == 0 {
			code, reason = closeSessionInvalid, "session ended"
		}
		c.kick(code, reason)
	}
	s.srv.forget(s)

	s.pmu.Lock()
	s.eph, _ = s.srv.hub.sync(s, s.eph, nil)
	s.pmu.Unlock()
	// On a restart the device is expected back within the presence TTL, so
	// its key is left to expire: dropping it would show every user of the
	// node offline and online again to all their friends on each deploy.
	if code == closeRestart {
		return
	}
	ctx, cancel := s.presenceCtx()
	defer cancel()
	if err := s.srv.deps.Presence.Drop(ctx, s.principal.UserID, s.id); err != nil {
		s.srv.log.Warn("gateway: presence drop", "user_id", s.principal.UserID, "err", err)
		return
	}
	s.refreshPresence()
}

// pump feeds stream events through the router into the session, starting
// at sequence next. It restarts the consumer whenever the router reports a
// change of topics, right after the event that caused it, so a guild's
// events start exactly after GUILD_CREATE and stop exactly at GUILD_DELETE.
func (s *session) pump(ctx context.Context, next uint64) {
	// A bug in one session must not take the whole node down.
	defer func() {
		if v := recover(); v != nil {
			s.srv.log.Error("gateway session panicked", "user_id", s.principal.UserID, "panic", v, "stack", string(debug.Stack()))
			s.end(closeUnknownError, "internal error")
		}
	}()
	for {
		var err error
		next, err = s.consume(ctx, next)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errRevoked) {
			s.end(closeAuthFailed, "session revoked")
			return
		}
		if err != nil {
			s.srv.log.Error("gateway session failed", "user_id", s.principal.UserID, "err", err)
			s.end(closeUnknownError, "internal error")
			return
		}
	}
}

var errRevoked = errors.New("gateway: session revoked")

// consume runs one ordered consumer until the topics change (nil error,
// with the next sequence to start from) or something fails.
func (s *session) consume(ctx context.Context, next uint64) (uint64, error) {
	s.routeMu.Lock()
	subjects := s.r.subjects()
	s.routeMu.Unlock()
	cons, err := s.srv.js.OrderedConsumer(ctx, events.StreamName, jetstream.OrderedConsumerConfig{
		FilterSubjects: subjects,
		DeliverPolicy:  jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:    next,
	})
	if err != nil {
		return next, fmt.Errorf("consumer: %w", err)
	}
	it, err := cons.Messages()
	if err != nil {
		return next, fmt.Errorf("messages: %w", err)
	}
	defer it.Stop()
	stop := context.AfterFunc(ctx, it.Stop)
	defer stop()

	for {
		msg, err := it.Next()
		if err != nil {
			return next, fmt.Errorf("next: %w", err)
		}
		meta, err := msg.Metadata()
		if err != nil {
			return next, fmt.Errorf("metadata: %w", err)
		}
		next = meta.Sequence.Stream + 1
		if !json.Valid(msg.Data()) {
			// The relay only publishes JSON from the outbox; anything else is
			// not ours to forward.
			s.srv.log.Warn("gateway: skipping event with invalid JSON", "subject", msg.Subject(), "seq", meta.Sequence.Stream)
			continue
		}
		id, _ := strconv.ParseInt(msg.Headers().Get(events.HeaderOutboxID), 10, 64) // 0 if absent
		s.routeMu.Lock()
		res, err := s.r.route(ctx, event{Subject: msg.Subject(), Type: msg.Headers().Get(events.HeaderType), Data: msg.Data(), ID: id})
		s.routeMu.Unlock()
		if err != nil {
			return next, err
		}
		if res.revoked {
			return next, errRevoked
		}
		s.emit(res.out)
		if res.resubscribe || res.ephChanged {
			s.syncEph()
		}
		if len(res.newFriends) > 0 {
			s.sendStatuses(res.newFriends)
		}
		if res.choiceChanged {
			s.refreshPresence()
		}
		if res.resubscribe {
			return next, nil
		}
	}
}
