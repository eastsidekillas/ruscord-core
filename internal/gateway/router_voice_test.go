package gateway

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/guild"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
)

// voiceOut renders dispatches as "TYPE:<user>@<channel>" for voice states
// ("left" for a null channel) and "TYPE:<id>" otherwise.
func voiceOut(ds []dispatch) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		if d.T != events.VoiceStateUpdate {
			out = append(out, got([]dispatch{d})...)
			continue
		}
		var st guild.VoiceState
		_ = json.Unmarshal(d.D, &st)
		where := "left"
		if st.ChannelID != nil {
			where = st.ChannelID.String()
		}
		out = append(out, d.T+":"+st.UserID.String()+"@"+where)
	}
	return out
}

// Voice states reach a session only for voice channels it can view: moving
// into a hidden channel looks like leaving, and a channel that becomes
// visible brings the states of those inside.
func TestRouteVoiceStates(t *testing.T) {
	f := newFixture()
	gt := events.GuildTopic(f.guildID)
	hiddenVoice := guild.Channel{ID: uuid.New(), GuildID: f.guildID, Type: guild.ChannelVoice, Name: "hidden voice",
		Overwrites: []guild.Overwrite{{Type: perm.TargetRole, ID: f.guildID, Deny: perm.ViewChannel}}}
	openedVoice := hiddenVoice
	openedVoice.Overwrites = []guild.Overwrite{}
	someone := uuid.New()
	in := func(user uuid.UUID, ch *uuid.UUID) guild.VoiceState {
		return guild.VoiceState{UserID: user, GuildID: f.guildID, ChannelID: ch}
	}
	vs := func(st guild.VoiceState) event { return ev(gt, events.VoiceStateUpdate, st) }
	at := func(user uuid.UUID, ch uuid.UUID) string {
		return events.VoiceStateUpdate + ":" + user.String() + "@" + ch.String()
	}
	left := func(user uuid.UUID) string { return events.VoiceStateUpdate + ":" + user.String() + "@left" }

	cases := []struct {
		name   string
		before []guild.VoiceState // in the snapshot
		in     event
		want   []string
		after  int // voice states kept by the router
	}{
		{name: "enters a visible channel", in: vs(in(someone, &f.voice.ID)), want: []string{at(someone, f.voice.ID)}, after: 1},
		{name: "enters a hidden channel", in: vs(in(someone, &hiddenVoice.ID)), after: 1},
		{name: "visible to hidden looks like leaving", before: []guild.VoiceState{in(someone, &f.voice.ID)},
			in: vs(in(someone, &hiddenVoice.ID)), want: []string{left(someone)}, after: 1},
		{name: "hidden to visible", before: []guild.VoiceState{in(someone, &hiddenVoice.ID)},
			in: vs(in(someone, &f.voice.ID)), want: []string{at(someone, f.voice.ID)}, after: 1},
		{name: "leaves a visible channel", before: []guild.VoiceState{in(someone, &f.voice.ID)},
			in: vs(in(someone, nil)), want: []string{left(someone)}, after: 0},
		{name: "leaves a hidden channel", before: []guild.VoiceState{in(someone, &hiddenVoice.ID)},
			in: vs(in(someone, nil)), after: 0},
		{name: "mutes in a visible channel", before: []guild.VoiceState{in(someone, &f.voice.ID)},
			in:   vs(guild.VoiceState{UserID: someone, GuildID: f.guildID, ChannelID: &f.voice.ID, SelfMute: true}),
			want: []string{at(someone, f.voice.ID)}, after: 1},
		{name: "a hidden channel opens: its people come with it", before: []guild.VoiceState{in(someone, &hiddenVoice.ID), in(f.owner, &f.voice.ID)},
			in:   ev(gt, events.ChannelUpdate, openedVoice),
			want: []string{events.ChannelCreate + ":" + hiddenVoice.ID.String(), at(someone, hiddenVoice.ID), typePermissionsUpdate + ":-"}, after: 2},
		{name: "a voice channel is deleted: its states go", before: []guild.VoiceState{in(someone, &f.voice.ID)},
			in:   ev(gt, events.ChannelDelete, map[string]any{"id": f.voice.ID, "guild_id": f.guildID}),
			want: []string{events.ChannelDelete + ":" + f.voice.ID.String(), typePermissionsUpdate + ":-"}, after: 0},
		{name: "another guild in the payload", in: vs(guild.VoiceState{UserID: someone, GuildID: uuid.New(), ChannelID: &f.voice.ID})},
		{name: "malformed", in: event{Subject: gt, Type: events.VoiceStateUpdate, Data: json.RawMessage(`{"user_id": 5}`)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := f.snapshot()
			snap.Channels = append(snap.Channels, hiddenVoice)
			snap.VoiceStates = slices.Clone(c.before)
			r := newRouter(f.me, f.authSID, fakeLoader{}, []guild.Snapshot{snap}, nil)
			res, err := r.route(t.Context(), c.in)
			if err != nil {
				t.Fatal(err)
			}
			if g := voiceOut(res.out); !slices.Equal(g, c.want) {
				t.Errorf("dispatches = %v, want %v", g, c.want)
			}
			if n := len(r.guilds[f.guildID].snap.VoiceStates); n != c.after {
				t.Errorf("router keeps %d voice states, want %d", n, c.after)
			}
		})
	}
}

// GUILD_CREATE and READY show the states of visible channels only.
func TestFullHidesVoiceStatesOfHiddenChannels(t *testing.T) {
	f := newFixture()
	hiddenVoice := guild.Channel{ID: uuid.New(), GuildID: f.guildID, Type: guild.ChannelVoice, Name: "hidden voice",
		Overwrites: []guild.Overwrite{{Type: perm.TargetRole, ID: f.guildID, Deny: perm.ViewChannel}}}
	snap := f.snapshot()
	snap.Channels = append(snap.Channels, hiddenVoice)
	snap.VoiceStates = []guild.VoiceState{
		{UserID: uuid.New(), GuildID: f.guildID, ChannelID: &f.voice.ID},
		{UserID: uuid.New(), GuildID: f.guildID, ChannelID: &hiddenVoice.ID},
	}
	full := newGuildState(snap).full()
	if len(full.VoiceStates) != 1 || *full.VoiceStates[0].ChannelID != f.voice.ID {
		t.Fatalf("voice states %+v", full.VoiceStates)
	}
}
