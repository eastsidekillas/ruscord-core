package message

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
	"github.com/eastsidekillas/ruscord-core/internal/social"
)

type env struct {
	*apitest.Env
	guilds  *guild.Service
	socials *social.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := apitest.New(t)
	guilds := guild.NewService(e.PG, e.Auth, e.Limiter)
	socials := social.NewService(e.PG, e.Auth, guilds, e.Limiter)
	guilds.OnChannelsDeleted(DeleteChannels)
	socials.OnChannelsDeleted(DeleteChannels)
	guild.NewHandler(guilds, e.RequireAuth).Register(e.Mux)
	social.NewHandler(socials, e.RequireAuth).Register(e.Mux)
	NewHandler(NewService(e.PG, e.Auth, guilds, socials, e.Limiter, e.Eph), e.RequireAuth).Register(e.Mux)
	return &env{Env: e, guilds: guilds, socials: socials}
}

var (
	obj  = apitest.Obj
	list = apitest.List
	str  = apitest.Str
)

func permStr(p perm.Permissions) string { return strconv.FormatUint(uint64(p), 10) }

// server makes a guild owned by owner with the given members and returns
// its id and its text and voice channel ids.
func (e *env) server(owner *apitest.User, members ...*apitest.User) (gid, text, voice string) {
	e.T.Helper()
	g, err := e.guilds.Create(context.Background(), owner.ID, guild.CreateGuildInput{Name: "сервер"})
	if err != nil {
		e.T.Fatal(err)
	}
	for _, m := range members {
		if _, err := e.guilds.AddMember(context.Background(), g.ID, m.ID); err != nil {
			e.T.Fatal(err)
		}
	}
	return g.ID.String(), g.Channels[0].ID.String(), g.Channels[1].ID.String()
}

func (e *env) count(channelID string) int {
	e.T.Helper()
	var n int
	if err := e.PG.QueryRow(context.Background(), `SELECT count(*) FROM messages WHERE channel_id = $1`, channelID).Scan(&n); err != nil {
		e.T.Fatal(err)
	}
	return n
}

func msgs(ch string) string { return "/api/v1/channels/" + ch + "/messages" }

func send(u *apitest.User, ch, content string) map[string]any {
	return obj(u.Expect(http.StatusCreated, "POST", msgs(ch), map[string]any{"content": content}))
}

func TestGuildMessages(t *testing.T) {
	e := newEnv(t)
	owner, bob, eve := e.User("owner"), e.User("bob"), e.User("eve")
	_, text, voice := e.server(owner, bob)

	m := send(bob, text, "  привет  ")
	if m["content"] != "привет" || obj(m["author"])["username"] != "bob" || m["guild_id"] == nil {
		t.Fatalf("message = %v", m)
	}
	// A non-member cannot tell the channel exists.
	eve.Expect(http.StatusNotFound, "POST", msgs(text), map[string]any{"content": "x"})
	eve.Expect(http.StatusNotFound, "GET", msgs(text), nil)
	eve.Expect(http.StatusNotFound, "GET", msgs(uuid.NewString()), nil)
	for method, req := range map[string]any{"POST": map[string]any{"content": "x"}, "GET": nil} {
		body := obj(bob.Expect(http.StatusBadRequest, method, msgs(voice), req))
		if body["code"] != "not_text_channel" || !strings.Contains(str(body["message"]), "читать") {
			t.Fatalf("%s voice: %v", method, body)
		}
	}

	// Nonce: the retry returns the first message with 200.
	first := obj(bob.Expect(http.StatusCreated, "POST", msgs(text), map[string]any{"content": "раз", "nonce": "n-1"}))
	retry := obj(bob.Expect(http.StatusOK, "POST", msgs(text), map[string]any{"content": "раз", "nonce": "n-1"}))
	if first["id"] != retry["id"] || e.count(text) != 2 {
		t.Fatalf("nonce retry made a duplicate: %v vs %v", first["id"], retry["id"])
	}
	// The same nonce from someone else is a different message.
	owner.Expect(http.StatusCreated, "POST", msgs(text), map[string]any{"content": "раз", "nonce": "n-1"})

	for i := range 5 {
		send(owner, text, "msg "+strconv.Itoa(i))
	}
	all := list(bob.Expect(http.StatusOK, "GET", msgs(text), nil))
	if len(all) != 8 || obj(all[0])["content"] != "msg 4" {
		t.Fatalf("history = %d, newest %v", len(all), obj(all[0])["content"])
	}
	page := list(bob.Expect(http.StatusOK, "GET", msgs(text)+"?limit=2&before="+str(obj(all[1])["id"]), nil))
	if len(page) != 2 || obj(page[0])["id"] != obj(all[2])["id"] || obj(page[1])["id"] != obj(all[3])["id"] {
		t.Fatalf("before page = %v", page)
	}
	after := list(bob.Expect(http.StatusOK, "GET", msgs(text)+"?limit=2&after="+str(obj(all[4])["id"]), nil))
	if len(after) != 2 || obj(after[0])["id"] != obj(all[2])["id"] || obj(after[1])["id"] != obj(all[3])["id"] {
		t.Fatalf("after page = %v", after)
	}
	bob.Expect(http.StatusBadRequest, "GET", msgs(text)+"?limit=101", nil)
	bob.Expect(http.StatusBadRequest, "GET", msgs(text)+"?before=x", nil)

	// Validation.
	bob.Expect(http.StatusUnprocessableEntity, "POST", msgs(text), map[string]any{"content": "   "})
	bob.Expect(http.StatusUnprocessableEntity, "POST", msgs(text), map[string]any{"content": string(make([]rune, 4001))})
	bob.Expect(http.StatusUnprocessableEntity, "POST", msgs(text), map[string]any{"content": "x", "nonce": ""})
	bob.Expect(http.StatusUnprocessableEntity, "POST", msgs(text), map[string]any{"content": "a\u0000b"})
	bob.Expect(http.StatusUnprocessableEntity, "POST", msgs(text), map[string]any{"content": "x", "nonce": "a\u0000"})
	bob.Expect(http.StatusUnprocessableEntity, "POST", msgs(text), map[string]any{"content": "x", "reply_to": uuid.New()})
	reply := obj(bob.Expect(http.StatusCreated, "POST", msgs(text), map[string]any{"content": "ответ", "reply_to": m["id"]}))
	if reply["reply_to"] != m["id"] {
		t.Fatalf("reply = %v", reply)
	}
}

