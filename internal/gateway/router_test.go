package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// fixture: a guild owned by someone else where "me" is a plain member.
// "secret" is hidden from @everyone and open to the "mod" role.
type fixture struct {
	me, authSID, owner, other uuid.UUID
	guildID, modRole          uuid.UUID
	general, secret, voice    guild.Channel
	dm                        uuid.UUID
}

func newFixture() fixture {
	f := fixture{me: uuid.New(), authSID: uuid.New(), owner: uuid.New(), other: uuid.New(),
		guildID: uuid.New(), modRole: uuid.New(), dm: uuid.New()}
	f.general = guild.Channel{ID: uuid.New(), GuildID: f.guildID, Type: guild.ChannelText, Name: "general", Overwrites: []guild.Overwrite{}}
	f.voice = guild.Channel{ID: uuid.New(), GuildID: f.guildID, Type: guild.ChannelVoice, Name: "voice", Overwrites: []guild.Overwrite{}}
	f.secret = guild.Channel{ID: uuid.New(), GuildID: f.guildID, Type: guild.ChannelText, Name: "secret",
		Overwrites: []guild.Overwrite{
			{Type: perm.TargetRole, ID: f.guildID, Deny: perm.ViewChannel},
			{Type: perm.TargetRole, ID: f.modRole, Allow: perm.ViewChannel},
		}}
	return f
}

func (f fixture) roles(everyone perm.Permissions) []guild.Role {
	return []guild.Role{
		{ID: f.guildID, GuildID: f.guildID, Name: "@everyone", Position: 0, Permissions: everyone},
		{ID: f.modRole, GuildID: f.guildID, Name: "mod", Position: 1, Permissions: perm.ManageGuild},
	}
}

func (f fixture) member(roles ...uuid.UUID) guild.Member {
	return guild.Member{GuildID: f.guildID, UserID: f.me, RoleIDs: append([]uuid.UUID{}, roles...), JoinedAt: time.Unix(0, 0).UTC()}
}

func (f fixture) snapshot(roles ...uuid.UUID) guild.Snapshot {
	return guild.Snapshot{
		Guild:    guild.Guild{ID: f.guildID, Name: "g", OwnerID: f.owner},
		Roles:    f.roles(perm.DefaultEveryone),
		Channels: []guild.Channel{f.general, f.secret, f.voice},
		Member:   f.member(roles...),
	}
}

type fakeLoader struct {
	snaps map[uuid.UUID]guild.Snapshot
	err   error
}

func (l fakeLoader) Snapshot(_ context.Context, _, guildID uuid.UUID) (guild.Snapshot, error) {
	if l.err != nil {
		return guild.Snapshot{}, l.err
	}
	s, ok := l.snaps[guildID]
	if !ok {
		return guild.Snapshot{}, httpx.ErrNotFound
	}
	return s, nil
}

func ev(subject, typ string, data any) event {
	return event{Subject: subject, Type: typ, Data: mustJSON(data)}
}

// got renders dispatches as "TYPE:<id>" for comparison, where id is the
// object id, channel_id for messages, or "-" when there is none.
func got(ds []dispatch) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		var p struct {
			ID        *uuid.UUID `json:"id"`
			ChannelID *uuid.UUID `json:"channel_id"`
		}
		_ = json.Unmarshal(d.D, &p)
		id := "-"
		switch {
		case isMessage(d.T) && p.ChannelID != nil:
			id = p.ChannelID.String()
		case p.ID != nil:
			id = p.ID.String()
		}
		out = append(out, d.T+":"+id)
	}
	return out
}

