package guild

import (
	"image/color"
	"net/http"
	"strings"
	"testing"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
)

func TestGuildIcon(t *testing.T) {
	e := newEnv(t)
	owner, mod, bob, eve := e.User("owner"), e.User("mod"), e.User("bob"), e.User("eve")
	gid, text, _ := createGuild(owner)
	g := "/api/v1/guilds/" + gid
	e.join(gid, mod)
	e.join(gid, bob)
	role := str(obj(owner.Expect(http.StatusCreated, "POST", g+"/roles",
		map[string]any{"name": "mod", "permissions": permStr(perm.ManageGuild)}))["id"])
	owner.Expect(http.StatusNoContent, "PUT", g+"/members/"+mod.ID.String()+"/roles/"+role, nil)
	pic := apitest.PNG(64, 64, color.NRGBA{0, 128, 0, 255})

	// Without MANAGE_GUILD nothing is decoded or stored.
	for _, c := range []struct {
		name string
		u    *apitest.User
		code int
	}{
		{"member", bob, http.StatusForbidden},
		{"outsider", eve, http.StatusNotFound},
	} {
		if code, _ := c.u.Upload("PUT", g+"/icon", pic); code != c.code {
			t.Fatalf("%s: %d, want %d", c.name, code, c.code)
		}
		if code, _ := c.u.Do("DELETE", g+"/icon", nil); code != c.code {
			t.Fatalf("%s DELETE: %d, want %d", c.name, code, c.code)
		}
	}
	if len(e.Blobs.Keys()) != 0 || len(e.Garbage()) != 0 {
		t.Fatal("a forbidden upload stored a file")
	}
	if code, _ := mod.Upload("PUT", g+"/icon", []byte("<svg/>")); code != http.StatusUnprocessableEntity {
		t.Fatalf("SVG icon: %d", code)
	}

	code, out := mod.Upload("PUT", g+"/icon", pic)
	icon := str(obj(out)["icon"])
	if code != 200 || !strings.HasPrefix(icon, "/cdn/icons/"+gid+"/") {
		t.Fatalf("upload by a MANAGE_GUILD member: %d %v", code, out)
	}
	ev := e.Outbox("GUILD_UPDATE")
	if len(ev) != 1 || ev[0].Topic != "guild."+gid || ev[0].Data["icon"] != icon {
		t.Fatalf("GUILD_UPDATE %v", ev)
	}
	// Everywhere the guild is shown, the icon is too.
	if got := obj(bob.Expect(http.StatusOK, "GET", g, nil))["icon"]; got != icon {
		t.Fatalf("GET guild: icon %v", got)
	}
	if got := obj(list(bob.Expect(http.StatusOK, "GET", "/api/v1/users/@me/guilds", nil))[0])["icon"]; got != icon {
		t.Fatalf("guild list: icon %v", got)
	}
	code2 := str(obj(owner.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+text+"/invites", map[string]any{}))["code"])
	if got := obj(obj(eve.Expect(http.StatusOK, "GET", "/api/v1/invites/"+code2, nil))["guild"])["icon"]; got != icon {
		t.Fatalf("invite preview: icon %v", got)
	}
	// An unchanged icon writes nothing.
	mod.Upload("PUT", g+"/icon", pic)
	if n := len(e.Outbox("GUILD_UPDATE")); n != 1 {
		t.Fatalf("same icon again: %d GUILD_UPDATE", n)
	}
	// Name changes keep the icon.
	if got := obj(owner.Expect(http.StatusOK, "PATCH", g, map[string]any{"name": "Новое"}))["icon"]; got != icon {
		t.Fatalf("PATCH name dropped the icon: %v", got)
	}

	got := obj(mod.Expect(http.StatusOK, "DELETE", g+"/icon", nil))
	if got["icon"] != nil {
		t.Fatalf("after DELETE: %v", got)
	}
	e.Sweep()
	if len(e.Blobs.Keys()) != 0 {
		t.Fatalf("files left: %v", e.Blobs.Keys())
	}

	// Deleting the guild schedules its icon.
	mod.Upload("PUT", g+"/icon", pic)
	owner.Expect(http.StatusNoContent, "DELETE", g, nil)
	if h := e.Garbage()[strings.TrimPrefix(icon, "/cdn/")]; h != 24 {
		t.Fatalf("icon of a deleted guild due in %dh: %v", h, e.Garbage())
	}
	e.Sweep()
	if len(e.Blobs.Keys()) != 0 {
		t.Fatalf("icon of a deleted guild kept: %v", e.Blobs.Keys())
	}
}