func TestMessagePermissions(t *testing.T) {
	e := newEnv(t)
	owner, bob, mod := e.User("owner"), e.User("bob"), e.User("mod")
	gid, text, _ := e.server(owner, bob, mod)
	g := "/api/v1/guilds/" + gid
	ow := "/api/v1/channels/" + text + "/permissions/" + gid

	// @everyone: only the owner (who has everything) gets mention_everyone.
	if m := send(bob, text, "@everyone смотрите"); m["mention_everyone"] != false {
		t.Fatalf("member mention_everyone = %v", m["mention_everyone"])
	}
	if m := send(owner, text, "@here важно"); m["mention_everyone"] != true {
		t.Fatalf("owner mention_everyone = %v", m["mention_everyone"])
	}

	// Edit: own messages only; edited_at is set.
	m := send(bob, text, "опечатка")
	path := msgs(text) + "/" + str(m["id"])
	owner.Expect(http.StatusForbidden, "PATCH", path, map[string]any{"content": "чужое"})
	ed := obj(bob.Expect(http.StatusOK, "PATCH", path, map[string]any{"content": "исправлено"}))
	if ed["content"] != "исправлено" || ed["edited_at"] == nil {
		t.Fatalf("edited = %v", ed)
	}

	// Delete: own, or MANAGE_MESSAGES.
	role := str(obj(owner.Expect(http.StatusCreated, "POST", g+"/roles", map[string]any{"name": "mod", "permissions": permStr(perm.ManageMessages)}))["id"])
	owner.Expect(http.StatusNoContent, "PUT", g+"/members/"+mod.ID.String()+"/roles/"+role, nil)
	other := send(owner, text, "удалите меня")
	bob.Expect(http.StatusForbidden, "DELETE", msgs(text)+"/"+str(other["id"]), nil)
	mod.Expect(http.StatusNoContent, "DELETE", msgs(text)+"/"+str(other["id"]), nil)
	mod.Expect(http.StatusNotFound, "DELETE", msgs(text)+"/"+str(other["id"]), nil)
	bob.Expect(http.StatusNoContent, "DELETE", path, nil)

	// Read-only channel: history yes, sending and editing no.
	keep := send(bob, text, "старое")
	owner.Expect(http.StatusOK, "PUT", ow, map[string]any{"type": 1, "deny": permStr(perm.SendMessages)})
	bob.Expect(http.StatusForbidden, "POST", msgs(text), map[string]any{"content": "x"})
	bob.Expect(http.StatusForbidden, "PATCH", msgs(text)+"/"+str(keep["id"]), map[string]any{"content": "x"})
	bob.Expect(http.StatusOK, "GET", msgs(text), nil)

	// No history permission.
	owner.Expect(http.StatusOK, "PUT", ow, map[string]any{"type": 1, "deny": permStr(perm.ReadMessageHistory)})
	bob.Expect(http.StatusForbidden, "GET", msgs(text), nil)

	// Hidden channel looks missing.
	owner.Expect(http.StatusOK, "PUT", ow, map[string]any{"type": 1, "deny": permStr(perm.ViewChannel)})
	bob.Expect(http.StatusNotFound, "GET", msgs(text), nil)
	bob.Expect(http.StatusNotFound, "POST", msgs(text), map[string]any{"content": "x"})
	bob.Expect(http.StatusNotFound, "DELETE", msgs(text)+"/"+str(keep["id"]), nil)
}

