package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"image/color"
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
	"github.com/eastsidekillas/ruscord-core/internal/presence"
	"github.com/eastsidekillas/ruscord-core/internal/social"
)

// The whole path: REST writes → outbox → relay → JetStream → gateway →
// WebSocket, with real modules on a real Postgres
// (RUSCORD_TEST_POSTGRES_URL).

type stack struct {
	*apitest.Env
	wsURL string
	relay *events.Relay
}

// newStack runs the relay in the background, like the API process does.
func newStack(t *testing.T) *stack { return newStackRelay(t, true, Options{}) }

// newStackRelay with run=false leaves publishing to the test (flush), to
// reproduce a relay that lags behind the database.
func newStackRelay(t *testing.T, run bool, opts Options) *stack {
	t.Helper()
	e := apitest.New(t)
	nc := natstest.New(t)
	eph := events.NewEphemeral(nc)
	guilds := guild.NewService(e.PG, e.Auth, e.Limiter)
	socials := social.NewService(e.PG, e.Auth, guilds, e.Limiter)
	e.Auth.SetGuildIDs(guilds)
	guilds.WithFiles(e.Files)
	pres := presence.NewService(e.Redis, eph, guilds, PresenceTTL(opts))
	guilds.WithPresences(pres)
	guilds.OnChannelsDeleted(message.DeleteChannels)
	socials.OnChannelsDeleted(message.DeleteChannels)
	guild.NewHandler(guilds, e.RequireAuth).Register(e.Mux)
	social.NewHandler(socials, e.RequireAuth).Register(e.Mux)
	message.NewHandler(message.NewService(e.PG, e.Auth, guilds, socials, e.Limiter, eph), e.RequireAuth).Register(e.Mux)

	ctx, cancel := context.WithCancel(context.Background())
	relay, err := events.NewRelay(e.PG, nc, testdb.Logger())
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New(t.Context(), Deps{Auth: e.Auth, Guilds: guilds, Social: socials, Presence: pres, NATS: nc, Log: testdb.Logger()}, opts)
	if err != nil {
		t.Fatal(err)
	}
	gw.Register(e.Mux)
	relayDone, gwDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(relayDone)
		if run {
			relay.Run(ctx)
		}
	}()
	go func() { gw.Run(ctx); close(gwDone) }()
	t.Cleanup(func() { cancel(); <-relayDone; <-gwDone })
	return &stack{Env: e, wsURL: "ws" + strings.TrimPrefix(e.URL(), "http") + "/gateway?v=1", relay: relay}
}

