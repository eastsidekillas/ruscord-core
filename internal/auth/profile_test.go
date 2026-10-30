package auth_test

import (
	"bytes"
	"context"
	"image/color"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
)

// Profile tests use apitest with the guild module, for the fan-out.
// They need RUSCORD_TEST_POSTGRES_URL.

const me = "/api/v1/users/@me"

var (
	obj = apitest.Obj
	str = apitest.Str
	red = color.NRGBA{255, 0, 0, 255}
)

type env struct {
	*apitest.Env
	guilds *guild.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := apitest.New(t)
	guilds := guild.NewService(e.PG, e.Auth, e.Limiter)
	e.Auth.SetGuildIDs(guilds)
	guild.NewHandler(guilds, e.RequireAuth).Register(e.Mux)
	return &env{Env: e, guilds: guilds}
}

// inGuilds puts u into n new guilds and returns their topics.
func (e *env) inGuilds(u *apitest.User, n int) []string {
	e.T.Helper()
	var topics []string
	for range n {
		g := obj(u.Expect(http.StatusCreated, "POST", "/api/v1/guilds", map[string]any{"name": "Сервер"}))
		topics = append(topics, "guild."+str(g["id"]))
	}
	return topics
}

func topicsOf(evs []apitest.OutboxEvent) []string {
	var out []string
	for _, ev := range evs {
		out = append(out, ev.Topic)
	}
	return out
}

func TestUpdateProfileValidation(t *testing.T) {
	e := newEnv(t)
	u := e.User("timofey")
	cases := []struct {
		name  string
		body  map[string]any
		code  int
		field string
	}{
		{"nothing to change", map[string]any{}, 422, ""},
		{"empty name", map[string]any{"display_name": "   "}, 422, "display_name"},
		{"name of 33", map[string]any{"display_name": strings.Repeat("я", 33)}, 422, "display_name"},
		{"NUL in name", map[string]any{"display_name": "a\x00b"}, 422, "display_name"},
		{"bio of 191", map[string]any{"bio": strings.Repeat("ё", 191)}, 422, "bio"},
		{"NUL in bio", map[string]any{"bio": "a\x00"}, 422, "bio"},
		{"unknown status", map[string]any{"status": "away"}, 422, "status"},
		{"one bad field rejects all", map[string]any{"display_name": "Ок", "bio": strings.Repeat("x", 191)}, 422, "bio"},
		{"unknown field", map[string]any{"avatar": "/cdn/x"}, 400, ""},
		{"null fields are absent", map[string]any{"display_name": nil}, 422, ""},
	}
	for _, c := range cases {
		code, out := u.Do("PATCH", me, c.body)
		if code != c.code {
			t.Errorf("%s: %d %v, want %d", c.name, code, out, c.code)
			continue
		}
		if c.field != "" {
			if _, ok := obj(obj(out)["fields"])[c.field]; !ok {
				t.Errorf("%s: no message for %s in %v", c.name, c.field, out)
			}
		}
	}
	if evs := e.Outbox("USER_UPDATE"); len(evs) != 0 {
		t.Fatalf("rejected changes wrote %d events", len(evs))
	}
	// The longest values fit: rune counts, not bytes.
	got := obj(u.Expect(http.StatusOK, "PATCH", me, map[string]any{
		"display_name": "  " + strings.Repeat("я", 32) + " ", "bio": strings.Repeat("ё", 190)}))
	if got["display_name"] != strings.Repeat("я", 32) || got["bio"] != strings.Repeat("ё", 190) {
		t.Fatalf("saved %v", got)
	}
}

