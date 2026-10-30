package gateway

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/platform/natstest"
	"github.com/eastsidekillas/ruscord-core/internal/platform/testdb"
	"github.com/eastsidekillas/ruscord-core/internal/presence"
)

func TestRouteEphemeral(t *testing.T) {
	f := newFixture()
	friend, stranger := uuid.New(), uuid.New()
	eg, edm := events.EphGuildTopic(f.guildID), events.EphChannelTopic(f.dm)
	typing := func(ch, user uuid.UUID) map[string]any {
		return map[string]any{"channel_id": ch, "user_id": user, "timestamp": time.Now()}
	}
	pres := func(user uuid.UUID, st presence.Status) presence.Update {
		return presence.Update{UserID: user, Status: st}
	}
	ty, pu := events.TypingStart, events.PresenceUpdate

	cases := []struct {
		name string
		in   event
		want []string
	}{
		{"typing in a visible channel", ev(eg, ty, typing(f.general.ID, f.other)), []string{ty + ":" + f.general.ID.String()}},
		{"typing in a hidden channel", ev(eg, ty, typing(f.secret.ID, f.other)), nil},
		{"typing in an unknown channel", ev(eg, ty, typing(uuid.New(), f.other)), nil},
		{"my own typing", ev(eg, ty, typing(f.general.ID, f.me)), nil},
		{"typing in a guild I am not in", ev(events.EphGuildTopic(uuid.New()), ty, typing(f.general.ID, f.other)), nil},
		{"typing in my DM", ev(edm, ty, typing(f.dm, f.other)), []string{ty + ":" + f.dm.String()}},
		{"typing in someone else's DM", ev(events.EphChannelTopic(uuid.New()), ty, typing(f.dm, f.other)), nil},
		{"DM typing naming another channel", ev(edm, ty, typing(uuid.New(), f.other)), nil},
		{"typing on a presence subject", ev(events.EphPresenceTopic(friend), ty, typing(f.general.ID, friend)), nil},

		{"presence in my guild", ev(eg, pu, pres(f.other, presence.Online)), []string{pu + ":-"}},
		{"presence in a guild I am not in", ev(events.EphGuildTopic(uuid.New()), pu, pres(f.other, presence.Online)), nil},
		{"presence of a friend", ev(events.EphPresenceTopic(friend), pu, pres(friend, presence.DND)), []string{pu + ":-"}},
		{"presence of a non-friend", ev(events.EphPresenceTopic(stranger), pu, pres(stranger, presence.Online)), nil},
		{"friend subject carrying someone else", ev(events.EphPresenceTopic(friend), pu, pres(stranger, presence.Online)), nil},
		{"my own presence", ev(eg, pu, pres(f.me, presence.Online)), nil},
		{"presence on a DM subject", ev(edm, pu, pres(f.other, presence.Online)), nil},

		{"stream subject", ev(events.GuildTopic(f.guildID), ty, typing(f.general.ID, f.other)), nil},
		{"malformed subject", ev("eph.guild.nope", ty, typing(f.general.ID, f.other)), nil},
		{"unknown kind", ev("eph.thing."+f.guildID.String(), ty, typing(f.general.ID, f.other)), nil},
		{"unknown type", ev(eg, "SOMETHING", typing(f.general.ID, f.other)), nil},
		{"malformed payload", event{Subject: eg, Type: ty, Data: json.RawMessage(`{"channel_id": 5}`)}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRouter(f.me, f.authSID, fakeLoader{}, []guild.Snapshot{f.snapshot()}, []uuid.UUID{f.dm})
			r.friends[friend] = true
			got := gotEph(r.routeEphemeral(c.in).out)
			if !slices.Equal(got, c.want) {
				t.Errorf("dispatches = %v, want %v", got, c.want)
			}
		})
	}
}

// gotEph renders typing by channel and presence as "-", like got().
func gotEph(ds []dispatch) []string {
	var out []string
	for _, d := range ds {
		if d.T == events.TypingStart {
			var p struct {
				ChannelID uuid.UUID `json:"channel_id"`
			}
			_ = json.Unmarshal(d.D, &p)
			out = append(out, d.T+":"+p.ChannelID.String())
			continue
		}
		out = append(out, d.T+":-")
	}
	return out
}

// One change of a friend who shares two guilds arrives three times; the
// client gets it once. A real change goes through.
func TestRoutePresenceOnce(t *testing.T) {
	f := newFixture()
	second := newFixture()
	snap2 := second.snapshot()
	snap2.Member.UserID = f.me
	r := newRouter(f.me, f.authSID, fakeLoader{}, []guild.Snapshot{f.snapshot(), snap2}, nil)
	r.friends[f.other] = true
	upd := presence.Update{UserID: f.other, Status: presence.Online}
	n := 0
	for _, subj := range []string{events.EphGuildTopic(f.guildID), events.EphGuildTopic(second.guildID), events.EphPresenceTopic(f.other)} {
		n += len(r.routeEphemeral(ev(subj, events.PresenceUpdate, upd)).out)
	}
	if n != 1 {
		t.Fatalf("one change delivered %d times", n)
	}
	upd.Status = presence.Idle
	if len(r.routeEphemeral(ev(events.EphPresenceTopic(f.other), events.PresenceUpdate, upd)).out) != 1 {
		t.Fatal("a real change was suppressed")
	}
	// Leaving a guild forgets what was delivered: a status missed meanwhile
	// must not suppress the next one.
	if _, err := r.route(t.Context(), ev(events.UserTopic(f.me), events.GuildDelete, map[string]any{"id": second.guildID})); err != nil {
		t.Fatal(err)
	}
	if len(r.routeEphemeral(ev(events.EphGuildTopic(f.guildID), events.PresenceUpdate, upd)).out) != 1 {
		t.Fatal("after leaving a guild a status was suppressed by a stale record")
	}
}

