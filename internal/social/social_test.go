package social

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
)

type env struct {
	*apitest.Env
	svc    *Service
	guilds *guild.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := apitest.New(t)
	guilds := guild.NewService(e.PG, e.Auth, e.Limiter)
	svc := NewService(e.PG, e.Auth, guilds, e.Limiter)
	NewHandler(svc, e.RequireAuth).Register(e.Mux)
	return &env{Env: e, svc: svc, guilds: guilds}
}

var (
	obj  = apitest.Obj
	list = apitest.List
	str  = apitest.Str
)

const rels = "/api/v1/users/@me/relationships"
const dms = "/api/v1/users/@me/channels"

// relOf returns the caller's relationship type with other, 0 when none.
func relOf(u *apitest.User, other *apitest.User) float64 {
	for _, r := range list(u.Expect(http.StatusOK, "GET", rels, nil)) {
		if str(obj(r)["id"]) == other.ID.String() {
			return obj(r)["type"].(float64)
		}
	}
	return 0
}

func befriend(a, b *apitest.User) {
	a.Expect(http.StatusNoContent, "PUT", rels+"/"+b.ID.String(), map[string]any{})
	b.Expect(http.StatusNoContent, "PUT", rels+"/"+a.ID.String(), map[string]any{})
}

func TestFriendRequests(t *testing.T) {
	e := newEnv(t)
	ann, bob, eve := e.User("ann"), e.User("bob"), e.User("eve")

	ann.Expect(http.StatusNoContent, "POST", rels, map[string]any{"username": "BOB"})
	if relOf(ann, bob) != 4 || relOf(bob, ann) != 3 {
		t.Fatalf("after request: ann %v, bob %v", relOf(ann, bob), relOf(bob, ann))
	}
	// Repeating the request changes nothing; accepting makes friends on both sides.
	ann.Expect(http.StatusNoContent, "PUT", rels+"/"+bob.ID.String(), map[string]any{})
	bob.Expect(http.StatusNoContent, "PUT", rels+"/"+ann.ID.String(), map[string]any{"type": 1})
	if relOf(ann, bob) != 1 || relOf(bob, ann) != 1 {
		t.Fatal("not friends after accept")
	}
	r := list(ann.Expect(http.StatusOK, "GET", rels, nil))
	if obj(obj(r[0])["user"])["username"] != "bob" {
		t.Fatalf("relationship user = %v", r[0])
	}

	// Unfriending removes both sides.
	bob.Expect(http.StatusNoContent, "DELETE", rels+"/"+ann.ID.String(), nil)
	if relOf(ann, bob) != 0 || relOf(bob, ann) != 0 {
		t.Fatal("friendship survived DELETE")
	}
	bob.Expect(http.StatusNotFound, "DELETE", rels+"/"+ann.ID.String(), nil)

	// Declining an incoming request clears the sender's outgoing one too.
	eve.Expect(http.StatusNoContent, "PUT", rels+"/"+ann.ID.String(), map[string]any{})
	ann.Expect(http.StatusNoContent, "DELETE", rels+"/"+eve.ID.String(), nil)
	if relOf(eve, ann) != 0 {
		t.Fatal("declined request still pending for sender")
	}

	ann.Expect(http.StatusBadRequest, "PUT", rels+"/"+ann.ID.String(), map[string]any{})
	ann.Expect(http.StatusBadRequest, "POST", rels, map[string]any{"username": "nobody"})
	ann.Expect(http.StatusUnprocessableEntity, "PUT", rels+"/"+bob.ID.String(), map[string]any{"type": 3})
	ann.Expect(http.StatusNotFound, "PUT", rels+"/not-a-uuid", map[string]any{})

	want := "RELATIONSHIP_ADD,RELATIONSHIP_ADD,RELATIONSHIP_ADD,RELATIONSHIP_ADD,RELATIONSHIP_REMOVE,RELATIONSHIP_REMOVE," +
		"RELATIONSHIP_ADD,RELATIONSHIP_ADD,RELATIONSHIP_REMOVE,RELATIONSHIP_REMOVE"
	if got := strings.Join(e.OutboxTypes(), ","); got != want {
		t.Fatalf("outbox = %s", got)
	}
}

