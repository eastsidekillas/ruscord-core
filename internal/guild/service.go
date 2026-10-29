package guild

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/ratelimit"
)

var (
	// ErrNotFound is returned for missing objects and for objects the caller
	// cannot see, so existence cannot be probed.
	ErrNotFound       = httpx.NewError(http.StatusNotFound, "not_found", "Не найдено")
	errOwnerCannotGo  = httpx.NewError(http.StatusBadRequest, "owner_cannot_leave", "Владелец не может покинуть сервер, сначала удалите его")
	errTooManyGuilds  = httpx.NewError(http.StatusBadRequest, "too_many_guilds", "Достигнут лимит серверов")
	errTooManyChans   = httpx.NewError(http.StatusBadRequest, "too_many_channels", "Достигнут лимит каналов на сервере")
	errTooManyRoles   = httpx.NewError(http.StatusBadRequest, "too_many_roles", "Достигнут лимит ролей на сервере")
	errAlreadyMember  = httpx.NewError(http.StatusConflict, "already_member", "Пользователь уже на сервере")
	errEveryoneEdit   = httpx.NewError(http.StatusBadRequest, "everyone_role", "Роль @everyone нельзя переименовать или переместить, можно менять только её права")
	errEveryoneDelete = httpx.NewError(http.StatusBadRequest, "everyone_role", "Роль @everyone нельзя удалить")
	errEveryoneAssign = httpx.NewError(http.StatusBadRequest, "everyone_role", "Роль @everyone есть у всех участников, её нельзя выдать или снять")
)

var (
	ruleGuildCreate   = ratelimit.Rule{Name: "guild_create", Limit: 10, Window: time.Hour}
	ruleChannelCreate = ratelimit.Rule{Name: "channel_create", Limit: 30, Window: time.Minute}
	ruleRoleCreate    = ratelimit.Rule{Name: "role_create", Limit: 30, Window: time.Minute}
	ruleInviteCreate  = ratelimit.Rule{Name: "invite_create", Limit: 20, Window: time.Minute}
	// Lookups and joins by code are limited per user so codes cannot be enumerated.
	ruleInviteUse = ratelimit.Rule{Name: "invite_use", Limit: 30, Window: time.Minute}
)

// UserDirectory resolves public profiles; auth.Service implements it.
type UserDirectory interface {
	Users(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]auth.PublicUser, error)
}

type Service struct {
	pg      *pgxpool.Pool
	users   UserDirectory
	limiter *ratelimit.Limiter
	now     func() time.Time
}

func NewService(pg *pgxpool.Pool, users UserDirectory, limiter *ratelimit.Limiter) *Service {
	return &Service{pg: pg, users: users, limiter: limiter, now: time.Now}
}

func (s *Service) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pg, fn)
}

// mutate locks the guild, loads the caller's access and runs fn in one
// transaction. Every write to a guild goes through it, so permission checks
// and the change see the same state.
func (s *Service) mutate(ctx context.Context, guildID, userID uuid.UUID, fn func(pgx.Tx, access) error) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		if err := lockGuild(ctx, tx, guildID); err != nil {
			return notFound(err)
		}
		a, err := loadAccess(ctx, tx, guildID, userID)
		if err != nil {
			return notFound(err)
		}
		return fn(tx, a)
	})
}

func notFound(err error) error {
	if errors.Is(err, errNotFound) {
		return ErrNotFound
	}
	return err
}

func validation(fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	return httpx.Validation(fields)
}

