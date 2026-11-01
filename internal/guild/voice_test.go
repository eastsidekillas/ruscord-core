package guild

import (
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// apiCode is the code of an API error, or "" for nil and other errors.
func apiCode(err error) string {
	var e *httpx.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestVoiceAccess(t *testing.T) {
	e := newEnv(t)
	owner, bob, mod, stranger := e.User("owner"), e.User("bob"), e.User("mod"), e.User("stranger")
	gid, text, voice := createGuild(owner)
	e.join(gid, bob)
	e.join(gid, mod)
	g := "/api/v1/guilds/" + gid

	hidden := str(obj(owner.Expect(http.StatusCreated, "POST", g+"/channels", map[string]any{"type": 2, "name": "тайная"}))["id"])
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+hidden+"/permissions/"+gid,
		map[string]any{"type": 1, "deny": permStr(perm.ViewChannel)})
	mute := str(obj(owner.Expect(http.StatusCreated, "POST", g+"/channels", map[string]any{"type": 2, "name": "только слушать"}))["id"])
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+mute+"/permissions/"+gid,
		map[string]any{"type": 1, "deny": permStr(perm.Connect)})
	small := str(obj(owner.Expect(http.StatusCreated, "POST", g+"/channels", map[string]any{"type": 2, "name": "на двоих", "user_limit": 1}))["id"])
	role := str(obj(owner.Expect(http.StatusCreated, "POST", g+"/roles", map[string]any{"name": "мод", "permissions": permStr(perm.MoveMembers)}))["id"])
	owner.Expect(http.StatusNoContent, "PUT", g+"/members/"+mod.ID.String()+"/roles/"+role, nil)
	// Someone is already in the small channel.
	if ok, _, err := e.svc.VoiceJoined(t.Context(), owner.ID, uuid.MustParse(small), "PA_owner"); !ok || err != nil {
		t.Fatalf("owner joins: %v %v", ok, err)
	}

	for _, c := range []struct {
		name    string
		user    uuid.UUID
		channel string
		want    string // API error code, "" = allowed
	}{
		{"member, voice channel", bob.ID, voice, ""},
		{"text channel", bob.ID, text, "not_voice_channel"},
		{"hidden channel", bob.ID, hidden, "not_found"},
		{"no CONNECT", bob.ID, mute, "forbidden"},
		{"full channel", bob.ID, small, "channel_full"},
		{"full channel, MOVE_MEMBERS", mod.ID, small, ""},
		{"the one inside counts as there", owner.ID, small, ""},
		{"not a member", stranger.ID, voice, "not_found"},
		{"no such channel", bob.ID, uuid.NewString(), "not_found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.svc.VoiceAccess(t.Context(), c.user, uuid.MustParse(c.channel))
			if got := apiCode(err); got != c.want {
				t.Fatalf("error %v (code %q), want %q", err, got, c.want)
			}
		})
	}
}

