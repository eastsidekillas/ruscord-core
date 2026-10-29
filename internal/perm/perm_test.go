package perm

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// Bit values are persisted. If this test fails, a released bit moved.
func TestBitValuesAreStable(t *testing.T) {
	golden := map[Permissions]uint64{
		ViewChannel: 1, SendMessages: 2, ReadMessageHistory: 4, AttachFiles: 8, AddReactions: 16,
		MentionEveryone: 32, ManageMessages: 64, Connect: 128, Speak: 256, Video: 512, Stream: 1024,
		StreamHQ: 2048, UseVoiceActivity: 4096, MuteMembers: 8192, DeafenMembers: 16384, MoveMembers: 32768,
		CreateInvite: 65536, ChangeNickname: 131072, ManageNicknames: 262144, KickMembers: 524288,
		BanMembers: 1048576, ManageChannels: 2097152, ManageRoles: 4194304, ManageGuild: 8388608,
		ViewAuditLog: 16777216, Administrator: 33554432,
	}
	for p, v := range golden {
		if uint64(p) != v {
			t.Errorf("%s = %d, want %d", p, uint64(p), v)
		}
	}
	if len(golden) != len(names) {
		t.Errorf("golden table has %d entries, names has %d: add the new bit to both", len(golden), len(names))
	}
	if uint64(All) != 1<<26-1 {
		t.Errorf("All = %b", uint64(All))
	}
}

func TestString(t *testing.T) {
	if got := (ViewChannel | Connect).String(); got != "VIEW_CHANNEL|CONNECT" {
		t.Errorf("String = %q", got)
	}
	if got := Permissions(0).String(); got != "NONE" {
		t.Errorf("String(0) = %q", got)
	}
	if got := Permissions(1 << 40).String(); got != "UNKNOWN" {
		t.Errorf("String(unknown bit) = %q", got)
	}
}

// fixture: a guild with roles mod (pos 2) and member (pos 1).
type fixture struct {
	g                   Guild
	owner, mod, plain   Member
	modRole, memberRole Role
}

func newFixture(everyone Permissions) fixture {
	gid := uuid.New()
	modRole := Role{ID: uuid.New(), Position: 2, Permissions: KickMembers | MuteMembers | ManageMessages | ManageRoles}
	memberRole := Role{ID: uuid.New(), Position: 1, Permissions: StreamHQ}
	f := fixture{
		g: Guild{
			ID:      gid,
			OwnerID: uuid.New(),
			Roles: map[uuid.UUID]Role{
				gid:           {ID: gid, Position: 0, Permissions: everyone},
				modRole.ID:    modRole,
				memberRole.ID: memberRole,
			},
		},
		modRole:    modRole,
		memberRole: memberRole,
	}
	f.owner = Member{UserID: f.g.OwnerID}
	f.mod = Member{UserID: uuid.New(), RoleIDs: []uuid.UUID{modRole.ID, memberRole.ID}}
	f.plain = Member{UserID: uuid.New(), RoleIDs: []uuid.UUID{memberRole.ID}}
	return f
}

func TestBase(t *testing.T) {
	f := newFixture(DefaultEveryone)
	tests := []struct {
		name string
		m    Member
		want Permissions
	}{
		{"owner gets all", f.owner, All},
		{"everyone only", Member{UserID: uuid.New()}, DefaultEveryone},
		{"roles add up", f.plain, DefaultEveryone | StreamHQ},
		{"mod", f.mod, DefaultEveryone | StreamHQ | KickMembers | MuteMembers | ManageMessages | ManageRoles},
		{"unknown role ignored", Member{UserID: uuid.New(), RoleIDs: []uuid.UUID{uuid.New()}}, DefaultEveryone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Base(f.g, tt.m); got != tt.want {
				t.Errorf("Base = %s, want %s", got, tt.want)
			}
		})
	}

	admin := Role{ID: uuid.New(), Position: 3, Permissions: Administrator}
	f.g.Roles[admin.ID] = admin
	if got := Base(f.g, Member{UserID: uuid.New(), RoleIDs: []uuid.UUID{admin.ID}}); got != All {
		t.Errorf("administrator Base = %s, want All", got)
	}

	// Unknown bits from a newer schema never leak into results.
	f.g.Roles[f.g.ID] = Role{ID: f.g.ID, Permissions: DefaultEveryone | 1<<50}
	if got := Base(f.g, Member{UserID: uuid.New()}); got&^All != 0 {
		t.Errorf("unknown bit leaked: %b", uint64(got))
	}
}

