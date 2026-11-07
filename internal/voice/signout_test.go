package voice

import (
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
)

// Revoked sessions end the user's voice and call connections (S4 of
// docs/audit/2026-11-04.md): LiveKit would keep them alive on its own.
func TestSignedOutLeavesVoiceAndCalls(t *testing.T) {
	e := newEnv(t)
	e.Auth.OnSignedOut(e.svc)
	ann, bob := e.User("ann"), e.User("bob")
	gid, _, voice := newGuild(ann)
	if _, err := e.guilds.AddMember(t.Context(), uuid.MustParse(gid), bob.ID); err != nil {
		t.Fatal(err)
	}
	e.inRoom(bob, voice)
	befriend(ann, bob)
	dm := apitest.Str(apitest.Obj(ann.Expect(http.StatusOK, "POST", dms, map[string]any{"recipient_id": bob.ID}))["id"])
	room := apitest.Str(apitest.Obj(ann.Expect(http.StatusOK, "POST", "/api/v1/channels/"+dm+"/call", nil))["room"])
	e.enter(ann, room)
	bob.Expect(http.StatusOK, "POST", "/api/v1/channels/"+dm+"/call", nil)
	e.enter(bob, room)

	// A password change signs out Bob's other devices.
	bob.Expect(http.StatusNoContent, "POST", "/api/v1/users/@me/password",
		map[string]any{"current_password": apitest.Password, "new_password": "another-password-2"})

	for _, want := range []string{"RemoveParticipant voice:" + voice + " " + bob.ID.String(), "RemoveParticipant " + room + " " + bob.ID.String()} {
		if !slices.Contains(e.rooms.calls, want) {
			t.Errorf("no %q in %v", want, e.rooms.calls)
		}
	}
	if st := e.states(ann, gid); len(st) != 0 {
		t.Errorf("Bob is still in voice: %v", st)
	}
	// Ann stays in the call.
	if ps := e.rooms.rooms[room]; len(ps) != 1 || ps[0].Identity != ann.ID.String() {
		t.Errorf("call room: %v", ps)
	}
	// Nothing to end: signing out again is quiet.
	if err := e.svc.SignedOut(t.Context(), bob.ID); err != nil {
		t.Fatal(err)
	}
}
