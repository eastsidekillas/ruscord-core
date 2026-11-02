package voice

import (
	"net/http"
	"slices"
	"testing"

	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
)

const (
	rels = "/api/v1/users/@me/relationships"
	dms  = "/api/v1/users/@me/channels"
)

func befriend(a, b *apitest.User) {
	a.Expect(http.StatusNoContent, "PUT", rels+"/"+b.ID.String(), map[string]any{})
	b.Expect(http.StatusNoContent, "PUT", rels+"/"+a.ID.String(), map[string]any{})
}

// callOfDM returns the call shown in u's DM list for channel dm, nil when none.
func callOfDM(u *apitest.User, dm string) map[string]any {
	for _, d := range apitest.List(u.Expect(http.StatusOK, "GET", dms, nil)) {
		if apitest.Str(apitest.Obj(d)["id"]) == dm {
			return apitest.Obj(apitest.Obj(d)["call"])
		}
	}
	return nil
}

func ids(v any) []string {
	var out []string
	for _, x := range apitest.List(v) {
		out = append(out, apitest.Str(x))
	}
	return out
}

// A call in a 1:1 DM (docs/adr/0032-dm-calls.md): starting rings the other,
// declining stops it, LiveKit's webhooks make members, the last one leaving
// ends the call and closes its room; strangers are removed, a block forbids.
func TestDMCall(t *testing.T) {
	e := newEnv(t)
	ann, bob, eve := e.User("ann"), e.User("bob"), e.User("eve")
	befriend(ann, bob)
	dm := apitest.Str(apitest.Obj(ann.Expect(http.StatusOK, "POST", dms, map[string]any{"recipient_id": bob.ID}))["id"])
	path := "/api/v1/channels/" + dm + "/call"

	eve.Expect(http.StatusNotFound, "POST", path, nil)
	info := apitest.Obj(ann.Expect(http.StatusOK, "POST", path, nil))
	call := apitest.Obj(info["call"])
	room := apitest.Str(info["room"])
	if room != "call:"+apitest.Str(call["id"]) || !slices.Equal(ids(call["ringing"]), []string{bob.ID.String()}) {
		t.Fatalf("started call %v in room %q", call, room)
	}
	cl := parse(t, apitest.Str(info["token"]), testSecret, e.lk.now())
	if cl.Video.Room != room || !slices.Contains(cl.Video.CanPublishSources, "microphone") || !slices.Contains(cl.Video.CanPublishSources, "camera") {
		t.Fatalf("call token %+v", cl.Video)
	}
	if evs := e.Outbox(events.CallCreate); len(evs) != 1 || evs[0].Topic != "channel."+dm {
		t.Fatalf("CALL_CREATE %+v", evs)
	}

	// Bob declines: the ringing stops, the call stays.
	bob.Expect(http.StatusNoContent, "POST", path+"/decline", nil)
	if c := callOfDM(ann, dm); c == nil || len(ids(c["ringing"])) != 0 {
		t.Fatalf("after decline %v", c)
	}

	// Ann connects; Bob joins after all with a token for the same room.
	e.enter(ann, room)
	again := apitest.Obj(bob.Expect(http.StatusOK, "POST", path, nil))
	if apitest.Str(again["room"]) != room {
		t.Fatalf("second join went to %v, want %s", again["room"], room)
	}
	e.enter(bob, room)
	if c := callOfDM(bob, dm); !slices.Equal(ids(c["members"]), []string{ann.ID.String(), bob.ID.String()}) {
		t.Fatalf("members %v", c["members"])
	}

	// Someone who is not in the DM is removed from the room.
	e.enter(eve, room)
	if e.permOf(room, eve) != nil {
		t.Fatal("a stranger stays in the call")
	}

	// The last one out ends the call and closes the room.
	e.leave(ann, room)
	if callOfDM(bob, dm) == nil {
		t.Fatal("the call ended with someone still in it")
	}
	e.leave(bob, room)
	if callOfDM(bob, dm) != nil || len(e.Outbox(events.CallDelete)) != 1 || !e.called("DeleteRoom") {
		t.Fatalf("the call did not end: %v, calls %v", callOfDM(bob, dm), e.rooms.calls)
	}

	// Blocked: no calls in that DM.
	bob.Expect(http.StatusNoContent, "PUT", rels+"/"+ann.ID.String(), map[string]any{"type": 2})
	ann.Expect(http.StatusForbidden, "POST", path, nil)
}

// A group call rings everyone else; one nobody joined ends after the ringing.
func TestGroupCallEndsWhenNobodyCame(t *testing.T) {
	e := newEnv(t)
	ann, bob, eve := e.User("ann"), e.User("bob"), e.User("eve")
	befriend(ann, bob)
	befriend(ann, eve)
	group := apitest.Str(apitest.Obj(ann.Expect(http.StatusOK, "POST", dms,
		map[string]any{"recipients": []string{bob.ID.String(), eve.ID.String()}}))["id"])
	call := apitest.Obj(apitest.Obj(ann.Expect(http.StatusOK, "POST", "/api/v1/channels/"+group+"/call", nil))["call"])
	if r := ids(call["ringing"]); len(r) != 2 || slices.Contains(r, ann.ID.String()) {
		t.Fatalf("group call rings %v", r)
	}
	// Nobody connected, the ringing is over: the periodic check ends it.
	if _, err := e.PG.Exec(t.Context(), `UPDATE dm_calls SET ring_until = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if callOfDM(ann, group) != nil || len(e.Outbox(events.CallDelete)) != 1 {
		t.Fatal("an idle call did not end")
	}
}