// Who hears about a change: the user's own sessions always; their guilds
// only when what others see (name, avatar) changed.
func TestProfileFanOut(t *testing.T) {
	e := newEnv(t)
	u := e.User("timofey")
	guilds := e.inGuilds(u, 3)
	self := "user." + u.ID.String()

	steps := []struct {
		name   string
		body   map[string]any
		guilds bool
	}{
		{"bio only", map[string]any{"bio": "Привет"}, false},
		{"status only", map[string]any{"status": "dnd"}, false},
		{"same name again", map[string]any{"display_name": "timofey"}, false},
		{"new name", map[string]any{"display_name": "Тимофей"}, true},
		{"name and bio", map[string]any{"display_name": "Тима", "bio": ""}, true},
	}
	for _, s := range steps {
		before := len(e.Outbox("USER_PROFILE_UPDATE"))
		u.Expect(http.StatusOK, "PATCH", me, s.body)
		own := e.Outbox("USER_UPDATE")
		if last := own[len(own)-1]; last.Topic != self {
			t.Fatalf("%s: USER_UPDATE to %s", s.name, last.Topic)
		}
		after := e.Outbox("USER_PROFILE_UPDATE")[before:]
		if !s.guilds {
			if len(after) != 0 {
				t.Fatalf("%s: guilds told: %v", s.name, topicsOf(after))
			}
			continue
		}
		if got := topicsOf(after); strings.Join(got, ",") != strings.Join(guilds, ",") {
			t.Fatalf("%s: USER_PROFILE_UPDATE to %v, want %v", s.name, got, guilds)
		}
		pub := obj(after[0].Data["user"])
		if pub["display_name"] != s.body["display_name"] || pub["id"] != u.ID.String() {
			t.Fatalf("%s: public user %v", s.name, pub)
		}
		// Only the public profile leaves for the guilds.
		for _, private := range []string{"email", "bio", "status"} {
			if _, ok := pub[private]; ok {
				t.Fatalf("%s: %s sent to guilds: %v", s.name, private, pub)
			}
		}
	}
	got := obj(u.Expect(http.StatusOK, "GET", me, nil))
	if got["display_name"] != "Тима" || got["bio"] != "" || got["status"] != "dnd" {
		t.Fatalf("after the steps: %v", got)
	}
}

func TestAvatar(t *testing.T) {
	e := newEnv(t)
	u := e.User("timofey")
	guilds := e.inGuilds(u, 2)
	avatar := me + "/avatar"

	// Refusals change nothing.
	for _, c := range []struct {
		name string
		data []byte
		code int
	}{
		{"SVG", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), 422},
		{"text", []byte("hello"), 422},
		{"empty", nil, 400},
		{"over 8 MiB", bytes.Repeat([]byte{0}, 8<<20+1), 413},
	} {
		if code, out := u.Upload("PUT", avatar, c.data); code != c.code {
			t.Fatalf("%s: %d %v, want %d", c.name, code, out, c.code)
		}
	}
	if len(e.Blobs.Keys()) != 0 || len(e.Outbox("USER_UPDATE")) != 0 {
		t.Fatal("a refused upload stored a file or wrote an event")
	}

	code, out := u.Upload("PUT", avatar, apitest.PNG(600, 300, red))
	first := str(obj(out)["avatar"])
	prefix := "/cdn/avatars/" + u.ID.String() + "/"
	if code != 200 || !strings.HasPrefix(first, prefix) || !strings.HasSuffix(first, ".jpg") {
		t.Fatalf("upload: %d %v", code, out)
	}
	firstKey := strings.TrimPrefix(first, "/cdn/")
	// The file is served, and the public profile in the guilds carries it.
	if code, h := e.Fetch(first); code != 200 || h.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("GET %s: %d %s", first, code, h.Get("Content-Type"))
	}
	pub := e.Outbox("USER_PROFILE_UPDATE")
	if len(pub) != len(guilds) || obj(pub[0].Data["user"])["avatar"] != first {
		t.Fatalf("profile events %v", pub)
	}
	// Until the referring row is committed, an upload is scheduled as garbage.
	if h, ok := e.Garbage()[firstKey]; !ok || h != 1 {
		t.Fatalf("garbage after upload: %v", e.Garbage())
	}

	// The same picture again changes nothing for others.
	u.Upload("PUT", avatar, apitest.PNG(600, 300, red))
	if n := len(e.Outbox("USER_PROFILE_UPDATE")); n != len(guilds) {
		t.Fatalf("re-upload of the same avatar told the guilds again (%d events)", n)
	}

	// A new picture replaces the old one; the old file stays for a day.
	_, out = u.Upload("PUT", avatar, apitest.PNG(10, 10, color.NRGBA{0, 0, 255, 128}))
	second := str(obj(out)["avatar"])
	if second == first || !strings.HasSuffix(second, ".png") {
		t.Fatalf("second avatar %q (first %q): transparent pictures stay PNG", second, first)
	}
	if h := e.Garbage()[firstKey]; h != 24 {
		t.Fatalf("old avatar due in %dh, want 24: %v", h, e.Garbage())
	}
	e.Sweep()
	keys := strings.Join(e.Blobs.Keys(), " ")
	if strings.Contains(keys, firstKey) || !strings.Contains(keys, strings.TrimPrefix(second, "/cdn/")) {
		t.Fatalf("after the sweep the store has %s", keys)
	}
	if len(e.Garbage()) != 0 {
		t.Fatalf("rows left: %v", e.Garbage())
	}

	// Removing the avatar.
	got := obj(u.Expect(http.StatusOK, "DELETE", avatar, nil))
	if got["avatar"] != nil {
		t.Fatalf("after DELETE: %v", got)
	}
	pub = e.Outbox("USER_PROFILE_UPDATE")
	if last := pub[len(pub)-1]; obj(last.Data["user"])["avatar"] != nil {
		t.Fatalf("last profile event %v", last)
	}
	e.Sweep()
	if len(e.Blobs.Keys()) != 0 {
		t.Fatalf("files left after removing the avatar: %v", e.Blobs.Keys())
	}
	// Removing again is not an error and tells nobody.
	n := len(pub)
	u.Expect(http.StatusOK, "DELETE", avatar, nil)
	if len(e.Outbox("USER_PROFILE_UPDATE")) != n {
		t.Fatal("removing a missing avatar told the guilds")
	}
}

