package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/coder/websocket"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
)

// client drives one connection: handshake, heartbeats and limits.
type client struct {
	srv  *Server
	c    *conn
	sess *session

	windowStart time.Time
	windowCount int
}

func (cl *client) run(ctx context.Context, version string) {
	c := cl.c
	defer func() {
		c.kick(closeUnknownError, "")
		if cl.sess != nil {
			cl.sess.detach(c, c.closeCode())
		}
	}()
	if version != Version {
		c.kick(closeInvalidVersion, "unsupported version")
		cl.drain(ctx)
		return
	}
	hb := cl.srv.opts.Heartbeat
	c.send(encode(outFrame{Op: opHello, D: mustJSON(map[string]int64{"heartbeat_interval": hb.Milliseconds()})}))

	identifyTimer := time.AfterFunc(cl.srv.opts.IdentifyTimeout, func() { c.kick(closeNotAuthenticated, "identify timeout") })
	defer identifyTimer.Stop()
	heartbeatTimeout := hb + hb/2
	heartbeatTimer := time.AfterFunc(heartbeatTimeout, func() { c.kick(closeSessionTimedOut, "heartbeat timeout") })
	defer heartbeatTimer.Stop()

	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			if code := websocket.CloseStatus(err); code != -1 {
				c.kick(code, "") // the client closed; keep its code for detach
			}
			return
		}
		if c.closeCode() != 0 {
			continue // closing: ignore whatever arrives until the close completes
		}
		if !cl.allow() {
			c.kick(closeRateLimited, "rate limited")
			continue
		}
		if typ != websocket.MessageText {
			c.kick(closeDecodeError, "text frames only")
			continue
		}
		var f inFrame
		if json.Unmarshal(data, &f) != nil {
			c.kick(closeDecodeError, "invalid json")
			continue
		}
		switch f.Op {
		case opHeartbeat:
			var seq *uint64
			if json.Unmarshal(f.D, &seq) != nil {
				c.kick(closeDecodeError, "invalid heartbeat")
				continue
			}
			heartbeatTimer.Reset(heartbeatTimeout)
			if cl.sess != nil && seq != nil {
				cl.sess.ack(*seq)
			}
			c.send(encode(outFrame{Op: opHeartbeatAck}))
		case opIdentify, opResume:
			if cl.sess != nil {
				c.kick(closeAlreadyAuthenticated, "already authenticated")
				continue
			}
			if code, reason := cl.authenticate(ctx, f); code != 0 {
				c.kick(code, reason)
				continue
			}
			identifyTimer.Stop()
		default:
			c.kick(closeUnknownOpcode, "unknown opcode")
		}
	}
}

// drain reads until the close handshake completes after a kick.
func (cl *client) drain(ctx context.Context) {
	for {
		if _, _, err := cl.c.ws.Read(ctx); err != nil {
			return
		}
	}
}

// allow is a fixed-window limit on frames from the client.
func (cl *client) allow() bool {
	now := cl.srv.now()
	if now.Sub(cl.windowStart) >= time.Minute {
		cl.windowStart, cl.windowCount = now, 0
	}
	cl.windowCount++
	return cl.windowCount <= maxInboundPerMin
}

func (cl *client) authenticate(ctx context.Context, f inFrame) (websocket.StatusCode, string) {
	if f.Op == opIdentify {
		var d identifyData
		if json.Unmarshal(f.D, &d) != nil {
			return closeDecodeError, "invalid identify"
		}
		return cl.identify(ctx, d)
	}
	var d resumeData
	if json.Unmarshal(f.D, &d) != nil {
		return closeDecodeError, "invalid resume"
	}
	return cl.resume(ctx, d)
}

func (cl *client) principal(ctx context.Context, ticket string) (auth.Principal, websocket.StatusCode) {
	if ticket == "" || len(ticket) > maxTicketLen {
		return auth.Principal{}, closeAuthFailed
	}
	p, err := cl.srv.deps.Auth.ConsumeTicket(ctx, ticket)
	if errors.Is(err, auth.ErrInvalidTicket) {
		return auth.Principal{}, closeAuthFailed
	}
	if err != nil {
		cl.srv.log.Error("gateway: consume ticket", "err", err)
		return auth.Principal{}, closeUnknownError
	}
	return p, 0
}

// identify starts a new session for the ticket's user.
func (cl *client) identify(ctx context.Context, d identifyData) (websocket.StatusCode, string) {
	s := cl.srv
	p, code := cl.principal(ctx, d.Ticket)
	if code != 0 {
		return code, "authentication failed"
	}
	if !s.reserve(p.UserID) {
		return closeTooManySessions, "too many sessions"
	}
	ss, err := s.start(ctx, p, cl.c)
	if err != nil {
		s.release(p.UserID)
		return cl.failure("identify", err)
	}
	cl.sess = ss
	return 0, ""
}

// resume attaches the connection to a session of this node. Only the same
// sign-in session (same user, same device) may resume it.
func (cl *client) resume(ctx context.Context, d resumeData) (websocket.StatusCode, string) {
	s := cl.srv
	p, code := cl.principal(ctx, d.Ticket)
	if code != 0 {
		return code, "authentication failed"
	}
	if len(d.SessionID) > maxSessionIDLength {
		return closeSessionInvalid, "session invalid"
	}
	ss := s.lookup(d.SessionID)
	if ss == nil || ss.principal != p {
		return closeSessionInvalid, "session invalid"
	}
	if err := s.checkActive(ctx, p); err != nil {
		if errors.Is(err, errInactive) {
			ss.end(closeAuthFailed, "session revoked")
		}
		return cl.failure("resume", err)
	}
	if !ss.resume(cl.c, d.Seq) {
		ss.end(closeSessionInvalid, "session invalid")
		return closeSessionInvalid, "session invalid"
	}
	cl.sess = ss
	return 0, ""
}

// failure maps an error of identify or resume to a close code.
func (cl *client) failure(op string, err error) (websocket.StatusCode, string) {
	switch {
	case errors.Is(err, errInactive):
		return closeAuthFailed, "authentication failed"
	case errors.Is(err, errShuttingDown):
		return closeRestart, "server restart"
	}
	cl.srv.log.Error("gateway: "+op, "err", err)
	return closeUnknownError, "internal error"
}
