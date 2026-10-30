package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/platform/natstest"
)

// Every way a connection can be refused or cut, with the close code the
// client sees. The code tells the client whether to resume, identify again
// or stop, so each one is part of the protocol.
func TestHandshakeCloseCodes(t *testing.T) {
	h := newHarness(t, Options{IdentifyTimeout: 300 * time.Millisecond, Heartbeat: 400 * time.Millisecond, MaxSessionsPerUser: 1})

	identify := func(c *wsClient, p auth.Principal) {
		c.expectOp(opHello)
		c.send(opIdentify, map[string]string{"ticket": h.ticket(p)})
		c.expectDispatch(typeReady)
	}
	cases := []struct {
		name  string
		query string
		run   func(c *wsClient)
		want  websocket.StatusCode
	}{
		{name: "missing version", query: "", run: func(*wsClient) {}, want: closeInvalidVersion},
		{name: "unknown version", query: "?v=2", run: func(*wsClient) {}, want: closeInvalidVersion},
		{name: "silence until identify timeout", run: func(c *wsClient) { c.expectOp(opHello) }, want: closeNotAuthenticated},
		{name: "heartbeats do not replace identify", run: func(c *wsClient) {
			c.expectOp(opHello)
			for range 3 {
				c.send(opHeartbeat, nil)
				c.expectOp(opHeartbeatAck)
				time.Sleep(120 * time.Millisecond)
			}
		}, want: closeNotAuthenticated},
		{name: "invalid json", run: func(c *wsClient) { c.sendRaw(websocket.MessageText, []byte("{op:")) }, want: closeDecodeError},
		{name: "binary frame", run: func(c *wsClient) { c.sendRaw(websocket.MessageBinary, []byte(`{"op":1}`)) }, want: closeDecodeError},
		{name: "heartbeat with junk", run: func(c *wsClient) { c.send(opHeartbeat, "seven") }, want: closeDecodeError},
		{name: "identify with junk", run: func(c *wsClient) { c.send(opIdentify, 5) }, want: closeDecodeError},
		{name: "unknown opcode", run: func(c *wsClient) { c.send(42, nil) }, want: closeUnknownOpcode},
		{name: "client sends a dispatch", run: func(c *wsClient) { c.send(opDispatch, nil) }, want: closeUnknownOpcode},
		{name: "unknown ticket", run: func(c *wsClient) { c.send(opIdentify, map[string]string{"ticket": "nope"}) }, want: closeAuthFailed},
		{name: "empty ticket", run: func(c *wsClient) { c.send(opIdentify, map[string]string{}) }, want: closeAuthFailed},
		{name: "overlong ticket", run: func(c *wsClient) {
			c.send(opIdentify, map[string]string{"ticket": string(bytes.Repeat([]byte("a"), maxTicketLen+1))})
		}, want: closeAuthFailed},
		{name: "revoked sign-in session", run: func(c *wsClient) {
			p := h.user()
			h.auth.mu.Lock()
			h.auth.inactive[p.SessionID] = true
			h.auth.mu.Unlock()
			c.send(opIdentify, map[string]string{"ticket": h.ticket(p)})
		}, want: closeAuthFailed},
		{name: "ticket used twice", run: func(c *wsClient) {
			p := h.user()
			tk := h.ticket(p)
			first := h.dial("?v=1")
			first.expectOp(opHello)
			first.send(opIdentify, map[string]string{"ticket": tk})
			first.expectDispatch(typeReady)
			c.send(opIdentify, map[string]string{"ticket": tk})
		}, want: closeAuthFailed},
		{name: "identify twice", run: func(c *wsClient) {
			identify(c, h.user())
			c.send(opIdentify, map[string]string{"ticket": h.ticket(h.user())})
		}, want: closeAlreadyAuthenticated},
		{name: "resume after identify", run: func(c *wsClient) {
			identify(c, h.user())
			c.send(opResume, map[string]any{"ticket": "x", "session_id": "y", "seq": 1})
		}, want: closeAlreadyAuthenticated},
		{name: "missed heartbeat", run: func(c *wsClient) { identify(c, h.user()) }, want: closeSessionTimedOut},
		{name: "frame over the size limit", run: func(c *wsClient) {
			c.sendRaw(websocket.MessageText, bytes.Repeat([]byte(" "), maxInboundBytes+1))
		}, want: websocket.StatusMessageTooBig},
		{name: "too many frames", run: func(c *wsClient) {
			for range maxInboundPerMin + 1 {
				c.sendRaw(websocket.MessageText, []byte(`{"op":1,"d":null}`))
			}
		}, want: closeRateLimited},
		{name: "second session of the same user over the cap", run: func(c *wsClient) {
			p := h.user()
			first := h.dial("?v=1")
			identify(first, p)
			c.send(opIdentify, map[string]string{"ticket": h.ticket(auth.Principal{UserID: p.UserID, SessionID: uuid.New()})})
		}, want: closeTooManySessions},
		{name: "resume of an unknown session", run: func(c *wsClient) {
			c.send(opResume, map[string]any{"ticket": h.ticket(h.user()), "session_id": "nope", "seq": 1})
		}, want: closeSessionInvalid},
		{name: "resume with a bad ticket", run: func(c *wsClient) {
			c.send(opResume, map[string]any{"ticket": "nope", "session_id": "nope", "seq": 1})
		}, want: closeAuthFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h.t = t
			q := c.query
			if q == "" && c.name != "missing version" {
				q = "?v=1"
			}
			ws := h.dial(q)
			c.run(ws)
			ws.expectClose(c.want)
		})
	}
}

