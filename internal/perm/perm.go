// Package perm computes what a member may do in a guild or channel.
//
// It is pure: no database, no I/O. Every module (REST handlers, gateway
// event filtering, voice token grants) calls the same functions, so a
// permission rule exists in exactly one place.
package perm

import (
	"strings"

	"github.com/google/uuid"
)

// Permissions is a bit set. Values are stored in the database (roles and
// channel overwrites), so a bit's position must never change once released.
type Permissions uint64

const (
	ViewChannel        Permissions = 1 << 0
	SendMessages       Permissions = 1 << 1
	ReadMessageHistory Permissions = 1 << 2
	AttachFiles        Permissions = 1 << 3
	AddReactions       Permissions = 1 << 4
	MentionEveryone    Permissions = 1 << 5
	ManageMessages     Permissions = 1 << 6
	Connect            Permissions = 1 << 7
	Speak              Permissions = 1 << 8
	Video              Permissions = 1 << 9
	Stream             Permissions = 1 << 10 // screen share
	StreamHQ           Permissions = 1 << 11 // screen share above 720p
	UseVoiceActivity   Permissions = 1 << 12 // without it: push-to-talk only
	MuteMembers        Permissions = 1 << 13
	DeafenMembers      Permissions = 1 << 14
	MoveMembers        Permissions = 1 << 15
	CreateInvite       Permissions = 1 << 16
	ChangeNickname     Permissions = 1 << 17
	ManageNicknames    Permissions = 1 << 18
	KickMembers        Permissions = 1 << 19
	BanMembers         Permissions = 1 << 20
	ManageChannels     Permissions = 1 << 21
	ManageRoles        Permissions = 1 << 22
	ManageGuild        Permissions = 1 << 23
	ViewAuditLog       Permissions = 1 << 24
	Administrator      Permissions = 1 << 25

	lastBit = Administrator
)

// All is every defined permission.
const All = lastBit<<1 - 1

// DefaultEveryone is what the @everyone role gets in a new guild.
const DefaultEveryone = ViewChannel | SendMessages | ReadMessageHistory | AttachFiles | AddReactions |
	Connect | Speak | Video | Stream | UseVoiceActivity | CreateInvite | ChangeNickname

// textOnly and voiceOnly are removed together with the permission they depend on.
const (
	dependsOnSend    = MentionEveryone | AttachFiles
	dependsOnConnect = Speak | Video | Stream | StreamHQ | UseVoiceActivity | MuteMembers | DeafenMembers | MoveMembers
)

// Has reports whether p contains every bit of need.
func (p Permissions) Has(need Permissions) bool { return p&need == need }

var names = []struct {
	bit  Permissions
	name string
}{
	{ViewChannel, "VIEW_CHANNEL"}, {SendMessages, "SEND_MESSAGES"}, {ReadMessageHistory, "READ_MESSAGE_HISTORY"},
	{AttachFiles, "ATTACH_FILES"}, {AddReactions, "ADD_REACTIONS"}, {MentionEveryone, "MENTION_EVERYONE"},
	{ManageMessages, "MANAGE_MESSAGES"}, {Connect, "CONNECT"}, {Speak, "SPEAK"}, {Video, "VIDEO"},
	{Stream, "STREAM"}, {StreamHQ, "STREAM_HQ"}, {UseVoiceActivity, "USE_VOICE_ACTIVITY"},
	{MuteMembers, "MUTE_MEMBERS"}, {DeafenMembers, "DEAFEN_MEMBERS"}, {MoveMembers, "MOVE_MEMBERS"},
	{CreateInvite, "CREATE_INVITE"}, {ChangeNickname, "CHANGE_NICKNAME"}, {ManageNicknames, "MANAGE_NICKNAMES"},
	{KickMembers, "KICK_MEMBERS"}, {BanMembers, "BAN_MEMBERS"}, {ManageChannels, "MANAGE_CHANNELS"},
	{ManageRoles, "MANAGE_ROLES"}, {ManageGuild, "MANAGE_GUILD"}, {ViewAuditLog, "VIEW_AUDIT_LOG"},
	{Administrator, "ADMINISTRATOR"},
}

func (p Permissions) String() string {
	if p == 0 {
		return "NONE"
	}
	var out []string
	for _, n := range names {
		if p&n.bit != 0 {
			out = append(out, n.name)
		}
	}
	if rest := p &^ All; rest != 0 {
		out = append(out, "UNKNOWN")
	}
	return strings.Join(out, "|")
}

// Role is a guild role. The @everyone role has the guild's ID.
type Role struct {
	ID          uuid.UUID
	Position    int // higher outranks lower; @everyone is 0
	Permissions Permissions
}

// Guild is the guild-level data the computation needs.
type Guild struct {
	ID      uuid.UUID // also the @everyone role ID
	OwnerID uuid.UUID
	Roles   map[uuid.UUID]Role
}

// Member is a user's membership in a guild.
type Member struct {
	UserID  uuid.UUID
	RoleIDs []uuid.UUID // without @everyone, it is implied
}

type TargetType uint8

const (
	TargetRole TargetType = iota + 1
	TargetMember
)

// Overwrite adjusts permissions in one channel for a role or a member.
type Overwrite struct {
	Type     TargetType
	TargetID uuid.UUID
	Allow    Permissions
	Deny     Permissions
}

