package guild

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestInvites(t *testing.T) {
	e := newEnv(t)
	owner, bob, carol, dave := e.User("owner"), e.User("bob"), e.User("carol"), e.User("dave")
	gid, text, _ := createGuild(owner)
	g := "/api/v1/guilds/" + gid

	inv := obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+text+"/invites", map[string]any{}))
	code := str(inv["code"])
	if len(code) != inviteCodeLen || inv["expires_at"] == nil {
		t.Fatalf("invite = %v", inv)
	}

	p := obj(bob.Expect(http.StatusOK, "GET", "/api/v1/invites/"+code, nil))
	if obj(p["guild"])["id"] != gid || p["member_count"] != float64(1) || obj(p["inviter"])["username"] != "owner" {
		t.Fatalf("preview = %v", p)
	}
	full := obj(bob.Expect(http.StatusOK, "POST", "/api/v1/invites/"+code, nil))
	if full["id"] != gid || obj(full["member"])["user_id"] != bob.ID.String() {
		t.Fatalf("accept = %v", full)
	}
	// Accepting again is harmless and does not use the invite up.
	bob.Expect(http.StatusOK, "POST", "/api/v1/invites/"+code, nil)
	invs := list(owner.Expect(http.StatusOK, "GET", g+"/invites", nil))
	if len(invs) != 1 || obj(invs[0])["uses"] != float64(1) {
		t.Fatalf("invites = %v", invs)
	}
	bob.Expect(http.StatusForbidden, "GET", g+"/invites", nil)
	carol.Expect(http.StatusNotFound, "GET", g+"/invites", nil)

	// max_uses
	one := str(obj(bob.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+text+"/invites", map[string]any{"max_uses": 1}))["code"])
	carol.Expect(http.StatusOK, "POST", "/api/v1/invites/"+one, nil)
	dave.Expect(http.StatusNotFound, "GET", "/api/v1/invites/"+one, nil)
	dave.Expect(http.StatusNotFound, "POST", "/api/v1/invites/"+one, nil)

	// Who may revoke: the creator, MANAGE_CHANNELS in the channel, MANAGE_GUILD.
	mine := str(obj(bob.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+text+"/invites", map[string]any{"max_age": 0}))["code"])
	carol.Expect(http.StatusForbidden, "DELETE", "/api/v1/invites/"+mine, nil)
	dave.Expect(http.StatusNotFound, "DELETE", "/api/v1/invites/"+mine, nil)
	bob.Expect(http.StatusNoContent, "DELETE", "/api/v1/invites/"+mine, nil)
	bob.Expect(http.StatusNotFound, "DELETE", "/api/v1/invites/"+mine, nil)
	owner.Expect(http.StatusNoContent, "DELETE", "/api/v1/invites/"+one, nil)

	// CREATE_INVITE follows channel permissions; hidden channels stay hidden.
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+text+"/permissions/"+gid, map[string]any{"type": 1, "deny": permStr(1 << 16)})
	bob.Expect(http.StatusForbidden, "POST", "/api/v1/channels/"+text+"/invites", map[string]any{})
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+text+"/permissions/"+gid, map[string]any{"type": 1, "deny": permStr(1 << 0)})
	bob.Expect(http.StatusNotFound, "POST", "/api/v1/channels/"+text+"/invites", map[string]any{})
	owner.Expect(http.StatusNoContent, "DELETE", "/api/v1/channels/"+text+"/permissions/"+gid, nil)

	// Validation and malformed codes.
	owner.Expect(http.StatusUnprocessableEntity, "POST", "/api/v1/channels/"+text+"/invites", map[string]any{"max_age": 604801})
	owner.Expect(http.StatusUnprocessableEntity, "POST", "/api/v1/channels/"+text+"/invites", map[string]any{"max_uses": 101})
	dave.Expect(http.StatusNotFound, "GET", "/api/v1/invites/short", nil)
	dave.Expect(http.StatusNotFound, "GET", "/api/v1/invites/"+strings.Repeat("Z", 8), nil)

	// Expiry, with the service clock moved past it.
	soon := str(obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+text+"/invites", map[string]any{"max_age": 60}))["code"])
	dave.Expect(http.StatusOK, "GET", "/api/v1/invites/"+soon, nil)
	e.svc.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	dave.Expect(http.StatusNotFound, "GET", "/api/v1/invites/"+soon, nil)
	dave.Expect(http.StatusNotFound, "POST", "/api/v1/invites/"+soon, nil)
}

// An invite made while the channel was public stops working once the channel
// is hidden from @everyone, and the preview no longer reveals it.
func TestInviteToChannelHiddenLater(t *testing.T) {
	e := newEnv(t)
	owner, eve := e.User("owner"), e.User("eve")
	gid, text, _ := createGuild(owner)
	code := str(obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+text+"/invites", map[string]any{}))["code"])
	eve.Expect(http.StatusOK, "GET", "/api/v1/invites/"+code, nil)

	ow := "/api/v1/channels/" + text + "/permissions/" + gid
	owner.Expect(http.StatusOK, "PUT", ow, map[string]any{"type": 1, "deny": permStr(1 << 0)})
	if body := eve.Expect(http.StatusNotFound, "GET", "/api/v1/invites/"+code, nil); strings.Contains(str(obj(body)["message"]), "general") {
		t.Fatalf("404 leaks the channel: %v", body)
	}
	eve.Expect(http.StatusNotFound, "POST", "/api/v1/invites/"+code, nil)
	eve.Expect(http.StatusNotFound, "GET", "/api/v1/guilds/"+gid, nil)

	// The owner still sees the guild via the invite, being a member already.
	owner.Expect(http.StatusOK, "POST", "/api/v1/invites/"+code, nil)

	// Opening the channel again revives the invite.
	owner.Expect(http.StatusNoContent, "DELETE", ow, nil)
	eve.Expect(http.StatusOK, "POST", "/api/v1/invites/"+code, nil)
}

func TestInviteLookupsAreRateLimited(t *testing.T) {
	e := newEnv(t)
	eve := e.User("eve")
	for range ruleInviteUse.Limit {
		eve.Expect(http.StatusNotFound, "GET", "/api/v1/invites/AAAAAAAA", nil)
	}
	eve.Expect(http.StatusTooManyRequests, "POST", "/api/v1/invites/AAAAAAAA", nil)
}

func TestEveryoneRoleErrors(t *testing.T) {
	e := newEnv(t)
	owner := e.User("owner")
	gid, _, _ := createGuild(owner)
	g := "/api/v1/guilds/" + gid
	msg := func(v any) string { return str(obj(v)["message"]) }

	if m := msg(owner.Expect(http.StatusBadRequest, "PATCH", g+"/roles/"+gid, map[string]any{"name": "все"})); !strings.Contains(m, "переименовать") {
		t.Fatalf("rename message = %q", m)
	}
	if m := msg(owner.Expect(http.StatusBadRequest, "DELETE", g+"/roles/"+gid, nil)); !strings.Contains(m, "удалить") {
		t.Fatalf("delete message = %q", m)
	}
	if m := msg(owner.Expect(http.StatusBadRequest, "PUT", g+"/members/@me/roles/"+gid, nil)); !strings.Contains(m, "выдать") {
		t.Fatalf("assign message = %q", m)
	}
	// Its permissions can still be changed.
	owner.Expect(http.StatusOK, "PATCH", g+"/roles/"+gid, map[string]any{"permissions": permStr(1)})
}
