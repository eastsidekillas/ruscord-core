package voice

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/apitest"
	"github.com/eastsidekillas/ruscord-core/internal/platform/testdb"
)

// Integration tests need a real Postgres: set RUSCORD_TEST_POSTGRES_URL.

type env struct {
	*apitest.Env
	guilds *guild.Service
	svc    *Service
	lk     *LiveKit
	rooms  *fakeRoomService
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := apitest.New(t)
	guilds := guild.NewService(e.PG, e.Auth, e.Limiter)
	guild.NewHandler(guilds, e.RequireAuth).Register(e.Mux)
	f := &fakeRoomService{t: t, rooms: map[string][]Participant{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	lk := NewLiveKit(srv.URL, testKey, testSecret)
	svc := NewService(lk, guilds, e.Auth, e.Limiter, "", testdb.Logger())
	svc.Register(e.Mux, e.RequireAuth)
	svc.RegisterWebhook(e.Mux, lk)
	guilds.WithVoice(svc)
	return &env{Env: e, guilds: guilds, svc: svc, lk: lk, rooms: f}
}

// webhook posts a signed LiveKit event and returns the status.
func (e *env) webhook(event, room string, p Participant) int {
	e.T.Helper()
	body := fmt.Sprintf(`{"event":%q,"room":{"name":%q},"participant":{"sid":%q,"identity":%q},"id":"EV_1","createdAt":"1"}`,
		event, room, p.Sid, p.Identity)
	r := signedWebhook(e.T, body, testKey, testSecret, "", e.lk.now())
	req, _ := http.NewRequestWithContext(e.T.Context(), http.MethodPost, e.URL()+WebhookPath, r.Body)
	req.Header = r.Header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.T.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func (e *env) states(u *apitest.User, gid string) []any {
	e.T.Helper()
	return apitest.List(apitest.Obj(u.Expect(http.StatusOK, "GET", "/api/v1/guilds/"+gid, nil))["voice_states"])
}

func newGuild(u *apitest.User) (gid, text, voice string) {
	g := apitest.Obj(u.Expect(http.StatusCreated, "POST", "/api/v1/guilds", map[string]any{"name": "Голос"}))
	chs := apitest.List(g["channels"])
	return apitest.Str(g["id"]), apitest.Str(apitest.Obj(chs[0])["id"]), apitest.Str(apitest.Obj(chs[1])["id"])
}

func permStr(p perm.Permissions) string { return fmt.Sprint(uint64(p)) }

func TestJoinIssuesAToken(t *testing.T) {
	e := newEnv(t)
	owner, bob := e.User("owner"), e.User("bob")
	gid, text, voice := newGuild(owner)
	if _, err := e.guilds.AddMember(t.Context(), uuid.MustParse(gid), bob.ID); err != nil {
		t.Fatal(err)
	}
	// Bob may listen and talk, not share the screen.
	owner.Expect(http.StatusOK, "PUT", "/api/v1/channels/"+voice+"/permissions/"+bob.ID.String(),
		map[string]any{"type": 2, "deny": permStr(perm.Stream)})

	info := apitest.Obj(bob.Expect(http.StatusOK, "POST", "/api/v1/channels/"+voice+"/voice", nil))
	// The owner has every right, STREAM_HQ included; Bob only the defaults.
	if info["room"] != "voice:"+voice || info["url"] != "" || info["push_to_talk"] != false || info["high_quality"] != false {
		t.Fatalf("join info %v", info)
	}
	cl := parse(t, apitest.Str(info["token"]), testSecret, e.lk.now())
	if cl.Subject != bob.ID.String() || cl.Name != "bob" || cl.Video.Room != "voice:"+voice ||
		!slices.Equal(cl.Video.CanPublishSources, []string{"microphone", "camera"}) || cl.Video.RoomAdmin {
		t.Fatalf("token claims %+v %+v", cl, cl.Video)
	}
	if own := apitest.Obj(owner.Expect(http.StatusOK, "POST", "/api/v1/channels/"+voice+"/voice", nil)); own["high_quality"] != true {
		t.Fatalf("owner join info %v", own)
	}
	bob.Expect(http.StatusBadRequest, "POST", "/api/v1/channels/"+text+"/voice", nil)
	e.User("stranger").Expect(http.StatusNotFound, "POST", "/api/v1/channels/"+voice+"/voice", nil)
}

func TestWebhooksFollowTheRoom(t *testing.T) {
	e := newEnv(t)
	owner, bob, eve := e.User("owner"), e.User("bob"), e.User("eve")
	gid, _, voice := newGuild(owner)
	gid2, _, voice2 := newGuild(owner)
	if _, err := e.guilds.AddMember(t.Context(), uuid.MustParse(gid), bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.guilds.AddMember(t.Context(), uuid.MustParse(gid2), bob.ID); err != nil {
		t.Fatal(err)
	}
	room, room2 := "voice:"+voice, "voice:"+voice2
	pb := Participant{Sid: "PA_b1", Identity: bob.ID.String()}
	e.rooms.rooms[room] = []Participant{pb}

	if code := e.webhook("participant_joined", room, pb); code != http.StatusOK {
		t.Fatalf("joined: %d", code)
	}
	if st := e.states(owner, gid); len(st) != 1 {
		t.Fatalf("states %v", st)
	}

	// Muting oneself is reported through the API.
	st := apitest.Obj(bob.Expect(http.StatusOK, "PATCH", "/api/v1/channels/"+voice+"/voice", map[string]any{"self_mute": true, "self_deaf": true}))
	if st["self_mute"] != true || st["self_deaf"] != true {
		t.Fatalf("self voice %v", st)
	}
	owner.Expect(http.StatusConflict, "PATCH", "/api/v1/channels/"+voice+"/voice", map[string]any{"self_mute": true})

	// Someone who may not be there (not a member) is removed from the room.
	pe := Participant{Sid: "PA_e", Identity: eve.ID.String()}
	e.rooms.rooms[room] = append(e.rooms.rooms[room], pe)
	e.webhook("participant_joined", room, pe)
	if !slices.Contains(e.rooms.calls, "RemoveParticipant "+room+" "+eve.ID.String()) || len(e.states(owner, gid)) != 1 {
		t.Fatalf("an outsider in the room: calls %v", e.rooms.calls)
	}

	// Joining a room in another guild leaves the previous one.
	pb2 := Participant{Sid: "PA_b2", Identity: bob.ID.String()}
	e.rooms.rooms[room2] = []Participant{pb2}
	e.webhook("participant_joined", room2, pb2)
	if !slices.Contains(e.rooms.calls, "RemoveParticipant "+room+" "+bob.ID.String()) {
		t.Fatalf("the old room was not left: %v", e.rooms.calls)
	}
	if len(e.states(owner, gid)) != 0 || len(e.states(owner, gid2)) != 1 {
		t.Fatal("the state did not move to the other guild")
	}
	// The old connection's "left" arrives late: the new state stays.
	e.webhook("participant_left", room, pb)
	if len(e.states(owner, gid2)) != 1 {
		t.Fatal("a late left cleared the new state")
	}
	e.webhook("participant_left", room2, pb2)
	if len(e.states(owner, gid2)) != 0 {
		t.Fatal("left did not clear the state")
	}

	// Not our rooms, not our users, broken signatures.
	if code := e.webhook("participant_joined", "call:x", pb); code != http.StatusOK {
		t.Fatalf("a foreign room: %d", code)
	}
	if code := e.webhook("participant_joined", room, Participant{Sid: "PA_x", Identity: "agent"}); code != http.StatusOK {
		t.Fatalf("a foreign identity: %d", code)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, e.URL()+WebhookPath, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "nope")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook: %d", resp.StatusCode)
	}
}

// Webhooks get lost and LiveKit forgets rooms on restart: the periodic
// check makes the states match the rooms.
func TestReconcile(t *testing.T) {
	e := newEnv(t)
	owner, bob, eve := e.User("owner"), e.User("bob"), e.User("eve")
	gid, _, voice := newGuild(owner)
	_, _, voice2 := newGuild(owner)
	if _, err := e.guilds.AddMember(t.Context(), uuid.MustParse(gid), bob.ID); err != nil {
		t.Fatal(err)
	}
	room := "voice:" + voice
	// Recorded, but LiveKit lost the room (restart).
	if ok, _, err := e.guilds.VoiceJoined(t.Context(), owner.ID, uuid.MustParse(voice2), "PA_gone"); !ok || err != nil {
		t.Fatalf("seed: %v %v", ok, err)
	}
	// In the room, but the joined webhook never came; eve may not be there.
	e.rooms.rooms[room] = []Participant{{Sid: "PA_b", Identity: bob.ID.String()}, {Sid: "PA_e", Identity: eve.ID.String()}}

	if err := e.svc.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	st := e.states(owner, gid)
	if len(st) != 1 || apitest.Obj(st[0])["user_id"] != bob.ID.String() {
		t.Fatalf("states after the check: %v", st)
	}
	rooms, _ := e.guilds.VoiceRooms(t.Context())
	if len(rooms[uuid.MustParse(voice2)]) != 0 {
		t.Fatalf("the state of a forgotten room stays: %v", rooms)
	}
	if !slices.Contains(e.rooms.calls, "RemoveParticipant "+room+" "+eve.ID.String()) {
		t.Fatalf("eve was not removed: %v", e.rooms.calls)
	}
	// A second pass changes nothing.
	before := len(e.Outbox("VOICE_STATE_UPDATE"))
	if err := e.svc.Reconcile(t.Context()); err != nil || len(e.Outbox("VOICE_STATE_UPDATE")) != before {
		t.Fatalf("the second pass changed states: %v", err)
	}
}