func TestDirectMessageChannel(t *testing.T) {
	e := newEnv(t)
	ann, bob, eve := e.User("ann"), e.User("bob"), e.User("eve")
	ann.Expect(http.StatusNoContent, "PUT", "/api/v1/users/@me/relationships/"+bob.ID.String(), map[string]any{})
	bob.Expect(http.StatusNoContent, "PUT", "/api/v1/users/@me/relationships/"+ann.ID.String(), map[string]any{})
	dm := str(obj(ann.Expect(http.StatusOK, "POST", "/api/v1/users/@me/channels", map[string]any{"recipient_id": bob.ID}))["id"])

	m := send(ann, dm, "привет")
	if _, ok := m["guild_id"]; ok {
		t.Fatalf("DM message has guild_id: %v", m)
	}
	if n := len(list(bob.Expect(http.StatusOK, "GET", msgs(dm), nil))); n != 1 {
		t.Fatalf("bob sees %d messages", n)
	}
	eve.Expect(http.StatusNotFound, "GET", msgs(dm), nil)
	eve.Expect(http.StatusNotFound, "POST", msgs(dm), map[string]any{"content": "x"})
	// No MANAGE_MESSAGES in DMs: only your own messages can be deleted.
	bob.Expect(http.StatusForbidden, "DELETE", msgs(dm)+"/"+str(m["id"]), nil)

	var topic string
	if err := e.PG.QueryRow(t.Context(), `SELECT topic FROM outbox WHERE type = 'MESSAGE_CREATE'`).Scan(&topic); err != nil || topic != "channel."+dm {
		t.Fatalf("DM message topic = %q, %v", topic, err)
	}

	// A block makes the DM read-only for both.
	bob.Expect(http.StatusNoContent, "PUT", "/api/v1/users/@me/relationships/"+ann.ID.String(), map[string]any{"type": 2})
	ann.Expect(http.StatusForbidden, "POST", msgs(dm), map[string]any{"content": "x"})
	bob.Expect(http.StatusForbidden, "POST", msgs(dm), map[string]any{"content": "x"})
	ann.Expect(http.StatusOK, "GET", msgs(dm), nil)
}

