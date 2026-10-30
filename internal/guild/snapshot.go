package guild

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/slicex"
)

// Snapshot is a guild with everything needed to work out what one member
// sees, including the channels hidden from them. The gateway keeps one per
// guild of a session and updates it from events, so it can tell when a
// channel becomes visible or hidden without asking the database.
type Snapshot struct {
	Guild    Guild
	Roles    []Role
	Channels []Channel
	Member   Member
	// Cursor is the id of the newest outbox event of the guild's topic that
	// the snapshot already contains. Every state change of a guild is written
	// under its lock, so ids up to Cursor are exactly the changes the
	// snapshot includes and must not be applied again (messages excepted:
	// they are written without the lock and change no state).
	Cursor int64
}

// View is what the member of a Snapshot may do.
type View struct {
	Base perm.Permissions
	// Channels holds the permissions in every channel the member can view;
	// hidden channels are absent.
	Channels map[uuid.UUID]perm.Permissions
}

// View evaluates the member's permissions with internal/perm.
func (s Snapshot) View() View {
	pg := permGuild(s.Guild, s.Roles)
	pm := perm.Member{UserID: s.Member.UserID, RoleIDs: s.Member.RoleIDs}
	v := View{Base: perm.Base(pg, pm), Channels: make(map[uuid.UUID]perm.Permissions, len(s.Channels))}
	for _, ch := range s.Channels {
		if p := channelPerm(pg, pm, ch); p.Has(perm.ViewChannel) {
			v.Channels[ch.ID] = p
		}
	}
	return v
}

// Full is the guild as the member sees it: hidden channels are left out.
func (s Snapshot) Full(v View) Full {
	visible := make([]Channel, 0, len(v.Channels))
	for _, ch := range s.Channels {
		if _, ok := v.Channels[ch.ID]; ok {
			visible = append(visible, ch)
		}
	}
	m := s.Member
	return Full{Guild: s.Guild, Roles: s.Roles, Channels: visible, Member: &m, Permissions: v.Base}
}

// Snapshots returns every guild userID is a member of, in join order.
func (s *Service) Snapshots(ctx context.Context, userID uuid.UUID) ([]Snapshot, error) {
	return s.snapshots(ctx, userID, nil)
}

// Snapshot returns one guild of userID; httpx.ErrNotFound when the guild
// does not exist or userID is not a member.
func (s *Service) Snapshot(ctx context.Context, userID, guildID uuid.UUID) (Snapshot, error) {
	ss, err := s.snapshots(ctx, userID, &guildID)
	if err != nil {
		return Snapshot{}, err
	}
	if len(ss) == 0 {
		return Snapshot{}, notFound(errNotFound)
	}
	return ss[0], nil
}

// snapshots loads guilds in a fixed number of queries inside one read-only
// repeatable-read transaction, so roles, channels and membership agree.
func (s *Service) snapshots(ctx context.Context, userID uuid.UUID, only *uuid.UUID) ([]Snapshot, error) {
	var out []Snapshot
	err := pgx.BeginTxFunc(ctx, s.pg, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			memberSelect+`WHERE m.user_id = $1 AND ($2::uuid IS NULL OR m.guild_id = $2)`+memberGroup+
				`ORDER BY m.joined_at, m.guild_id`, userID, only)
		if err != nil {
			return err
		}
		members, err := pgx.CollectRows(rows, scanMember)
		if err != nil || len(members) == 0 {
			return err
		}
		if err := s.attachUsers(ctx, slicex.Ptrs(members)); err != nil {
			return err
		}
		ids := make([]uuid.UUID, len(members))
		for i, m := range members {
			ids[i] = m.GuildID
		}
		guilds, err := guildsIn(ctx, tx, ids)
		if err != nil {
			return err
		}
		roles, err := rolesIn(ctx, tx, ids)
		if err != nil {
			return err
		}
		channels, err := channelsIn(ctx, tx, ids)
		if err != nil {
			return err
		}
		topics := make([]string, len(ids))
		for i, id := range ids {
			topics[i] = events.GuildTopic(id)
		}
		cursors, err := events.LatestIDs(ctx, tx, topics)
		if err != nil {
			return err
		}
		out = make([]Snapshot, 0, len(members))
		for _, m := range members {
			out = append(out, Snapshot{Guild: guilds[m.GuildID], Roles: roles[m.GuildID], Channels: channels[m.GuildID],
				Member: m, Cursor: cursors[events.GuildTopic(m.GuildID)]})
		}
		return nil
	})
	return out, err
}