func checkName(fields map[string]string, key, v string, minLen, maxLen int) {
	if n := utf8.RuneCountInString(v); n < minLen || n > maxLen {
		fields[key] = "Длина от " + itoa(minLen) + " до " + itoa(maxLen) + " символов"
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// optional distinguishes an absent JSON field from an explicit null.
type optional[T any] struct {
	Set   bool
	Value *T
}

func (o *optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// ---- guilds ----

type CreateGuildInput struct {
	Name string `json:"name"`
}

// Create makes a guild owned by userID with an @everyone role, a text and a
// voice channel.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, in CreateGuildInput) (Full, error) {
	in.Name = strings.TrimSpace(in.Name)
	fields := map[string]string{}
	checkName(fields, "name", in.Name, 2, 100)
	if err := validation(fields); err != nil {
		return Full{}, err
	}
	if err := s.limiter.Check(ctx, ruleGuildCreate, userID.String()); err != nil {
		return Full{}, err
	}

	var full Full
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := checkGuildCount(ctx, tx, userID); err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO guilds (id, name, owner_id) VALUES ($1, $2, $3)`, id, in.Name, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO roles (id, guild_id, name, position, permissions) VALUES ($1, $1, '@everyone', 0, $2)`,
			id, int64(perm.DefaultEveryone)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO members (guild_id, user_id) VALUES ($1, $2)`, id, userID); err != nil {
			return err
		}
		for i, c := range []struct {
			typ  ChannelType
			name string
		}{{ChannelText, "general"}, {ChannelVoice, "General"}} {
			chID, err := uuid.NewV7()
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO channels (id, guild_id, type, name, position) VALUES ($1, $2, $3, $4, $5)`,
				chID, id, c.typ, c.name, i); err != nil {
				return err
			}
		}
		full, err = s.full(ctx, tx, id, userID)
		if err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{Topic: events.UserTopic(userID), Type: events.GuildCreate, Data: full})
	})
	return full, err
}