func TestRoute(t *testing.T) {
	f := newFixture()
	gt, ut, dmt := events.GuildTopic(f.guildID), events.UserTopic(f.me), events.ChannelTopic(f.dm)
	msgIn := func(ch uuid.UUID) map[string]any {
		return map[string]any{"id": uuid.New(), "channel_id": ch, "guild_id": f.guildID}
	}
	newGuild := uuid.New()
	hiddenNew := guild.Channel{ID: uuid.New(), GuildID: f.guildID, Name: "hidden-new",
		Overwrites: []guild.Overwrite{{Type: perm.TargetMember, ID: f.me, Deny: perm.ViewChannel}}}
	openSecret := f.secret
	openSecret.Overwrites = []guild.Overwrite{}
	hideGeneral := f.general
	hideGeneral.Overwrites = []guild.Overwrite{{Type: perm.TargetRole, ID: f.guildID, Deny: perm.ViewChannel}}
	noView := f.roles(perm.DefaultEveryone &^ perm.ViewChannel)
	invite := map[string]any{"code": "abcdefgh", "guild_id": f.guildID, "channel_id": f.general.ID}

	cases := []struct {
		name        string
		roles       []uuid.UUID // my roles in the guild
		loader      fakeLoader
		in          event
		want        []string
		resubscribe bool
		revoked     bool
	}{
		// messages
		{name: "message in a visible channel", in: ev(gt, events.MessageCreate, msgIn(f.general.ID)),
			want: []string{events.MessageCreate + ":" + f.general.ID.String()}},
		{name: "message in a hidden channel is dropped", in: ev(gt, events.MessageCreate, msgIn(f.secret.ID))},
		{name: "edit in a hidden channel is dropped", in: ev(gt, events.MessageUpdate, msgIn(f.secret.ID))},
		{name: "delete in a hidden channel is dropped", in: ev(gt, events.MessageDelete, msgIn(f.secret.ID))},
		{name: "role opens the secret channel", roles: []uuid.UUID{f.modRole}, in: ev(gt, events.MessageCreate, msgIn(f.secret.ID)),
			want: []string{events.MessageCreate + ":" + f.secret.ID.String()}},
		{name: "message of an unknown channel is dropped", in: ev(gt, events.MessageCreate, msgIn(uuid.New()))},
		{name: "message in my DM", in: ev(dmt, events.MessageCreate, map[string]any{"id": uuid.New(), "channel_id": f.dm}),
			want: []string{events.MessageCreate + ":" + f.dm.String()}},
		{name: "message in someone else's DM is dropped", in: ev(events.ChannelTopic(uuid.New()), events.MessageCreate, map[string]any{"id": uuid.New()})},
		{name: "non-message event on a DM topic is dropped", in: ev(dmt, events.ChannelUpdate, map[string]any{"id": f.dm})},

		// channel visibility
		{name: "channel update stays visible", in: ev(gt, events.ChannelUpdate, f.general),
			want: []string{events.ChannelUpdate + ":" + f.general.ID.String()}},
		{name: "overwrite hides a channel", in: ev(gt, events.ChannelUpdate, hideGeneral),
			want: []string{events.ChannelDelete + ":" + f.general.ID.String()}},
		{name: "overwrite reveals a channel", in: ev(gt, events.ChannelUpdate, openSecret),
			want: []string{events.ChannelCreate + ":" + f.secret.ID.String()}},
		{name: "hidden channel stays hidden", in: ev(gt, events.ChannelUpdate, f.secret)},
		{name: "new hidden channel is not announced", in: ev(gt, events.ChannelCreate, hiddenNew)},
		{name: "new visible channel", in: ev(gt, events.ChannelCreate, guild.Channel{ID: f.dm, GuildID: f.guildID, Overwrites: []guild.Overwrite{}}),
			want: []string{events.ChannelCreate + ":" + f.dm.String()}},
		{name: "delete of a visible channel", in: ev(gt, events.ChannelDelete, map[string]any{"id": f.general.ID, "guild_id": f.guildID}),
			want: []string{events.ChannelDelete + ":" + f.general.ID.String()}},
		{name: "delete of a hidden channel is dropped", in: ev(gt, events.ChannelDelete, map[string]any{"id": f.secret.ID, "guild_id": f.guildID})},

		// roles and members change what is visible
		{name: "everyone loses VIEW_CHANNEL", in: ev(gt, events.RoleUpdate, map[string]any{"guild_id": f.guildID, "roles": noView}),
			want: []string{events.RoleUpdate + ":-", events.ChannelDelete + ":" + f.general.ID.String(), events.ChannelDelete + ":" + f.voice.ID.String()}},
		{name: "role list without changes in view", in: ev(gt, events.RoleCreate, map[string]any{"guild_id": f.guildID, "roles": f.roles(perm.DefaultEveryone)}),
			want: []string{events.RoleCreate + ":-"}},
		{name: "I get the mod role", in: ev(gt, events.MemberUpdate, f.member(f.modRole)),
			want: []string{events.MemberUpdate + ":-", events.ChannelCreate + ":" + f.secret.ID.String()}},
		{name: "I lose the mod role", roles: []uuid.UUID{f.modRole}, in: ev(gt, events.MemberUpdate, f.member()),
			want: []string{events.MemberUpdate + ":-", events.ChannelDelete + ":" + f.secret.ID.String()}},
		{name: "someone else gets the mod role", in: ev(gt, events.MemberUpdate, guild.Member{GuildID: f.guildID, UserID: f.other, RoleIDs: []uuid.UUID{f.modRole}}),
			want: []string{events.MemberUpdate + ":-"}},
		{name: "I become the owner", in: ev(gt, events.GuildUpdate, guild.Guild{ID: f.guildID, Name: "g", OwnerID: f.me}),
			want: []string{events.GuildUpdate + ":" + f.guildID.String(), events.ChannelCreate + ":" + f.secret.ID.String()}},
		{name: "member add", in: ev(gt, events.MemberAdd, guild.Member{GuildID: f.guildID, UserID: f.other}), want: []string{events.MemberAdd + ":-"}},
		{name: "member remove", in: ev(gt, events.MemberRemove, map[string]any{"guild_id": f.guildID, "user_id": f.other}), want: []string{events.MemberRemove + ":-"}},

		// invites need MANAGE_GUILD
		{name: "invite hidden without MANAGE_GUILD", in: ev(gt, events.InviteCreate, invite)},
		{name: "invite visible with MANAGE_GUILD", roles: []uuid.UUID{f.modRole}, in: ev(gt, events.InviteCreate, invite), want: []string{events.InviteCreate + ":-"}},
		{name: "invite delete visible with MANAGE_GUILD", roles: []uuid.UUID{f.modRole}, in: ev(gt, events.InviteDelete, invite), want: []string{events.InviteDelete + ":-"}},

		// guild membership changes the topics
		{name: "guild deleted", in: ev(gt, events.GuildDelete, map[string]any{"id": f.guildID}),
			want: []string{events.GuildDelete + ":" + f.guildID.String()}, resubscribe: true},
		{name: "kicked", in: ev(ut, events.GuildDelete, map[string]any{"id": f.guildID}),
			want: []string{events.GuildDelete + ":" + f.guildID.String()}, resubscribe: true},
		{name: "delete of a guild I am not in", in: ev(ut, events.GuildDelete, map[string]any{"id": newGuild})},
		{name: "joined a guild", loader: fakeLoader{snaps: map[uuid.UUID]guild.Snapshot{newGuild: {Guild: guild.Guild{ID: newGuild}, Member: guild.Member{UserID: f.me}}}},
			in: ev(ut, events.GuildCreate, map[string]any{"id": newGuild}), want: []string{events.GuildCreate + ":" + newGuild.String()}, resubscribe: true},
		{name: "joined and already left", in: ev(ut, events.GuildCreate, map[string]any{"id": newGuild})},
		{name: "event of a guild I am not in", in: ev(events.GuildTopic(newGuild), events.MessageCreate, map[string]any{"channel_id": uuid.New()})},

		// direct messages
		{name: "new DM", in: ev(ut, events.ChannelCreate, map[string]any{"id": uuid.Nil, "type": 1}), want: []string{events.ChannelCreate + ":" + uuid.Nil.String()}, resubscribe: true},
		{name: "known DM again", in: ev(ut, events.ChannelCreate, map[string]any{"id": f.dm, "type": 1}), want: []string{events.ChannelCreate + ":" + f.dm.String()}},
		{name: "left a group", in: ev(ut, events.ChannelDelete, map[string]any{"id": f.dm}), want: []string{events.ChannelDelete + ":" + f.dm.String()}, resubscribe: true},
		{name: "group renamed", in: ev(ut, events.ChannelUpdate, map[string]any{"id": f.dm}), want: []string{events.ChannelUpdate + ":" + f.dm.String()}},
		{name: "recipient added", in: ev(ut, events.RecipientAdd, map[string]any{"channel_id": f.dm}), want: []string{events.RecipientAdd + ":-"}},
		{name: "friend request", in: ev(ut, events.RelationshipAdd, map[string]any{"id": f.other, "type": 3}), want: []string{events.RelationshipAdd + ":" + f.other.String()}},
		{name: "friend removed", in: ev(ut, events.RelationshipRemove, map[string]any{"id": f.other, "type": 1}), want: []string{events.RelationshipRemove + ":" + f.other.String()}},

		// sessions and junk
		{name: "my session revoked", in: ev(ut, events.SessionRevoke, map[string]any{"session_id": f.authSID}), revoked: true},
		{name: "another session of mine revoked", in: ev(ut, events.SessionRevoke, map[string]any{"session_id": uuid.New()})},
		{name: "another user's topic", in: ev(events.UserTopic(f.other), events.RelationshipAdd, map[string]any{"id": f.me})},
		{name: "unknown event type", in: ev(ut, "SOMETHING_NEW", map[string]any{"id": f.me})},
		{name: "unknown guild event type", in: ev(gt, "SOMETHING_NEW", map[string]any{"id": f.me})},
		{name: "malformed subject", in: ev("guild.nope", events.MessageCreate, msgIn(f.general.ID))},
		{name: "malformed payload", in: event{Subject: gt, Type: events.ChannelUpdate, Data: json.RawMessage(`{"id": 5}`)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRouter(f.me, f.authSID, c.loader, []guild.Snapshot{f.snapshot(c.roles...)}, []uuid.UUID{f.dm})
			res, err := r.route(t.Context(), c.in)
			if err != nil {
				t.Fatal(err)
			}
			if g := got(res.out); !slices.Equal(g, c.want) {
				t.Errorf("dispatches = %v, want %v", g, c.want)
			}
			if res.resubscribe != c.resubscribe || res.revoked != c.revoked {
				t.Errorf("resubscribe=%v revoked=%v, want %v %v", res.resubscribe, res.revoked, c.resubscribe, c.revoked)
			}
		})
	}
}

