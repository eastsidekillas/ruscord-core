package guild

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	platformstore "github.com/eastsidekillas/ruscord-core/internal/platform/store"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
)

// Integration tests need a real Postgres: set RUSCORD_TEST_POSTGRES_URL.
// Each test gets its own schema, so tests can run in parallel.

type env struct {
	t      *testing.T
	srv    *httptest.Server
	svc    *Service
	pg     *pgxpool.Pool
	signer *auth.Signer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	url := os.Getenv("RUSCORD_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("RUSCORD_TEST_POSTGRES_URL not set")
	}
	t.Parallel()
	ctx := context.Background()
	schema := "t_" + strings.ReplaceAll(uuid.NewString(), "-", "")

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS citext"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pg, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	if err := platformstore.Migrate(ctx, pg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	signer, _ := auth.NewSigner(make([]byte, 32))
	authSvc, err := auth.NewService(ctx, auth.Deps{
		PG: pg, Signer: signer, Limiter: ratelimit.New(rdb), Tickets: auth.NewTickets(rdb),
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	authHTTP := auth.NewHandler(authSvc, false)
	authHTTP.Register(mux)
	svc := NewService(pg, authSvc)
	NewHandler(svc, authHTTP.RequireAuth).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, svc: svc, pg: pg, signer: signer}
}

type user struct {
	e     *env
	id    uuid.UUID
	token string
}

// user inserts an account directly; auth itself is covered in internal/auth.
func (e *env) user(name string) *user {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	if _, err := e.pg.Exec(context.Background(),
		`INSERT INTO users (id, email, username, display_name, password_hash) VALUES ($1, $2, $3::text, $3::text, 'x')`,
		id, name+"@example.com", name); err != nil {
		e.t.Fatal(err)
	}
	tok, err := e.signer.Issue(auth.Principal{UserID: id, SessionID: uuid.New()}, time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	return &user{e: e, id: id, token: tok}
}

// do sends a request and decodes the JSON answer into a generic value.
func (u *user) do(method, path string, body any) (int, any) {
	t := u.e.t
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	}
	req, _ := http.NewRequestWithContext(t.Context(), method, u.e.srv.URL+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "rc_at", Value: u.token}) //nolint:gosec // request cookie in a test client
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (u *user) expect(want int, method, path string, body any) any {
	u.e.t.Helper()
	got, out := u.do(method, path, body)
	if got != want {
		u.e.t.Fatalf("%s %s: status %d, want %d: %v", method, path, got, want, out)
	}
	return out
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func list(v any) []any         { l, _ := v.([]any); return l }
func str(v any) string         { s, _ := v.(string); return s }

func permStr(p perm.Permissions) string { return strconv.FormatUint(uint64(p), 10) }

func (e *env) outboxTypes() []string {
	e.t.Helper()
	rows, err := e.pg.Query(context.Background(), `SELECT type FROM outbox ORDER BY id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func (e *env) join(guildID string, u *user) {
	e.t.Helper()
	if _, err := e.svc.AddMember(context.Background(), uuid.MustParse(guildID), u.id); err != nil {
		e.t.Fatal(err)
	}
}

func createGuild(u *user) (id, text, voice string) {
	g := obj(u.expect(http.StatusCreated, "POST", "/api/v1/guilds", map[string]any{"name": "Тестовый"}))
	chs := list(g["channels"])
	if len(chs) != 2 {
		u.e.t.Fatalf("new guild has %d channels, want 2", len(chs))
	}
	return str(g["id"]), str(obj(chs[0])["id"]), str(obj(chs[1])["id"])
}

func TestGuildLifecycle(t *testing.T) {
	e := newEnv(t)
	owner, bob, eve := e.user("owner"), e.user("bob"), e.user("eve")
	gid, _, _ := createGuild(owner)
	g := "/api/v1/guilds/" + gid

	full := obj(owner.expect(http.StatusOK, "GET", g, nil))
	if full["permissions"] != permStr(perm.All) {
		t.Fatalf("owner permissions = %v", full["permissions"])
	}
	if roles := list(full["roles"]); len(roles) != 1 || str(obj(roles[0])["id"]) != gid {
		t.Fatalf("roles = %v, want only @everyone with the guild id", roles)
	}
	if mine := list(owner.expect(http.StatusOK, "GET", "/api/v1/users/@me/guilds", nil)); len(mine) != 1 {
		t.Fatalf("my guilds = %v", mine)
	}

	// A non-member cannot tell the guild exists.
	for _, c := range []struct{ method, path string }{
		{"GET", g}, {"PATCH", g}, {"DELETE", g}, {"GET", g + "/channels"}, {"GET", g + "/members"},
		{"GET", g + "/roles"}, {"POST", g + "/roles"}, {"DELETE", g + "/members/" + owner.id.String()},
		{"GET", "/api/v1/guilds/" + uuid.NewString()},
	} {
		body := map[string]any{}
		if c.method == "PATCH" {
			body["name"] = "pwned"
		}
		if c.method == "POST" {
			body["name"] = "x"
		}
		eve.expect(http.StatusNotFound, c.method, c.path, body)
	}

	e.join(gid, bob)
	full = obj(bob.expect(http.StatusOK, "GET", g, nil))
	if full["permissions"] != permStr(perm.DefaultEveryone) {
		t.Fatalf("member permissions = %v", full["permissions"])
	}
	bob.expect(http.StatusForbidden, "PATCH", g, map[string]any{"name": "pwned"})
	bob.expect(http.StatusForbidden, "DELETE", g, nil)
	owner.expect(http.StatusOK, "PATCH", g, map[string]any{"name": "Новое имя"})

	ms := list(owner.expect(http.StatusOK, "GET", g+"/members?limit=1", nil))
	if len(ms) != 1 || obj(obj(ms[0])["user"])["username"] == nil {
		t.Fatalf("members page = %v", ms)
	}
	rest := list(owner.expect(http.StatusOK, "GET", g+"/members?after="+str(obj(ms[0])["user_id"]), nil))
	if len(rest) != 1 {
		t.Fatalf("second page = %v", rest)
	}

	bob.expect(http.StatusNoContent, "PATCH", g+"/members/@me", map[string]any{"nick": "Бобби"})
	owner.expect(http.StatusBadRequest, "DELETE", g+"/members/@me", nil)
	bob.expect(http.StatusNoContent, "DELETE", g+"/members/@me", nil)
	bob.expect(http.StatusNotFound, "GET", g, nil)
	if _, err := e.svc.AddMember(t.Context(), uuid.MustParse(gid), owner.id); err != errAlreadyMember { //nolint:errorlint // sentinel
		t.Fatalf("re-adding owner: %v", err)
	}

	owner.expect(http.StatusNoContent, "DELETE", g, nil)
	owner.expect(http.StatusNotFound, "GET", g, nil)

	want := []string{"GUILD_CREATE", "GUILD_MEMBER_ADD", "GUILD_CREATE", "GUILD_UPDATE",
		"GUILD_MEMBER_UPDATE", "GUILD_MEMBER_REMOVE", "GUILD_DELETE", "GUILD_DELETE"}
	if got := e.outboxTypes(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("outbox = %v\nwant     %v", got, want)
	}
}

func TestChannelVisibility(t *testing.T) {
	e := newEnv(t)
	owner, bob := e.user("owner"), e.user("bob")
	gid, text, _ := createGuild(owner)
	e.join(gid, bob)
	g := "/api/v1/guilds/" + gid

	secret := str(obj(owner.expect(http.StatusCreated, "POST", g+"/channels",
		map[string]any{"type": 0, "name": "admins"}))["id"])
	owner.expect(http.StatusOK, "PUT", "/api/v1/channels/"+secret+"/permissions/"+gid,
		map[string]any{"type": 1, "deny": permStr(perm.ViewChannel)})

	if n := len(list(bob.expect(http.StatusOK, "GET", g+"/channels", nil))); n != 2 {
		t.Fatalf("bob sees %d channels, want 2", n)
	}
	if n := len(list(owner.expect(http.StatusOK, "GET", g+"/channels", nil))); n != 3 {
		t.Fatalf("owner sees %d channels, want 3", n)
	}
	// Hidden channel: 404, visible one without the right: 403.
	bob.expect(http.StatusNotFound, "PATCH", "/api/v1/channels/"+secret, map[string]any{"name": "x"})
	bob.expect(http.StatusNotFound, "DELETE", "/api/v1/channels/"+secret, nil)
	bob.expect(http.StatusForbidden, "PATCH", "/api/v1/channels/"+text, map[string]any{"name": "x"})
	bob.expect(http.StatusForbidden, "POST", g+"/channels", map[string]any{"type": 0, "name": "x"})

	// A member overwrite opens the channel for bob alone.
	owner.expect(http.StatusOK, "PUT", "/api/v1/channels/"+secret+"/permissions/"+bob.id.String(),
		map[string]any{"type": 2, "allow": permStr(perm.ViewChannel | perm.ManageChannels)})
	ch := obj(bob.expect(http.StatusOK, "PATCH", "/api/v1/channels/"+secret, map[string]any{"topic": "только для своих"}))
	if ch["topic"] != "только для своих" {
		t.Fatalf("topic = %v", ch["topic"])
	}

	// Categories: a channel can move under one and back out.
	cat := str(obj(owner.expect(http.StatusCreated, "POST", g+"/channels", map[string]any{"type": 4, "name": "Голос"}))["id"])
	ch = obj(owner.expect(http.StatusOK, "PATCH", "/api/v1/channels/"+text, map[string]any{"parent_id": cat}))
	if ch["parent_id"] != cat {
		t.Fatalf("parent_id = %v", ch["parent_id"])
	}
	ch = obj(owner.expect(http.StatusOK, "PATCH", "/api/v1/channels/"+text, map[string]any{"parent_id": nil}))
	if _, ok := ch["parent_id"]; ok {
		t.Fatalf("parent_id not cleared: %v", ch["parent_id"])
	}
	owner.expect(http.StatusUnprocessableEntity, "PATCH", "/api/v1/channels/"+text, map[string]any{"parent_id": text})
	owner.expect(http.StatusUnprocessableEntity, "PATCH", "/api/v1/channels/"+text, map[string]any{"user_limit": 5})

	owner.expect(http.StatusNoContent, "DELETE", "/api/v1/channels/"+secret+"/permissions/"+bob.id.String(), nil)
	bob.expect(http.StatusNotFound, "PATCH", "/api/v1/channels/"+secret, map[string]any{"name": "x"})
	owner.expect(http.StatusNoContent, "DELETE", "/api/v1/channels/"+secret, nil)
}

func TestOverwriteCannotEscalate(t *testing.T) {
	e := newEnv(t)
	owner, mod := e.user("owner"), e.user("mod")
	gid, text, _ := createGuild(owner)
	e.join(gid, mod)
	g := "/api/v1/guilds/" + gid
	role := str(obj(owner.expect(http.StatusCreated, "POST", g+"/roles",
		map[string]any{"name": "mod", "permissions": permStr(perm.ManageRoles)}))["id"])
	owner.expect(http.StatusNoContent, "PUT", g+"/members/"+mod.id.String()+"/roles/"+role, nil)

	ow := "/api/v1/channels/" + text + "/permissions/"
	// The mod may not grant what they lack, not even to themself.
	mod.expect(http.StatusForbidden, "PUT", ow+mod.id.String(), map[string]any{"type": 2, "allow": permStr(perm.ManageChannels)})
	mod.expect(http.StatusForbidden, "PUT", ow+gid, map[string]any{"type": 1, "deny": permStr(perm.Administrator)})
	mod.expect(http.StatusOK, "PUT", ow+gid, map[string]any{"type": 1, "deny": permStr(perm.SendMessages)})

	// An overwrite set by the owner with bits the mod lacks cannot be removed by the mod.
	// (An overwrite on the mod's own role would hand the bit to the mod, so use another member.)
	o := ow + owner.id.String()
	owner.expect(http.StatusOK, "PUT", o, map[string]any{"type": 2, "allow": permStr(perm.ManageMessages)})
	mod.expect(http.StatusForbidden, "DELETE", o, nil)
	mod.expect(http.StatusForbidden, "PUT", o, map[string]any{"type": 2})

	// Unknown targets and malformed input.
	mod.expect(http.StatusUnprocessableEntity, "PUT", ow+uuid.NewString(), map[string]any{"type": 2})
	mod.expect(http.StatusUnprocessableEntity, "PUT", ow+gid, map[string]any{"type": 3})
	mod.expect(http.StatusUnprocessableEntity, "PUT", ow+gid,
		map[string]any{"type": 1, "allow": permStr(perm.SendMessages), "deny": permStr(perm.SendMessages)})
	mod.expect(http.StatusUnprocessableEntity, "PUT", ow+gid, map[string]any{"type": 1, "allow": permStr(1 << 40)})
	mod.expect(http.StatusBadRequest, "PUT", ow+gid, map[string]any{"type": 1, "allow": 1})
}

func TestRoleHierarchy(t *testing.T) {
	e := newEnv(t)
	owner, bob, carol := e.user("owner"), e.user("bob"), e.user("carol")
	gid, _, _ := createGuild(owner)
	e.join(gid, bob)
	e.join(gid, carol)
	g := "/api/v1/guilds/" + gid
	modPerms := perm.ManageRoles | perm.KickMembers

	mod := str(obj(owner.expect(http.StatusCreated, "POST", g+"/roles", map[string]any{"name": "mod", "permissions": permStr(modPerms)}))["id"])
	low := str(obj(owner.expect(http.StatusCreated, "POST", g+"/roles", map[string]any{"name": "low"}))["id"])
	owner.expect(http.StatusNoContent, "PUT", g+"/members/"+bob.id.String()+"/roles/"+mod, nil)

	positions := func() map[string]int {
		out := map[string]int{}
		seen := map[int]bool{}
		roles := list(owner.expect(http.StatusOK, "GET", g+"/roles", nil))
		for _, r := range roles {
			p := int(obj(r)["position"].(float64))
			if seen[p] {
				t.Fatalf("duplicate position %d: %v", p, roles)
			}
			seen[p] = true
			out[str(obj(r)["name"])] = p
		}
		for i := range len(roles) {
			if !seen[i] {
				t.Fatalf("positions not contiguous: %v", roles)
			}
		}
		return out
	}
	if p := positions(); p["mod"] != 2 || p["low"] != 1 {
		t.Fatalf("positions = %v", p)
	}

	member := func(u *user) string { return g + "/members/" + u.id.String() }
	bob.expect(http.StatusNoContent, "PUT", member(carol)+"/roles/"+low, nil)
	bob.expect(http.StatusForbidden, "PUT", member(carol)+"/roles/"+mod, nil)  // not below bob's top role
	bob.expect(http.StatusForbidden, "PUT", member(owner)+"/roles/"+low, nil)  // nobody outranks the owner
	bob.expect(http.StatusBadRequest, "PUT", member(carol)+"/roles/"+gid, nil) // @everyone is implied
	bob.expect(http.StatusNotFound, "PUT", member(carol)+"/roles/"+uuid.NewString(), nil)
	bob.expect(http.StatusNoContent, "DELETE", member(carol)+"/roles/"+low, nil)

	bob.expect(http.StatusForbidden, "POST", g+"/roles", map[string]any{"name": "x", "permissions": permStr(perm.ManageGuild)})
	bob.expect(http.StatusCreated, "POST", g+"/roles", map[string]any{"name": "new", "permissions": permStr(perm.KickMembers)})
	if p := positions(); p["mod"] != 3 || p["low"] != 2 || p["new"] != 1 {
		t.Fatalf("positions after create = %v", p)
	}
	bob.expect(http.StatusForbidden, "PATCH", g+"/roles/"+mod, map[string]any{"name": "boss"})
	bob.expect(http.StatusForbidden, "PATCH", g+"/roles/"+low, map[string]any{"position": 3})
	bob.expect(http.StatusForbidden, "PATCH", g+"/roles/"+low, map[string]any{"permissions": permStr(perm.Administrator)})
	bob.expect(http.StatusForbidden, "PATCH", g+"/roles/"+gid, map[string]any{"permissions": permStr(perm.DefaultEveryone | perm.ManageGuild)})
	bob.expect(http.StatusBadRequest, "DELETE", g+"/roles/"+gid, nil)
	bob.expect(http.StatusUnprocessableEntity, "PATCH", g+"/roles/"+low, map[string]any{"position": 0})
	bob.expect(http.StatusOK, "PATCH", g+"/roles/"+low, map[string]any{"position": 1})
	if p := positions(); p["mod"] != 3 || p["low"] != 1 || p["new"] != 2 {
		t.Fatalf("positions after move = %v", p)
	}
	owner.expect(http.StatusOK, "PATCH", g+"/roles/"+low, map[string]any{"position": 3})
	if p := positions(); p["low"] != 3 || p["mod"] != 2 || p["new"] != 1 {
		t.Fatalf("positions after owner move = %v", p)
	}
	// Now "low" outranks bob's role: he can no longer touch it.
	bob.expect(http.StatusForbidden, "DELETE", g+"/roles/"+low, nil)
	owner.expect(http.StatusNoContent, "DELETE", g+"/roles/"+low, nil)
	if p := positions(); p["mod"] != 2 || p["new"] != 1 {
		t.Fatalf("positions after delete = %v", p)
	}

	// Kicks follow the hierarchy too.
	carol.expect(http.StatusForbidden, "DELETE", member(bob), nil)
	bob.expect(http.StatusForbidden, "DELETE", member(owner), nil)
	bob.expect(http.StatusNoContent, "DELETE", member(carol), nil)
	carol.expect(http.StatusNotFound, "GET", g, nil)
	bob.expect(http.StatusNotFound, "DELETE", member(carol), nil)
}

func TestValidationAndNoEventsOnFailure(t *testing.T) {
	e := newEnv(t)
	owner, bob := e.user("owner"), e.user("bob")
	gid, text, _ := createGuild(owner)
	e.join(gid, bob)
	g := "/api/v1/guilds/" + gid
	before := len(e.outboxTypes())

	owner.expect(http.StatusUnprocessableEntity, "POST", "/api/v1/guilds", map[string]any{"name": " x "})
	owner.expect(http.StatusBadRequest, "POST", "/api/v1/guilds", map[string]any{"name": "ok", "owner_id": bob.id})
	owner.expect(http.StatusUnprocessableEntity, "POST", g+"/channels", map[string]any{"type": 1, "name": "dm"})
	owner.expect(http.StatusUnprocessableEntity, "POST", g+"/channels", map[string]any{"type": 0, "name": "t", "bitrate": 96000})
	owner.expect(http.StatusUnprocessableEntity, "POST", g+"/channels", map[string]any{"type": 2, "name": "v", "user_limit": 100})
	owner.expect(http.StatusUnprocessableEntity, "POST", g+"/channels", map[string]any{"type": 0, "name": "t", "parent_id": text})
	owner.expect(http.StatusUnprocessableEntity, "POST", g+"/roles", map[string]any{"permissions": "0"})
	owner.expect(http.StatusUnprocessableEntity, "PATCH", g+"/members/@me", map[string]any{"nick": strings.Repeat("я", 33)})
	owner.expect(http.StatusNotFound, "GET", "/api/v1/guilds/not-a-uuid", nil)
	owner.expect(http.StatusBadRequest, "GET", g+"/members?limit=0", nil)
	bob.expect(http.StatusForbidden, "DELETE", "/api/v1/channels/"+text, nil)
	bob.expect(http.StatusForbidden, "POST", g+"/roles", map[string]any{"name": "x"})

	if after := len(e.outboxTypes()); after != before {
		t.Fatalf("failed requests wrote %d events", after-before)
	}

	v := obj(owner.expect(http.StatusCreated, "POST", g+"/channels", map[string]any{"type": 2, "name": "Игры", "user_limit": 5}))
	if v["bitrate"] != float64(64000) || v["user_limit"] != float64(5) {
		t.Fatalf("voice channel = %v", v)
	}
	if got := e.outboxTypes(); got[len(got)-1] != "CHANNEL_CREATE" {
		t.Fatalf("last event = %v", got[len(got)-1])
	}
}

// The router must accept every pattern without conflicts.
func TestRoutesRegister(t *testing.T) {
	NewHandler(&Service{}, func(h http.Handler) http.Handler { return h }).Register(http.NewServeMux())
}
