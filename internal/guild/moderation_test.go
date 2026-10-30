package guild

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
)

// Who may ban whom: BAN_MEMBERS plus a higher top role for members, never
// the owner or yourself; a non-member can be banned too.
func TestBanRules(t *testing.T) {
	e := newEnv(t)
	owner, mod, peer, member, outsider, plain := e.User("owner"), e.User("mod"), e.User("peer"), e.User("member"),
		e.User("outsider"), e.User("plain")
	gid, _, _ := createGuild(owner)
	g := "/api/v1/guilds/" + gid
	for _, u := range []*apitest.User{mod, peer, member, plain} {
		e.join(gid, u)
	}
	role := func(name string, p perm.Permissions) string {
		return str(obj(owner.Expect(http.StatusCreated, "POST", g+"/roles", map[string]any{"name": name, "permissions": permStr(p)}))["id"])
	}
	high := role("high", perm.BanMembers)
	low := role("low", perm.BanMembers) // a new role takes position 1: below "high"
	owner.Expect(http.StatusNoContent, "PUT", g+"/members/"+mod.ID.String()+"/roles/"+high, nil)
	owner.Expect(http.StatusNoContent, "PUT", g+"/members/"+peer.ID.String()+"/roles/"+high, nil)
	owner.Expect(http.StatusNoContent, "PUT", g+"/members/"+member.ID.String()+"/roles/"+low, nil)

	ban := func(by, target *apitest.User) int {
		code, _ := by.Do("PUT", g+"/bans/"+target.ID.String(), map[string]any{"reason": "спам"})
		return code
	}
	cases := []struct {
		name       string
		by, target *apitest.User
		code       int
	}{
		{"without BAN_MEMBERS", plain, member, http.StatusForbidden},
		{"yourself", mod, mod, http.StatusBadRequest},
		{"the owner", mod, owner, http.StatusForbidden},
		{"an equal role", mod, peer, http.StatusForbidden},
		{"a lower role", mod, member, http.StatusNoContent},
		{"again: the reason is updated", mod, member, http.StatusNoContent},
		{"someone who is not a member", mod, outsider, http.StatusNoContent},
		{"by someone outside the guild", outsider, plain, http.StatusNotFound},
	}
	for _, c := range cases {
		if got := ban(c.by, c.target); got != c.code {
			t.Errorf("%s: %d, want %d", c.name, got, c.code)
		}
	}
	if code, _ := mod.Do("PUT", g+"/bans/"+uuid.NewString(), nil); code != http.StatusNotFound {
		t.Errorf("unknown user: %d", code)
	}
	if code, _ := mod.Do("PUT", g+"/bans/"+plain.ID.String(), map[string]any{"reason": string(make([]byte, 513))}); code != http.StatusUnprocessableEntity {
		t.Errorf("long reason: %d", code)
	}

	// The banned member is out, sees the guild go, and cannot come back.
	if code, _ := member.Do("GET", g, nil); code != http.StatusNotFound {
		t.Fatalf("banned member still sees the guild: %d", code)
	}
	if got := e.OutboxTypes(); !contains(got, "GUILD_MEMBER_REMOVE") || !contains(got, "GUILD_DELETE") {
		t.Fatalf("events %v", got)
	}
	for _, u := range []*apitest.User{member, outsider} {
		if _, err := e.svc.AddMember(t.Context(), uuid.MustParse(gid), u.ID); err == nil {
			t.Fatalf("%s joined while banned", u.ID)
		}
	}
	text := str(obj(list(owner.Expect(http.StatusOK, "GET", g+"/channels", nil))[0])["id"])
	code := str(obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+text+"/invites", map[string]any{}))["code"])
	if c, out := member.Do("POST", "/api/v1/invites/"+code, nil); c != http.StatusForbidden || obj(out)["code"] != "banned" {
		t.Fatalf("banned user by invite: %d %v", c, out)
	}

	// The list: only for BAN_MEMBERS, newest first, with the user.
	plain.Expect(http.StatusForbidden, "GET", g+"/bans", nil)
	bans := list(mod.Expect(http.StatusOK, "GET", g+"/bans", nil))
	if len(bans) != 2 || obj(obj(bans[1])["user"])["username"] != "member" || obj(bans[1])["reason"] != "спам" {
		t.Fatalf("bans %v", bans)
	}

	// Unban lets them in again.
	plain.Expect(http.StatusForbidden, "DELETE", g+"/bans/"+member.ID.String(), nil)
	mod.Expect(http.StatusNoContent, "DELETE", g+"/bans/"+member.ID.String(), nil)
	mod.Expect(http.StatusNotFound, "DELETE", g+"/bans/"+member.ID.String(), nil)
	member.Expect(http.StatusOK, "POST", "/api/v1/invites/"+code, nil)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