func TestReadyAndHeartbeat(t *testing.T) {
	h := newHarness(t, Options{Heartbeat: time.Second})
	f := newFixture()
	p := auth.Principal{UserID: f.me, SessionID: uuid.New()}
	h.guilds.snaps[f.me] = []guild.Snapshot{f.snapshot()}

	c := h.dial("?v=1")
	hello := c.expectOp(opHello)
	var hd struct {
		Interval int64 `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.D, &hd); err != nil || hd.Interval != 1000 {
		t.Fatalf("HELLO = %s", hello.D)
	}
	c.send(opIdentify, map[string]string{"ticket": h.ticket(p)})
	var ready readyData
	if err := json.Unmarshal(c.expectDispatch(typeReady).D, &ready); err != nil {
		t.Fatal(err)
	}
	if ready.V != 1 || len(ready.SessionID) < 40 || ready.User.ID != f.me || ready.DMs == nil || ready.Relationships == nil {
		t.Fatalf("READY = %+v", ready)
	}
	if len(ready.Guilds) != 1 || len(ready.Guilds[0].Channels) != 2 {
		t.Fatalf("READY guilds = %+v, want one guild without the hidden channel", ready.Guilds)
	}
	c.send(opHeartbeat, 1)
	c.expectOp(opHeartbeatAck)
	c.send(opHeartbeat, nil)
	c.expectOp(opHeartbeatAck)
}

// Events reach the client in stream order with consecutive s, filtered by
// what the user may see, and topics change right after a join.
func TestDispatchOrderAndFilter(t *testing.T) {
	h := newHarness(t, Options{})
	f := newFixture()
	p := auth.Principal{UserID: f.me, SessionID: uuid.New()}
	h.guilds.snaps[f.me] = []guild.Snapshot{f.snapshot()}
	c, _ := h.connect(p)
	gt := events.GuildTopic(f.guildID)

	h.publish(gt, events.MessageCreate, map[string]any{"id": uuid.New(), "channel_id": f.secret.ID, "content": "hidden"})
	h.publish(gt, events.MessageCreate, map[string]any{"id": uuid.New(), "channel_id": f.general.ID, "content": "one"})
	h.publish(events.GuildTopic(uuid.New()), events.MessageCreate, map[string]any{"channel_id": f.general.ID, "content": "other guild"})
	h.publish(gt, events.MessageCreate, map[string]any{"id": uuid.New(), "channel_id": f.general.ID, "content": "two"})

	for _, want := range []struct {
		s       uint64
		content string
	}{{2, "one"}, {3, "two"}} {
		fr := c.expectDispatch(events.MessageCreate)
		var m struct{ Content string }
		_ = json.Unmarshal(fr.D, &m)
		if m.Content != want.content || fr.S != want.s {
			t.Fatalf("s=%d content=%q, want s=%d %q", fr.S, m.Content, want.s, want.content)
		}
	}

	// Joining a guild mid-session: GUILD_CREATE first, then its events.
	other := newFixture()
	joined := other.snapshot()
	joined.Member.UserID = f.me
	h.guilds.mu.Lock()
	h.guilds.snaps[f.me] = append(h.guilds.snaps[f.me], joined)
	h.guilds.mu.Unlock()
	h.publish(events.GuildTopic(other.guildID), events.MessageCreate, map[string]any{"channel_id": other.general.ID, "content": "before join"})
	h.publish(events.UserTopic(f.me), events.GuildCreate, map[string]any{"id": other.guildID})
	h.publish(events.GuildTopic(other.guildID), events.MessageCreate, map[string]any{"channel_id": other.general.ID, "content": "after join"})
	if fr := c.expectDispatch(events.GuildCreate); fr.S != 4 {
		t.Fatalf("GUILD_CREATE s = %d", fr.S)
	}
	fr := c.expectDispatch(events.MessageCreate)
	if !bytes.Contains(fr.D, []byte("after join")) || fr.S != 5 {
		t.Fatalf("after join: s=%d %s", fr.S, fr.D)
	}
}

// A dropped connection resumes with the frames it missed, numbered as
// before, then RESUMED, then live events.
func TestResume(t *testing.T) {
	h := newHarness(t, Options{ResumeWindow: time.Minute})
	f := newFixture()
	p := auth.Principal{UserID: f.me, SessionID: uuid.New()}
	h.guilds.snaps[f.me] = []guild.Snapshot{f.snapshot()}
	c, ready := h.connect(p)
	gt := events.GuildTopic(f.guildID)
	msg := func(text string) {
		h.publish(gt, events.MessageCreate, map[string]any{"channel_id": f.general.ID, "content": text})
	}

	msg("seen")
	if fr := c.expectDispatch(events.MessageCreate); fr.S != 2 {
		t.Fatalf("s = %d", fr.S)
	}
	// The network drops: the client closes with a resumable code.
	_ = c.ws.Close(4000, "network")
	msg("missed 1")
	msg("missed 2")

	r := h.dial("?v=1")
	r.expectOp(opHello)
	// Wait until the pump has consumed both while detached.
	waitFor(t, func() bool {
		ss := h.srv.lookup(ready.SessionID)
		ss.mu.Lock()
		defer ss.mu.Unlock()
		return ss.buf.last() == 4
	})
	r.send(opResume, map[string]any{"ticket": h.ticket(p), "session_id": ready.SessionID, "seq": 2})
	for _, want := range []struct {
		s    uint64
		text string
	}{{3, "missed 1"}, {4, "missed 2"}} {
		fr := r.expectDispatch(events.MessageCreate)
		if fr.S != want.s || !bytes.Contains(fr.D, []byte(want.text)) {
			t.Fatalf("replayed s=%d %s, want s=%d %q", fr.S, fr.D, want.s, want.text)
		}
	}
	r.expectDispatch(typeResumed)
	msg("live")
	if fr := r.expectDispatch(events.MessageCreate); fr.S != 5 {
		t.Fatalf("live s = %d", fr.S)
	}

	// Another user's ticket cannot take the session over.
	thief := h.dial("?v=1")
	thief.expectOp(opHello)
	thief.send(opResume, map[string]any{"ticket": h.ticket(h.user()), "session_id": ready.SessionID, "seq": 5})
	thief.expectClose(closeSessionInvalid)
	// Nor can the same user from another device (another sign-in session).
	other := h.dial("?v=1")
	other.expectOp(opHello)
	other.send(opResume, map[string]any{"ticket": h.ticket(auth.Principal{UserID: f.me, SessionID: uuid.New()}), "session_id": ready.SessionID, "seq": 5})
	other.expectClose(closeSessionInvalid)
	// The rightful connection is untouched.
	r.send(opHeartbeat, 5)
	r.expectOp(opHeartbeatAck)
}

func TestResumeRefused(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(h *harness, c *wsClient, p auth.Principal)
		seq     uint64
		want    websocket.StatusCode
	}{
		{name: "seq ahead of the session", seq: 9, want: closeSessionInvalid,
			prepare: func(_ *harness, c *wsClient, _ auth.Principal) { _ = c.ws.Close(4000, "") }},
		{name: "after the resume window", seq: 1, want: closeSessionInvalid,
			prepare: func(h *harness, c *wsClient, _ auth.Principal) {
				_ = c.ws.Close(4000, "")
				time.Sleep(100 * time.Millisecond) // let the server notice the drop
				h.clock.Add(time.Second)
				time.Sleep(300 * time.Millisecond) // a few janitor ticks
			}},
		{name: "after a normal close", seq: 1, want: closeSessionInvalid,
			prepare: func(_ *harness, c *wsClient, _ auth.Principal) {
				_ = c.ws.Close(websocket.StatusNormalClosure, "")
				time.Sleep(100 * time.Millisecond)
			}},
		{name: "after the sign-in session was revoked", seq: 1, want: closeAuthFailed,
			prepare: func(h *harness, c *wsClient, p auth.Principal) {
				_ = c.ws.Close(4000, "")
				h.auth.mu.Lock()
				h.auth.inactive[p.SessionID] = true
				h.auth.mu.Unlock()
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, Options{ResumeWindow: 400 * time.Millisecond})
			p := h.user()
			ws, ready := h.connect(p)
			c.prepare(h, ws, p)
			r := h.dial("?v=1")
			r.expectOp(opHello)
			r.send(opResume, map[string]any{"ticket": h.ticket(p), "session_id": ready.SessionID, "seq": c.seq})
			r.expectClose(c.want)
		})
	}
}

// SESSION_REVOKE for the connection's sign-in session closes it at once.
func TestRevokedSessionIsClosed(t *testing.T) {
	h := newHarness(t, Options{})
	p := h.user()
	c, _ := h.connect(p)
	other, _ := h.connect(auth.Principal{UserID: p.UserID, SessionID: uuid.New()})
	h.publish(events.UserTopic(p.UserID), events.SessionRevoke, map[string]any{"session_id": p.SessionID})
	c.expectClose(closeAuthFailed)
	// The other device of the same user stays connected.
	h.publish(events.UserTopic(p.UserID), events.RelationshipAdd, map[string]any{"id": uuid.New(), "type": 3})
	other.expectDispatch(events.RelationshipAdd)
}

func TestShutdownAsksToReconnect(t *testing.T) {
	h := newHarness(t, Options{})
	c, _ := h.connect(h.user())
	h.cancel()
	c.expectClose(closeRestart)
}

// The HTTP server's read and write timeouts protect REST; a WebSocket must
// outlive them.
func TestHTTPTimeoutsDoNotCutWebSocket(t *testing.T) {
	h := newHarnessWith(t, natstest.New(t), Options{Heartbeat: time.Second}, func(s *http.Server) {
		s.ReadTimeout, s.WriteTimeout = 200*time.Millisecond, 200*time.Millisecond
	})
	c, _ := h.connect(h.user())
	for range 4 {
		time.Sleep(150 * time.Millisecond)
		c.send(opHeartbeat, nil)
		c.expectOp(opHeartbeatAck)
	}
}

// A client that does not read is cut off instead of growing memory, and
// its session stays resumable.
func TestSlowConsumerIsDisconnected(t *testing.T) {
	c := newConn(nil, 2)
	c.send([]byte("1"))
	c.send([]byte("2"))
	if c.closeCode() != 0 {
		t.Fatal("closed before the queue was full")
	}
	c.send([]byte("3"))
	if c.closeCode() != closeSlowConsumer || endsSession(closeSlowConsumer) {
		t.Fatalf("close code %d, want resumable %d", c.closeCode(), closeSlowConsumer)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A RESUME while the old socket is still open (the client noticed the drop
// first) takes the session over; the old socket closing must not end it.
func TestResumeReplacesLiveConnection(t *testing.T) {
	h := newHarness(t, Options{})
	f := newFixture()
	p := auth.Principal{UserID: f.me, SessionID: uuid.New()}
	h.guilds.snaps[f.me] = []guild.Snapshot{f.snapshot()}
	old, ready := h.connect(p)

	r := h.dial("?v=1")
	r.expectOp(opHello)
	r.send(opResume, map[string]any{"ticket": h.ticket(p), "session_id": ready.SessionID, "seq": 1})
	r.expectDispatch(typeResumed)
	old.expectClose(closeSessionInvalid)

	h.publish(events.GuildTopic(f.guildID), events.MessageCreate, map[string]any{"channel_id": f.general.ID, "content": "still here"})
	if fr := r.expectDispatch(events.MessageCreate); fr.S != 2 {
		t.Fatalf("s = %d after takeover", fr.S)
	}
}
