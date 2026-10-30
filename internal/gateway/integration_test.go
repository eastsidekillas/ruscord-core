package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"testing"

	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/message"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
	"github.com/eastsidekillas/ruscord-core/internal/platform/natstest"
	"github.com/eastsidekillas/ruscord-core/internal/platform/testdb"
	"github.com/eastsidekillas/ruscord-core/internal/social"
)

// The whole path: REST writes → outbox → relay → JetStream → gateway →
// WebSocket, with real modules on a real Postgres
// (RUSCORD_TEST_POSTGRES_URL).

type stack struct {
	*apitest.Env
	wsURL string
}

func newStack(t *testing.T) *stack {
	t.Helper()
	e := apitest.New(t)
	nc := natstest.New(t)
	guilds := guild.NewService(e.PG, e.Auth, e.Limiter)
	socials := social.NewService(e.PG, e.Auth, guilds, e.Limiter)
	guilds.OnChannelsDeleted(message.DeleteChannels)
	socials.OnChannelsDeleted(message.DeleteChannels)
	guild.NewHandler(guilds, e.RequireAuth).Register(e.Mux)
	social.NewHandler(socials, e.RequireAuth).Register(e.Mux)
	message.NewHandler(message.NewService(e.PG, e.Auth, guilds, socials, e.Limiter), e.RequireAuth).Register(e.Mux)

	ctx, cancel := context.WithCancel(context.Background())
	relay, err := events.NewRelay(e.PG, nc, testdb.Logger())
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New(t.Context(), Deps{Auth: e.Auth, Guilds: guilds, Social: socials, NATS: nc, Log: testdb.Logger()}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	gw.Register(e.Mux)
	relayDone, gwDone := make(chan struct{}), make(chan struct{})
	go func() { relay.Run(ctx); close(relayDone) }()
	go func() { gw.Run(ctx); close(gwDone) }()
	t.Cleanup(func() { cancel(); <-relayDone; <-gwDone })
	return &stack{Env: e, wsURL: "ws" + strings.TrimPrefix(e.URL(), "http") + "/gateway?v=1"}
}

// connect opens a gateway connection for u with a real ticket.
func (s *stack) connect(u *apitest.User) (*wsClient, readyData) {
	s.T.Helper()
	c := dialURL(s.T, s.wsURL)
	c.expectOp(opHello)
	c.send(opIdentify, map[string]string{"ticket": u.Ticket()})
	var r readyData
	if err := json.Unmarshal(c.expectDispatch(typeReady).D, &r); err != nil {
		s.T.Fatal(err)
	}
	return c, r
}

func idOf(f frame) string {
	var p struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(f.D, &p)
	return p.ID
}

func TestGatewayEndToEnd(t *testing.T) {
	s := newStack(t)
	owner, bob := s.User("owner"), s.User("bob")
	bobWS, ready := s.connect(bob)
	if len(ready.Guilds) != 0 || ready.User.ID != bob.ID {
		t.Fatalf("READY for a new user = %+v", ready)
	}

	// Bob joins by invite: the guild arrives without anything hidden.
	g := apitest.Obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/guilds", map[string]any{"name": "Сервер"}))
	gid := apitest.Str(g["id"])
	general := apitest.Str(apitest.Obj(apitest.List(g["channels"])[0])["id"])
	code := apitest.Str(apitest.Obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+general+"/invites", map[string]any{}))["code"])
	bob.Expect(http.StatusOK, "POST", "/api/v1/invites/"+code, nil)
	if f := bobWS.expectDispatch(events.GuildCreate); idOf(f) != gid {
		t.Fatalf("GUILD_CREATE for %s, want %s", idOf(f), gid)
	}
	// His own GUILD_MEMBER_ADD precedes GUILD_CREATE in the stream, before
	// the guild's topic was his, so it is not delivered: GUILD_CREATE has him.

	// A channel created and then hidden: Bob sees it appear and go away,
	// then nothing of it, not even its rename or messages.
	secret := apitest.Str(apitest.Obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/guilds/"+gid+"/channels",
		map[string]any{"name": "secret", "type": 0}))["id"])
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+secret+"/permissions/"+gid,
		map[string]any{"type": 1, "deny": permString(perm.ViewChannel)})
	if f := bobWS.expectDispatch(events.ChannelCreate); idOf(f) != secret {
		t.Fatal("CHANNEL_CREATE of the new channel expected")
	}
	if f := bobWS.expectDispatch(events.ChannelDelete); idOf(f) != secret {
		t.Fatal("CHANNEL_DELETE when the channel was hidden expected")
	}
	owner.Expect(http.StatusOK, "PATCH", "/api/v1/channels/"+secret, map[string]any{"name": "raid-plans"})
	owner.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+secret+"/messages", map[string]any{"content": "тайна"})
	owner.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+general+"/messages", map[string]any{"content": "всем привет"})
	f := bobWS.expectDispatch(events.MessageCreate)
	if !bytes.Contains(f.D, []byte("всем привет")) {
		t.Fatalf("first message Bob sees: %s", f.D)
	}

	// A role that opens the channel: Bob gets it once the role is his.
	role := apitest.Str(apitest.Obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/guilds/"+gid+"/roles",
		map[string]any{"name": "mod"}))["id"])
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+secret+"/permissions/"+role,
		map[string]any{"type": 1, "allow": permString(perm.ViewChannel)})
	bobWS.expectDispatch(events.RoleCreate)
	owner.Expect(http.StatusNoContent, "PUT", "/api/v1/guilds/"+gid+"/members/"+bob.ID.String()+"/roles/"+role, nil)
	bobWS.expectDispatch(events.MemberUpdate)
	f = bobWS.expectDispatch(events.ChannelCreate)
	if idOf(f) != secret || !bytes.Contains(f.D, []byte("raid-plans")) {
		t.Fatalf("revealed channel: %s", f.D)
	}
	owner.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+secret+"/messages", map[string]any{"content": "теперь видно"})
	if f := bobWS.expectDispatch(events.MessageCreate); !bytes.Contains(f.D, []byte("теперь видно")) {
		t.Fatalf("message after reveal: %s", f.D)
	}

	// Kicked: the guild goes away and its events stop. A friend request
	// after a message in the guild shows the message was not delivered.
	owner.Expect(http.StatusNoContent, "DELETE", "/api/v1/guilds/"+gid+"/members/"+bob.ID.String(), nil)
	bobWS.expectDispatch(events.MemberRemove)
	if f := bobWS.expectDispatch(events.GuildDelete); idOf(f) != gid {
		t.Fatal("GUILD_DELETE after kick expected")
	}
	owner.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+general+"/messages", map[string]any{"content": "без Боба"})
	owner.Expect(http.StatusNoContent, "PUT", "/api/v1/users/@me/relationships/"+bob.ID.String(), map[string]any{})
	if f := bobWS.expectDispatch(events.RelationshipAdd); idOf(f) != owner.ID.String() {
		t.Fatalf("friend request: %s", f.D)
	}

	// A DM opened mid-session is subscribed at once.
	bob.Expect(http.StatusNoContent, "PUT", "/api/v1/users/@me/relationships/"+owner.ID.String(), map[string]any{})
	bobWS.expectDispatch(events.RelationshipAdd)
	dm := apitest.Str(apitest.Obj(owner.Expect(http.StatusOK, "POST", "/api/v1/users/@me/channels",
		map[string]any{"recipient_id": bob.ID}))["id"])
	if f := bobWS.expectDispatch(events.ChannelCreate); idOf(f) != dm {
		t.Fatal("CHANNEL_CREATE of the DM expected")
	}
	owner.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+dm+"/messages", map[string]any{"content": "лично"})
	if f := bobWS.expectDispatch(events.MessageCreate); !bytes.Contains(f.D, []byte("лично")) {
		t.Fatalf("DM message: %s", f.D)
	}

	// A reconnect sees the same world in READY.
	_, again := s.connect(bob)
	if len(again.Guilds) != 0 || len(again.DMs) != 1 || len(again.Relationships) != 1 {
		t.Fatalf("READY after the changes: %d guilds, %d dms, %d relationships", len(again.Guilds), len(again.DMs), len(again.Relationships))
	}
}

// Logging out closes the gateway connections of that sign-in session.
func TestGatewayClosedOnLogout(t *testing.T) {
	s := newStack(t)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	post := func(path string, body any) map[string]any {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL()+path, bytes.NewReader(mustJSON(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s: %d %v", path, resp.StatusCode, out)
		}
		return out
	}
	post("/api/v1/auth/register", map[string]any{"email": "eve@example.com", "username": "eve", "password": "long-password-1"})
	ticket := apitest.Str(post("/api/v1/gateway/ticket", nil)["ticket"])

	c := dialURL(t, s.wsURL)
	c.expectOp(opHello)
	c.send(opIdentify, map[string]string{"ticket": ticket})
	c.expectDispatch(typeReady)

	post("/api/v1/auth/logout", nil)
	c.expectClose(closeAuthFailed)
}

func permString(p perm.Permissions) string { return strconv.FormatUint(uint64(p), 10) }
