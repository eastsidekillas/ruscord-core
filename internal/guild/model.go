// Package guild owns guilds ("servers"), their channels, roles, members and
// channel permission overwrites. It loads permission inputs from the database
// and evaluates them with internal/perm.
package guild

import (
	"time"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
)

type ChannelType int16

// Channel types share the numbering of the client protocol; 1 and 3 are
// direct messages, owned by internal/social.
const (
	ChannelText     ChannelType = 0
	ChannelVoice    ChannelType = 2
	ChannelCategory ChannelType = 4
)

func (t ChannelType) inGuild() bool {
	return t == ChannelText || t == ChannelVoice || t == ChannelCategory
}

type Guild struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	OwnerID   uuid.UUID `json:"owner_id"`
	CreatedAt time.Time `json:"created_at"`
}

type Role struct {
	ID          uuid.UUID        `json:"id"`
	GuildID     uuid.UUID        `json:"guild_id"`
	Name        string           `json:"name"`
	Position    int              `json:"position"`
	Permissions perm.Permissions `json:"permissions"`
	Color       int              `json:"color"`
}

type Overwrite struct {
	Type  perm.TargetType  `json:"type"` // 1 role, 2 member
	ID    uuid.UUID        `json:"id"`
	Allow perm.Permissions `json:"allow"`
	Deny  perm.Permissions `json:"deny"`
}

type Channel struct {
	ID         uuid.UUID   `json:"id"`
	GuildID    uuid.UUID   `json:"guild_id"`
	Type       ChannelType `json:"type"`
	Name       string      `json:"name"`
	Topic      string      `json:"topic,omitempty"`
	ParentID   *uuid.UUID  `json:"parent_id,omitempty"`
	Position   int         `json:"position"`
	UserLimit  int         `json:"user_limit,omitempty"`
	Bitrate    int         `json:"bitrate,omitempty"`
	Overwrites []Overwrite `json:"permission_overwrites"`
}

type Member struct {
	GuildID  uuid.UUID   `json:"guild_id"`
	UserID   uuid.UUID   `json:"user_id"`
	Nick     *string     `json:"nick"`
	RoleIDs  []uuid.UUID `json:"roles"`
	JoinedAt time.Time   `json:"joined_at"`
	User     any         `json:"user,omitempty"`
}

// Full is a guild as one member sees it: channels they cannot view are left
// out, and Permissions are their guild-wide permissions.
type Full struct {
	Guild
	Roles       []Role           `json:"roles"`
	Channels    []Channel        `json:"channels"`
	Member      *Member          `json:"member"`
	Permissions perm.Permissions `json:"permissions"`
	// ChannelPermissions are the member's permissions in each channel of
	// Channels, computed by internal/perm, so clients never reimplement the
	// rules (docs/adr/0017-client-permissions.md).
	ChannelPermissions map[uuid.UUID]perm.Permissions `json:"channel_permissions"`
}

func (o Overwrite) perm() perm.Overwrite {
	return perm.Overwrite{Type: o.Type, TargetID: o.ID, Allow: o.Allow, Deny: o.Deny}
}

// Limits keep a single guild or user from growing without bound.
const (
	maxGuildsPerUser    = 100
	maxChannelsPerGuild = 500
	maxRolesPerGuild    = 250
)
