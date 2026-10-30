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
	"github.com/jackc/pgx/v5"
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
	InviteCreate  = "INVITE_CREATE"
	InviteDelete  = "INVITE_DELETE"

	RelationshipAdd    = "RELATIONSHIP_ADD"
	RelationshipRemove = "RELATIONSHIP_REMOVE"
	RecipientAdd       = "CHANNEL_RECIPIENT_ADD"
	RecipientRemove    = "CHANNEL_RECIPIENT_REMOVE"

	MessageCreate = "MESSAGE_CREATE"
	MessageUpdate = "MESSAGE_UPDATE"
	MessageDelete = "MESSAGE_DELETE"

	// UserUpdate goes to the user's own topic when their account changes
	// (for now, the chosen status). Carries the full private user object.
	UserUpdate = "USER_UPDATE"

	// SessionRevoke goes to the user topic when a sign-in session is revoked
	// (logout, refresh-token reuse). It is internal: the gateway closes the
	// connections of that session and never forwards it to clients.
	SessionRevoke = "SESSION_REVOKE"
)

// Topics. Gateway nodes subscribe to the guilds and users of their sessions.
func GuildTopic(id uuid.UUID) string { return "guild." + id.String() }
func UserTopic(id uuid.UUID) string  { return "user." + id.String() }

// ChannelTopic carries events of a direct-message channel to its recipients.
func ChannelTopic(id uuid.UUID) string { return "channel." + id.String() }

type Event struct {
	Topic string
	Type  string
	Data  any
}

// LatestIDs returns the id of the newest outbox row of each topic visible to
// q; topics without rows are absent. Inside a snapshot transaction it marks
// which events the snapshot already contains. Published rows are kept for an
// hour (Relay.keep), far longer than a gateway snapshot replays.
func LatestIDs(ctx context.Context, q querier, topics []string) (map[string]int64, error) {
	rows, err := q.Query(ctx, `SELECT topic, max(id) FROM outbox WHERE topic = ANY($1) GROUP BY topic`, topics)
	if err != nil {
		return nil, fmt.Errorf("events: latest ids: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int64, len(topics))
	for rows.Next() {
		var topic string
		var id int64
		if err := rows.Scan(&topic, &id); err != nil {
			return nil, err
		}
		out[topic] = id
	}
	return out, rows.Err()
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
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
	if len(evs) == 0 {
		return nil
	}
	// Delivered on commit and deduplicated within the transaction; wakes the relay.
	if _, err := tx.Exec(ctx, `SELECT pg_notify('`+notifyChannel+`', '')`); err != nil {
		return fmt.Errorf("events: notify: %w", err)
	}
	return nil
}