func checkGuildCount(ctx context.Context, q querier, userID uuid.UUID) error {
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM members WHERE user_id = $1`, userID).Scan(&n); err != nil {
		return err
	}
	if n >= maxGuildsPerUser {
		return errTooManyGuilds
	}
	return nil
}

// full builds the guild as userID sees it: channels without VIEW_CHANNEL are left out.
func (s *Service) full(ctx context.Context, q querier, guildID, userID uuid.UUID) (Full, error) {
	a, err := loadAccess(ctx, q, guildID, userID)
	if err != nil {
		return Full{}, err
	}
	chs, err := listChannels(ctx, q, guildID)
	if err != nil {
		return Full{}, err
	}
	visible := make([]Channel, 0, len(chs))
	for _, ch := range chs {
		if a.in(ch).Has(perm.ViewChannel) {
			visible = append(visible, ch)
		}
	}
	m := a.full
	return Full{Guild: a.guild, Roles: a.roles, Channels: visible, Member: &m, Permissions: a.base()}, nil
}

// Get returns the guild if userID is a member.
func (s *Service) Get(ctx context.Context, userID, guildID uuid.UUID) (Full, error) {
	f, err := s.full(ctx, s.pg, guildID, userID)
	return f, notFound(err)
}

// ListMine returns the guilds userID is a member of, in join order.
func (s *Service) ListMine(ctx context.Context, userID uuid.UUID) ([]Guild, error) {
	rows, err := s.pg.Query(ctx,
		`SELECT g.id, g.name, g.owner_id, g.created_at
		 FROM guilds g JOIN members m ON m.guild_id = g.id
		 WHERE m.user_id = $1 ORDER BY m.joined_at, g.id`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Guild, error) {
		var g Guild
		err := r.Scan(&g.ID, &g.Name, &g.OwnerID, &g.CreatedAt)
		return g, err
	})
}

type UpdateGuildInput struct {
	Name *string `json:"name"`
}

func (s *Service) Update(ctx context.Context, userID, guildID uuid.UUID, in UpdateGuildInput) (Guild, error) {
	fields := map[string]string{}
	if in.Name != nil {
		*in.Name = strings.TrimSpace(*in.Name)
		checkName(fields, "name", *in.Name, 2, 100)
	}
	if err := validation(fields); err != nil {
		return Guild{}, err
	}
	var g Guild
	err := s.mutate(ctx, guildID, userID, func(tx pgx.Tx, a access) error {
		if !a.base().Has(perm.ManageGuild) {
			return httpx.ErrForbidden
		}
		g = a.guild
		if in.Name != nil {
			g.Name = *in.Name
		}
		if _, err := tx.Exec(ctx, `UPDATE guilds SET name = $2 WHERE id = $1`, g.ID, g.Name); err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{Topic: events.GuildTopic(g.ID), Type: events.GuildUpdate, Data: g})
	})
	return g, err
}

// Delete removes the guild. Only the owner may do it.
func (s *Service) Delete(ctx context.Context, userID, guildID uuid.UUID) error {
	return s.mutate(ctx, guildID, userID, func(tx pgx.Tx, a access) error {
		if a.guild.OwnerID != userID {
			return httpx.ErrForbidden
		}
		if _, err := tx.Exec(ctx, `DELETE FROM guilds WHERE id = $1`, guildID); err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{
			Topic: events.GuildTopic(guildID), Type: events.GuildDelete, Data: map[string]any{"id": guildID},
		})
	})
}

// ---- channels ----

type CreateChannelInput struct {
	Type      ChannelType `json:"type"`
	Name      string      `json:"name"`
	Topic     string      `json:"topic"`
	ParentID  *uuid.UUID  `json:"parent_id"`
	UserLimit int         `json:"user_limit"`
	Bitrate   int         `json:"bitrate"`
}

func validateChannel(fields map[string]string, typ ChannelType, name, topic string, userLimit, bitrate int) {
	checkName(fields, "name", name, 1, 100)
	if utf8.RuneCountInString(topic) > 1024 {
		fields["topic"] = "Не длиннее 1024 символов"
	}
	if userLimit < 0 || userLimit > 99 {
		fields["user_limit"] = "От 0 до 99"
	}
	if bitrate != 0 && (bitrate < 8000 || bitrate > 384000) {
		fields["bitrate"] = "От 8000 до 384000"
	}
	if typ != ChannelVoice && (userLimit != 0 || bitrate != 0) {
		fields["type"] = "user_limit и bitrate есть только у голосовых каналов"
	}
}

func (s *Service) CreateChannel(ctx context.Context, userID, guildID uuid.UUID, in CreateChannelInput) (Channel, error) {
	in.Name, in.Topic = strings.TrimSpace(in.Name), strings.TrimSpace(in.Topic)
	fields := map[string]string{}
	if !in.Type.inGuild() {
		fields["type"] = "Допустимы 0 (текст), 2 (голос), 4 (категория)"
	}
	validateChannel(fields, in.Type, in.Name, in.Topic, in.UserLimit, in.Bitrate)
	if in.Type == ChannelCategory && in.ParentID != nil {
		fields["parent_id"] = "Категория не может быть вложенной"
	}
	if err := validation(fields); err != nil {
		return Channel{}, err
	}
	if in.Bitrate == 0 {
		in.Bitrate = 64000
	}
	if err := s.limiter.Check(ctx, ruleChannelCreate, userID.String()); err != nil {
		return Channel{}, err
	}

	var ch Channel
	err := s.mutate(ctx, guildID, userID, func(tx pgx.Tx, a access) error {
		if !a.base().Has(perm.ManageChannels) {
			return httpx.ErrForbidden
		}
		var n, maxPos int
		if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(max(position), -1) FROM channels WHERE guild_id = $1`, guildID).
			Scan(&n, &maxPos); err != nil {
			return err
		}
		if n >= maxChannelsPerGuild {
			return errTooManyChans
		}
		if in.ParentID != nil {
			if err := checkParent(ctx, tx, guildID, *in.ParentID); err != nil {
				return err
			}
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO channels (id, guild_id, type, name, topic, parent_id, position, user_limit, bitrate)
			 VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, $8, $9)`,
			id, guildID, in.Type, in.Name, in.Topic, in.ParentID, maxPos+1, in.UserLimit, in.Bitrate); err != nil {
			return err
		}
		ch, err = getChannel(ctx, tx, id)
		if err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{Topic: events.GuildTopic(guildID), Type: events.ChannelCreate, Data: ch})
	})
	return ch, err
}

func checkParent(ctx context.Context, q querier, guildID, parentID uuid.UUID) error {
	p, err := getChannel(ctx, q, parentID)
	if errors.Is(err, errNotFound) || (err == nil && (p.GuildID != guildID || p.Type != ChannelCategory)) {
		return validation(map[string]string{"parent_id": "Нет такой категории на этом сервере"})
	}
	return err
}

// withChannel resolves a guild channel and the caller's permissions in it.
// A channel the caller cannot view is reported as not found.
func (s *Service) withChannel(ctx context.Context, userID, channelID uuid.UUID, fn func(pgx.Tx, access, Channel, perm.Permissions) error) error {
	ch, err := getChannel(ctx, s.pg, channelID)
	if err != nil {
		return notFound(err)
	}
	return s.mutate(ctx, ch.GuildID, userID, func(tx pgx.Tx, a access) error {
		// Re-read under the guild lock: the channel may have changed.
		ch, err := getChannel(ctx, tx, channelID)
		if err != nil {
			return notFound(err)
		}
		p := a.in(ch)
		if !p.Has(perm.ViewChannel) {
			return ErrNotFound
		}
		return fn(tx, a, ch, p)
	})
}

type UpdateChannelInput struct {
	Name      *string             `json:"name"`
	Topic     *string             `json:"topic"`
	ParentID  optional[uuid.UUID] `json:"parent_id"`
	Position  *int                `json:"position"`
	UserLimit *int                `json:"user_limit"`
	Bitrate   *int                `json:"bitrate"`
}

func (s *Service) UpdateChannel(ctx context.Context, userID, channelID uuid.UUID, in UpdateChannelInput) (Channel, error) {
	var out Channel
	err := s.withChannel(ctx, userID, channelID, func(tx pgx.Tx, _ access, ch Channel, p perm.Permissions) error {
		if !p.Has(perm.ManageChannels) {
			return httpx.ErrForbidden
		}
		if in.Name != nil {
			ch.Name = strings.TrimSpace(*in.Name)
		}
		if in.Topic != nil {
			ch.Topic = strings.TrimSpace(*in.Topic)
		}
		if in.Position != nil {
			ch.Position = *in.Position
		}
		limit, bitrate := 0, 0
		if ch.Type == ChannelVoice {
			limit, bitrate = ch.UserLimit, ch.Bitrate
		}
		if in.UserLimit != nil {
			limit = *in.UserLimit
		}
		if in.Bitrate != nil {
			bitrate = *in.Bitrate
		}
		fields := map[string]string{}
		validateChannel(fields, ch.Type, ch.Name, ch.Topic, limit, bitrate)
		if ch.Position < 0 {
			fields["position"] = "Не меньше 0"
		}
		if in.ParentID.Set {
			ch.ParentID = in.ParentID.Value
			if ch.ParentID != nil && ch.Type == ChannelCategory {
				fields["parent_id"] = "Категория не может быть вложенной"
			}
		}
		if err := validation(fields); err != nil {
			return err
		}
		if ch.ParentID != nil && in.ParentID.Set {
			if err := checkParent(ctx, tx, ch.GuildID, *ch.ParentID); err != nil {
				return err
			}
		}
		if ch.Type == ChannelVoice {
			ch.UserLimit, ch.Bitrate = limit, bitrate
		}
		if _, err := tx.Exec(ctx,
			`UPDATE channels SET name = $2, topic = NULLIF($3, ''), parent_id = $4, position = $5,
			        user_limit = $6, bitrate = COALESCE(NULLIF($7, 0), bitrate)
			 WHERE id = $1`,
			ch.ID, ch.Name, ch.Topic, ch.ParentID, ch.Position, ch.UserLimit, ch.Bitrate); err != nil {
			return err
		}
		var err error
		out, err = getChannel(ctx, tx, ch.ID)
		if err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{Topic: events.GuildTopic(ch.GuildID), Type: events.ChannelUpdate, Data: out})
	})
	return out, err
}

func (s *Service) DeleteChannel(ctx context.Context, userID, channelID uuid.UUID) error {
	return s.withChannel(ctx, userID, channelID, func(tx pgx.Tx, _ access, ch Channel, p perm.Permissions) error {
		if !p.Has(perm.ManageChannels) {
			return httpx.ErrForbidden
		}
		if _, err := tx.Exec(ctx, `DELETE FROM channels WHERE id = $1`, ch.ID); err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{
			Topic: events.GuildTopic(ch.GuildID), Type: events.ChannelDelete,
			Data: map[string]any{"id": ch.ID, "guild_id": ch.GuildID},
		})
	})
}

// ListChannels returns the guild's channels that userID can view.
func (s *Service) ListChannels(ctx context.Context, userID, guildID uuid.UUID) ([]Channel, error) {
	f, err := s.Get(ctx, userID, guildID)
	return f.Channels, err
}

type OverwriteInput struct {
	Type  perm.TargetType  `json:"type"`
	Allow perm.Permissions `json:"allow"`
	Deny  perm.Permissions `json:"deny"`
}

// PutOverwrite sets the overwrite for a role or member in a channel. Like in
// Discord, the caller needs MANAGE_ROLES in that channel and may only allow or
// deny permissions they have there, so an overwrite cannot escalate anyone
// above the caller.
func (s *Service) PutOverwrite(ctx context.Context, userID, channelID, targetID uuid.UUID, in OverwriteInput) (Channel, error) {
	fields := map[string]string{}
	if in.Type != perm.TargetRole && in.Type != perm.TargetMember {
		fields["type"] = "1 (роль) или 2 (участник)"
	}
	if in.Allow&^perm.All != 0 {
		fields["allow"] = "Неизвестные биты"
	}
	if in.Deny&^perm.All != 0 {
		fields["deny"] = "Неизвестные биты"
	}
	if in.Allow&in.Deny != 0 {
		fields["deny"] = "Бит не может быть одновременно в allow и deny"
	}
	if err := validation(fields); err != nil {
		return Channel{}, err
	}
	var out Channel
	err := s.withChannel(ctx, userID, channelID, func(tx pgx.Tx, a access, ch Channel, p perm.Permissions) error {
		if !p.Has(perm.ManageRoles) || !p.Has(in.Allow|in.Deny) {
			return httpx.ErrForbidden
		}
		for _, o := range ch.Overwrites {
			if o.Type == in.Type && o.ID == targetID && !p.Has(o.Allow|o.Deny) {
				return httpx.ErrForbidden
			}
		}
		switch in.Type {
		case perm.TargetRole:
			if _, ok := a.role(targetID); !ok {
				return validation(map[string]string{"target_id": "Нет такой роли на сервере"})
			}
		case perm.TargetMember:
			if _, err := getMember(ctx, tx, ch.GuildID, targetID); err != nil {
				if errors.Is(err, errNotFound) {
					return validation(map[string]string{"target_id": "Нет такого участника на сервере"})
				}
				return err
			}
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO permission_overwrites (channel_id, target_type, target_id, allow, deny)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (channel_id, target_type, target_id) DO UPDATE SET allow = $4, deny = $5`,
			ch.ID, in.Type, targetID, int64(in.Allow), int64(in.Deny)); err != nil { //nolint:gosec // bits fit in 63
			return err
		}
		var err error
		out, err = getChannel(ctx, tx, ch.ID)
		if err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{Topic: events.GuildTopic(ch.GuildID), Type: events.ChannelUpdate, Data: out})
	})
	return out, err
}