func TestRouteFriendsAndChoice(t *testing.T) {
	f := newFixture()
	r := newRouter(f.me, f.authSID, fakeLoader{}, nil, []uuid.UUID{f.dm})
	ut := events.UserTopic(f.me)
	rel := func(typ string, kind int) routed {
		t.Helper()
		res, err := r.route(t.Context(), ev(ut, typ, map[string]any{"id": f.other, "type": kind}))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// An incoming request is not a friendship.
	if res := rel(events.RelationshipAdd, 3); res.ephChanged || len(res.newFriends) != 0 || len(res.out) != 1 {
		t.Fatalf("incoming request: %+v", res)
	}
	res := rel(events.RelationshipAdd, relFriend)
	if !res.ephChanged || !slices.Equal(res.newFriends, []uuid.UUID{f.other}) || len(res.out) != 1 {
		t.Fatalf("new friend: %+v", res)
	}
	if !slices.Contains(r.ephSubjects(), events.EphPresenceTopic(f.other)) || !slices.Contains(r.ephSubjects(), events.EphChannelTopic(f.dm)) {
		t.Fatalf("subjects after the friendship: %v", r.ephSubjects())
	}
	if res := rel(events.RelationshipAdd, relFriend); res.ephChanged || len(res.newFriends) != 0 {
		t.Fatalf("a repeated event must change nothing: %+v", res)
	}
	r.seen[f.other] = presence.Online
	if res := rel(events.RelationshipRemove, relFriend); !res.ephChanged {
		t.Fatal("removing a friend must drop the subscription")
	}
	if _, ok := r.seen[f.other]; ok || slices.Contains(r.ephSubjects(), events.EphPresenceTopic(f.other)) {
		t.Fatal("a former friend is still followed")
	}

	// The chosen status follows USER_UPDATE; junk is ignored.
	upd := func(st string) routed {
		t.Helper()
		res, _ := r.route(t.Context(), ev(ut, events.UserUpdate, map[string]any{"id": f.me, "status": st}))
		return res
	}
	if res := upd("dnd"); !res.choiceChanged || r.choice != presence.DND || len(res.out) != 1 {
		t.Fatalf("dnd: %+v, choice %s", res, r.choice)
	}
	if res := upd("dnd"); res.choiceChanged {
		t.Fatal("the same status is not a change")
	}
	if res := upd("offline"); res.choiceChanged || r.choice != presence.DND {
		t.Fatal("offline is not a choice")
	}
}

type recv struct {
	ch chan event
}

func (r recv) ephemeral(ev event) { r.ch <- ev }

func TestHub(t *testing.T) {
	nc := natstest.New(t)
	h := newHub(nc, testdb.Logger())
	a, b := recv{make(chan event, 4)}, recv{make(chan event, 4)}
	s1, s2 := "eph.guild."+uuid.NewString(), "eph.presence."+uuid.NewString()

	haveA, err := h.sync(a, nil, []string{s1, s2})
	if err != nil {
		t.Fatal(err)
	}
	haveB, _ := h.sync(b, nil, []string{s1})
	if h.subscribed() != 2 {
		t.Fatalf("%d NATS subscriptions, want one per subject", h.subscribed())
	}
	pub := func(subj, data string) {
		msg := &nats.Msg{Subject: subj, Data: []byte(data), Header: nats.Header{}}
		msg.Header.Set(events.HeaderType, events.TypingStart)
		if err := nc.PublishMsg(msg); err != nil {
			t.Fatal(err)
		}
		_ = nc.Flush()
	}
	recvd := func(r recv) []string {
		var out []string
		deadline := time.After(300 * time.Millisecond)
		for {
			select {
			case ev := <-r.ch:
				out = append(out, ev.Subject+" "+ev.Type)
			case <-deadline:
				return out
			}
		}
	}
	pub(s1, `{}`)
	pub(s1, `not json`)
	if got := recvd(a); !slices.Equal(got, []string{s1 + " " + events.TypingStart}) {
		t.Fatalf("a got %v", got)
	}
	if got := recvd(b); len(got) != 1 {
		t.Fatalf("b got %v", got)
	}

	// b leaves: the shared subject stays for a; a leaves: it is gone.
	haveB, _ = h.sync(b, haveB, nil)
	if h.subscribed() != 2 || len(haveB) != 0 {
		t.Fatal("a subject still in use was unsubscribed")
	}
	_, _ = h.sync(a, haveA, []string{s2})
	if h.subscribed() != 1 {
		t.Fatalf("%d subscriptions after a dropped s1", h.subscribed())
	}
	pub(s1, `{}`)
	if got := recvd(a); len(got) != 0 {
		t.Fatalf("a still gets s1: %v", got)
	}
}
