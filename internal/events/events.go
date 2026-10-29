// Package events defines the realtime events and writes them to the outbox.
//
// Writers call Write inside the transaction that makes the change, so an
// event exists if and only if the change was committed (docs/adr/0003).
package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Event types, as clients see them in DISPATCH.t.
const (
	GuildCreate   = "GUILD_CREATE"
	GuildUpdate   = "GUILD_UPDATE"
	GuildDelete   = "GUILD_DELETE"
	ChannelCreate = "CHANNEL_CREATE"
	ChannelUpdate = "CHANNEL_UPDATE"
	ChannelDelete = "CHANNEL_DELETE"
	RoleCreate    = "GUILD_ROLE_CREATE"
	RoleUpdate    = "GUILD_ROLE_UPDATE"
	RoleDelete    = "GUILD_ROLE_DELETE"
	MemberAdd     = "GUILD_MEMBER_ADD"
	MemberUpdate  = "GUILD_MEMBER_UPDATE"
	MemberRemove  = "GUILD_MEMBER_REMOVE"
)

// Topics. Gateway nodes subscribe to the guilds and users of their sessions.
func GuildTopic(id uuid.UUID) string { return "guild." + id.String() }
func UserTopic(id uuid.UUID) string  { return "user." + id.String() }

type Event struct {
	Topic string
	Type  string
	Data  any
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Write appends events to the outbox using tx.
func Write(ctx context.Context, tx execer, evs ...Event) error {
	for _, e := range evs {
		payload, err := json.Marshal(e.Data)
		if err != nil {
			return fmt.Errorf("events: marshal %s: %w", e.Type, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO outbox (topic, type, payload) VALUES ($1, $2, $3)`,
			e.Topic, e.Type, payload); err != nil {
			return fmt.Errorf("events: write %s: %w", e.Type, err)
		}
	}
	return nil
}