// DeleteOverwrite removes every overwrite for targetID in the channel.
func (s *Service) DeleteOverwrite(ctx context.Context, userID, channelID, targetID uuid.UUID) error {
	return s.withChannel(ctx, userID, channelID, func(tx pgx.Tx, _ access, ch Channel, p perm.Permissions) error {
		if !p.Has(perm.ManageRoles) {
			return httpx.ErrForbidden
		}
		found := false
		for _, o := range ch.Overwrites {
			if o.ID != targetID {
				continue
			}
			found = true
			if !p.Has(o.Allow | o.Deny) {
				return httpx.ErrForbidden
			}
		}
		if !found {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `DELETE FROM permission_overwrites WHERE channel_id = $1 AND target_id = $2`, ch.ID, targetID); err != nil {
			return err
		}
		out, err := getChannel(ctx, tx, ch.ID)
		if err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{Topic: events.GuildTopic(ch.GuildID), Type: events.ChannelUpdate, Data: out})
	})
}

// ---- roles ----

func (s *Service) ListRoles(ctx context.Context, userID, guildID uuid.UUID) ([]Role, error) {
	a, err := loadAccess(ctx, s.pg, guildID, userID)
	if err != nil {
		return nil, notFound(err)
	}
	return a.roles, nil
}

type RoleInput struct {
	Name        *string           `json:"name"`
	Permissions *perm.Permissions `json:"permissions"`
	Color       *int              `json:"color"`
	Position    *int              `json:"position"`
}

