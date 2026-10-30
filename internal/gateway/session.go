package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/events"
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

// end stops the session for good and closes its connection with code (if
// code is not 0). Safe to call more than once.
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
	if c != nil && code != 0 {
		c.kick(code, reason)
	}
	s.srv.forget(s)
}

// pump feeds stream events through the router into the session, starting
// at sequence next. It restarts the consumer whenever the router reports a
// change of topics, right after the event that caused it, so a guild's
// events start exactly after GUILD_CREATE and stop exactly at GUILD_DELETE.
func (s *session) pump(ctx context.Context, r *router, next uint64) {
	for {
		var err error
		next, err = s.consume(ctx, r, next)
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
func (s *session) consume(ctx context.Context, r *router, next uint64) (uint64, error) {
	cons, err := s.srv.js.OrderedConsumer(ctx, events.StreamName, jetstream.OrderedConsumerConfig{
		FilterSubjects: r.subjects(),
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
		res, err := r.route(ctx, event{Subject: msg.Subject(), Type: msg.Headers().Get(events.HeaderType), Data: msg.Data()})
		if err != nil {
			return next, err
		}
		if res.revoked {
			return next, errRevoked
		}
		s.emit(res.out)
		next = meta.Sequence.Stream + 1
		if res.resubscribe {
			return next, nil
		}
	}
}