// Base returns the member's guild-wide permissions, before any channel
// overwrite. The owner and administrators get everything.
func Base(g Guild, m Member) Permissions {
	if m.UserID == g.OwnerID {
		return All
	}
	p := g.Roles[g.ID].Permissions
	for _, id := range m.RoleIDs {
		if r, ok := g.Roles[id]; ok {
			p |= r.Permissions
		}
	}
	if p.Has(Administrator) {
		return All
	}
	return p & All
}

// InChannel returns the member's permissions in a guild channel. Overwrites
// apply in a fixed order, each level overriding the previous one:
// @everyone, then all of the member's roles together, then the member.
// Within a level, allow wins over deny.
func InChannel(g Guild, m Member, overwrites []Overwrite) Permissions {
	p := Base(g, m)
	if p == All {
		return All
	}

	var (
		everyone            *Overwrite
		member              *Overwrite
		roleAllow, roleDeny Permissions
	)
	hasRole := make(map[uuid.UUID]bool, len(m.RoleIDs))
	for _, id := range m.RoleIDs {
		hasRole[id] = true
	}
	for i := range overwrites {
		ow := &overwrites[i]
		switch {
		case ow.Type == TargetRole && ow.TargetID == g.ID:
			everyone = ow
		case ow.Type == TargetRole && hasRole[ow.TargetID]:
			roleAllow |= ow.Allow
			roleDeny |= ow.Deny
		case ow.Type == TargetMember && ow.TargetID == m.UserID:
			member = ow
		}
	}

	if everyone != nil {
		p = (p &^ everyone.Deny) | everyone.Allow
	}
	p = (p &^ roleDeny) | roleAllow
	if member != nil {
		p = (p &^ member.Deny) | member.Allow
	}
	return implicit(p & All &^ Administrator)
}

// implicit removes permissions that make no sense without their base one.
func implicit(p Permissions) Permissions {
	if !p.Has(ViewChannel) {
		return 0
	}
	if !p.Has(SendMessages) {
		p &^= dependsOnSend
	}
	if !p.Has(Connect) {
		p &^= dependsOnConnect
	}
	if !p.Has(Stream) {
		p &^= StreamHQ
	}
	return p
}

// dmPermissions is what every recipient may do in a DM or group DM.
const dmPermissions = ViewChannel | SendMessages | ReadMessageHistory | AttachFiles | AddReactions |
	Connect | Speak | Video | Stream | UseVoiceActivity

// InDM returns a user's permissions in a direct-message channel. A blocked
// pair can still read history but cannot write or call.
func InDM(isRecipient, blocked bool) Permissions {
	switch {
	case !isRecipient:
		return 0
	case blocked:
		return ViewChannel | ReadMessageHistory
	default:
		return dmPermissions
	}
}

// HighestPosition is the position of the member's top role (0 = @everyone only).
func HighestPosition(g Guild, m Member) int {
	top := 0
	for _, id := range m.RoleIDs {
		if r, ok := g.Roles[id]; ok && r.Position > top {
			top = r.Position
		}
	}
	return top
}

// Outranks reports whether actor may act on target (kick, ban, edit roles,
// server mute): the owner outranks everyone, nobody outranks the owner, and
// otherwise the actor's top role must be strictly higher.
func Outranks(g Guild, actor, target Member) bool {
	switch {
	case target.UserID == g.OwnerID:
		return false
	case actor.UserID == g.OwnerID:
		return true
	default:
		return HighestPosition(g, actor) > HighestPosition(g, target)
	}
}

// CanAssignRole reports whether actor may grant or remove role: they need
// MANAGE_ROLES, the role must sit below their top role, and they cannot hand
// out permissions they do not have themselves.
func CanAssignRole(g Guild, actor Member, role Role) bool {
	if actor.UserID == g.OwnerID {
		return true
	}
	base := Base(g, actor)
	if !base.Has(ManageRoles) || role.ID == g.ID {
		return false
	}
	if role.Position >= HighestPosition(g, actor) {
		return false
	}
	return base.Has(role.Permissions & All)
}

// MediaSource is a kind of track a participant may publish in a voice room.
type MediaSource string

const (
	SourceMicrophone  MediaSource = "microphone"
	SourceCamera      MediaSource = "camera"
	SourceScreen      MediaSource = "screen_share"
	SourceScreenAudio MediaSource = "screen_share_audio"
)

// VoiceGrant is what the voice module puts into a LiveKit token. Admin rights
// in the room are never granted to clients: moderation goes through the server.
type VoiceGrant struct {
	CanJoin     bool
	Sources     []MediaSource
	PushToTalk  bool // client must use push-to-talk
	HighQuality bool // screen share above 720p allowed
}

// Voice maps channel permissions to a media grant.
func Voice(p Permissions) VoiceGrant {
	if !p.Has(ViewChannel | Connect) {
		return VoiceGrant{}
	}
	g := VoiceGrant{CanJoin: true, PushToTalk: !p.Has(UseVoiceActivity), HighQuality: p.Has(StreamHQ)}
	if p.Has(Speak) {
		g.Sources = append(g.Sources, SourceMicrophone)
	}
	if p.Has(Video) {
		g.Sources = append(g.Sources, SourceCamera)
	}
	if p.Has(Stream) {
		g.Sources = append(g.Sources, SourceScreen, SourceScreenAudio)
	}
	return g
}
