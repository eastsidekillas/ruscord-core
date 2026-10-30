package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/natstest"
	"github.com/eastsidekillas/ruscord-core/internal/platform/testdb"
	"github.com/eastsidekillas/ruscord-core/internal/social"
)

// Protocol tests run the real server against an in-process NATS and fake
// modules; no database is needed.

type fakeAuth struct {
	mu       sync.Mutex
	tickets  map[string]auth.Principal
	inactive map[uuid.UUID]bool
}

func (a *fakeAuth) ConsumeTicket(_ context.Context, t string) (auth.Principal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.tickets[t]
	if !ok {
		return auth.Principal{}, auth.ErrInvalidTicket
	}
	delete(a.tickets, t)
	return p, nil
}

func (a *fakeAuth) SessionActive(_ context.Context, sid uuid.UUID) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.inactive[sid], nil
}

func (a *fakeAuth) Me(_ context.Context, p auth.Principal) (auth.User, error) {
	return auth.User{ID: p.UserID, Username: "user"}, nil
}

type fakeGuilds struct {
	mu    sync.Mutex
	snaps map[uuid.UUID][]guild.Snapshot // by user
}

func (g *fakeGuilds) Snapshots(_ context.Context, user uuid.UUID) ([]guild.Snapshot, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.snaps[user], nil
}

func (g *fakeGuilds) Snapshot(_ context.Context, user, id uuid.UUID) (guild.Snapshot, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range g.snaps[user] {
		if s.Guild.ID == id {
			return s, nil
		}
	}
	return guild.Snapshot{}, httpx.ErrNotFound
}

type fakeSocial struct{}

func (fakeSocial) ListDMs(context.Context, uuid.UUID) ([]social.DM, error) { return nil, nil }
func (fakeSocial) List(context.Context, uuid.UUID) ([]social.Relationship, error) {
	return nil, nil
}

type harness struct {
	t      *testing.T
	srv    *Server
	http   *httptest.Server
	auth   *fakeAuth
	guilds *fakeGuilds
	js     jetstream.JetStream
	cancel context.CancelFunc
	clock  *testClock
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	return newHarnessWith(t, natstest.New(t), opts, nil)
}

// newHarnessWith lets a test supply NATS and an http.Server configuration.
func newHarnessWith(t *testing.T, nc *nats.Conn, opts Options, configure func(*http.Server)) *harness {
	t.Helper()
	h := &harness{t: t, auth: &fakeAuth{tickets: map[string]auth.Principal{}, inactive: map[uuid.UUID]bool{}},
		guilds: &fakeGuilds{snaps: map[uuid.UUID][]guild.Snapshot{}}, clock: &testClock{t: time.Unix(1_700_000_000, 0)}}
	if opts.Now == nil {
		opts.Now = h.clock.Now
	}
	srv, err := New(t.Context(), Deps{Auth: h.auth, Guilds: h.guilds, Social: fakeSocial{}, NATS: nc, Log: testdb.Logger()}, opts)
	if err != nil {
		t.Fatal(err)
	}
	h.srv, h.js = srv, srv.js
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	done := make(chan struct{})
	go func() { srv.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	mux := http.NewServeMux()
	srv.Register(mux)
	h.http = httptest.NewUnstartedServer(mux)
	if configure != nil {
		configure(h.http.Config)
	}
	h.http.Start()
	t.Cleanup(h.http.Close)
	return h
}

// ticket registers a single-use ticket for p.
func (h *harness) ticket(p auth.Principal) string {
	h.auth.mu.Lock()
	defer h.auth.mu.Unlock()
	t := uuid.NewString()
	h.auth.tickets[t] = p
	return t
}

func (h *harness) user() auth.Principal {
	return auth.Principal{UserID: uuid.New(), SessionID: uuid.New()}
}

// publish appends an event to the stream the way the outbox relay does.
func (h *harness) publish(subject, typ string, data any) {
	h.t.Helper()
	msg := &nats.Msg{Subject: subject, Data: mustJSON(data), Header: nats.Header{}}
	msg.Header.Set(events.HeaderType, typ)
	if _, err := h.js.PublishMsg(h.t.Context(), msg); err != nil {
		h.t.Fatal(err)
	}
}

type wsClient struct {
	t  *testing.T
	ws *websocket.Conn
}

type frame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  uint64          `json:"s"`
	T  string          `json:"t"`
}

func (h *harness) dial(query string) *wsClient {
	h.t.Helper()
	return dialURL(h.t, "ws"+strings.TrimPrefix(h.http.URL, "http")+"/gateway"+query)
}

func dialURL(t *testing.T, url string) *wsClient {
	t.Helper()
	ws, resp, err := websocket.Dial(t.Context(), url, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(1 << 20)
	t.Cleanup(func() { _ = ws.CloseNow() })
	return &wsClient{t: t, ws: ws}
}

// connect dials, checks HELLO and identifies; it returns READY.
func (h *harness) connect(p auth.Principal) (*wsClient, readyData) {
	h.t.Helper()
	c := h.dial("?v=1")
	c.expectOp(opHello)
	c.send(opIdentify, map[string]string{"ticket": h.ticket(p)})
	f := c.expectDispatch(typeReady)
	if f.S != 1 {
		h.t.Fatalf("READY s = %d, want 1", f.S)
	}
	var r readyData
	if err := json.Unmarshal(f.D, &r); err != nil {
		h.t.Fatal(err)
	}
	return c, r
}

func (c *wsClient) send(op int, d any) {
	c.t.Helper()
	if err := c.ws.Write(c.t.Context(), websocket.MessageText, mustJSON(map[string]any{"op": op, "d": d})); err != nil {
		c.t.Fatal(err)
	}
}

func (c *wsClient) sendRaw(typ websocket.MessageType, b []byte) {
	c.t.Helper()
	_ = c.ws.Write(c.t.Context(), typ, b)
}

// read returns the next frame, or the close code when the server closed.
func (c *wsClient) read(timeout time.Duration) (frame, websocket.StatusCode) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), timeout)
	defer cancel()
	_, b, err := c.ws.Read(ctx)
	if err != nil {
		if code := websocket.CloseStatus(err); code != -1 {
			return frame{}, code
		}
		c.t.Fatalf("read: %v", err)
	}
	var f frame
	if err := json.Unmarshal(b, &f); err != nil {
		c.t.Fatal(err)
	}
	return f, 0
}

func (c *wsClient) next() frame {
	c.t.Helper()
	f, code := c.read(5 * time.Second)
	if code != 0 {
		c.t.Fatalf("closed with %d, want a frame", code)
	}
	return f
}

func (c *wsClient) expectOp(op int) frame {
	c.t.Helper()
	f := c.next()
	if f.Op != op {
		c.t.Fatalf("op %d (%s %s), want %d", f.Op, f.T, f.D, op)
	}
	return f
}

func (c *wsClient) expectDispatch(t string) frame {
	c.t.Helper()
	f := c.expectOp(opDispatch)
	if f.T != t {
		c.t.Fatalf("dispatch %s (%s), want %s", f.T, f.D, t)
	}
	return f
}

// expectClose reads frames until the close and checks its code.
func (c *wsClient) expectClose(want websocket.StatusCode) {
	c.t.Helper()
	for {
		f, code := c.read(5 * time.Second)
		if code == 0 {
			if f.Op == opHeartbeatAck || f.Op == opHello {
				continue
			}
			c.t.Fatalf("got op %d %s before close, want close %d", f.Op, f.T, want)
		}
		if code != want {
			c.t.Fatalf("close code %d, want %d", code, want)
		}
		return
	}
}