func TestMessagesGoWithTheirChannel(t *testing.T) {
	e := newEnv(t)
	owner, bob := e.User("owner"), e.User("bob")
	gid, text, _ := e.server(owner, bob)
	second := str(obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/guilds/"+gid+"/channels", map[string]any{"type": 0, "name": "второй"}))["id"])
	send(owner, text, "в первом")
	send(owner, second, "во втором")

	owner.Expect(http.StatusNoContent, "DELETE", "/api/v1/channels/"+second, nil)
	if e.count(second) != 0 || e.count(text) != 1 {
		t.Fatalf("after channel delete: second %d, text %d", e.count(second), e.count(text))
	}
	owner.Expect(http.StatusNoContent, "DELETE", "/api/v1/guilds/"+gid, nil)
	if e.count(text) != 0 {
		t.Fatalf("guild delete left %d messages", e.count(text))
	}

	// The last member leaving a group DM deletes it and its messages.
	owner.Expect(http.StatusNoContent, "PUT", "/api/v1/users/@me/relationships/"+bob.ID.String(), map[string]any{})
	bob.Expect(http.StatusNoContent, "PUT", "/api/v1/users/@me/relationships/"+owner.ID.String(), map[string]any{})
	grp := str(obj(owner.Expect(http.StatusOK, "POST", "/api/v1/users/@me/channels", map[string]any{"recipients": []uuid.UUID{bob.ID}}))["id"])
	send(bob, grp, "в группе")
	owner.Expect(http.StatusNoContent, "DELETE", "/api/v1/users/@me/channels/"+grp, nil)
	if e.count(grp) != 1 {
		t.Fatal("messages must stay while someone is in the group")
	}
	bob.Expect(http.StatusNoContent, "DELETE", "/api/v1/users/@me/channels/"+grp, nil)
	if e.count(grp) != 0 {
		t.Fatalf("deleted group left %d messages", e.count(grp))
	}
}

func TestSendRateLimit(t *testing.T) {
	e := newEnv(t)
	owner := e.User("owner")
	_, text, _ := e.server(owner)
	for range ruleSend.Limit {
		send(owner, text, "спам")
	}
	owner.Expect(http.StatusTooManyRequests, "POST", msgs(text), map[string]any{"content": "ещё"})
}

// Typing goes out as one ephemeral event per window, never through the
// outbox, and only for people who could send a message there.
func TestTyping(t *testing.T) {
	e := newEnv(t)
	owner, bob, eve := e.User("owner"), e.User("bob"), e.User("eve")
	gid, text, voice := e.server(owner, bob)
	typing := func(ch string) string { return "/api/v1/channels/" + ch + "/typing" }

	bob.Expect(http.StatusNoContent, "POST", typing(text), nil)
	got := e.Eph.Take()
	if len(got) != 1 || got[0].Subject != events.EphGuildTopic(uuid.MustParse(gid)) || got[0].Type != events.TypingStart {
		t.Fatalf("published %+v", got)
	}
	ty := got[0].Data.(Typing)
	if ty.UserID != bob.ID || ty.ChannelID.String() != text || ty.GuildID == nil || ty.GuildID.String() != gid || ty.Timestamp.IsZero() {
		t.Fatalf("payload %+v", ty)
	}
	// Within the window: 204, but nothing is sent again.
	bob.Expect(http.StatusNoContent, "POST", typing(text), nil)
	if n := len(e.Eph.Take()); n != 0 {
		t.Fatalf("%d events inside the window", n)
	}
	// Someone else in the same channel is not throttled by Bob.
	owner.Expect(http.StatusNoContent, "POST", typing(text), nil)
	if n := len(e.Eph.Take()); n != 1 {
		t.Fatalf("owner typing: %d events", n)
	}

	// Not text, not a member, not existing: same answers as sending.
	bob.Expect(http.StatusBadRequest, "POST", typing(voice), nil)
	eve.Expect(http.StatusNotFound, "POST", typing(text), nil)
	bob.Expect(http.StatusNotFound, "POST", typing(uuid.NewString()), nil)
	// Read-only: cannot type there either; hidden: does not exist.
	ow := "/api/v1/channels/" + text + "/permissions/" + gid
	owner.Expect(http.StatusOK, "PUT", ow, map[string]any{"type": 1, "deny": permStr(perm.SendMessages)})
	bob.Expect(http.StatusForbidden, "POST", typing(text), nil)
	owner.Expect(http.StatusOK, "PUT", ow, map[string]any{"type": 1, "deny": permStr(perm.ViewChannel)})
	bob.Expect(http.StatusNotFound, "POST", typing(text), nil)
	if n := len(e.Eph.Take()); n != 0 {
		t.Fatalf("%d events from refused requests", n)
	}
	if types := e.OutboxTypes(); slices.Contains(types, events.TypingStart) {
		t.Fatal("typing must never be written to the outbox")
	}

	// A DM goes to the channel's ephemeral subject.
	ann := e.User("ann")
	ann.Expect(http.StatusNoContent, "PUT", "/api/v1/users/@me/relationships/"+bob.ID.String(), map[string]any{})
	bob.Expect(http.StatusNoContent, "PUT", "/api/v1/users/@me/relationships/"+ann.ID.String(), map[string]any{})
	dm := str(obj(ann.Expect(http.StatusOK, "POST", "/api/v1/users/@me/channels", map[string]any{"recipient_id": bob.ID}))["id"])
	ann.Expect(http.StatusNoContent, "POST", typing(dm), nil)
	got = e.Eph.Take()
	if len(got) != 1 || got[0].Subject != events.EphChannelTopic(uuid.MustParse(dm)) || got[0].Data.(Typing).GuildID != nil {
		t.Fatalf("DM typing: %+v", got)
	}
	// A block makes the DM read-only: no typing either.
	bob.Expect(http.StatusNoContent, "PUT", "/api/v1/users/@me/relationships/"+ann.ID.String(), map[string]any{"type": 2})
	ann.Expect(http.StatusForbidden, "POST", typing(dm), nil)
}
