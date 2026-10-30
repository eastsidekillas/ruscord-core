package auth_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/message"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
	"github.com/eastsidekillas/ruscord-core/internal/social"
)

// deletionEnv has every module a deletion touches.
type deletionEnv struct {
	*env
	social *social.Service
}

func newDeletionEnv(t *testing.T) *deletionEnv {
	t.Helper()
	e := newEnv(t)
	socials := social.NewService(e.PG, e.Auth, e.guilds, e.Limiter)
	e.guilds.OnChannelsDeleted(message.DeleteChannels)
	socials.OnChannelsDeleted(message.DeleteChannels)
	social.NewHandler(socials, e.RequireAuth).Register(e.Mux)
	message.NewHandler(message.NewService(e.PG, e.Auth, e.guilds, socials, e.Limiter, e.Eph), e.RequireAuth).Register(e.Mux)
	e.Auth.OnAccountDeleted(e.guilds, socials)
	return &deletionEnv{env: e, social: socials}
}

const rels = "/api/v1/users/@me/relationships"

func befriend(a, b *apitest.User) {
	a.Expect(http.StatusNoContent, "PUT", rels+"/"+b.ID.String(), map[string]any{})
	b.Expect(http.StatusNoContent, "PUT", rels+"/"+a.ID.String(), map[string]any{})
}

