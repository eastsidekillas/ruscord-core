package guild

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/slicex"
)

const (
	inviteCodeLen      = 8
	maxInvitesPerGuild = 1000
	defaultInviteAge   = 24 * time.Hour
	maxInviteAge       = 7 * 24 * time.Hour
)

var errTooManyInvites = httpx.NewError(http.StatusBadRequest, "too_many_invites", "Достигнут лимит приглашений на сервере")

type Invite struct {
	Code      string     `json:"code"`
	GuildID   uuid.UUID  `json:"guild_id"`
	ChannelID uuid.UUID  `json:"channel_id"`
	InviterID *uuid.UUID `json:"inviter_id"`
	MaxUses   int        `json:"max_uses"`
	Uses      int        `json:"uses"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
	Inviter   any        `json:"inviter,omitempty"`
}

// InvitePreview is what someone who is not yet a member sees.
type InvitePreview struct {
	Code        string     `json:"code"`
	Guild       inviteRef  `json:"guild"`
	Channel     inviteRef  `json:"channel"`
	Inviter     any        `json:"inviter,omitempty"`
	MemberCount int        `json:"member_count"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

type inviteRef struct {
	ID   uuid.UUID    `json:"id"`
	Name string       `json:"name"`
	Type *ChannelType `json:"type,omitempty"`
	Icon *string      `json:"icon,omitempty"` // guilds only
}

const codeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// newInviteCode returns a random base62 code. 62^8 ≈ 2·10^14 codes plus the
// per-user limit on lookups make guessing a live code impractical.
func newInviteCode() string {
	b := make([]byte, inviteCodeLen)
	for i := 0; i < len(b); {
		var r [1]byte
		_, _ = rand.Read(r[:])
		// 248 = 4·62: rejecting larger bytes keeps the distribution uniform.
		if r[0] < 248 {
			b[i] = codeAlphabet[int(r[0])%len(codeAlphabet)]
			i++
		}
	}
	return string(b)
}

func validCode(code string) bool {
	return len(code) >= 8 && len(code) <= 16 && strings.Trim(code, codeAlphabet) == ""
}

const inviteCols = `code, guild_id, channel_id, inviter_id, max_uses, uses, expires_at, created_at`

func scanInvite(r pgx.Row) (Invite, error) {
	var inv Invite
	err := r.Scan(&inv.Code, &inv.GuildID, &inv.ChannelID, &inv.InviterID, &inv.MaxUses, &inv.Uses, &inv.ExpiresAt, &inv.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invite{}, errNotFound
	}
	return inv, err
}

func getInvite(ctx context.Context, q querier, code string, lock bool) (Invite, error) {
	sql := `SELECT ` + inviteCols + ` FROM invites WHERE code = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	return scanInvite(q.QueryRow(ctx, sql, code))
}

// checkVisible reports httpx.ErrNotFound unless userID, joining with @everyone
// only, could view the invite's channel. An invite to a channel that was
// hidden after the invite was made must not reveal the channel or let anyone in.
func checkVisible(ctx context.Context, q querier, inv Invite, userID uuid.UUID) (Guild, Channel, error) {
	ch, err := getChannel(ctx, q, inv.ChannelID)
	if err != nil {
		return Guild{}, Channel{}, notFound(err)
	}
	a, err := newcomerAccess(ctx, q, inv.GuildID, userID)
	if err != nil {
		return Guild{}, Channel{}, notFound(err)
	}
	if !a.in(ch).Has(perm.ViewChannel) {
		return Guild{}, Channel{}, httpx.ErrNotFound
	}
	return a.guild, ch, nil
}

func (inv Invite) usable(now time.Time) bool {
	if inv.ExpiresAt != nil && !now.Before(*inv.ExpiresAt) {
		return false
	}
	return inv.MaxUses == 0 || inv.Uses < inv.MaxUses
}

type CreateInviteInput struct {
	// MaxAge is in seconds; 0 means the invite never expires. Absent means a day.
	MaxAge  *int `json:"max_age"`
	MaxUses int  `json:"max_uses"`
}

// CreateInvite needs CREATE_INVITE in the channel the invite points to.
func (s *Service) CreateInvite(ctx context.Context, userID, channelID uuid.UUID, in CreateInviteInput) (Invite, error) {
	age := defaultInviteAge
	fields := httpx.Fields{}
	if in.MaxAge != nil {
		// Check the number before multiplying: a huge value would overflow.
		if *in.MaxAge < 0 || *in.MaxAge > int(maxInviteAge/time.Second) {
			fields["max_age"] = "От 0 (бессрочно) до 604800 секунд"
		}
		age = time.Duration(*in.MaxAge) * time.Second
	}
	if in.MaxUses < 0 || in.MaxUses > 100 {
		fields["max_uses"] = "От 0 (без ограничений) до 100"
	}
	if err := fields.Err(); err != nil {
		return Invite{}, err
	}
	if err := s.limiter.Check(ctx, ruleInviteCreate, userID.String()); err != nil {
		return Invite{}, err
	}

	var inv Invite
	err := s.withChannel(ctx, userID, channelID, func(tx pgx.Tx, _ access, ch Channel, p perm.Permissions) error {
		if !p.Has(perm.CreateInvite) || ch.Type == ChannelCategory {
			return httpx.ErrForbidden
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM invites WHERE guild_id = $1`, ch.GuildID).Scan(&n); err != nil {
			return err
		}
		if n >= maxInvitesPerGuild {
			return errTooManyInvites
		}
		var expires *time.Time
		if age > 0 {
			t := s.now().UTC().Add(age).Truncate(time.Microsecond)
			expires = &t
		}
		for range 3 {
			code := newInviteCode()
			tag, err := tx.Exec(ctx,
				`INSERT INTO invites (code, guild_id, channel_id, inviter_id, max_uses, expires_at)
				 VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (code) DO NOTHING`,
				code, ch.GuildID, ch.ID, userID, in.MaxUses, expires)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				inv, err = getInvite(ctx, tx, code, false)
				if err != nil {
					return err
				}
				return events.Write(ctx, tx, events.Event{Topic: events.GuildTopic(ch.GuildID), Type: events.InviteCreate, Data: inv})
			}
		}
		return errors.New("guild: could not generate a unique invite code")
	})
	return inv, err
}

