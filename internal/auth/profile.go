package auth

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/events"
	"github.com/eastsidekillas/ruscord-core/internal/platform/blob"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/eastsidekillas/ruscord-core/internal/platform/imaging"
	"github.com/eastsidekillas/ruscord-core/internal/presence"
)

// GuildIDs is what auth needs from the guild module; guild.Service
// implements it. A change of what others see of the user (name, avatar)
// goes to each of their guilds; an account that owns guilds cannot be
// deleted until they are deleted.
type GuildIDs interface {
	GuildIDs(ctx context.Context, user uuid.UUID) ([]uuid.UUID, error)
	OwnedGuildCount(ctx context.Context, user uuid.UUID) (int, error)
}

// SetGuildIDs connects the guild module for the profile fan-out. It is set
// after construction because the guild module depends on auth.
func (s *Service) SetGuildIDs(g GuildIDs) { s.guilds = g }

var (
	errNoChanges = httpx.Validation(map[string]string{"": "Укажите хотя бы одно поле: display_name, bio, status"})
)

// UpdateMeInput is the body of PATCH /users/@me; absent fields stay as they are.
type UpdateMeInput struct {
	DisplayName *string `json:"display_name"`
	Bio         *string `json:"bio"`
	Status      *string `json:"status"`
}

// UpdateMe changes the caller's own profile and settings.
func (s *Service) UpdateMe(ctx context.Context, p Principal, in UpdateMeInput) (User, error) {
	if in.DisplayName == nil && in.Bio == nil && in.Status == nil {
		return User{}, errNoChanges
	}
	fields := httpx.Fields{}
	if in.DisplayName != nil {
		n := strings.TrimSpace(*in.DisplayName)
		in.DisplayName = &n
		fields.Length("display_name", n, 1, 32)
	}
	if in.Bio != nil {
		b := strings.TrimSpace(*in.Bio)
		in.Bio = &b
		fields.Length("bio", b, 0, 190)
	}
	if err := fields.Err(); err != nil {
		return User{}, err
	}
	if in.Status != nil && !presence.ValidChoice(presence.Status(*in.Status)) {
		return User{}, httpx.Validation(map[string]string{"status": "Одно из: online, idle, dnd, invisible"})
	}
	if err := s.limit(ctx, ruleProfile, p.UserID.String()); err != nil {
		return User{}, err
	}
	return s.change(ctx, p.UserID, func(tx pgx.Tx, cur current) (bool, error) {
		_, err := tx.Exec(ctx,
			`UPDATE users SET display_name = COALESCE($2, display_name), bio = COALESCE($3, bio),
			                  status = COALESCE($4, status), updated_at = now()
			 WHERE id = $1`, p.UserID, in.DisplayName, in.Bio, in.Status)
		return in.DisplayName != nil && *in.DisplayName != cur.displayName, err
	})
}

// SetAvatar makes an uploaded picture the caller's avatar.
func (s *Service) SetAvatar(ctx context.Context, p Principal, upload []byte) (User, error) {
	if err := s.limit(ctx, RuleImageUpload, p.UserID.String()); err != nil {
		return User{}, err
	}
	key, err := imaging.Store(ctx, s.files, blob.KindAvatar, p.UserID, upload)
	if err != nil {
		return User{}, err
	}
	return s.setAvatar(ctx, p.UserID, &key)
}

// DeleteAvatar removes the caller's avatar.
func (s *Service) DeleteAvatar(ctx context.Context, p Principal) (User, error) {
	if err := s.limit(ctx, ruleProfile, p.UserID.String()); err != nil {
		return User{}, err
	}
	return s.setAvatar(ctx, p.UserID, nil)
}

func (s *Service) setAvatar(ctx context.Context, id uuid.UUID, key *string) (User, error) {
	return s.change(ctx, id, func(tx pgx.Tx, cur current) (bool, error) {
		if blob.SameKey(cur.avatar, key) {
			return false, nil
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET avatar = $2, updated_at = now() WHERE id = $1`, id, key); err != nil {
			return false, err
		}
		if cur.avatar != nil {
			// Others' screens may still show the old file for a while.
			if err := blob.Schedule(ctx, tx, *cur.avatar, blob.ReplacedGrace); err != nil {
				return false, err
			}
		}
		return true, nil
	})
}

// current is the user row as it was before a change, read under its lock.
type current struct {
	displayName string
	avatar      *string
}

// change runs apply on the locked user row, then tells the user's sessions
// (USER_UPDATE with the private user) and, when apply reports that what
// others see changed, every guild of the user (USER_PROFILE_UPDATE with the
// public user). The lock orders concurrent changes of one user, so their
// events leave in the order of their effects.
func (s *Service) change(ctx context.Context, id uuid.UUID, apply func(tx pgx.Tx, cur current) (bool, error)) (User, error) {
	var u User
	err := pgx.BeginFunc(ctx, s.store.pg, func(tx pgx.Tx) error {
		var cur current
		err := tx.QueryRow(ctx, `SELECT display_name, avatar FROM users WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, id).
			Scan(&cur.displayName, &cur.avatar)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.ErrUnauthorized
		}
		if err != nil {
			return err
		}
		public, err := apply(tx, cur)
		if err != nil {
			return err
		}
		if u, err = scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id)); err != nil {
			return err
		}
		evs := []events.Event{{Topic: events.UserTopic(id), Type: events.UserUpdate, Data: u}}
		if public && s.guilds != nil {
			guilds, err := s.guilds.GuildIDs(ctx, id)
			if err != nil {
				return err
			}
			pub := map[string]PublicUser{"user": u.Public()}
			for _, g := range guilds {
				evs = append(evs, events.Event{Topic: events.GuildTopic(g), Type: events.UserProfileUpdate, Data: pub})
			}
		}
		return events.Write(ctx, tx, evs...)
	})
	return u, err
}

// Bio returns a user's "about me"; httpx.ErrNotFound for unknown users.
func (s *Service) Bio(ctx context.Context, id uuid.UUID) (string, error) {
	var bio string
	err := s.store.pg.QueryRow(ctx, `SELECT CASE WHEN deleted_at IS NULL THEN bio ELSE '' END FROM users WHERE id = $1`, id).Scan(&bio)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", httpx.ErrNotFound
	}
	return bio, err
}

// avatarInUse is the blob collector's reference check for avatars.
func (s *Service) avatarInUse(ctx context.Context, key string) (bool, error) {
	var used bool
	err := s.store.pg.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE avatar = $1)`, key).Scan(&used)
	return used, err
}
