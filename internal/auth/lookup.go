package auth

import (
	"context"
	"errors"

	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PublicUser is what other users may see about an account.
type PublicUser struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
}

const publicCols = `id, username, display_name`

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
		var u PublicUser
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName); err != nil {
			return nil, err
		}
		out[u.ID] = u
	}
	return out, rows.Err()
}

// UserByUsername finds a public profile by username, ignoring case.
func (s *Service) UserByUsername(ctx context.Context, username string) (PublicUser, error) {
	var u PublicUser
	err := s.store.pg.QueryRow(ctx,
		`SELECT `+publicCols+` FROM users WHERE username = $1`, username).
		Scan(&u.ID, &u.Username, &u.DisplayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return PublicUser{}, httpx.ErrNotFound
	}
	return u, err
}