func TestInChannel(t *testing.T) {
	f := newFixture(DefaultEveryone)
	everyoneOW := func(allow, deny Permissions) Overwrite {
		return Overwrite{Type: TargetRole, TargetID: f.g.ID, Allow: allow, Deny: deny}
	}
	roleOW := func(r Role, allow, deny Permissions) Overwrite {
		return Overwrite{Type: TargetRole, TargetID: r.ID, Allow: allow, Deny: deny}
	}
	memberOW := func(m Member, allow, deny Permissions) Overwrite {
		return Overwrite{Type: TargetMember, TargetID: m.UserID, Allow: allow, Deny: deny}
	}

	tests := []struct {
		name    string
		m       Member
		ow      []Overwrite
		has     Permissions
		hasNot  Permissions
		isEmpty bool
	}{
		{name: "no overwrites keeps base", m: f.plain, has: DefaultEveryone | StreamHQ},
		{name: "private channel: everyone denied view", m: f.plain,
			ow: []Overwrite{everyoneOW(0, ViewChannel)}, isEmpty: true},
		{name: "role allow beats everyone deny", m: f.plain,
			ow: []Overwrite{everyoneOW(0, ViewChannel), roleOW(f.memberRole, ViewChannel, 0)}, has: ViewChannel | SendMessages},
		{name: "member deny beats role allow", m: f.plain,
			ow: []Overwrite{roleOW(f.memberRole, SendMessages, 0), memberOW(f.plain, 0, SendMessages)}, has: ViewChannel, hasNot: SendMessages},
		{name: "member allow beats role deny", m: f.plain,
			ow: []Overwrite{roleOW(f.memberRole, 0, Speak), memberOW(f.plain, Speak, 0)}, has: Speak},
		{name: "allow wins over deny across a member's roles", m: f.mod,
			ow: []Overwrite{roleOW(f.modRole, 0, SendMessages), roleOW(f.memberRole, SendMessages, 0)}, has: SendMessages},
		{name: "overwrite for a role the member lacks is ignored", m: f.plain,
			ow: []Overwrite{roleOW(f.modRole, 0, ViewChannel)}, has: ViewChannel},
		{name: "overwrite for another member is ignored", m: f.plain,
			ow: []Overwrite{memberOW(f.mod, 0, ViewChannel)}, has: ViewChannel},
		{name: "read-only channel drops attachments and mentions", m: f.plain,
			ow: []Overwrite{everyoneOW(MentionEveryone, SendMessages)}, has: ViewChannel | ReadMessageHistory, hasNot: SendMessages | AttachFiles | MentionEveryone},
		{name: "no connect drops all voice permissions", m: f.mod,
			ow: []Overwrite{everyoneOW(0, Connect)}, hasNot: Connect | Speak | Video | Stream | StreamHQ | MuteMembers},
		{name: "no stream drops stream hq", m: f.plain,
			ow: []Overwrite{everyoneOW(0, Stream)}, has: Connect, hasNot: Stream | StreamHQ},
		{name: "owner ignores overwrites", m: f.owner,
			ow: []Overwrite{everyoneOW(0, All), memberOW(f.owner, 0, All)}, has: All},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InChannel(f.g, tt.m, tt.ow)
			if tt.isEmpty && got != 0 {
				t.Fatalf("got %s, want NONE", got)
			}
			if !got.Has(tt.has) {
				t.Errorf("got %s, missing %s", got, tt.has&^got)
			}
			if got&tt.hasNot != 0 {
				t.Errorf("got %s, must not have %s", got, got&tt.hasNot)
			}
		})
	}
}

func TestAdministratorIgnoresOverwrites(t *testing.T) {
	f := newFixture(DefaultEveryone)
	admin := Role{ID: uuid.New(), Position: 5, Permissions: Administrator}
	f.g.Roles[admin.ID] = admin
	m := Member{UserID: uuid.New(), RoleIDs: []uuid.UUID{admin.ID}}
	ow := []Overwrite{{Type: TargetMember, TargetID: m.UserID, Deny: All}}
	if got := InChannel(f.g, m, ow); got != All {
		t.Fatalf("admin in channel = %s, want All", got)
	}
}

// Properties that must hold for any input, checked on random guilds.
func TestInvariants(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test data, not security
	randPerm := func() Permissions { return Permissions(rng.Uint64()) & (All &^ Administrator) }
	for i := 0; i < 2000; i++ {
		f := newFixture(randPerm())
		f.g.Roles[f.memberRole.ID] = Role{ID: f.memberRole.ID, Position: 1, Permissions: randPerm()}
		var ow []Overwrite
		for j := 0; j < rng.IntN(4); j++ {
			target := []uuid.UUID{f.g.ID, f.memberRole.ID, f.plain.UserID}[rng.IntN(3)]
			typ := TargetRole
			if target == f.plain.UserID {
				typ = TargetMember
			}
			ow = append(ow, Overwrite{Type: typ, TargetID: target, Allow: randPerm(), Deny: randPerm()})
		}
		got := InChannel(f.g, f.plain, ow)

		if got&^All != 0 {
			t.Fatalf("unknown bits in result: %b", uint64(got))
		}
		if got != 0 && !got.Has(ViewChannel) {
			t.Fatalf("non-empty permissions without VIEW_CHANNEL: %s", got)
		}
		if !got.Has(SendMessages) && got&dependsOnSend != 0 {
			t.Fatalf("send-dependent bits without SEND_MESSAGES: %s", got)
		}
		if !got.Has(Connect) && got&dependsOnConnect != 0 {
			t.Fatalf("voice bits without CONNECT: %s", got)
		}
		if got.Has(Administrator) {
			t.Fatal("ADMINISTRATOR must never appear in channel permissions of a non-admin")
		}
		if InChannel(f.g, f.owner, ow) != All {
			t.Fatal("owner must always have All")
		}
		// A member-level deny is final, whatever the role overwrites say.
		bit := Permissions(1) << rng.IntN(25)
		withDeny := slices.DeleteFunc(slices.Clone(ow), func(o Overwrite) bool { return o.Type == TargetMember })
		withDeny = append(withDeny, Overwrite{Type: TargetMember, TargetID: f.plain.UserID, Deny: bit})
		if InChannel(f.g, f.plain, withDeny).Has(bit) {
			t.Fatalf("member deny of %s did not stick", bit)
		}
	}
}