func TestCrossingRequestsBecomeFriends(t *testing.T) {
	e := newEnv(t)
	ann, bob := e.User("ann"), e.User("bob")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.svc.Request(context.Background(), bob.ID, ann.ID)
	}()
	_ = e.svc.Request(context.Background(), ann.ID, bob.ID)
	<-done
	if relOf(ann, bob) != 1 || relOf(bob, ann) != 1 {
		t.Fatalf("crossing requests: ann %v, bob %v", relOf(ann, bob), relOf(bob, ann))
	}
}

func TestBlockIsInvisible(t *testing.T) {
	e := newEnv(t)
	ann, bob := e.User("ann"), e.User("bob")
	befriend(ann, bob)

	ann.Expect(http.StatusNoContent, "PUT", rels+"/"+bob.ID.String(), map[string]any{"type": 2})
	if relOf(ann, bob) != 2 || relOf(bob, ann) != 0 {
		t.Fatalf("after block: ann %v, bob %v", relOf(ann, bob), relOf(bob, ann))
	}
	// Bob cannot tell a block from any other failure.
	_, body := bob.Do("PUT", rels+"/"+ann.ID.String(), map[string]any{})
	if obj(body)["code"] != "request_failed" {
		t.Fatalf("request to blocker = %v", body)
	}
	_, unknown := bob.Do("POST", rels, map[string]any{"username": "ghost"})
	if obj(unknown)["code"] != obj(body)["code"] || obj(unknown)["message"] != obj(body)["message"] {
		t.Fatalf("blocked %v and unknown %v answers differ", body, unknown)
	}
	ann.Expect(http.StatusBadRequest, "PUT", rels+"/"+bob.ID.String(), map[string]any{}) // unblock first

	// Both can block each other; unblocking is one-sided.
	bob.Expect(http.StatusNoContent, "PUT", rels+"/"+ann.ID.String(), map[string]any{"type": 2})
	ann.Expect(http.StatusNoContent, "DELETE", rels+"/"+bob.ID.String(), nil)
	if relOf(ann, bob) != 0 || relOf(bob, ann) != 2 {
		t.Fatalf("after ann unblocks: ann %v, bob %v", relOf(ann, bob), relOf(bob, ann))
	}
}

