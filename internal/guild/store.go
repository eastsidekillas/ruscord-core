package guild

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
)

var errNotFound = errors.New("guild: not found")

// querier is satisfied by both the pool and a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func getGuild(ctx context.Context, q querier, id uuid.UUID) (Guild, error) {
	var g Guild
	err := q.QueryRow(ctx, `SELECT id, name, owner_id, created_at FROM guilds WHERE id = $1`, id).
		Scan(&g.ID, &g.Name, &g.OwnerID, &g.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Guild{}, errNotFound
	}
	return g, err
}

func listRoles(ctx context.Context, q querier, guildID uuid.UUID) ([]Role, error) {
	rows, err := q.Query(ctx,
		`SELECT id, guild_id, name, position, permissions, color FROM roles WHERE guild_id = $1 ORDER BY position, id`, guildID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Role, error) {
		var role Role
		var p int64
		err := r.Scan(&role.ID, &role.GuildID, &role.Name, &role.Position, &p, &role.Color)
		role.Permissions = perm.Permissions(p) //nolint:gosec // stored from a uint64 with the top bit unused
		return role, err
	})
}

// memberSelect reads members with their role ids; callers add WHERE, ORDER and LIMIT.
const memberSelect = `SELECT m.guild_id, m.user_id, m.nick, m.joined_at,
        COALESCE(array_agg(mr.role_id) FILTER (WHERE mr.role_id IS NOT NULL), '{}')
 FROM members m LEFT JOIN member_roles mr ON mr.guild_id = m.guild_id AND mr.user_id = m.user_id `

const memberGroup = ` GROUP BY m.guild_id, m.user_id `

func scanMember(r pgx.CollectableRow) (Member, error) {
	var m Member
	err := r.Scan(&m.GuildID, &m.UserID, &m.Nick, &m.JoinedAt, &m.RoleIDs)
	return m, err
}

func getMember(ctx context.Context, q querier, guildID, userID uuid.UUID) (Member, error) {
	rows, err := q.Query(ctx, memberSelect+`WHERE m.guild_id = $1 AND m.user_id = $2`+memberGroup, guildID, userID)
	if err != nil {
		return Member{}, err
	}
	m, err := pgx.CollectExactlyOneRow(rows, scanMember)
	if errors.Is(err, pgx.ErrNoRows) {
		return Member{}, errNotFound
	}
	return m, err
}

func listMembers(ctx context.Context, q querier, guildID, after uuid.UUID, limit int) ([]Member, error) {
	rows, err := q.Query(ctx,
		memberSelect+`WHERE m.guild_id = $1 AND m.user_id > $2`+memberGroup+`ORDER BY m.user_id LIMIT $3`,
		guildID, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanMember)
}

func listChannels(ctx context.Context, q querier, guildID uuid.UUID) ([]Channel, error) {
	rows, err := q.Query(ctx,
		`SELECT id, guild_id, type, COALESCE(name, ''), COALESCE(topic, ''), parent_id, position, user_limit, bitrate
		 FROM channels WHERE guild_id = $1 ORDER BY position, id`, guildID)
	if err != nil {
		return nil, err
	}
	chs, err := pgx.CollectRows(rows, scanChannel)
	if err != nil {
		return nil, err
	}
	ows, err := overwritesFor(ctx, q, `c.guild_id = $1`, guildID)
	if err != nil {
		return nil, err
	}
	for i := range chs {
		chs[i].Overwrites = overwritesOf(ows, chs[i].ID)
	}
	return chs, nil
}

func getChannel(ctx context.Context, q querier, id uuid.UUID) (Channel, error) {
	rows, err := q.Query(ctx,
		`SELECT id, guild_id, type, COALESCE(name, ''), COALESCE(topic, ''), parent_id, position, user_limit, bitrate
		 FROM channels WHERE id = $1 AND guild_id IS NOT NULL`, id)
	if err != nil {
		return Channel{}, err
	}
	ch, err := pgx.CollectExactlyOneRow(rows, scanChannel)
	if errors.Is(err, pgx.ErrNoRows) {
		return Channel{}, errNotFound
	}
	if err != nil {
		return Channel{}, err
	}
	ows, err := overwritesFor(ctx, q, `c.id = $1`, id)
	if err != nil {
		return Channel{}, err
	}
	ch.Overwrites = overwritesOf(ows, id)
	return ch, nil
}