func (in RoleInput) validate(creating bool) error {
	fields := map[string]string{}
	if in.Name != nil {
		*in.Name = strings.TrimSpace(*in.Name)
		checkName(fields, "name", *in.Name, 1, 100)
	} else if creating {
		fields["name"] = "Обязательное поле"
	}
	if in.Permissions != nil && *in.Permissions&^perm.All != 0 {
		fields["permissions"] = "Неизвестные биты"
	}
	if in.Color != nil && (*in.Color < 0 || *in.Color > 0xFFFFFF) {
		fields["color"] = "От 0 до 16777215"
	}
	if creating && in.Position != nil {
		fields["position"] = "Новая роль создаётся над @everyone, порядок меняется через PATCH"
	}
	return validation(fields)
}

// CreateRole adds a role right above @everyone (position 1).
func (s *Service) CreateRole(ctx context.Context, userID, guildID uuid.UUID, in RoleInput) (Role, error) {
	if err := in.validate(true); err != nil {
		return Role{}, err
	}
	if err := s.limiter.Check(ctx, ruleRoleCreate, userID.String()); err != nil {
		return Role{}, err
	}
	r := Role{GuildID: guildID, Name: *in.Name, Position: 1}
	if in.Permissions != nil {
		r.Permissions = *in.Permissions
	}
	if in.Color != nil {
		r.Color = *in.Color
	}
	err := s.mutate(ctx, guildID, userID, func(tx pgx.Tx, a access) error {
		if len(a.roles) >= maxRolesPerGuild {
			return errTooManyRoles
		}
		// Position 0 here means "below every existing role": after the shift
		// the new role sits under the caller's top role whatever it is.
		if !perm.CanAssignRole(a.pg, a.member, perm.Role{ID: uuid.Nil, Position: 0, Permissions: r.Permissions}) {
			return httpx.ErrForbidden
		}
		var err error
		if r.ID, err = uuid.NewV7(); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE roles SET position = position + 1 WHERE guild_id = $1 AND position >= 1`, guildID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO roles (id, guild_id, name, position, permissions, color) VALUES ($1, $2, $3, 1, $4, $5)`,
			r.ID, guildID, r.Name, int64(r.Permissions), r.Color); err != nil { //nolint:gosec // bits fit in 63
			return err
		}
		return writeRoles(ctx, tx, guildID, events.RoleCreate, r)
	})
	return r, err
}

