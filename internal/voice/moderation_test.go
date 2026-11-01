package voice

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
)

// inRoom puts u in a voice channel the way LiveKit would: in the room, then
// the joined webhook.
func (e *env) inRoom(u *apitest.User, channel string) Participant {
	e.T.Helper()
	p := Participant{Sid: "PA_" + u.ID.String()[:8], Identity: u.ID.String(),
		Permission: &Permission{CanSubscribe: true, CanPublish: true, CanPublishSources: []string{"MICROPHONE", "CAMERA", "SCREEN_SHARE", "SCREEN_SHARE_AUDIO"}}}
	room := "voice:" + channel
	e.rooms.rooms[room] = append(e.rooms.rooms[room], p)
	if code := e.webhook("participant_joined", room, p); code != http.StatusOK {
		e.T.Fatalf("joined: %d", code)
	}
	return p
}

func (e *env) permOf(room string, u *apitest.User) *Permission {
	for _, p := range e.rooms.rooms[room] {
		if p.Identity == u.ID.String() {
			return p.Permission
		}
	}
	return nil
}

func (e *env) called(prefix string) bool {
	return slices.ContainsFunc(e.rooms.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

// Rooms follow changes of permissions and membership right away, not at
// the next reconnect (docs/adr/0026-voice-moderation.md).
func TestRoomsFollowPermissions(t *testing.T) {
	e := newEnv(t)
	owner, bob, eve := e.User("owner"), e.User("bob"), e.User("eve")
	gid, _, voice := newGuild(owner)
	for _, u := range []*apitest.User{bob, eve} {
		if _, err := e.guilds.AddMember(t.Context(), uuid.MustParse(gid), u.ID); err != nil {
			t.Fatal(err)
		}
	}
	room := "voice:" + voice
	e.inRoom(owner, voice)
	e.inRoom(bob, voice)
	e.inRoom(eve, voice)

	// Lost STREAM: bob may not share his screen any more; owner is untouched.
	e.rooms.calls = nil
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+voice+"/permissions/"+bob.ID.String(),
		map[string]any{"type": 2, "deny": permStr(perm.Stream)})
	if p := e.permOf(room, bob); p == nil || !slices.Equal(p.CanPublishSources, []string{"MICROPHONE", "CAMERA"}) {
		t.Fatalf("bob's permission after losing STREAM: %+v", p)
	}
	if slices.ContainsFunc(e.rooms.calls, func(c string) bool { return strings.Contains(c, owner.ID.String()) }) {
		t.Fatalf("someone whose rights did not change was updated: %v", e.rooms.calls)
	}

	// A moderator mutes and deafens bob; bob cannot do it to the owner.
	st := apitest.Obj(owner.Expect(http.StatusOK, "PATCH", "/api/v1/guilds/"+gid+"/voice-states/"+bob.ID.String(), map[string]any{"mute": true, "deaf": true}))
	if st["mute"] != true || st["deaf"] != true {
		t.Fatalf("server voice state %v", st)
	}
	if p := e.permOf(room, bob); p == nil || slices.Contains(p.CanPublishSources, "MICROPHONE") || p.CanSubscribe {
		t.Fatalf("bob muted and deafened: %+v", p)
	}
	// Leaving and coming back does not undo it: the new token has no
	// microphone and no incoming sound.
	e.rooms.rooms[room] = slices.DeleteFunc(e.rooms.rooms[room], func(p Participant) bool { return p.Identity == bob.ID.String() })
	if ok := e.webhook("participant_left", room, Participant{Sid: "PA_" + bob.ID.String()[:8], Identity: bob.ID.String()}); ok != http.StatusOK {
		t.Fatalf("left: %d", ok)
	}
	join := apitest.Obj(bob.Expect(http.StatusOK, "POST", "/api/v1/channels/"+voice+"/voice", nil))
	cl := parse(t, apitest.Str(join["token"]), testSecret, e.lk.now())
	if slices.Contains(cl.Video.CanPublishSources, "microphone") || *cl.Video.CanSubscribe || slices.Contains(apitest.List(join["sources"]), any("microphone")) {
		t.Fatalf("rejoin token of a muted and deafened member: %+v, sources %v", cl.Video, join["sources"])
	}
	e.inRoom(bob, voice)
	if st := e.states(owner, gid); !slices.ContainsFunc(st, func(v any) bool { o := apitest.Obj(v); return o["user_id"] == bob.ID.String() && o["mute"] == true }) {
		t.Fatalf("mute lost on rejoin: %v", st)
	}
	bob.Expect(http.StatusForbidden, "PATCH", "/api/v1/guilds/"+gid+"/voice-states/"+owner.ID.String(), map[string]any{"mute": true})
	owner.Expect(http.StatusUnprocessableEntity, "PATCH", "/api/v1/guilds/"+gid+"/voice-states/"+bob.ID.String(), map[string]any{})
	owner.Expect(http.StatusOK, "PATCH", "/api/v1/guilds/"+gid+"/voice-states/"+bob.ID.String(), map[string]any{"mute": false, "deaf": false})
	if p := e.permOf(room, bob); p == nil || !slices.Contains(p.CanPublishSources, "MICROPHONE") || !p.CanSubscribe {
		t.Fatalf("bob unmuted: %+v", p)
	}

	// Disconnecting needs MOVE_MEMBERS.
	bob.Expect(http.StatusForbidden, "DELETE", "/api/v1/guilds/"+gid+"/voice-states/"+eve.ID.String(), nil)
	owner.Expect(http.StatusNoContent, "DELETE", "/api/v1/guilds/"+gid+"/voice-states/"+eve.ID.String(), nil)
	if e.permOf(room, eve) != nil || len(e.states(owner, gid)) != 2 {
		t.Fatalf("eve still in the room: %v", e.states(owner, gid))
	}
	owner.Expect(http.StatusNotFound, "DELETE", "/api/v1/guilds/"+gid+"/voice-states/"+eve.ID.String(), nil)

	// Kicked: out of the room at once, the others stay.
	owner.Expect(http.StatusNoContent, "DELETE", "/api/v1/guilds/"+gid+"/members/"+bob.ID.String(), nil)
	if e.permOf(room, bob) != nil || len(e.states(owner, gid)) != 1 {
		t.Fatalf("a kicked member stays in the room: %v", e.rooms.rooms[room])
	}

	// Banned: same.
	e.inRoom(eve, voice)
	owner.Expect(http.StatusNoContent, "PUT", "/api/v1/guilds/"+gid+"/bans/"+eve.ID.String(), map[string]any{})
	if e.permOf(room, eve) != nil {
		t.Fatal("a banned member stays in the room")
	}

	// The channel is deleted: its room is closed.
	owner.Expect(http.StatusNoContent, "DELETE", "/api/v1/channels/"+voice, nil)
	if _, ok := e.rooms.rooms[room]; ok || !e.called("DeleteRoom") {
		t.Fatalf("the room of a deleted channel stays open: %v", e.rooms.calls)
	}
}

// Losing CONNECT through a role takes everyone with that role out.
func TestRolesChangeTakesOut(t *testing.T) {
	e := newEnv(t)
	owner, bob := e.User("owner"), e.User("bob")
	gid, _, voice := newGuild(owner)
	if _, err := e.guilds.AddMember(t.Context(), uuid.MustParse(gid), bob.ID); err != nil {
		t.Fatal(err)
	}
	e.inRoom(bob, voice)
	// @everyone (the role with the guild's id) loses CONNECT.
	owner.Expect(http.StatusOK, "PATCH", "/api/v1/guilds/"+gid+"/roles/"+gid,
		map[string]any{"permissions": permStr(perm.DefaultEveryone &^ perm.Connect)})
	if e.permOf("voice:"+voice, bob) != nil || len(e.states(owner, gid)) != 0 {
		t.Fatal("bob stays without CONNECT")
	}
}

// When LiveKit was unreachable during a change, the periodic check applies it.
func TestReconcileEnforces(t *testing.T) {
	e := newEnv(t)
	owner, bob := e.User("owner"), e.User("bob")
	gid, _, voice := newGuild(owner)
	if _, err := e.guilds.AddMember(t.Context(), uuid.MustParse(gid), bob.ID); err != nil {
		t.Fatal(err)
	}
	e.inRoom(bob, voice)
	e.guilds.WithVoice(nil) // the change happens without enforcement
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+voice+"/permissions/"+bob.ID.String(),
		map[string]any{"type": 2, "deny": permStr(perm.Speak)})
	if p := e.permOf("voice:"+voice, bob); !slices.Contains(p.CanPublishSources, "MICROPHONE") {
		t.Fatal("setup: enforced too early")
	}
	if err := e.svc.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p := e.permOf("voice:"+voice, bob); slices.Contains(p.CanPublishSources, "MICROPHONE") {
		t.Fatalf("the check did not apply the lost SPEAK: %+v", p)
	}
}
