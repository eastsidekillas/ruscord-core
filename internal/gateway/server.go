package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/presence"
	"github.com/eastsidekillas/ruscord-core/internal/social"
)

// Auth is what the gateway needs from the auth module.
type Auth interface {
	ConsumeTicket(ctx context.Context, ticket string) (auth.Principal, error)
	SessionActive(ctx context.Context, sid uuid.UUID) (bool, error)
	Me(ctx context.Context, p auth.Principal) (auth.User, error)
}

// Guilds is what the gateway needs from the guild module.
type Guilds interface {
	GuildLoader
	Snapshots(ctx context.Context, userID uuid.UUID) ([]guild.Snapshot, error)
}

// Social is what the gateway needs from the social module.
type Social interface {
	ListDMs(ctx context.Context, me uuid.UUID) ([]social.DM, error)
	List(ctx context.Context, me uuid.UUID) ([]social.Relationship, error)
}

// Presence is what the gateway needs from internal/presence.
type Presence interface {
	Touch(ctx context.Context, user uuid.UUID, sid string, state presence.Status) error
	Drop(ctx context.Context, user uuid.UUID, sid string) error
	Refresh(ctx context.Context, user uuid.UUID, choice presence.Status) (presence.Status, error)
	Statuses(ctx context.Context, users []uuid.UUID) (map[uuid.UUID]presence.Status, error)
	TTL() time.Duration
}

type Deps struct {
	Auth     Auth
	Guilds   Guilds
	Social   Social
	Presence Presence
	NATS     *nats.Conn
	Log      *slog.Logger
}

type Options struct {
	// AllowedOrigins are the browser origins allowed to connect, as in
	// RUSCORD_ALLOWED_ORIGINS; requests without Origin are allowed.
	AllowedOrigins     []string
	Heartbeat          time.Duration // interval the client heartbeats at
	IdentifyTimeout    time.Duration // first IDENTIFY or RESUME must arrive within it
	ResumeWindow       time.Duration // how long a dropped session stays resumable
	MaxSessionsPerUser int           // on this node
	Now                func() time.Time
}

func (o *Options) defaults() {
	if o.Heartbeat <= 0 {
		o.Heartbeat = 30 * time.Second
	}
	if o.IdentifyTimeout <= 0 {
		o.IdentifyTimeout = 10 * time.Second
	}
	if o.ResumeWindow <= 0 {
		o.ResumeWindow = 2 * time.Minute
	}
	if o.MaxSessionsPerUser <= 0 {
		o.MaxSessionsPerUser = 20
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// PresenceTTL is the presence TTL to use with these options: a dropped
// connection stays online for its resume window, and a live one survives a
// late heartbeat.
func PresenceTTL(o Options) time.Duration {
	o.defaults()
	return o.ResumeWindow + 2*o.Heartbeat
}

// Replay buffer caps per session (see replayBuffer). The outbound queue of
// a connection holds a full replay plus a margin, so a RESUME always fits.
const (
	replayMaxFrames = 2048
	replayMaxBytes  = 2 << 20
	outQueue        = replayMaxFrames + 64
)

// Server accepts gateway connections and owns the sessions of this node.
type Server struct {
	deps   Deps
	opts   Options
	log    *slog.Logger
	js     jetstream.JetStream
	stream jetstream.Stream
	hub    *hub

	base       context.Context // parent of every session pump
	stopPumps  context.CancelFunc
	mu         sync.Mutex
	sessions   map[string]*session
	perUser    map[uuid.UUID]int
	closing    bool // set by shutdown; no new sessions after it
	shutdownMu sync.Once
}

// New prepares the gateway. It makes sure the events stream exists, because
// the gateway may start before any API process ran the relay.
func New(ctx context.Context, deps Deps, opts Options) (*Server, error) {
	opts.defaults()
	js, err := jetstream.New(deps.NATS)
	if err != nil {
		return nil, err
	}
	st, err := events.EnsureStream(ctx, js)
	if err != nil {
		return nil, fmt.Errorf("gateway: events stream: %w", err)
	}
	// A dropped connection must stay "online" while it can resume, and a
	// live one must not expire between heartbeats.
	if min := opts.ResumeWindow + opts.Heartbeat; deps.Presence.TTL() < min {
		return nil, fmt.Errorf("gateway: presence TTL %s is shorter than resume window + heartbeat (%s)", deps.Presence.TTL(), min)
	}
	base, stop := context.WithCancel(context.Background())
	return &Server{deps: deps, opts: opts, log: deps.Log, js: js, stream: st, base: base, stopPumps: stop,
		hub: newHub(deps.NATS, deps.Log), sessions: map[string]*session{}, perUser: map[uuid.UUID]int{}}, nil
}

func (s *Server) now() time.Time { return s.opts.Now() }

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /gateway", s.serveWS)
}

// Run expires abandoned sessions until ctx is done, then closes every
// connection with 1012 (service restart) so clients reconnect elsewhere.
func (s *Server) Run(ctx context.Context) {
	tick := time.NewTicker(max(s.opts.ResumeWindow/4, 100*time.Millisecond))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.shutdown()
			return
		case <-tick.C:
			now := s.now()
			for _, ss := range s.snapshot() {
				if ss.expired(now, s.opts.ResumeWindow) {
					ss.end(0, "")
				}
			}
		}
	}
}