func scanChannel(r pgx.CollectableRow) (Channel, error) {
	var c Channel
	err := r.Scan(&c.ID, &c.GuildID, &c.Type, &c.Name, &c.Topic, &c.ParentID, &c.Position, &c.UserLimit, &c.Bitrate)
	if c.Type != ChannelVoice {
		c.UserLimit, c.Bitrate = 0, 0
	}
	return c, err
}

// overwritesOf never returns nil, so the JSON is [] rather than null.
func overwritesOf(ows map[uuid.UUID][]Overwrite, channelID uuid.UUID) []Overwrite {
	if o := ows[channelID]; o != nil {
		return o
	}
	return []Overwrite{}
}

func overwritesFor(ctx context.Context, q querier, where string, arg any) (map[uuid.UUID][]Overwrite, error) {
	rows, err := q.Query(ctx,
		`SELECT o.channel_id, o.target_type, o.target_id, o.allow, o.deny
		 FROM permission_overwrites o JOIN channels c ON c.id = o.channel_id
		 WHERE `+where+` ORDER BY o.target_type, o.target_id`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]Overwrite{}
	for rows.Next() {
		var chID uuid.UUID
		var o Overwrite
		var allow, deny int64
		if err := rows.Scan(&chID, &o.Type, &o.ID, &allow, &deny); err != nil {
			return nil, err
		}
		o.Allow, o.Deny = perm.Permissions(allow), perm.Permissions(deny) //nolint:gosec // see listRoles
		out[chID] = append(out[chID], o)
	}
	return out, rows.Err()
}

// access is everything needed to evaluate a user's permissions in a guild.
type access struct {
	guild  Guild
	roles  []Role
	pg     perm.Guild
	member perm.Member
	full   Member
}

// loadAccess returns errNotFound when the guild does not exist or the user
// is not a member: callers answer 404 in both cases, so membership of a
// guild cannot be probed.
func loadAccess(ctx context.Context, q querier, guildID, userID uuid.UUID) (access, error) {
	m, err := getMember(ctx, q, guildID, userID)
	if err != nil {
		return access{}, err
	}
	a, err := loadGuildAccess(ctx, q, guildID, userID, m.RoleIDs)
	a.full = m
	return a, err
}

// newcomerAccess is what userID would get right after joining: @everyone
// only, no roles yet. Invites use it to check the channel they lead to.
func newcomerAccess(ctx context.Context, q querier, guildID, userID uuid.UUID) (access, error) {
	return loadGuildAccess(ctx, q, guildID, userID, nil)
}

func loadGuildAccess(ctx context.Context, q querier, guildID, userID uuid.UUID, roleIDs []uuid.UUID) (access, error) {
	g, err := getGuild(ctx, q, guildID)
	if err != nil {
		return access{}, err
	}
	roles, err := listRoles(ctx, q, guildID)
	if err != nil {
		return access{}, err
	}
	pg := perm.Guild{ID: g.ID, OwnerID: g.OwnerID, Roles: make(map[uuid.UUID]perm.Role, len(roles))}
	for _, r := range roles {
		pg.Roles[r.ID] = perm.Role{ID: r.ID, Position: r.Position, Permissions: r.Permissions}
	}
	return access{guild: g, roles: roles, pg: pg, member: perm.Member{UserID: userID, RoleIDs: roleIDs}}, nil
}

func (a access) base() perm.Permissions { return perm.Base(a.pg, a.member) }

func (a access) in(ch Channel) perm.Permissions {
	ows := make([]perm.Overwrite, len(ch.Overwrites))
	for i, o := range ch.Overwrites {
		ows[i] = o.perm()
	}
	return perm.InChannel(a.pg, a.member, ows)
}

func (a access) role(id uuid.UUID) (Role, bool) {
	for _, r := range a.roles {
		if r.ID == id {
			return r, true
		}
	}
	return Role{}, false
}

func (a access) permMember(m Member) perm.Member {
	return perm.Member{UserID: m.UserID, RoleIDs: m.RoleIDs}
}

func lockGuild(ctx context.Context, q querier, id uuid.UUID) error {
	var x uuid.UUID
	err := q.QueryRow(ctx, `SELECT id FROM guilds WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&x)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	return err
}