// A hidden channel's name or topic must never reach the client, whatever
// the sequence of events.
func TestRouteNeverLeaksHiddenChannel(t *testing.T) {
	f := newFixture()
	gt := events.GuildTopic(f.guildID)
	r := newRouter(f.me, f.authSID, fakeLoader{}, []guild.Snapshot{f.snapshot()}, nil)
	renamed := f.secret
	renamed.Name, renamed.Topic = "plans-for-the-raid", "nobody should see this"
	steps := []event{
		ev(gt, events.ChannelUpdate, renamed),
		ev(gt, events.MessageCreate, map[string]any{"channel_id": f.secret.ID, "content": "nobody should see this"}),
		ev(gt, events.RoleUpdate, map[string]any{"roles": f.roles(perm.DefaultEveryone | perm.ManageMessages)}),
		ev(gt, events.ChannelDelete, map[string]any{"id": f.secret.ID, "guild_id": f.guildID}),
	}
	for _, s := range steps {
		res, err := r.route(t.Context(), s)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range res.out {
			if containsAny(string(d.D), f.secret.ID.String(), "plans-for-the-raid", "nobody should see this") {
				t.Fatalf("%s leaked the hidden channel: %s", d.T, d.D)
			}
		}
	}
}

func containsAny(s string, subs ...string) bool {
	return slices.ContainsFunc(subs, func(x string) bool { return strings.Contains(s, x) })
}