func (s *Server) shutdown() {
	s.shutdownMu.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		for _, ss := range s.snapshot() {
			ss.end(closeRestart, "server restart")
		}
		s.stopPumps()
	})
}

// SessionCount is the number of sessions this node holds (attached or
// waiting for RESUME), for metrics.
func (s *Server) SessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *Server) snapshot() []*session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*session, 0, len(s.sessions))
	for _, ss := range s.sessions {
		out = append(out, ss)
	}
	return out
}

// reserve counts a new session of user against the per-node cap.
func (s *Server) reserve(user uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perUser[user] >= s.opts.MaxSessionsPerUser {
		return false
	}
	s.perUser[user]++
	return true
}

// evictDetached ends the user's session that has been detached longest, to
// make room under the cap: a client that crashes and identifies anew must
// not be locked out by its own abandoned sessions. It reports whether one
// was ended.
func (s *Server) evictDetached(user uuid.UUID) bool {
	var oldest *session
	var since time.Time
	for _, ss := range s.snapshot() {
		if ss.principal.UserID != user {
			continue
		}
		ss.mu.Lock()
		detached, at := ss.conn == nil && !ss.ended, ss.detachedAt
		ss.mu.Unlock()
		if detached && (oldest == nil || at.Before(since)) {
			oldest, since = ss, at
		}
	}
	if oldest == nil {
		return false
	}
	oldest.end(0, "")
	return true
}

func (s *Server) release(user uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseLocked(user)
}

func (s *Server) releaseLocked(user uuid.UUID) {
	if s.perUser[user]--; s.perUser[user] <= 0 {
		delete(s.perUser, user)
	}
}

func (s *Server) forget(ss *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[ss.id] == ss {
		delete(s.sessions, ss.id)
		s.releaseLocked(ss.principal.UserID)
	}
}

func (s *Server) lookup(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	// net/http clears the server's read and write deadlines when the
	// connection is hijacked, so its timeouts do not cut a WebSocket; the
	// gateway has heartbeats instead (TestHTTPTimeoutsDoNotCutWebSocket).
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.opts.AllowedOrigins})
	if err != nil {
		return // Accept has answered
	}
	ws.SetReadLimit(maxInboundBytes)
	c := newConn(ws, outQueue)
	// The writer outlives request cancellation: on shutdown it still has to
	// send the close frame. It ends when the connection is kicked.
	go c.writeLoop(context.WithoutCancel(r.Context()))
	stop := context.AfterFunc(r.Context(), func() { c.kick(closeRestart, "server restart") })
	defer stop()

	cl := &client{srv: s, c: c}
	cl.run(context.WithoutCancel(r.Context()), r.URL.Query().Get("v"))
}