func TestVoiceStates(t *testing.T) {
	e := newEnv(t)
	owner, bob := e.User("owner"), e.User("bob")
	gid, _, voice := createGuild(owner)
	gid2, _, voice2 := createGuild(owner)
	e.join(gid, bob)
	e.join(gid2, bob)
	g := "/api/v1/guilds/" + gid
	other := str(obj(owner.Expect(http.StatusCreated, "POST", g+"/channels", map[string]any{"type": 2, "name": "вторая"}))["id"])
	vc, vc2, oc := uuid.MustParse(voice), uuid.MustParse(voice2), uuid.MustParse(other)
	ctx := t.Context()
	events := func() int { return len(e.Outbox("VOICE_STATE_UPDATE")) }
	statesIn := func(guildID string) []any {
		return list(obj(owner.Expect(http.StatusOK, "GET", "/api/v1/guilds/"+guildID, nil))["voice_states"])
	}

	if ok, old, err := e.svc.VoiceJoined(ctx, bob.ID, vc, "PA_1"); !ok || old != nil || err != nil {
		t.Fatalf("join: %v %v %v", ok, old, err)
	}
	if st := statesIn(gid); len(st) != 1 || obj(st[0])["channel_id"] != voice || obj(st[0])["user_id"] != bob.ID.String() ||
		obj(obj(st[0])["user"])["username"] != "bob" {
		t.Fatalf("voice_states = %v (with the profile)", st)
	}
	if ev := e.Outbox("VOICE_STATE_UPDATE"); obj(ev[0].Data["user"])["username"] != "bob" {
		t.Fatalf("VOICE_STATE_UPDATE without the profile: %v", ev[0].Data)
	}
	// LiveKit repeats a webhook: nothing new happens.
	if ok, _, _ := e.svc.VoiceJoined(ctx, bob.ID, vc, "PA_1"); !ok || events() != 1 {
		t.Fatalf("repeated join: ok=%v, %d events", ok, events())
	}

	// Muting oneself is seen by others.
	if st, err := e.svc.SetSelfVoice(ctx, bob.ID, vc, true, false); err != nil || !st.SelfMute {
		t.Fatalf("self mute: %+v %v", st, err)
	}
	if _, err := e.svc.SetSelfVoice(ctx, owner.ID, vc, true, false); apiCode(err) != "not_in_voice" {
		t.Fatalf("mute outside voice: %v", err)
	}

	// Another channel of the same guild: the state moves, the old room is reported.
	if ok, old, err := e.svc.VoiceJoined(ctx, bob.ID, oc, "PA_2"); !ok || old == nil || *old != vc || err != nil {
		t.Fatalf("move: %v %v %v", ok, old, err)
	}
	if st := statesIn(gid); len(st) != 1 || obj(st[0])["channel_id"] != other || obj(st[0])["self_mute"] != true {
		t.Fatalf("after the move: %v (self mute carries over)", st)
	}

	// A "left" of the older connection does not clear the newer one.
	if cleared, err := e.svc.VoiceLeft(ctx, bob.ID, vc, "PA_1"); cleared || err != nil {
		t.Fatalf("stale left: %v %v", cleared, err)
	}

	// Another guild: the first guild hears that bob left, the second that he came.
	if ok, old, err := e.svc.VoiceJoined(ctx, bob.ID, vc2, "PA_3"); !ok || old == nil || *old != oc || err != nil {
		t.Fatalf("other guild: %v %v %v", ok, old, err)
	}
	if len(statesIn(gid)) != 0 || len(statesIn(gid2)) != 1 {
		t.Fatalf("states after moving guilds: %v / %v", statesIn(gid), statesIn(gid2))
	}
	evs := e.Outbox("VOICE_STATE_UPDATE")
	last2 := evs[len(evs)-2:]
	if last2[0].Topic != "guild."+gid || last2[0].Data["channel_id"] != nil || last2[1].Topic != "guild."+gid2 {
		t.Fatalf("events of the move: %+v", last2)
	}

	if cleared, err := e.svc.VoiceLeft(ctx, bob.ID, vc2, "PA_3"); !cleared || err != nil {
		t.Fatalf("left: %v %v", cleared, err)
	}
	if len(statesIn(gid2)) != 0 {
		t.Fatal("state kept after leaving")
	}

	// Rooms for the check against LiveKit.
	if _, _, err := e.svc.VoiceJoined(ctx, owner.ID, vc, "PA_o"); err != nil {
		t.Fatal(err)
	}
	rooms, err := e.svc.VoiceRooms(ctx)
	if err != nil || len(rooms) != 1 || len(rooms[vc]) != 1 || rooms[vc][0].Participant != "PA_o" {
		t.Fatalf("rooms = %v, %v", rooms, err)
	}
}

// A join LiveKit reports for someone who may not be there (any more) is
// not recorded, and the caller removes them.
func TestVoiceJoinedRechecksAccess(t *testing.T) {
	e := newEnv(t)
	owner, bob, stranger := e.User("owner"), e.User("bob"), e.User("stranger")
	gid, text, voice := createGuild(owner)
	e.join(gid, bob)
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+voice+"/permissions/"+bob.ID.String(),
		map[string]any{"type": 2, "deny": permStr(perm.Connect)})
	for _, c := range []struct {
		name    string
		user    uuid.UUID
		channel string
	}{
		{"lost CONNECT", bob.ID, voice},
		{"not a member", stranger.ID, voice},
		{"a text channel", owner.ID, text},
		{"no such channel", owner.ID, uuid.NewString()},
	} {
		t.Run(c.name, func(t *testing.T) {
			ok, _, err := e.svc.VoiceJoined(t.Context(), c.user, uuid.MustParse(c.channel), "PA_x")
			if ok || err != nil {
				t.Fatalf("ok=%v err=%v, want refused", ok, err)
			}
		})
	}
	if n := len(e.Outbox("VOICE_STATE_UPDATE")); n != 0 {
		t.Fatalf("%d voice events for refused joins", n)
	}
}

// Members see only the voice states of channels they can view.
func TestVoiceStatesFollowVisibility(t *testing.T) {
	e := newEnv(t)
	owner, bob := e.User("owner"), e.User("bob")
	gid, _, _ := createGuild(owner)
	e.join(gid, bob)
	g := "/api/v1/guilds/" + gid
	hidden := str(obj(owner.Expect(http.StatusCreated, "POST", g+"/channels", map[string]any{"type": 2, "name": "тайная"}))["id"])
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+hidden+"/permissions/"+gid,
		map[string]any{"type": 1, "deny": permStr(perm.ViewChannel)})
	if ok, _, err := e.svc.VoiceJoined(t.Context(), owner.ID, uuid.MustParse(hidden), "PA_o"); !ok || err != nil {
		t.Fatalf("owner joins the hidden channel: %v %v", ok, err)
	}
	if n := len(list(obj(owner.Expect(http.StatusOK, "GET", g, nil))["voice_states"])); n != 1 {
		t.Fatalf("owner sees %d states", n)
	}
	if n := len(list(obj(bob.Expect(http.StatusOK, "GET", g, nil))["voice_states"])); n != 0 {
		t.Fatalf("bob sees %d states of a hidden channel", n)
	}
	snap, err := e.svc.Snapshot(t.Context(), bob.ID, uuid.MustParse(gid))
	if err != nil || len(snap.VoiceStates) != 1 {
		t.Fatalf("the snapshot keeps hidden states for the gateway: %v %v", snap.VoiceStates, err)
	}
}
