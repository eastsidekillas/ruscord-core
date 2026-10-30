package guild

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/eastsidekillas/ruscord-core/internal/auth"
	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/slicex"
)

var (
	errBanned    = httpx.NewError(http.StatusForbidden, "banned", "Вы заблокированы на этом сервере")
	errBanSelf   = httpx.NewError(http.StatusBadRequest, "ban_self", "Нельзя заблокировать себя")
	errBanTarget = httpx.NewError(http.StatusForbidden, "forbidden", "Нельзя заблокировать этого участника")
)

// Ban is one entry of a guild's ban list.
type Ban struct {
	UserID      uuid.UUID       `json:"user_id"`
	User        auth.PublicUser `json:"user"`
	Reason      string          `json:"reason"`
	ModeratorID *uuid.UUID      `json:"moderator_id"`
	CreatedAt   time.Time       `json:"created_at"`
}

type BanInput struct {
	Reason string `json:"reason"`
}

// BanMember bans targetID from the guild, removing them if they are a member.
// Needs BAN_MEMBERS and, for a member, a higher top role (like a kick); the
// owner is always a member and outranks everyone, so cannot be banned. A user who is not a member can be banned too, so a
// raider who left cannot come back. Banning again updates the reason.
func (s *Service) BanMember(ctx context.Context, userID, guildID, targetID uuid.UUID, in BanInput) error {
	reason := strings.TrimSpace(in.Reason)
	fields := httpx.Fields{}
	fields.Length("reason", reason, 0, 512)
	if err := fields.Err(); err != nil {
		return err
	}
	if targetID == userID {
		return errBanSelf
	}
	return s.mutate(ctx, guildID, userID, func(tx pgx.Tx, a access) error {
		if !a.base().Has(perm.BanMembers) {
			return httpx.ErrForbidden
		}
		target, err := getMember(ctx, tx, guildID, targetID)
		member := err == nil
		switch {
		case member && !perm.Outranks(a.pg, a.member, a.permMember(target)):
			return errBanTarget
		case err != nil && !errors.Is(err, errNotFound):
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO guild_bans (guild_id, user_id, reason, moderator_id) VALUES ($1, $2, $3, $4)
			 ON CONFLICT (guild_id, user_id) DO UPDATE SET reason = $3, moderator_id = $4`,
			guildID, targetID, reason, userID); err != nil {
			return userNotFound(err)
		}
		if member {
			return dropMember(ctx, tx, guildID, targetID)
		}
		return nil
	})
}

// Unban lifts a ban; needs BAN_MEMBERS.
func (s *Service) Unban(ctx context.Context, userID, guildID, targetID uuid.UUID) error {
	return s.mutate(ctx, guildID, userID, func(tx pgx.Tx, a access) error {
		if !a.base().Has(perm.BanMembers) {
			return httpx.ErrForbidden
		}
		tag, err := tx.Exec(ctx, `DELETE FROM guild_bans WHERE guild_id = $1 AND user_id = $2`, guildID, targetID)
		if err == nil && tag.RowsAffected() == 0 {
			return httpx.ErrNotFound
		}
		return err
	})
}

// ListBans returns the guild's bans, newest first; needs BAN_MEMBERS.
func (s *Service) ListBans(ctx context.Context, userID, guildID uuid.UUID) ([]Ban, error) {
	a, err := loadAccess(ctx, s.pg, guildID, userID)
	if err != nil {
		return nil, notFound(err)
	}
	if !a.base().Has(perm.BanMembers) {
		return nil, httpx.ErrForbidden
	}
	rows, err := s.pg.Query(ctx,
		`SELECT user_id, reason, moderator_id, created_at FROM guild_bans
		 WHERE guild_id = $1 ORDER BY created_at DESC, user_id`, guildID)
	if err != nil {
		return nil, err
	}
	bans, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Ban, error) {
		var b Ban
		err := r.Scan(&b.UserID, &b.Reason, &b.ModeratorID, &b.CreatedAt)
		return b, err
	})
	if err != nil {
		return nil, err
	}
	return bans, auth.Attach(ctx, s.users, slicex.Ptrs(bans), func(b *Ban) *uuid.UUID { return &b.UserID },
		func(b *Ban, u auth.PublicUser) { b.User = u })
}

func checkNotBanned(ctx context.Context, q querier, guildID, userID uuid.UUID) error {
	var banned bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM guild_bans WHERE guild_id = $1 AND user_id = $2)`,
		guildID, userID).Scan(&banned)
	if err == nil && banned {
		return errBanned
	}
	return err
}

// userNotFound turns a reference to a user that does not exist into 404.
func userNotFound(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return httpx.ErrNotFound
	}
	return err
}

// OwnedGuildCount reports how many guilds userID owns; such an account
// cannot be deleted until they are.
func (s *Service) OwnedGuildCount(ctx context.Context, userID uuid.UUID) (int, error) {
	var n int
	err := s.pg.QueryRow(ctx, `SELECT count(*) FROM guilds WHERE owner_id = $1`, userID).Scan(&n)
	return n, err
}

// CleanupAccount takes a deleted account out of every guild, with the usual
// events. A guild it still owns (created in the minutes its access token
// outlived the deletion) is deleted, like its owner would. Idempotent.
func (s *Service) CleanupAccount(ctx context.Context, userID uuid.UUID) error {
	ids, err := s.GuildIDs(ctx, userID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		err := s.RemoveMember(ctx, userID, id, userID)
		if errors.Is(err, errOwnerCannotGo) {
			err = s.Delete(ctx, userID, id)
		}
		if err != nil && !errors.Is(err, httpx.ErrNotFound) {
			return err
		}
	}
	return nil
}