// Five uploads per ten minutes, refused pictures included: decoding is the
// expensive part.
func TestAvatarUploadLimit(t *testing.T) {
	e := newEnv(t)
	u := e.User("timofey")
	u.Upload("PUT", me+"/avatar", []byte("not a picture"))
	for i, shade := range []uint8{10, 20, 30, 40} {
		if code, out := u.Upload("PUT", me+"/avatar", apitest.PNG(8, 8, color.NRGBA{shade, 0, 0, 255})); code != 200 {
			t.Fatalf("upload %d: %d %v", i, code, out)
		}
	}
	if code, _ := u.Upload("PUT", me+"/avatar", apitest.PNG(8, 8, red)); code != http.StatusTooManyRequests {
		t.Fatalf("sixth upload: %d, want 429", code)
	}
	// Removing is not an upload.
	u.Expect(http.StatusOK, "DELETE", me+"/avatar", nil)
}

// Avatars are per owner: the same picture of two users is two files, so
// one user's change never touches another's.
func TestAvatarKeysPerOwner(t *testing.T) {
	e := newEnv(t)
	a, b := e.User("ann"), e.User("bob")
	pic := apitest.PNG(32, 32, red)
	_, ao := a.Upload("PUT", me+"/avatar", pic)
	_, bo := b.Upload("PUT", me+"/avatar", pic)
	if str(obj(ao)["avatar"]) == str(obj(bo)["avatar"]) {
		t.Fatal("two owners share a key")
	}
	a.Expect(http.StatusOK, "DELETE", me+"/avatar", nil)
	e.Sweep()
	if code, _ := e.Fetch(str(obj(bo)["avatar"])); code != 200 {
		t.Fatalf("bob's avatar after ann removed hers: %d", code)
	}
}

// Users without the guild module wired still update their own profile.
func TestProfileWithoutGuilds(t *testing.T) {
	e := apitest.New(t)
	u := e.User("timofey")
	u.Expect(http.StatusOK, "PATCH", me, map[string]any{"display_name": "Тимофей"})
	if len(e.Outbox("USER_PROFILE_UPDATE")) != 0 || len(e.Outbox("USER_UPDATE")) != 1 {
		t.Fatal("events without guilds")
	}
	if bio, err := e.Auth.Bio(context.Background(), uuid.New()); err == nil || bio != "" {
		t.Fatal("Bio of an unknown user must fail")
	}
}