// PreviewInvite shows where a code leads. Expired and used-up codes are not found.
func (s *Service) PreviewInvite(ctx context.Context, userID uuid.UUID, code string) (InvitePreview, error) {
	if err := s.limiter.Check(ctx, ruleInviteUse, userID.String()); err != nil {
		return InvitePreview{}, err
	}
	if !validCode(code) {
		return InvitePreview{}, httpx.ErrNotFound
	}
	inv, err := getInvite(ctx, s.pg, code, false)
	if err != nil {
		return InvitePreview{}, notFound(err)
	}
	if !inv.usable(s.now()) {
		return InvitePreview{}, httpx.ErrNotFound
	}
	g, ch, err := checkVisible(ctx, s.pg, inv, userID)
	if err != nil {
		return InvitePreview{}, err
	}
	var count int
	if err := s.pg.QueryRow(ctx, `SELECT count(*) FROM members WHERE guild_id = $1`, g.ID).Scan(&count); err != nil {
		return InvitePreview{}, err
	}
	typ := ch.Type
	out := InvitePreview{
		Code:        inv.Code,
		Guild:       inviteRef{ID: g.ID, Name: g.Name, Icon: g.Icon},
		Channel:     inviteRef{ID: ch.ID, Name: ch.Name, Type: &typ},
		MemberCount: count,
		ExpiresAt:   inv.ExpiresAt,
	}
	if err := auth.Attach(ctx, s.users, []*InvitePreview{&out}, func(*InvitePreview) *uuid.UUID { return inv.InviterID },
		func(p *InvitePreview, u auth.PublicUser) { p.Inviter = u }); err != nil {
		return InvitePreview{}, err
	}
	return out, nil
}