func TestInDM(t *testing.T) {
	if InDM(false, false) != 0 {
		t.Error("outsider must have no access to a DM")
	}
	blocked := InDM(true, true)
	if blocked.Has(SendMessages) || blocked.Has(Connect) || !blocked.Has(ReadMessageHistory) {
		t.Errorf("blocked DM = %s", blocked)
	}
	ok := InDM(true, false)
	if !ok.Has(SendMessages | Connect | Speak | Video | Stream) {
		t.Errorf("DM = %s", ok)
	}
	if ok.Has(ManageMessages) || ok.Has(StreamHQ) {
		t.Errorf("DM must not grant moderation or HQ stream: %s", ok)
	}
}

func TestOutranks(t *testing.T) {
	f := newFixture(DefaultEveryone)
	everyoneOnly := Member{UserID: uuid.New()}
	tests := []struct {
		name          string
		actor, target Member
		want          bool
	}{
		{"owner over mod", f.owner, f.mod, true},
		{"mod over owner", f.mod, f.owner, false},
		{"mod over plain", f.mod, f.plain, true},
		{"plain over mod", f.plain, f.mod, false},
		{"equal rank", f.plain, Member{UserID: uuid.New(), RoleIDs: []uuid.UUID{f.memberRole.ID}}, false},
		{"plain over everyone-only", f.plain, everyoneOnly, true},
		{"owner over self", f.owner, f.owner, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Outranks(f.g, tt.actor, tt.target); got != tt.want {
				t.Errorf("Outranks = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCanAssignRole(t *testing.T) {
	f := newFixture(DefaultEveryone)
	lowRole := Role{ID: uuid.New(), Position: 1, Permissions: SendMessages}
	dangerous := Role{ID: uuid.New(), Position: 1, Permissions: BanMembers}
	higher := Role{ID: uuid.New(), Position: 9}
	f.g.Roles[lowRole.ID], f.g.Roles[dangerous.ID], f.g.Roles[higher.ID] = lowRole, dangerous, higher

	tests := []struct {
		name  string
		actor Member
		role  Role
		want  bool
	}{
		{"owner assigns anything", f.owner, higher, true},
		{"mod assigns lower role", f.mod, lowRole, true},
		{"mod cannot assign role above", f.mod, higher, false},
		{"mod cannot assign own top role", f.mod, f.modRole, false},
		{"mod cannot grant perms they lack", f.mod, dangerous, false},
		{"no MANAGE_ROLES", f.plain, lowRole, false},
		{"nobody assigns @everyone", f.mod, f.g.Roles[f.g.ID], false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanAssignRole(f.g, tt.actor, tt.role); got != tt.want {
				t.Errorf("CanAssignRole = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVoice(t *testing.T) {
	tests := []struct {
		name string
		p    Permissions
		want VoiceGrant
	}{
		{"no view", Connect | Speak, VoiceGrant{}},
		{"no connect", ViewChannel | Speak, VoiceGrant{}},
		{"listen only, push-to-talk", ViewChannel | Connect, VoiceGrant{CanJoin: true, PushToTalk: true}},
		{"speak", ViewChannel | Connect | Speak | UseVoiceActivity,
			VoiceGrant{CanJoin: true, Sources: []MediaSource{SourceMicrophone}}},
		{"everything", ViewChannel | Connect | Speak | Video | Stream | StreamHQ | UseVoiceActivity,
			VoiceGrant{CanJoin: true, HighQuality: true, Sources: []MediaSource{SourceMicrophone, SourceCamera, SourceScreen, SourceScreenAudio}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Voice(tt.p)
			if got.CanJoin != tt.want.CanJoin || got.PushToTalk != tt.want.PushToTalk ||
				got.HighQuality != tt.want.HighQuality || !slices.Equal(got.Sources, tt.want.Sources) {
				t.Errorf("Voice(%s) = %+v, want %+v", tt.p, got, tt.want)
			}
		})
	}
}