// writeRoles emits the changed role. Positions of other roles may have
// shifted too, so clients get the whole ordered list as well.
func writeRoles(ctx context.Context, tx pgx.Tx, guildID uuid.UUID, typ string, r Role) error {
	roles, err := listRoles(ctx, tx, guildID)
	if err != nil {
		return err
	}
	return events.Write(ctx, tx, events.Event{
		Topic: events.GuildTopic(guildID), Type: typ,
		Data: map[string]any{"guild_id": guildID, "role": r, "roles": roles},
	})
}

// UpdateRole edits a role. The caller must be able to assign the role both
// before and after the change, so nobody can raise a role to or above their
// own, or give it permissions they lack.
func (s *Service) UpdateRole(ctx context.Context, userID, guildID, roleID uuid.UUID, in RoleInput) (Role, error) {
	if err := in.validate(false); err != nil {
		return Role{}, err
	}
	var out Role
	err := s.mutate(ctx, guildID, userID, func(tx pgx.Tx, a access) error {
		r, ok := a.role(roleID)
		if !ok {
			return ErrNotFound
		}
		everyone := r.ID == guildID
		if everyone {
			// @everyone has no position to assign, so only MANAGE_ROLES and
			// the no-escalation rule apply.
			if in.Name != nil || in.Position != nil {
				return errEveryoneEdit
			}
			base := a.base()
			if !base.Has(perm.ManageRoles) || (in.Permissions != nil && !base.Has(*in.Permissions)) {
				return httpx.ErrForbidden
			}
		} else if !perm.CanAssignRole(a.pg, a.member, perm.Role{ID: r.ID, Position: r.Position, Permissions: r.Permissions}) {
			return httpx.ErrForbidden
		}

		next := r
		if in.Name != nil {
			next.Name = *in.Name
		}
		if in.Permissions != nil {
			next.Permissions = *in.Permissions
		}
		if in.Color != nil {
			next.Color = *in.Color
		}
		if in.Position != nil {
			next.Position = *in.Position
			if next.Position < 1 || next.Position > len(a.roles)-1 {
				return validation(map[string]string{"position": "От 1 до " + itoa(len(a.roles)-1)})
			}
		}
		if !everyone && !perm.CanAssignRole(a.pg, a.member, perm.Role{ID: next.ID, Position: next.Position, Permissions: next.Permissions}) {
			return httpx.ErrForbidden
		}

		if next.Position != r.Position {
			if err := moveRole(ctx, tx, guildID, r.Position, next.Position); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE roles SET name = $2, permissions = $3, color = $4, position = $5 WHERE id = $1`,
			r.ID, next.Name, int64(next.Permissions), next.Color, next.Position); err != nil { //nolint:gosec // bits fit in 63
			return err
		}
		out = next
		return writeRoles(ctx, tx, guildID, events.RoleUpdate, out)
	})
	return out, err
}

// moveRole shifts the roles between from and to by one so that the role at
// from can take position to. Positions stay contiguous.
func moveRole(ctx context.Context, tx pgx.Tx, guildID uuid.UUID, from, to int) error {
	var sql string
	if to > from {
		sql = `UPDATE roles SET position = position - 1 WHERE guild_id = $1 AND position > $2 AND position <= $3`
	} else {
		sql = `UPDATE roles SET position = position + 1 WHERE guild_id = $1 AND position >= $3 AND position < $2`
	}
	_, err := tx.Exec(ctx, sql, guildID, from, to)
	return err
}

func (s *Service) DeleteRole(ctx context.Context, userID, guildID, roleID uuid.UUID) error {
	return s.mutate(ctx, guildID, userID, func(tx pgx.Tx, a access) error {
		r, ok := a.role(roleID)
		if !ok {
			return ErrNotFound
		}
		if r.ID == guildID {
			return errEveryoneDelete
		}
		if !perm.CanAssignRole(a.pg, a.member, perm.Role{ID: r.ID, Position: r.Position, Permissions: r.Permissions}) {
			return httpx.ErrForbidden
		}
		if _, err := tx.Exec(ctx, `DELETE FROM roles WHERE id = $1`, r.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE roles SET position = position - 1 WHERE guild_id = $1 AND position > $2`, guildID, r.Position); err != nil {
			return err
		}
		return writeRoles(ctx, tx, guildID, events.RoleDelete, r)
	})
}

