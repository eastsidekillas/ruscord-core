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
	gs, err := guildsIn(ctx, q, []uuid.UUID{id})
	if err != nil {
		return Guild{}, err
	}
	g, ok := gs[id]
	if !ok {
		return Guild{}, errNotFound
	}
	return g, nil
}

func guildsIn(ctx context.Context, q querier, ids []uuid.UUID) (map[uuid.UUID]Guild, error) {
	rows, err := q.Query(ctx, `SELECT id, name, owner_id, created_at FROM guilds WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	gs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Guild, error) {
		var g Guild
		err := r.Scan(&g.ID, &g.Name, &g.OwnerID, &g.CreatedAt)
		return g, err
	})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]Guild, len(gs))
	for _, g := range gs {
		out[g.ID] = g
	}
	return out, nil
}

func listRoles(ctx context.Context, q querier, guildID uuid.UUID) ([]Role, error) {
	rs, err := rolesIn(ctx, q, []uuid.UUID{guildID})
	return rs[guildID], err
}

// rolesIn returns the roles of each guild, ordered by position.
func rolesIn(ctx context.Context, q querier, guildIDs []uuid.UUID) (map[uuid.UUID][]Role, error) {
	rows, err := q.Query(ctx,
		`SELECT id, guild_id, name, position, permissions, color FROM roles
		 WHERE guild_id = ANY($1) ORDER BY guild_id, position, id`, guildIDs)
	if err != nil {
		return nil, err
	}
	roles, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Role, error) {
		var role Role
		var p int64
		err := r.Scan(&role.ID, &role.GuildID, &role.Name, &role.Position, &p, &role.Color)
		role.Permissions = perm.Permissions(p) //nolint:gosec // stored from a uint64 with the top bit unused
		return role, err
	})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID][]Role, len(guildIDs))
	for _, r := range roles {
		out[r.GuildID] = append(out[r.GuildID], r)
	}
	return out, nil
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
	chs, err := channelsIn(ctx, q, []uuid.UUID{guildID})
	return chs[guildID], err
}

// channelsIn returns the channels of each guild with their overwrites,
// ordered by position.
func channelsIn(ctx context.Context, q querier, guildIDs []uuid.UUID) (map[uuid.UUID][]Channel, error) {
	rows, err := q.Query(ctx,
		`SELECT id, guild_id, type, COALESCE(name, ''), COALESCE(topic, ''), parent_id, position, user_limit, bitrate
		 FROM channels WHERE guild_id = ANY($1) ORDER BY guild_id, position, id`, guildIDs)
	if err != nil {
		return nil, err
	}
	chs, err := pgx.CollectRows(rows, scanChannel)
	if err != nil {
		return nil, err
	}
	ows, err := overwritesFor(ctx, q, `c.guild_id = ANY($1)`, guildIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID][]Channel, len(guildIDs))
	for _, ch := range chs {
		ch.Overwrites = overwritesOf(ows, ch.ID)
		out[ch.GuildID] = append(out[ch.GuildID], ch)
	}
	return out, nil
}

func getChannel(ctx context.Context, q querier, id uuid.UUID) (Channel, error) {
	rows, err := q.Query(ctx,
		`SELECT id, guild_id, type, COALESCE(name, ''), COALESCE(topic, ''), parent_id, position, user_limit, bitrate
		 FROM channels WHERE id = $1`, id)
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
	return access{guild: g, roles: roles, pg: permGuild(g, roles), member: perm.Member{UserID: userID, RoleIDs: roleIDs}}, nil
}

// permGuild is the input of internal/perm for a guild and its roles.
func permGuild(g Guild, roles []Role) perm.Guild {
	pg := perm.Guild{ID: g.ID, OwnerID: g.OwnerID, Roles: make(map[uuid.UUID]perm.Role, len(roles))}
	for _, r := range roles {
		pg.Roles[r.ID] = perm.Role{ID: r.ID, Position: r.Position, Permissions: r.Permissions}
	}
	return pg
}

// channelPerm is what member may do in ch.
func channelPerm(pg perm.Guild, member perm.Member, ch Channel) perm.Permissions {
	ows := make([]perm.Overwrite, len(ch.Overwrites))
	for i, o := range ch.Overwrites {
		ows[i] = o.perm()
	}
	return perm.InChannel(pg, member, ows)
}

func (a access) base() perm.Permissions { return perm.Base(a.pg, a.member) }

func (a access) in(ch Channel) perm.Permissions { return channelPerm(a.pg, a.member, ch) }

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