// A guild joined mid-session takes part in filtering at once, and its
// GUILD_CREATE is rendered from the fresh snapshot: hidden channels absent.
func TestRouteJoinThenFilter(t *testing.T) {
	f := newFixture()
	r := newRouter(f.me, f.authSID, fakeLoader{snaps: map[uuid.UUID]guild.Snapshot{f.guildID: f.snapshot()}}, nil, nil)
	if subs := r.subjects(); len(subs) != 1 || subs[0] != events.UserTopic(f.me) {
		t.Fatalf("subjects before join = %v", subs)
	}
	res, err := r.route(t.Context(), ev(events.UserTopic(f.me), events.GuildCreate, map[string]any{"id": f.guildID}))
	if err != nil || !res.resubscribe || len(res.out) != 1 {
		t.Fatalf("join: %+v %v", res, err)
	}
	var full guild.Full
	if err := json.Unmarshal(res.out[0].D, &full); err != nil {
		t.Fatal(err)
	}
	if len(full.Channels) != 2 || slices.ContainsFunc(full.Channels, func(c guild.Channel) bool { return c.ID == f.secret.ID }) {
		t.Fatalf("GUILD_CREATE channels = %+v, want general and voice", full.Channels)
	}
	if subs := r.subjects(); !slices.Contains(subs, events.GuildTopic(f.guildID)) {
		t.Fatalf("subjects after join = %v", subs)
	}
	res, _ = r.route(t.Context(), ev(events.GuildTopic(f.guildID), events.MessageCreate, map[string]any{"channel_id": f.secret.ID}))
	if len(res.out) != 0 {
		t.Fatal("message in a hidden channel delivered after join")
	}
}