func TestDeleteAccount(t *testing.T) {
	e := newDeletionEnv(t)
	ann, bob, cat := e.User("ann"), e.User("bob"), e.User("cat")
	ctx := context.Background()

	// Bob: a member of Ann's guild with a message there, Ann's friend, in a
	// group with Ann and Cat, in a DM with Ann, owner of a guild of his own.
	g, err := e.guilds.Create(ctx, ann.ID, guild.CreateGuildInput{Name: "Сервер Ани"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.guilds.AddMember(ctx, g.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	text := g.Channels[0].ID.String()
	bob.Expect(http.StatusCreated, "POST", "/api/v1/channels/"+text+"/messages", map[string]any{"content": "привет"})
	befriend(ann, bob)
	befriend(bob, cat)
	group := str(obj(bob.Expect(http.StatusOK, "POST", "/api/v1/users/@me/channels",
		map[string]any{"recipients": []string{ann.ID.String(), cat.ID.String()}}))["id"])
	dm := str(obj(ann.Expect(http.StatusOK, "POST", "/api/v1/users/@me/channels", map[string]any{"recipient_id": bob.ID}))["id"])
	own, err := e.guilds.Create(ctx, bob.ID, guild.CreateGuildInput{Name: "Сервер Боба"})
	if err != nil {
		t.Fatal(err)
	}

	// Refusals change nothing.
	for _, c := range []struct {
		name string
		body map[string]any
		code int
	}{
		{"wrong password", map[string]any{"password": "nope"}, http.StatusForbidden},
		{"no password", map[string]any{}, http.StatusForbidden},
		{"owns a guild", map[string]any{"password": apitest.Password}, http.StatusBadRequest},
	} {
		if code, out := bob.Do("DELETE", me, c.body); code != c.code {
			t.Fatalf("%s: %d %v, want %d", c.name, code, out, c.code)
		}
	}
	bob.Expect(http.StatusOK, "GET", me, nil)
	if err := e.guilds.Delete(ctx, bob.ID, own.ID); err != nil {
		t.Fatal(err)
	}

	bob.Expect(http.StatusNoContent, "DELETE", me, map[string]any{"password": apitest.Password})

	// Bob cannot do anything, even with an access token still in date.
	bob.Expect(http.StatusUnauthorized, "GET", me, nil)
	bob.Expect(http.StatusUnauthorized, "PATCH", me, map[string]any{"bio": "я вернулся"})
	var active int
	_ = e.PG.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, bob.ID).Scan(&active)
	if active != 0 || len(e.Outbox("SESSION_REVOKE")) == 0 {
		t.Fatalf("%d sessions still active, revoke events %d", active, len(e.Outbox("SESSION_REVOKE")))
	}
	if code, _ := ann.Do("POST", rels, map[string]any{"username": "bob"}); code != http.StatusBadRequest {
		t.Fatalf("friend request to a deleted account by name: %d", code)
	}

	// Others: Bob is out of the guild, friends and the group; the DM and his
	// message stay, shown as a deleted user.
	members := list(ann.Expect(http.StatusOK, "GET", "/api/v1/guilds/"+g.ID.String()+"/members", nil))
	if len(members) != 1 {
		t.Fatalf("members after deletion: %v", members)
	}
	if rs := list(ann.Expect(http.StatusOK, "GET", rels, nil)); len(rs) != 0 {
		t.Fatalf("Ann's relationships: %v", rs)
	}
	for _, ch := range list(cat.Expect(http.StatusOK, "GET", "/api/v1/users/@me/channels", nil)) {
		if str(obj(ch)["id"]) == group && len(list(obj(ch)["recipients"])) != 2 {
			t.Fatalf("group still has Bob: %v", ch)
		}
	}
	msgs := list(ann.Expect(http.StatusOK, "GET", "/api/v1/channels/"+text+"/messages", nil))
	author := obj(obj(msgs[0])["author"])
	if obj(msgs[0])["content"] != "привет" || author["deleted"] != true || author["display_name"] != "Удалённый пользователь" ||
		author["username"] == "bob" || author["avatar"] != nil {
		t.Fatalf("message after deletion: %v", msgs[0])
	}
	for _, ch := range list(ann.Expect(http.StatusOK, "GET", "/api/v1/users/@me/channels", nil)) {
		if str(obj(ch)["id"]) != dm {
			continue
		}
		for _, r := range list(obj(ch)["recipients"]) {
			if str(obj(r)["id"]) == bob.ID.String() && obj(r)["deleted"] != true {
				t.Fatalf("DM recipient not shown as deleted: %v", r)
			}
		}
	}
	p := obj(ann.Expect(http.StatusOK, "GET", "/api/v1/users/"+bob.ID.String()+"/profile", nil))
	if p["bio"] != "" || obj(p["user"])["deleted"] != true {
		t.Fatalf("profile of a deleted account: %v", p)
	}

	// The account cannot sign in again; its email stays taken.
	if c := e.login(bob); c != http.StatusUnauthorized {
		t.Fatalf("sign-in after deletion: %d", c)
	}

	// The sweeper's second pass removes what an unexpired token joined meanwhile.
	if _, err := e.guilds.AddMember(ctx, g.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := e.Auth.SweepDeletedAccounts(ctx); err != nil || n != 0 {
		t.Fatalf("early sweep: %d %v", n, err)
	}
	if _, err := e.PG.Exec(ctx, `UPDATE users SET deleted_at = deleted_at - interval '1 hour', cleaned_at = cleaned_at - interval '1 hour' WHERE id = $1`, bob.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := e.Auth.SweepDeletedAccounts(ctx); err != nil || n != 1 {
		t.Fatalf("sweep after the tokens expired: %d %v", n, err)
	}
	if n := len(list(ann.Expect(http.StatusOK, "GET", "/api/v1/guilds/"+g.ID.String()+"/members", nil))); n != 1 {
		t.Fatalf("%d members after the second pass", n)
	}
	if n, _ := e.Auth.SweepDeletedAccounts(ctx); n != 0 {
		t.Fatal("a finished account was swept again")
	}
}

// login tries the account's password and reports the status.
func (e *deletionEnv) login(u *apitest.User) int {
	e.T.Helper()
	anon := e.User("anon_" + u.ID.String()[:8])
	code, _ := anon.Do("POST", "/api/v1/auth/login", map[string]any{"email": e.emailOf(u), "password": apitest.Password})
	return code
}

func (e *deletionEnv) emailOf(u *apitest.User) string {
	var email string
	_ = e.PG.QueryRow(context.Background(), `SELECT email FROM users WHERE id = $1`, u.ID).Scan(&email)
	return email
}