func TestDirectMessages(t *testing.T) {
	e := newEnv(t)
	ann, bob, cat, dan := e.User("ann"), e.User("bob"), e.User("cat"), e.User("dan")
	befriend(ann, bob)

	d := obj(ann.Expect(http.StatusOK, "POST", dms, map[string]any{"recipient_id": bob.ID}))
	if d["type"] != float64(1) || len(list(d["recipients"])) != 2 {
		t.Fatalf("dm = %v", d)
	}
	again := obj(bob.Expect(http.StatusOK, "POST", dms, map[string]any{"recipient_id": ann.ID}))
	if again["id"] != d["id"] {
		t.Fatalf("second open made a new DM: %v vs %v", again["id"], d["id"])
	}
	if n := len(list(bob.Expect(http.StatusOK, "GET", dms, nil))); n != 1 {
		t.Fatalf("bob has %d DMs", n)
	}

	// Strangers cannot DM; a shared guild allows it.
	ann.Expect(http.StatusForbidden, "POST", dms, map[string]any{"recipient_id": cat.ID})
	g, err := e.guilds.Create(t.Context(), ann.ID, guild.CreateGuildInput{Name: "общий"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.guilds.AddMember(t.Context(), g.ID, cat.ID); err != nil {
		t.Fatal(err)
	}
	cat.Expect(http.StatusOK, "POST", dms, map[string]any{"recipient_id": ann.ID})

	// A block stops new DMs both ways and turns an existing one read-only.
	cat.Expect(http.StatusNoContent, "PUT", rels+"/"+ann.ID.String(), map[string]any{"type": 2})
	_, body := ann.Do("POST", dms, map[string]any{"recipient_id": dan.ID})
	_, blocked := ann.Do("POST", dms, map[string]any{"recipient_id": cat.ID})
	if obj(blocked)["code"] != "cannot_dm" || obj(blocked)["message"] != obj(body)["message"] {
		t.Fatalf("blocked %v vs stranger %v", blocked, body)
	}
	dmID := uuid.MustParse(str(d["id"]))
	if p, _, err := e.svc.Permissions(t.Context(), ann.ID, dmID); err != nil || p != perm.InDM(true, false) {
		t.Fatalf("ann in DM with bob: %v %v", p, err)
	}
	var catDM uuid.UUID
	for _, x := range list(cat.Expect(http.StatusOK, "GET", dms, nil)) {
		catDM = uuid.MustParse(str(obj(x)["id"]))
	}
	if p, _, _ := e.svc.Permissions(t.Context(), ann.ID, catDM); p.Has(perm.SendMessages) || !p.Has(perm.ReadMessageHistory) {
		t.Fatalf("blocked DM permissions = %v", p)
	}
	if _, _, err := e.svc.Permissions(t.Context(), dan.ID, dmID); err == nil {
		t.Fatal("non-recipient got DM permissions")
	}
	ann.Expect(http.StatusForbidden, "POST", dms, map[string]any{"recipient_id": ann.ID})
}

func TestGroupDMs(t *testing.T) {
	e := newEnv(t)
	ann, bob, cat, dan := e.User("ann"), e.User("bob"), e.User("cat"), e.User("dan")
	befriend(ann, bob)
	befriend(ann, cat)
	befriend(bob, dan)

	ann.Expect(http.StatusBadRequest, "POST", dms, map[string]any{"recipients": []uuid.UUID{bob.ID, dan.ID}}) // dan is not ann's friend
	ann.Expect(http.StatusUnprocessableEntity, "POST", dms, map[string]any{"recipients": []uuid.UUID{}})
	ann.Expect(http.StatusUnprocessableEntity, "POST", dms, map[string]any{"recipient_id": bob.ID, "recipients": []uuid.UUID{cat.ID}})

	g := obj(ann.Expect(http.StatusOK, "POST", dms, map[string]any{"recipients": []uuid.UUID{bob.ID, bob.ID}, "name": " Отряд "}))
	id := str(g["id"])
	if g["type"] != float64(3) || g["name"] != "Отряд" || g["owner_id"] != ann.ID.String() || len(list(g["recipients"])) != 2 {
		t.Fatalf("group = %v", g)
	}
	rec := "/api/v1/channels/" + id + "/recipients/"
	bob.Expect(http.StatusNoContent, "PUT", rec+dan.ID.String(), nil) // bob's friend
	ann.Expect(http.StatusConflict, "PUT", rec+dan.ID.String(), nil)
	cat.Expect(http.StatusNotFound, "PUT", rec+cat.ID.String(), nil) // not in the group
	bob.Expect(http.StatusForbidden, "DELETE", rec+dan.ID.String(), nil)
	ann.Expect(http.StatusNoContent, "DELETE", rec+dan.ID.String(), nil)
	dan.Expect(http.StatusNotFound, "PATCH", dms+"/"+id, map[string]any{"name": "x"})

	r := obj(bob.Expect(http.StatusOK, "PATCH", dms+"/"+id, map[string]any{"name": ""}))
	if _, ok := r["name"]; ok {
		t.Fatalf("name not cleared: %v", r)
	}

	// The owner leaves: bob, the longest-standing member, takes over.
	ann.Expect(http.StatusNoContent, "DELETE", dms+"/"+id, nil)
	l := list(bob.Expect(http.StatusOK, "GET", dms, nil))
	if len(l) != 1 || obj(l[0])["owner_id"] != bob.ID.String() {
		t.Fatalf("after owner left: %v", l)
	}
	bob.Expect(http.StatusNoContent, "DELETE", dms+"/"+id, nil)
	if n := len(list(bob.Expect(http.StatusOK, "GET", dms, nil))); n != 0 {
		t.Fatalf("empty group still listed: %d", n)
	}

	// 1:1 DMs have no recipients to manage.
	d := str(obj(ann.Expect(http.StatusOK, "POST", dms, map[string]any{"recipient_id": bob.ID}))["id"])
	ann.Expect(http.StatusBadRequest, "PUT", "/api/v1/channels/"+d+"/recipients/"+cat.ID.String(), nil)
	ann.Expect(http.StatusBadRequest, "DELETE", dms+"/"+d, nil)

	// Group size cap: owner plus nine.
	var nine []uuid.UUID
	for i := range 9 {
		u := e.User("f" + string(rune('a'+i)))
		befriend(ann, u)
		nine = append(nine, u.ID)
	}
	big := str(obj(ann.Expect(http.StatusOK, "POST", dms, map[string]any{"recipients": nine}))["id"])
	ann.Expect(http.StatusBadRequest, "PUT", "/api/v1/channels/"+big+"/recipients/"+cat.ID.String(), nil)
	ann.Expect(http.StatusUnprocessableEntity, "POST", dms, map[string]any{"recipients": append(nine, cat.ID)})
}