// flush publishes everything waiting in the outbox. The relay's advisory
// lock is per database, so a relay of a parallel test may hold it: retry
// until this schema's outbox is empty, not until one batch sends nothing.
func (s *stack) flush() {
	s.T.Helper()
	waitFor(s.T, func() bool {
		if _, err := s.relay.PublishBatch(s.T.Context()); err != nil {
			s.T.Fatal(err)
		}
		var left int
		if err := s.PG.QueryRow(s.T.Context(), `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&left); err != nil {
			s.T.Fatal(err)
		}
		return left == 0
	})
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

// channelPerms reads the next frame, which must be GUILD_PERMISSIONS_UPDATE,
// and returns the permissions per channel id.
func channelPerms(c *wsClient) map[string]perm.Permissions {
	c.t.Helper()
	var p struct {
		ChannelPermissions map[string]perm.Permissions `json:"channel_permissions"`
	}
	if err := json.Unmarshal(c.expectDispatch(typePermissionsUpdate).D, &p); err != nil {
		c.t.Fatal(err)
	}
	return p.ChannelPermissions
}

// guildWith creates a guild of owner through the API, lets members join by
// invite, and returns its id and its first text channel.
func guildWith(owner *apitest.User, members ...*apitest.User) (gid, general string) {
	g := apitest.Obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/guilds", map[string]any{"name": "Сервер"}))
	gid = apitest.Str(g["id"])
	general = apitest.Str(apitest.Obj(apitest.List(g["channels"])[0])["id"])
	code := apitest.Str(apitest.Obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+general+"/invites", map[string]any{}))["code"])
	for _, m := range members {
		m.Expect(http.StatusOK, "POST", "/api/v1/invites/"+code, nil)
	}
	return gid, general
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
	gid, general := guildWith(owner, bob)
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
	if cp := channelPerms(bobWS); !cp[secret].Has(perm.SendMessages) {
		t.Fatalf("permissions in the new channel: %v", cp)
	}
	if f := bobWS.expectDispatch(events.ChannelDelete); idOf(f) != secret {
		t.Fatal("CHANNEL_DELETE when the channel was hidden expected")
	}
	if cp := channelPerms(bobWS); len(cp) != 2 || cp[secret] != 0 {
		t.Fatalf("permissions after hiding: %v", cp)
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
	if cp := channelPerms(bobWS); len(cp) != 3 || !cp[secret].Has(perm.ViewChannel) {
		t.Fatalf("permissions after the role: %v", cp)
	}

	// Read-only for everyone in general: Bob learns he cannot write there,
	// and nothing else about the channel changes for him.
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+general+"/permissions/"+gid,
		map[string]any{"type": 1, "deny": permString(perm.SendMessages)})
	bobWS.expectDispatch(events.ChannelUpdate)
	if cp := channelPerms(bobWS); cp[general].Has(perm.SendMessages) || !cp[general].Has(perm.ReadMessageHistory) {
		t.Fatalf("general after the overwrite: %v", cp[general])
	}
	if st, _ := bob.Do("POST", "/api/v1/channels/"+general+"/messages", map[string]any{"content": "нельзя"}); st != http.StatusForbidden {
		t.Fatalf("send to a read-only channel: %d, the server must agree with what the gateway told Bob", st)
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

// The relay lags: changes are committed but not yet published when Bob
// connects, so READY already contains them and the stream replays them
// afterwards. Replaying "secret open to mods" on top of a snapshot where Bob
// is a mod would reveal a channel he could see neither then nor now; the
// snapshot cursor (outbox id) makes the gateway skip what READY contains.
func TestGatewayLaggingRelayDoesNotLeak(t *testing.T) {
	s := newStackRelay(t, false, Options{})
	owner, bob := s.User("owner"), s.User("bob")
	gid, general := guildWith(owner, bob)
	secret := apitest.Str(apitest.Obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/guilds/"+gid+"/channels",
		map[string]any{"name": "secret", "type": 0}))["id"])
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+secret+"/permissions/"+gid,
		map[string]any{"type": 1, "deny": permString(perm.ViewChannel)})
	role := apitest.Str(apitest.Obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/guilds/"+gid+"/roles",
		map[string]any{"name": "mod"}))["id"])
	s.flush() // everything so far is history before Bob connects

	// Unpublished: open to mods, close to mods, make Bob a mod.
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+secret+"/permissions/"+role,
		map[string]any{"type": 1, "allow": permString(perm.ViewChannel)})
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+secret+"/permissions/"+role,
		map[string]any{"type": 1, "deny": permString(perm.ViewChannel)})
	owner.Expect(http.StatusNoContent, "PUT", "/api/v1/guilds/"+gid+"/members/"+bob.ID.String()+"/roles/"+role, nil)

	bobWS, ready := s.connect(bob)
	if len(ready.Guilds) != 1 || len(ready.Guilds[0].Channels) != 2 {
		t.Fatalf("READY channels: %+v", ready.Guilds)
	}
	owner.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+general+"/messages", map[string]any{"content": "маркер"})
	s.flush()
	// The replayed changes are all in READY: the first thing Bob gets is the
	// message after them.
	if f := bobWS.expectDispatch(events.MessageCreate); !bytes.Contains(f.D, []byte("маркер")) {
		t.Fatalf("first frame after READY: %s", f.D)
	}
	// Changes after READY still arrive.
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+secret+"/permissions/"+role,
		map[string]any{"type": 1, "allow": permString(perm.ViewChannel)})
	s.flush()
	if f := bobWS.expectDispatch(events.ChannelCreate); idOf(f) != secret {
		t.Fatalf("change after READY: %s", f.D)
	}
}

// Typing and presence across the real stack: REST → NATS → gateway →
// WebSocket, with permission filtering and the guild member list.
func TestGatewayTypingAndPresence(t *testing.T) {
	s := newStack(t)
	owner, bob := s.User("owner"), s.User("bob")
	gid, general := guildWith(owner, bob)
	// Otherwise GUILD_CREATE may still be on its way and arrive again after
	// READY (documented: personal *_CREATE events may repeat).
	s.flush()
	ownerWS, _ := s.connect(owner)
	bobWS, _ := s.connect(bob)

	// Bob comes online: the owner sees it through their shared small guild.
	if st := statusOf(t, ownerWS, bob.ID); st != presence.Online {
		t.Fatalf("bob online: %s", st)
	}
	// Bob reads the owner's status from the member list, not from events
	// published before he was listening.
	for _, m := range apitest.List(bob.Expect(http.StatusOK, "GET", "/api/v1/guilds/"+gid+"/members", nil)) {
		if apitest.Str(apitest.Obj(m)["user_id"]) == owner.ID.String() && apitest.Obj(m)["status"] != "online" {
			t.Fatalf("owner in the member list: %v", m)
		}
	}

	typingIn := func(f frame) string {
		var p struct {
			ChannelID string `json:"channel_id"`
			UserID    string `json:"user_id"`
		}
		_ = json.Unmarshal(f.D, &p)
		return p.ChannelID + " by " + p.UserID
	}
	bob.Expect(http.StatusNoContent, "POST", "/api/v1/channels/"+general+"/typing", nil)
	if got := typingIn(ownerWS.expectDispatch(events.TypingStart)); got != general+" by "+bob.ID.String() {
		t.Fatalf("typing: %s", got)
	}

	// Typing in a channel hidden from Bob never reaches him: the next thing
	// he gets after the hiding is the typing in general.
	secret := apitest.Str(apitest.Obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/guilds/"+gid+"/channels",
		map[string]any{"name": "secret", "type": 0}))["id"])
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+secret+"/permissions/"+gid,
		map[string]any{"type": 1, "deny": permString(perm.ViewChannel)})
	for _, typ := range []string{events.ChannelCreate, typePermissionsUpdate, events.ChannelDelete, typePermissionsUpdate} {
		bobWS.expectDispatch(typ)
	}
	// The owner still sees the channel: created, then updated; their
	// permissions change only with the new channel.
	for _, typ := range []string{events.ChannelCreate, typePermissionsUpdate, events.ChannelUpdate} {
		ownerWS.expectDispatch(typ)
	}
	owner.Expect(http.StatusNoContent, "POST", "/api/v1/channels/"+secret+"/typing", nil)
	owner.Expect(http.StatusNoContent, "POST", "/api/v1/channels/"+general+"/typing", nil)
	if got := typingIn(bobWS.expectDispatch(events.TypingStart)); got != general+" by "+owner.ID.String() {
		t.Fatalf("bob got typing from %s, the hidden channel leaked", got)
	}

	// Bob chooses "do not disturb", then invisible: his session gets his own
	// USER_UPDATE, the owner sees dnd, then offline.
	bob.Expect(http.StatusOK, "PATCH", "/api/v1/users/@me", map[string]any{"status": "dnd"})
	bobWS.expectDispatch(events.UserUpdate)
	if st := statusOf(t, ownerWS, bob.ID); st != presence.DND {
		t.Fatalf("bob dnd: %s", st)
	}
	bob.Expect(http.StatusOK, "PATCH", "/api/v1/users/@me", map[string]any{"status": "invisible"})
	bobWS.expectDispatch(events.UserUpdate)
	if st := statusOf(t, ownerWS, bob.ID); st != presence.Offline {
		t.Fatalf("bob invisible: %s", st)
	}
}

// A profile change crosses the real stack once per session, however many
// guilds the two users share.
func TestProfileUpdateOnce(t *testing.T) {
	s := newStack(t)
	owner, bob := s.User("owner"), s.User("bob")
	guildWith(owner, bob)
	guildWith(owner, bob)
	ownerWS, _ := s.connect(owner)

	_, out := bob.Upload("PUT", "/api/v1/users/@me/avatar", apitest.PNG(16, 16, color.NRGBA{9, 9, 9, 255}))
	avatar := apitest.Str(apitest.Obj(out)["avatar"])
	profileOf := func() map[string]any {
		t.Helper()
		var p struct {
			User map[string]any `json:"user"`
		}
		if err := json.Unmarshal(ownerWS.expectDispatch(events.UserProfileUpdate).D, &p); err != nil {
			t.Fatal(err)
		}
		return p.User
	}
	if u := profileOf(); u["id"] != bob.ID.String() || u["avatar"] != avatar || avatar == "" {
		t.Fatalf("profile update %v, avatar %q", u, avatar)
	}
	// The second guild's copy must not follow: the next frame is the next change.
	bob.Expect(http.StatusOK, "PATCH", "/api/v1/users/@me", map[string]any{"bio": "не для серверов"})
	bob.Expect(http.StatusOK, "PATCH", "/api/v1/users/@me", map[string]any{"display_name": "Боб"})
	if u := profileOf(); u["display_name"] != "Боб" || u["avatar"] != avatar {
		t.Fatalf("next profile update %v: a copy of the first or a bio change leaked", u)
	}
}
