package auth

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/eastsidekillas/ruscord-core/internal/platform/blob"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// PublicUser is what other users may see about an account.
type PublicUser struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	// Avatar is the URL path of the avatar (/cdn/...), or null.
	Avatar *string `json:"avatar"`
	// Deleted marks a deleted account; its name and avatar are not shown.
	Deleted bool `json:"deleted,omitempty"`
}

// Public is the part of u that other users may see.
func (u User) Public() PublicUser {
	return PublicUser{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Avatar: u.Avatar}
}

const publicCols = `id, username, display_name, avatar, deleted_at IS NOT NULL`

// deletedName is what others see instead of a deleted account's names.
const deletedName = "Удалённый пользователь"

func scanPublic(row pgx.Row) (PublicUser, error) {
	var u PublicUser
	var avatar *string
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &avatar, &u.Deleted)
	u.Avatar = blob.URLPtr(avatar)
	if u.Deleted {
		// The row keeps its data (docs/adr/0021-bans-account-deletion.md);
		// nobody is shown it.
		u.Username, u.DisplayName, u.Avatar = "deleted", deletedName, nil
	}
	return u, err
}

// Directory resolves public profiles; Service implements it. Other modules
// depend on this interface, not on the users table.
type Directory interface {
	Users(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]PublicUser, error)
}

// Attach looks up the public profile behind each item's user id in one query
// and stores it with set. Items whose id func returns nil are skipped.
func Attach[T any](ctx context.Context, dir Directory, items []*T, id func(*T) *uuid.UUID, set func(*T, PublicUser)) error {
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

// Users returns public profiles by id. Other modules use this instead of
// reading the users table directly. Unknown ids are absent from the map.
func (s *Service) Users(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]PublicUser, error) {
	out := make(map[uuid.UUID]PublicUser, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.store.pg.Query(ctx,
		`SELECT `+publicCols+` FROM users WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		u, err := scanPublic(rows)
		if err != nil {
			return nil, err
		}
		out[u.ID] = u
	}
	return out, rows.Err()
}

// UserByUsername finds a public profile by username, ignoring case.
func (s *Service) UserByUsername(ctx context.Context, username string) (PublicUser, error) {
	if !usernameRe.MatchString(username) {
		return PublicUser{}, httpx.ErrNotFound // cannot exist; also keeps NUL away from Postgres
	}
	u, err := scanPublic(s.store.pg.QueryRow(ctx, `SELECT `+publicCols+` FROM users WHERE username = $1 AND deleted_at IS NULL`, username))
	if errors.Is(err, pgx.ErrNoRows) {
		return PublicUser{}, httpx.ErrNotFound
	}
	return u, err
}