// ---- members ----

// AddMember puts userID into the guild. Invites call it once they are
// validated; it does no permission check of its own.
func (s *Service) AddMember(ctx context.Context, guildID, userID uuid.UUID) (Full, error) {
	var full Full
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := lockGuild(ctx, tx, guildID); err != nil {
			return notFound(err)
		}
		var err error
		full, err = s.addMember(ctx, tx, guildID, userID)
		return err
	})
	return full, err
}

// addMember inserts the member and writes the events. The caller holds the guild lock.
func (s *Service) addMember(ctx context.Context, tx pgx.Tx, guildID, userID uuid.UUID) (Full, error) {
	if err := checkGuildCount(ctx, tx, userID); err != nil {
		return Full{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO members (guild_id, user_id) VALUES ($1, $2)`, guildID, userID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Full{}, errAlreadyMember
		}
		return Full{}, err
	}
	m, err := getMember(ctx, tx, guildID, userID)
	if err != nil {
		return Full{}, err
	}
	if err := s.attachUsers(ctx, []*Member{&m}); err != nil {
		return Full{}, err
	}
	full, err := s.full(ctx, tx, guildID, userID)
	if err != nil {
		return Full{}, err
	}
	return full, events.Write(ctx, tx,
		events.Event{Topic: events.GuildTopic(guildID), Type: events.MemberAdd, Data: m},
		events.Event{Topic: events.UserTopic(userID), Type: events.GuildCreate, Data: full},
	)
}

func (s *Service) attachUsers(ctx context.Context, ms []*Member) error {
	return attachUsers(ctx, s.users, ms, func(m *Member) *uuid.UUID { return &m.UserID },
		func(m *Member, u auth.PublicUser) { m.User = u })
}

// ptrs returns pointers to the elements of xs, for helpers that fill them in place.
func ptrs[T any](xs []T) []*T {
	out := make([]*T, len(xs))
	for i := range xs {
		out[i] = &xs[i]
	}
	return out
}

// attachUsers looks up the public profile behind each item's user id in one
// query and stores it with set. Items without a user id are skipped.
func attachUsers[T any](ctx context.Context, dir UserDirectory, items []*T, id func(*T) *uuid.UUID, set func(*T, auth.PublicUser)) error {
	ids := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		if p := id(it); p != nil {
			ids = append(ids, *p)
		}
	}
	users, err := dir.Users(ctx, ids)
	if err != nil {
		return err
	}
	for _, it := range items {
		if p := id(it); p != nil {
			if u, ok := users[*p]; ok {
				set(it, u)
			}
		}
	}
	return nil
}

// ListMembers pages through members ordered by user id.
func (s *Service) ListMembers(ctx context.Context, userID, guildID, after uuid.UUID, limit int) ([]Member, error) {
	if _, err := getMember(ctx, s.pg, guildID, userID); err != nil {
		return nil, notFound(err)
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	ms, err := listMembers(ctx, s.pg, guildID, after, limit)
	if err != nil {
		return nil, err
	}
	return ms, s.attachUsers(ctx, ptrs(ms))
}

// SetMemberRole adds (add=true) or removes a role. The caller must be able to
// assign the role and must outrank the target, unless the target is the
// caller themself.
func (s *Service) SetMemberRole(ctx context.Context, userID, guildID, targetID, roleID uuid.UUID, add bool) error {
	return s.mutate(ctx, guildID, userID, func(tx pgx.Tx, a access) error {
		r, ok := a.role(roleID)
		if !ok {
			return ErrNotFound
		}
		if r.ID == guildID {
			return errEveryoneAssign
		}
		target, err := getMember(ctx, tx, guildID, targetID)
		if err != nil {
			return notFound(err)
		}
		if !perm.CanAssignRole(a.pg, a.member, perm.Role{ID: r.ID, Position: r.Position, Permissions: r.Permissions}) {
			return httpx.ErrForbidden
		}
		if targetID != userID && !perm.Outranks(a.pg, a.member, a.permMember(target)) {
			return httpx.ErrForbidden
		}
		var tag pgconn.CommandTag
		if add {
			tag, err = tx.Exec(ctx, `INSERT INTO member_roles (guild_id, user_id, role_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, guildID, targetID, roleID)
		} else {
			tag, err = tx.Exec(ctx, `DELETE FROM member_roles WHERE guild_id = $1 AND user_id = $2 AND role_id = $3`, guildID, targetID, roleID)
		}
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return s.writeMemberUpdate(ctx, tx, guildID, targetID)
	})
}