// AcceptInvite adds the caller to the invite's guild. A caller who is already
// a member gets the guild back without using up the invite.
func (s *Service) AcceptInvite(ctx context.Context, userID uuid.UUID, code string) (Full, error) {
	if err := s.limiter.Check(ctx, ruleInviteUse, userID.String()); err != nil {
		return Full{}, err
	}
	if !validCode(code) {
		return Full{}, httpx.ErrNotFound
	}
	inv, err := getInvite(ctx, s.pg, code, false)
	if err != nil {
		return Full{}, notFound(err)
	}
	var full Full
	err = s.tx(ctx, func(tx pgx.Tx) error {
		// Lock order everywhere is guild, then invite (ADR-0012).
		if err := lockGuild(ctx, tx, inv.GuildID); err != nil {
			return notFound(err)
		}
		inv, err := getInvite(ctx, tx, code, true)
		if err != nil {
			return notFound(err)
		}
		if !inv.usable(s.now()) {
			return httpx.ErrNotFound
		}
		if _, err := getMember(ctx, tx, inv.GuildID, userID); err == nil {
			full, err = s.full(ctx, tx, inv.GuildID, userID)
			return err
		} else if !errors.Is(err, errNotFound) {
			return err
		}
		if _, _, err := checkVisible(ctx, tx, inv, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE invites SET uses = uses + 1 WHERE code = $1`, code); err != nil {
			return err
		}
		full, err = s.addMember(ctx, tx, inv.GuildID, userID)
		return err
	})
	return full, err
}

// ListInvites needs MANAGE_GUILD.
func (s *Service) ListInvites(ctx context.Context, userID, guildID uuid.UUID) ([]Invite, error) {
	a, err := loadAccess(ctx, s.pg, guildID, userID)
	if err != nil {
		return nil, notFound(err)
	}
	if !a.base().Has(perm.ManageGuild) {
		return nil, httpx.ErrForbidden
	}
	rows, err := s.pg.Query(ctx, `SELECT `+inviteCols+` FROM invites WHERE guild_id = $1 ORDER BY created_at, code`, guildID)
	if err != nil {
		return nil, err
	}
	invs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Invite, error) { return scanInvite(r) })
	if err != nil {
		return nil, err
	}
	if err := auth.Attach(ctx, s.users, slicex.Ptrs(invs), func(i *Invite) *uuid.UUID { return i.InviterID },
		func(i *Invite, u auth.PublicUser) { i.Inviter = u }); err != nil {
		return nil, err
	}
	return invs, nil
}

// DeleteInvite is allowed to its creator, to MANAGE_CHANNELS in its channel
// and to MANAGE_GUILD.
func (s *Service) DeleteInvite(ctx context.Context, userID uuid.UUID, code string) error {
	if !validCode(code) {
		return httpx.ErrNotFound
	}
	inv, err := getInvite(ctx, s.pg, code, false)
	if err != nil {
		return notFound(err)
	}
	return s.mutate(ctx, inv.GuildID, userID, func(tx pgx.Tx, a access) error {
		inv, err := getInvite(ctx, tx, code, true)
		if err != nil {
			return notFound(err)
		}
		allowed := a.base().Has(perm.ManageGuild) || (inv.InviterID != nil && *inv.InviterID == userID)
		if !allowed {
			ch, err := getChannel(ctx, tx, inv.ChannelID)
			if err != nil {
				return notFound(err)
			}
			allowed = a.in(ch).Has(perm.ManageChannels)
		}
		if !allowed {
			return httpx.ErrForbidden
		}
		if _, err := tx.Exec(ctx, `DELETE FROM invites WHERE code = $1`, code); err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{
			Topic: events.GuildTopic(inv.GuildID), Type: events.InviteDelete,
			Data: map[string]any{"code": code, "guild_id": inv.GuildID, "channel_id": inv.ChannelID},
		})
	})
}