type readyData struct {
	V             int                   `json:"v"`
	SessionID     string                `json:"session_id"`
	User          auth.User             `json:"user"`
	Guilds        []guild.Full          `json:"guilds"`
	DMs           []social.DM           `json:"dms"`
	Relationships []social.Relationship `json:"relationships"`
	// Presences are the friends who are not offline; everyone else is.
	Presences []presence.Update `json:"presences"`
}

var (
	errInactive     = errors.New("gateway: sign-in session is not active")
	errShuttingDown = errors.New("gateway: shutting down")
)

// checkActive returns errInactive when the sign-in session behind p was
// revoked or expired: a ticket can be issued with an access token of a
// session revoked minutes ago.
func (s *Server) checkActive(ctx context.Context, p auth.Principal) error {
	ok, err := s.deps.Auth.SessionActive(ctx, p.SessionID)
	if err != nil {
		return err
	}
	if !ok {
		return errInactive
	}
	return nil
}

// start builds a session, queues READY on c and starts the pump.
//
// The stream position is read before anything else: every change committed
// after it is replayed on top of the snapshot (events carry whole objects,
// so the result converges), and the session check that follows cannot miss
// a revocation (an earlier one fails the check, a later one arrives as an
// event). READY is queued before the pump runs, so no event can overtake
// it, and after the session is registered, so a shutdown cannot miss it.
func (s *Server) start(ctx context.Context, p auth.Principal, c *conn) (*session, error) {
	info, err := s.stream.Info(ctx)
	if err != nil {
		return nil, err
	}
	from := info.State.LastSeq + 1
	if err := s.checkActive(ctx, p); err != nil {
		return nil, err
	}
	user, err := s.deps.Auth.Me(ctx, p)
	if err != nil {
		return nil, err
	}
	snaps, err := s.deps.Guilds.Snapshots(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	dms, err := s.deps.Social.ListDMs(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	rels, err := s.deps.Social.List(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	dmIDs := make([]uuid.UUID, len(dms))
	for i, d := range dms {
		dmIDs[i] = d.ID
	}
	r := newRouter(p.UserID, p.SessionID, s.deps.Guilds, snaps, dmIDs)
	var friendIDs []uuid.UUID
	for _, rel := range rels {
		if rel.Type == social.RelFriend {
			r.friends[rel.ID] = true
			friendIDs = append(friendIDs, rel.ID)
		}
	}
	if choice := presence.Status(user.Status); presence.ValidChoice(choice) {
		r.choice = choice
	}
	statuses, err := s.deps.Presence.Statuses(ctx, friendIDs)
	if err != nil {
		return nil, err
	}
	presences := []presence.Update{}
	for _, id := range friendIDs {
		if st := statuses[id]; st != presence.Offline {
			presences = append(presences, presence.Update{UserID: id, Status: st})
		}
		r.seen[id] = statuses[id]
	}
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	fulls := make([]guild.Full, 0, len(snaps))
	for _, sn := range snaps {
		fulls = append(fulls, r.guilds[sn.Guild.ID].full())
	}
	ready := encode(outFrame{Op: opDispatch, S: 1, T: typeReady, D: mustJSON(readyData{
		V: 1, SessionID: id, User: user, Guilds: fulls, DMs: nonNil(dms), Relationships: nonNil(rels),
		Presences: presences,
	})})

	pctx, cancel := context.WithCancel(s.base)
	ss := &session{id: id, principal: p, srv: s, cancel: cancel, conn: c, r: r, state: presence.Online,
		buf: newReplayBuffer(1, replayMaxFrames, replayMaxBytes)} // READY is s=1 and is not replayed
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		cancel()
		return nil, errShuttingDown
	}
	s.sessions[id] = ss
	s.mu.Unlock()
	c.send(ready)
	ss.syncEph()
	ss.touch()
	ss.refreshPresence()
	go ss.pump(pctx, from)
	return ss, nil
}

func newSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