func (s *Service) writeMemberUpdate(ctx context.Context, tx pgx.Tx, guildID, userID uuid.UUID) error {
	m, err := getMember(ctx, tx, guildID, userID)
	if err != nil {
		return err
	}
	if err := s.attachUsers(ctx, []*Member{&m}); err != nil {
		return err
	}
	return events.Write(ctx, tx, events.Event{Topic: events.GuildTopic(guildID), Type: events.MemberUpdate, Data: m})
}

// RemoveMember kicks targetID, or leaves the guild when targetID is the caller.
func (s *Service) RemoveMember(ctx context.Context, userID, guildID, targetID uuid.UUID) error {
	return s.mutate(ctx, guildID, userID, func(tx pgx.Tx, a access) error {
		if targetID == userID {
			if a.guild.OwnerID == userID {
				return errOwnerCannotGo
			}
		} else {
			target, err := getMember(ctx, tx, guildID, targetID)
			if err != nil {
				return notFound(err)
			}
			if !a.base().Has(perm.KickMembers) || !perm.Outranks(a.pg, a.member, a.permMember(target)) {
				return httpx.ErrForbidden
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM members WHERE guild_id = $1 AND user_id = $2`, guildID, targetID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM permission_overwrites o USING channels c
			 WHERE o.channel_id = c.id AND c.guild_id = $1 AND o.target_type = 2 AND o.target_id = $2`,
			guildID, targetID); err != nil {
			return err
		}
		return events.Write(ctx, tx,
			events.Event{Topic: events.GuildTopic(guildID), Type: events.MemberRemove,
				Data: map[string]any{"guild_id": guildID, "user_id": targetID}},
			events.Event{Topic: events.UserTopic(targetID), Type: events.GuildDelete,
				Data: map[string]any{"id": guildID}},
		)
	})
}

type UpdateSelfInput struct {
	Nick optional[string] `json:"nick"`
}

// UpdateSelf changes the caller's own nickname (null or "" clears it).
func (s *Service) UpdateSelf(ctx context.Context, userID, guildID uuid.UUID, in UpdateSelfInput) error {
	var nick *string
	if in.Nick.Value != nil {
		n := strings.TrimSpace(*in.Nick.Value)
		if n != "" {
			nick = &n
		}
	}
	if nick != nil {
		fields := map[string]string{}
		checkName(fields, "nick", *nick, 1, 32)
		if err := validation(fields); err != nil {
			return err
		}
	}
	if !in.Nick.Set {
		return nil
	}
	return s.mutate(ctx, guildID, userID, func(tx pgx.Tx, a access) error {
		if !a.base().Has(perm.ChangeNickname) {
			return httpx.ErrForbidden
		}
		if _, err := tx.Exec(ctx, `UPDATE members SET nick = $3 WHERE guild_id = $1 AND user_id = $2`, guildID, userID, nick); err != nil {
			return err
		}
		return s.writeMemberUpdate(ctx, tx, guildID, userID)
	})
}