func TestRouteLoaderFailureEndsSession(t *testing.T) {
	f := newFixture()
	r := newRouter(f.me, f.authSID, fakeLoader{err: errors.New("db down")}, nil, nil)
	if _, err := r.route(t.Context(), ev(events.UserTopic(f.me), events.GuildCreate, map[string]any{"id": f.guildID})); err == nil {
		t.Fatal("a failed load must be an error, not a silently skipped guild")
	}
}

// Events already contained in a snapshot must not be replayed on top of it.
// Mixing an old channel state with the newer roles of the snapshot could
// reveal a channel the user could see neither then nor now: here "secret"
// used to be open to mods, then closed to them, and only then did the user
// become a mod. The old CHANNEL_UPDATE (outbox id 5) is older than the
// snapshot (cursor 7) and is skipped; newer events apply; messages are not
// skipped, they change no state.
func TestRouteSkipsEventsInSnapshot(t *testing.T) {
	f := newFixture()
	gt := events.GuildTopic(f.guildID)
	snap := f.snapshot(f.modRole)
	closedToMods := f.secret
	closedToMods.Overwrites = []guild.Overwrite{
		{Type: perm.TargetRole, ID: f.guildID, Deny: perm.ViewChannel},
		{Type: perm.TargetRole, ID: f.modRole, Deny: perm.ViewChannel},
	}
	snap.Channels[1] = closedToMods
	snap.Cursor = 7
	r := newRouter(f.me, f.authSID, fakeLoader{}, []guild.Snapshot{snap}, nil)

	old := ev(gt, events.ChannelUpdate, f.secret) // still open to mods
	old.ID = 5
	if res, _ := r.route(t.Context(), old); len(res.out) != 0 {
		t.Fatalf("event older than the snapshot replayed: %v", got(res.out))
	}
	if _, visible := r.guilds[f.guildID].view.Channels[f.secret.ID]; visible {
		t.Fatal("old event changed the snapshot state")
	}
	msg := ev(gt, events.MessageCreate, map[string]any{"channel_id": f.general.ID})
	msg.ID = 6
	if res, _ := r.route(t.Context(), msg); len(res.out) != 1 {
		t.Fatal("a message must not be skipped by the snapshot cursor")
	}
	newer := ev(gt, events.ChannelUpdate, f.secret)
	newer.ID = 8
	if res, _ := r.route(t.Context(), newer); !slices.Equal(got(res.out), []string{events.ChannelCreate + ":" + f.secret.ID.String()}) {
		t.Fatalf("event newer than the snapshot: %v", got(res.out))
	}
}
